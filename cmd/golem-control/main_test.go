package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chucklehead-dev/golem/lib/sign"
)

// TestKeygen: the generated key signs, its public file verifies, the key
// is private to its owner, and keygen never overwrites a key.
func TestKeygen(t *testing.T) {
	dir := t.TempDir()
	if err := keygen(dir); err != nil {
		t.Fatal(err)
	}
	s, err := openSigner(t.Context(), "file:"+filepath.Join(dir, "publishing.key"))
	if err != nil || s.KeyID() != "publishing" {
		t.Fatalf("signer %v %v", s, err)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "publishing.pub"))
	blk, _ := pem.Decode(raw)
	k, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	sig, _ := s.Sign(t.Context(), []byte("m"))
	if err := (sign.Ed25519Keys{"publishing": k.(ed25519.PublicKey)}).Verify("publishing", []byte("m"), sig); err != nil {
		t.Fatalf("the public file does not verify the key's signature: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "publishing.key")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", fi.Mode().Perm())
	}
	if err := keygen(dir); err == nil || !strings.Contains(err.Error(), "never overwrites") {
		t.Fatalf("a second keygen: %v", err)
	}
}

func TestOpenSigner(t *testing.T) {
	for _, spec := range []string{"publishing.key", "vault:x", "file:/nonexistent"} {
		if _, err := openSigner(t.Context(), spec); err == nil {
			t.Errorf("%q accepted", spec)
		}
	}
	if _, err := openBucket(t.Context(), "s3:///prefix"); err == nil {
		t.Error("an s3 URL without a bucket accepted")
	}
}
