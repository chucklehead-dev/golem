package proxy

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/gateway/guard"
	gsnap "github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/lib/entra"
	"github.com/chucklehead-dev/golem/lib/key"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// TestInvalidKeyFlood: a source sending invalid keys is refused with
// limit.source and a Retry-After once past its failures, its body unread,
// and an Entra token from it refused before its signature is checked;
// but a valid golem key from the same source (a sidecar's address, shared
// by every caller of the pod) is served, as is a caller at another
// address.
func TestInvalidKeyFlood(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	good := key.New(key.Live)
	ents := snapshot.Entitlements{
		Scopes:    []snapshot.Scope{{ID: "team-a", Models: []string{"claude"}}},
		KeyOwners: []snapshot.Owner{{ID: "alice", Kind: "user", Scopes: []string{"team-a"}}},
		Keys:      []snapshot.Key{{Hash: hashHex(good), Principal: "alice", PrincipalKind: "user", Scope: "team-a"}},
	}
	h := handler(AnthropicMessages, &stub{events: anthropicStream(1)}, nil)
	c, _ := h.Snapshots.Current()
	c.Auth = auth.Compile(ents, key.Live, func() time.Time { return now }, nil)
	h.Snapshots = gsnap.Static(c)
	h.Guard = &guard.Guard{FailRate: 1, FailBurst: 5, Now: func() time.Time { return now }}

	call := func(addr, k string, body *countingReader) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/messages", body)
		r.RemoteAddr = addr + ":1234"
		r.Header.Set("X-Api-Key", k)
		r.Header.Set("Anthropic-Version", "2023-06-01")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	bad := key.New(key.Live)
	for i := range 5 {
		if w := call("6.6.6.6", bad, &countingReader{r: bytes.NewReader(smallRequest)}); w.Code != 401 {
			t.Fatalf("failure %d: status %d, want 401 within the burst", i, w.Code)
		}
	}
	for _, cred := range []string{bad, "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln"} {
		body := &countingReader{r: bytes.NewReader(smallRequest)}
		w := call("6.6.6.6", cred, body)
		if w.Code != 429 || !strings.Contains(w.Body.String(), "limit.source") || w.Header().Get("Retry-After") != "1" {
			t.Fatalf("past the failures: %d %s (Retry-After %q), want 429 limit.source", w.Code, w.Body, w.Header().Get("Retry-After"))
		}
		if body.n != 0 {
			t.Fatalf("read %d bytes of a refused source's body", body.n)
		}
	}
	if w := call("6.6.6.6", good, &countingReader{r: bytes.NewReader(smallRequest)}); w.Code != 200 {
		t.Fatalf("a valid key from the flooding address: %d %s; want it served", w.Code, w.Body)
	}
	if w := call("10.1.1.1", good, &countingReader{r: bytes.NewReader(smallRequest)}); w.Code != 200 {
		t.Fatalf("another address with a valid key: %d %s", w.Code, w.Body)
	}
}

type countingReader struct {
	r interface{ Read([]byte) (int, error) }
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

// TestEntraKeyTroubleIsNoFailure: refusals that are golem's or Entra's
// fault, not the credential's (Entra's keys not yet fetched, too old, or
// without the key a token names), do not count against the source's
// failures: an Entra outage must not lock out every caller behind a shared
// address. A bad key's refusal does count (the contrast).
func TestEntraKeyTroubleIsNoFailure(t *testing.T) {
	now := time.Date(2026, 11, 15, 9, 0, 0, 0, time.UTC)
	signer, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: signer}, (&jose.SignerOptions{}).WithHeader("kid", "k1").WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	token, err := jwt.Signed(s).Claims(map[string]any{"sub": "x", "exp": now.Add(time.Hour).Unix()}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	clock := now
	v := &entra.Verifier{Tenant: "t", Audiences: []string{"api://golem"}, Now: func() time.Time { return clock }}
	h := handler(AnthropicMessages, &stub{events: anthropicStream(1)}, nil)
	c, _ := h.Snapshots.Current()
	c.Auth = auth.Compile(snapshot.Entitlements{}, key.Live, func() time.Time { return now }, v)
	h.Snapshots = gsnap.Static(c)
	h.Guard = &guard.Guard{FailRate: 0.001, FailBurst: 2, Now: func() time.Time { return now }}
	call := func(cred string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(smallRequest))
		r.RemoteAddr = "10.0.0.1:1234"
		r.Header.Set("Authorization", "Bearer "+cred)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, tc := range []struct {
		trouble string
		set     func()
		says    string
	}{
		{"keys not fetched", func() {}, "gateway.not_ready"},
		{"an unknown key", func() {
			v.SetKeys(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &other.PublicKey, KeyID: "k2", Use: "sig"}}})
		}, entraUnknownKey},
		{"keys too old", func() {
			v.SetKeys(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &signer.PublicKey, KeyID: "k1", Use: "sig"}}})
			clock = now.Add(48 * time.Hour)
		}, entraKeysTooOld},
	} {
		tc.set()
		for range 5 {
			if w := call(token); !strings.Contains(w.Body.String(), tc.says) {
				t.Fatalf("%s: %d %s; want a refusal saying %q", tc.trouble, w.Code, w.Body, tc.says)
			}
		}
	}
	bad := key.New(key.Live)
	for i := range 2 {
		if w := call(bad); w.Code != 401 {
			t.Fatalf("bad key %d: %d %s; the source's failures were spent on Entra's trouble", i, w.Code, w.Body)
		}
	}
	if w := call(bad); w.Code != 429 {
		t.Fatalf("a third bad key: %d %s; want limit.source, its failures counted", w.Code, w.Body)
	}
}
