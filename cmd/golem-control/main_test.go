package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/chucklehead-dev/golem/control/service"
	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/bucket"
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

func TestAPINeedsTenant(t *testing.T) {
	if _, err := apiServer(t.Context(), "127.0.0.1:0", "", "api://golem", "", nil); err == nil || !strings.Contains(err.Error(), "-entra-tenant") {
		t.Fatalf("an API without a tenant: %v", err)
	}
}

// TestAPIStartsWithoutEntra: a replica started while Entra's key set
// cannot be fetched still serves its API (and so its publisher runs),
// refusing tokens as unavailable, not unauthenticated, until the keys
// arrive in the background.
func TestAPIStartsWithoutEntra(t *testing.T) {
	pk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	set, _ := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &pk.PublicKey, KeyID: "k1", Use: "sig"}}})
	var up atomic.Bool
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !up.Load() {
			http.Error(w, "Entra is down", http.StatusServiceUnavailable)
			return
		}
		w.Write(set)
	}))
	defer jwks.Close()
	srv, err := apiServer(t.Context(), "127.0.0.1:0", "tid", "api://golem-control", jwks.URL, &service.Service{State: &state.Store{}})
	if err != nil {
		t.Fatalf("the API with Entra unreachable: %v; it must start, and the publisher with it", err)
	}
	s, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: pk}, (&jose.SignerOptions{}).WithHeader("kid", "other"))
	tok, _ := jwt.Signed(s).Claims(map[string]any{"iss": "https://login.microsoftonline.com/tid/v2.0", "aud": "api://golem-control"}).Serialize()
	call := func() int {
		r := httptest.NewRequest("POST", "/golem.org.v1.OrgService/Whoami", strings.NewReader("{}"))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		srv.Handler.ServeHTTP(w, r)
		return w.Code
	}
	if code := call(); code != http.StatusServiceUnavailable {
		t.Fatalf("a token before the keys: %d, want 503 (unavailable: retry)", code)
	}
	up.Store(true)
	deadline := time.Now().Add(10 * time.Second)
	for call() == http.StatusServiceUnavailable {
		if time.Now().After(deadline) {
			t.Fatal("the keys were never fetched in the background")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if code := call(); code != http.StatusUnauthorized {
		t.Fatalf("a token of an unknown key once the keys are fetched: %d, want 401", code)
	}
}

// TestPubkey: the public half golem-control prints is what a gateway's
// sign.LoadKeys trusts, under the key ID the signer signs with.
func TestPubkey(t *testing.T) {
	dir := t.TempDir()
	if err := keygen(dir); err != nil {
		t.Fatal(err)
	}
	spec := "file:" + filepath.Join(dir, "publishing.key")
	var out bytes.Buffer
	if err := pubkey(t.Context(), &out, spec); err != nil {
		t.Fatal(err)
	}
	keys := t.TempDir()
	os.WriteFile(filepath.Join(keys, "any-name.pub"), out.Bytes(), 0o644)
	k, err := sign.LoadKeys(keys)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := openSigner(t.Context(), spec)
	sig, _ := s.Sign(t.Context(), []byte("m"))
	if err := k.Verify(s.KeyID(), []byte("m"), sig); err != nil {
		t.Fatalf("the printed key does not verify the signer's signature under its ID: %v", err)
	}
}

// TestRestoreCommand: a dry run shows what a restore would do and writes
// nothing; -apply writes it, and a group bound to a scope since the backup
// is gone.
func TestRestoreCommand(t *testing.T) {
	dir := t.TempDir()
	b, err := bucket.OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	st := &state.Store{Bucket: b}
	ctx := t.Context()
	if _, err := st.Update(ctx, func(d *state.Document) error {
		d.Scopes = map[string]state.Scope{"a": {Groups: []string{"g1"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	raw, err := st.Current().Encode()
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup.json")
	os.WriteFile(backup, raw, 0o600)
	if _, err := st.Update(ctx, func(d *state.Document) error {
		d.Scopes["a"] = state.Scope{Groups: []string{"g1", "g2"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	read := func() *state.Document {
		s := &state.Store{Bucket: b}
		if err := s.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		return s.Current()
	}
	var out strings.Builder
	if err := restore(ctx, &out, []string{"-bucket", dir, "-from", backup, "-actor", "ops"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "scope a's group g2") || !strings.Contains(out.String(), "nothing written") || len(read().Scopes["a"].Groups) != 2 {
		t.Fatalf("dry run:\n%s", out.String())
	}
	out.Reset()
	if err := restore(ctx, &out, []string{"-bucket", dir, "-from", backup, "-actor", "ops", "-apply"}); err != nil {
		t.Fatal(err)
	}
	if got := read().Scopes["a"].Groups; len(got) != 1 {
		t.Fatalf("after -apply, scope a's groups %v:\n%s", got, out.String())
	}
	if err := restore(ctx, &out, []string{"-bucket", dir, "-from", backup}); err == nil {
		t.Fatal("a restore with no actor")
	}
}
