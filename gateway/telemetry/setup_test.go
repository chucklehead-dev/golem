package telemetry

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/resource"
)

func TestPlaintextOK(t *testing.T) {
	for host, want := range map[string]bool{"127.0.0.1": true, "127.1.2.3": true, "::1": true,
		"localhost":         false, // a name is resolved when dialed: not held to this machine
		"collector.example": false, "10.0.0.5": false, "localhost.example": false} {
		if plaintextOK(host) != want {
			t.Errorf("plaintextOK(%q) = %v, want %v", host, !want, want)
		}
	}
}

// TestOTLPIgnoresInsecureEnv: the SDK's environment variables cannot turn
// TLS off, nor send telemetry elsewhere (golem#5). With every insecure and
// endpoint variable set against it, a span still reaches the TLS
// collector named, over TLS.
func TestOTLPIgnoresInsecureEnv(t *testing.T) {
	var got atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && r.URL.Path == "/v1/traces" {
			got.Add(1)
		}
	}))
	cert, der := localhostCert(t)
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	srv.StartTLS()
	defer srv.Close()
	ca := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("telemetry went to the environment's endpoint") }))
	defer elsewhere.Close()
	for k, v := range map[string]string{
		"OTEL_EXPORTER_OTLP_INSECURE": "true", "OTEL_EXPORTER_OTLP_TRACES_INSECURE": "true",
		"OTEL_EXPORTER_OTLP_ENDPOINT": elsewhere.URL, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": elsewhere.URL + "/v1/traces",
		"OTEL_EXPORTER_OTLP_CERTIFICATE": ca,
	} {
		t.Setenv(k, v)
	}
	// localhost: a name, so held to TLS (plaintextOK), and in the test
	// server's certificate.
	p, shutdown, err := OTLP(t.Context(), "localhost:"+port, resource.Empty())
	if err != nil {
		t.Fatal(err)
	}
	_, span := p.Tracer.Tracer("t").Start(context.Background(), "s")
	span.End()
	if err := shutdown(t.Context()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got.Load() == 0 {
		t.Fatal("no span reached the collector over TLS")
	}
}

// localhostCert is a self-signed certificate for localhost.
func localhostCert(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, IsCA: true, BasicConstraintsValid: true,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, der
}
