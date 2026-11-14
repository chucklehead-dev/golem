package sign

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadKeysRefusesARepeatedID: two files giving one key ID, by header
// or by file name and of either kind, are refused, naming both, rather
// than one shadowing the other.
func TestLoadKeysRefusesARepeatedID(t *testing.T) {
	edPub, _, _ := ed25519.GenerateKey(nil)
	edPub2, _, _ := ed25519.GenerateKey(nil)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	type file struct {
		name    string
		pub     any
		headers map[string]string
	}
	id := map[string]string{"Key-ID": "k1"}
	for name, files := range map[string][]file{
		"two Ed25519 keys by header":       {{"a.pub", edPub, id}, {"b.pub", edPub2, id}},
		"an Ed25519 and an ECDSA key":      {{"a.pub", &ec.PublicKey, id}, {"b.pub", edPub, id}},
		"a header and another file's name": {{"k1.pub", &ec.PublicKey, nil}, {"z.pub", edPub, id}},
		"the same key twice, by header":    {{"a.pub", edPub, id}, {"b.pub", edPub, id}},
	} {
		dir := t.TempDir()
		for _, f := range files {
			der, _ := x509.MarshalPKIXPublicKey(f.pub)
			if err := os.WriteFile(filepath.Join(dir, f.name), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Headers: f.headers, Bytes: der}), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		_, err := LoadKeys(dir)
		if err == nil || !strings.Contains(err.Error(), files[0].name) || !strings.Contains(err.Error(), files[1].name) || !strings.Contains(err.Error(), `key ID "k1"`) {
			t.Errorf("%s: %v, want the repeated key ID refused, naming both files", name, err)
		}
	}
}

// TestLoadKeys: Ed25519 and ECDSA public keys load by file name or Key-ID
// header, and verify their own signatures only.
func TestLoadKeys(t *testing.T) {
	dir := t.TempDir()
	edPub, edPriv, _ := ed25519.GenerateKey(nil)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	write := func(name string, pub any, headers map[string]string) {
		der, _ := x509.MarshalPKIXPublicKey(pub)
		os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Headers: headers, Bytes: der}), 0o644)
	}
	write("publishing.pub", edPub, nil)
	write("kms.pub", &ec.PublicKey, map[string]string{"Key-ID": "arn:aws:kms:eu-west-1:1:key/abc"})
	k, err := LoadKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("m")
	edSig, _ := Ed25519{ID: "publishing", Key: edPriv}.Sign(context.Background(), msg)
	if err := k.Verify("publishing", msg, edSig); err != nil {
		t.Fatalf("Ed25519: %v", err)
	}
	h := sha256.Sum256(msg)
	ecSig, _ := ecdsa.SignASN1(rand.Reader, ec, h[:])
	if err := k.Verify("arn:aws:kms:eu-west-1:1:key/abc", msg, ecSig); err != nil {
		t.Fatalf("ECDSA by Key-ID: %v", err)
	}
	if err := k.Verify("kms", msg, ecSig); err == nil {
		t.Fatal("the file name was trusted though a Key-ID was given")
	}
	if err := k.Verify("publishing", msg, ecSig); err == nil {
		t.Fatal("another key's signature verified")
	}
	if _, err := LoadKeys(t.TempDir()); err == nil {
		t.Fatal("no keys loaded without an error")
	}
}
