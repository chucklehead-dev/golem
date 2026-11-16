package entra

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// keySet is an Entra key set endpoint in memory: it fails while failing
// is set, and counts the fetches.
type keySet struct {
	set     []byte
	failing atomic.Bool
	fetches atomic.Int32
}

func (k *keySet) RoundTrip(*http.Request) (*http.Response, error) {
	k.fetches.Add(1)
	if k.failing.Load() {
		return nil, errors.New("dial tcp: login.microsoftonline.com: no route to host")
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(k.set))), Header: http.Header{}}, nil
}

func newKeySet(t *testing.T) (*keySet, *rsa.PrivateKey) {
	pk, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	set, _ := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &pk.PublicKey, KeyID: "k1", Use: "sig"}}})
	return &keySet{set: set}, pk
}

func token(t *testing.T, pk *rsa.PrivateKey, kid string) string {
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: pk}, (&jose.SignerOptions{}).WithHeader("kid", kid))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	raw, err := jwt.Signed(s).Claims(map[string]any{"iss": "https://login.microsoftonline.com/tid/v2.0", "aud": "api://golem",
		"exp": now.Add(time.Hour).Unix(), "nbf": now.Unix(), "iat": now.Unix(), "tid": "tid", "ver": "2.0",
		"oid": "alice", "scp": ScopeAccess}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestStartWithoutKeys: a verifier whose first fetch fails refuses tokens
// as unavailable ("not yet fetched"), not as invalid, while Run retries
// with backoff; the first fetch that succeeds makes its tokens verify.
func TestStartWithoutKeys(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ks, pk := newKeySet(t)
		ks.failing.Store(true)
		e := &Verifier{Tenant: "tid", Audiences: []string{"api://golem"}, Client: &http.Client{Transport: ks}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go e.Run(ctx)
		synctest.Wait()
		if ks.fetches.Load() != 1 || e.Ready() {
			t.Fatalf("%d fetches, ready %v: Run fetches at once without a key set", ks.fetches.Load(), e.Ready())
		}
		tok := token(t, pk, "k1")
		_, err := e.Verify(tok)
		if ve, ok := errors.AsType[*Error](err); !ok || !ve.Unavailable || !strings.Contains(ve.Msg, "not yet fetched") {
			t.Fatalf("before any key set: %v; want Unavailable, saying the keys are not yet fetched", err)
		}
		// The retries back off: 1s, then 2s.
		time.Sleep(time.Second)
		synctest.Wait()
		if ks.fetches.Load() != 2 {
			t.Fatalf("%d fetches after 1s, want the first retry", ks.fetches.Load())
		}
		ks.failing.Store(false)
		time.Sleep(1500 * time.Millisecond)
		synctest.Wait()
		if ks.fetches.Load() != 2 {
			t.Fatalf("%d fetches after 2.5s, want the second retry to wait 2s", ks.fetches.Load())
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if !e.Ready() {
			t.Fatalf("%d fetches, not ready: the second retry should have fetched the keys", ks.fetches.Load())
		}
		if id, err := e.Verify(tok); err != nil || id.ID != "alice" {
			t.Fatalf("after the fetch: %+v, %v", id, err)
		}
	})
}

// TestEmptyKeySetKeepsCache: a key set answered with no usable signing
// key (an empty one, or one of keys Verify cannot use) or that does not
// parse is a failed refresh: the cached keys stay, and tokens they sign
// still verify. Before any good set, such an answer leaves the verifier
// not ready.
func TestEmptyKeySetKeepsCache(t *testing.T) {
	ks, pk := newKeySet(t)
	good := ks.set
	e := &Verifier{Tenant: "tid", Audiences: []string{"api://golem"}, Client: &http.Client{Transport: ks}}
	ks.set = []byte(`{"keys":[]}`)
	if err := e.Refresh(t.Context()); err == nil || e.Ready() {
		t.Fatalf("an empty first set: %v, ready %v; want a failed refresh", err, e.Ready())
	}
	ks.set = good
	if err := e.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`{"keys":[]}`, `{}`, `{"keys":[{"kty":"oct","k":"c2VjcmV0","kid":"k1"}]}`, `not json`} {
		ks.set = []byte(bad)
		if err := e.Refresh(t.Context()); err == nil {
			t.Errorf("%s: refreshed", bad)
		}
		if id, err := e.Verify(token(t, pk, "k1")); err != nil || id.ID != "alice" {
			t.Errorf("after %s: %+v, %v; the cached keys must stay", bad, id, err)
		}
	}
}

// TestRefreshDeferred: a refresh asked for within a minute of the last one
// is made when the minute is up, not dropped until the hourly refresh.
func TestRefreshDeferred(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ks, pk := newKeySet(t)
		e := &Verifier{Tenant: "tid", Audiences: []string{"api://golem"}, Client: &http.Client{Transport: ks}}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go e.Run(ctx)
		synctest.Wait()
		if ks.fetches.Load() != 1 || !e.Ready() {
			t.Fatalf("%d fetches: Run fetches at once", ks.fetches.Load())
		}
		// Ten seconds on, a token signed by a key not yet seen (Entra has
		// just rotated) asks for a refresh.
		time.Sleep(10 * time.Second)
		if _, err := e.Verify(token(t, pk, "k2")); err == nil || !strings.Contains(err.Error(), "unknown key") {
			t.Fatalf("an unknown key: %v", err)
		}
		synctest.Wait()
		if ks.fetches.Load() != 1 {
			t.Fatalf("%d fetches: a refresh within the minute must wait for it", ks.fetches.Load())
		}
		time.Sleep(50*time.Second + time.Millisecond)
		synctest.Wait()
		if ks.fetches.Load() != 2 {
			t.Fatalf("%d fetches a minute after the last: the deferred refresh was dropped", ks.fetches.Load())
		}
		// And the hourly refresh follows the deferred one, an hour on.
		time.Sleep(time.Hour - time.Second)
		synctest.Wait()
		if ks.fetches.Load() != 2 {
			t.Fatalf("%d fetches: the hourly refresh counts from the last", ks.fetches.Load())
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if ks.fetches.Load() != 3 {
			t.Fatalf("%d fetches: the hourly refresh is due", ks.fetches.Load())
		}
	})
}
