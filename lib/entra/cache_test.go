package entra

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// TestCachedToken: a verified token is served from the cache, and a cached
// token is still refused once it expires, once the key set is replaced by
// one that does not verify it, and once the key set is too old.
func TestCachedToken(t *testing.T) {
	setup := func(t *testing.T) (*Verifier, *time.Time, string) {
		ks, pk := newKeySet(t)
		now := time.Now()
		e := &Verifier{Tenant: "tid", Audiences: []string{"api://golem"}, Client: &http.Client{Transport: ks},
			Now: func() time.Time { return now }}
		if err := e.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		tok := token(t, pk, "k1")
		if id, err := e.Verify(tok); err != nil || id.ID != "alice" {
			t.Fatalf("first verify: %+v, %v", id, err)
		}
		return e, &now, tok
	}
	refused := func(t *testing.T, e *Verifier, tok string, want func(*Error) bool) {
		t.Helper()
		_, err := e.Verify(tok)
		if ee, ok := errors.AsType[*Error](err); !ok || !want(ee) {
			t.Fatalf("got %v; want it refused", err)
		}
	}

	t.Run("served from the cache", func(t *testing.T) {
		e, _, tok := setup(t)
		if id, err := e.Verify(tok); err != nil || id.ID != "alice" || e.cached().Len() != 1 {
			t.Fatalf("second verify: %+v, %v, %d cached", id, err, e.cached().Len())
		}
	})
	t.Run("expired", func(t *testing.T) {
		e, now, tok := setup(t)
		*now = now.Add(time.Hour + 2*leeway)
		refused(t, e, tok, func(e *Error) bool { return e.Expired })
	})
	t.Run("keys rotated", func(t *testing.T) {
		e, _, tok := setup(t)
		other, err := rsa.GenerateKey(rand.Reader, 2048)
		if err != nil {
			t.Fatal(err)
		}
		e.SetKeys(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &other.PublicKey, KeyID: "k1", Use: "sig"}}})
		refused(t, e, tok, func(e *Error) bool { return !e.Expired && !e.Unavailable })
	})
	t.Run("keys too old", func(t *testing.T) {
		e, now, tok := setup(t)
		e.MaxKeyAge = 10 * time.Minute
		*now = now.Add(30 * time.Minute)
		refused(t, e, tok, func(e *Error) bool { return !e.Expired })
	})
}
