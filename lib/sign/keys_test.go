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
	"testing"
)

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
