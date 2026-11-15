package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	valkeygo "github.com/valkey-io/valkey-go"
)

// TestValkeyOption: the flags' TLS and AUTH reach the client's options,
// the password only from $VALKEY_PASSWORD, and a half-given configuration
// is refused saying what is missing.
func TestValkeyOption(t *testing.T) {
	dir := t.TempDir()
	ca, _ := testCA(t, dir)
	env := func(pw string) func(string) string {
		return func(k string) string {
			if k == "VALKEY_PASSWORD" {
				return pw
			}
			return ""
		}
	}
	base := options{valkeyAddr: "master.golem.abc123.euw1.cache.amazonaws.com:6379"}

	opt, err := valkeyOption(base, env(""))
	if err != nil || opt.TLSConfig != nil || opt.Username != "" || opt.Password != "" || opt.InitAddress[0] != base.valkeyAddr {
		t.Fatalf("plain: %+v, %v", opt, err)
	}
	withTLS := base
	withTLS.valkeyTLS, withTLS.valkeyUser = true, "golem-gateway"
	opt, err = valkeyOption(withTLS, env("s3cret"))
	switch {
	case err != nil:
		t.Fatal(err)
	case opt.TLSConfig == nil || opt.TLSConfig.RootCAs != nil || opt.TLSConfig.InsecureSkipVerify:
		t.Fatalf("-valkey-tls: %+v; want verification against the system's roots", opt.TLSConfig)
	case opt.TLSConfig.ServerName != "":
		t.Fatalf("ServerName %q: each dial's own host is to be verified (a cluster's nodes)", opt.TLSConfig.ServerName)
	case opt.Username != "golem-gateway" || opt.Password != "s3cret":
		t.Fatalf("AUTH %q/%q", opt.Username, opt.Password)
	}
	withCA := withTLS
	withCA.valkeyCA = ca
	if opt, err = valkeyOption(withCA, env("s3cret")); err != nil || opt.TLSConfig.RootCAs == nil {
		t.Fatalf("-valkey-ca: %v; want its roots", err)
	}

	notPEM := filepath.Join(dir, "not.pem")
	os.WriteFile(notPEM, []byte("not a certificate"), 0o600)
	for name, tc := range map[string]struct {
		o    options
		pw   string
		want string
	}{
		"a CA without TLS":     {options{valkeyAddr: "v:6379", valkeyCA: ca}, "", "add -valkey-tls"},
		"a user, no password":  {options{valkeyAddr: "v:6379", valkeyUser: "golem"}, "", "VALKEY_PASSWORD"},
		"a CA file missing":    {options{valkeyAddr: "v:6379", valkeyTLS: true, valkeyCA: filepath.Join(dir, "none.pem")}, "", "-valkey-ca"},
		"a CA file not in PEM": {options{valkeyAddr: "v:6379", valkeyTLS: true, valkeyCA: notPEM}, "", "PEM"},
	} {
		if _, err := valkeyOption(tc.o, env(tc.pw)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// TestPoolModeValkeyTLS: a pool-mode gateway reaches a Valkey that takes
// only TLS and an ACL user's AUTH (as ElastiCache with in-transit
// encryption and RBAC does) and counts the call's spend there.
func TestPoolModeValkeyTLS(t *testing.T) {
	bin, err := exec.LookPath("valkey-server")
	if err != nil {
		if bin, err = exec.LookPath("redis-server"); err != nil {
			t.Skip("no valkey-server or redis-server")
		}
	}
	dir := t.TempDir()
	ca, pair := testCA(t, dir)
	addr := freeAddr(t)
	_, port, _ := net.SplitHostPort(addr)
	cmd := exec.Command(bin, "--port", "0", "--tls-port", port, "--tls-cert-file", pair[0], "--tls-key-file", pair[1],
		"--tls-ca-cert-file", ca, "--tls-auth-clients", "no", "--save", "", "--appendonly", "no", "--dir", dir,
		"--user", "default", "off", "--user", "golem", "on", ">s3cret", "~*", "&*", "+@all")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
	for i := 0; ; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			break
		}
		if i == 100 {
			t.Skipf("%s did not listen for TLS (built without it?):\n%s", bin, out.String())
		}
		time.Sleep(20 * time.Millisecond) // the server starting: an external process
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	t.Setenv("VALKEY_PASSWORD", "s3cret")
	f := newPoolFixture(t)
	base, done := f.start(t, ctx, addr, "-valkey-tls", "-valkey-ca", ca, "-valkey-user", "golem")
	if code, body := f.call(t, base); code != 200 {
		t.Fatalf("the call: %d %s", code, body)
	}
	roots := x509.NewCertPool()
	pemCA, _ := os.ReadFile(ca)
	roots.AppendCertsFromPEM(pemCA)
	c, err := valkeygo.NewClient(valkeygo.ClientOption{InitAddress: []string{addr}, DisableCache: true,
		TLSConfig: &tls.Config{RootCAs: roots}, Username: "golem", Password: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	keys, err := c.Do(ctx, c.B().Keys().Pattern("{golem:test}:spend:*").Build()).AsStrSlice()
	if err != nil || len(keys) == 0 {
		t.Fatalf("Valkey holds %v (%v): the gateway did not count the call there (it is down: TLS or AUTH not used)", keys, err)
	}
	cancel()
	stopped(t, done)
}

// testCA writes a CA's certificate and a server certificate it signed for
// 127.0.0.1 and localhost into dir: the CA's PEM file, and the server's
// certificate and key files.
func testCA(t *testing.T, dir string) (string, [2]string) {
	t.Helper()
	write := func(name, typ string, der []byte) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test Valkey CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	srvKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	srvTmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "valkey"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(srvKey)
	return write("ca.pem", "CERTIFICATE", caDER), [2]string{write("valkey.crt", "CERTIFICATE", srvDER), write("valkey.key", "EC PRIVATE KEY", keyDER)}
}
