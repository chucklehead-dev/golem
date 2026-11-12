package auth

import (
	"encoding/hex"
	"net/http"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/lib/contract"
	"github.com/chucklehead-dev/golem/lib/key"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

func ent(keys ...snapshot.Key) snapshot.Entitlements {
	return snapshot.Entitlements{Scopes: []snapshot.Scope{{ID: "team-a", Models: []string{"claude", "gpt"}, PrincipalInFlight: 2}}, Keys: keys}
}

func hashOf(k string) string { h := key.Hash(k); return hex.EncodeToString(h[:]) }

func TestAuthenticate(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	good, narrow, revoked, expired, unknown := key.New(key.Live), key.New(key.Live), key.New(key.Live), key.New(key.Live), key.New(key.Live)
	k := Compile(ent(
		snapshot.Key{Hash: hashOf(good), Principal: "alice", Scope: "team-a"},
		snapshot.Key{Hash: hashOf(narrow), Principal: "bob", Scope: "team-a", Models: []string{"claude"}},
		snapshot.Key{Hash: hashOf(revoked), Principal: "carol", Scope: "team-a", Revoked: true},
		snapshot.Key{Hash: hashOf(expired), Principal: "dan", Scope: "team-a", ExpiresAt: now},
	), key.Live, func() time.Time { return now }, nil)

	for _, tc := range []struct {
		name   string
		header http.Header
		want   string // principal, or the refusal's code
	}{
		{"bearer", http.Header{"Authorization": {"Bearer " + good}}, "alice"},
		{"x-api-key", http.Header{"X-Api-Key": {good}}, "alice"},
		{"missing", http.Header{}, contract.AuthMissing.Name},
		{"a provider's key", http.Header{"X-Api-Key": {"sk-ant-api03-xyz"}}, contract.AuthMissing.Name},
		{"mistyped", http.Header{"X-Api-Key": {good[:len(good)-1]}}, contract.AuthKeyUnknown.Name},
		{"unknown", http.Header{"X-Api-Key": {unknown}}, contract.AuthKeyUnknown.Name},
		{"revoked", http.Header{"X-Api-Key": {revoked}}, contract.AuthKeyRevoked.Name},
		{"expired", http.Header{"X-Api-Key": {expired}}, contract.AuthKeyExpired.Name},
		{"dev key in production", http.Header{"X-Api-Key": {key.New(key.Dev)}}, contract.AuthKeyWrongInstallation.Name},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, r := k.Authenticate(tc.header)
			got := ""
			if p != nil {
				got = p.ID
			} else {
				got = r.Code.Name
			}
			if got != tc.want {
				t.Fatalf("got %s, want %s", got, tc.want)
			}
		})
	}

	b, _ := k.Authenticate(http.Header{"X-Api-Key": {narrow}})
	a, _ := k.Authenticate(http.Header{"X-Api-Key": {good}})
	if !a.Allows([]byte("gpt")) || b.Allows([]byte("gpt")) || !b.Allows([]byte("claude")) || a.Allows([]byte("opus")) {
		t.Fatal("rights: the scope's, narrowed by the key's, never wider")
	}
}

func TestImplicit(t *testing.T) {
	k := Compile(snapshot.Entitlements{Implicit: true}, key.Dev, time.Now, nil)
	p, r := k.Authenticate(http.Header{"Authorization": {"Bearer sk-anything-the-sdk-wants"}})
	if r != nil || p.ID != "local" || !p.Allows([]byte("whatever")) {
		t.Fatalf("%+v %+v: standalone's implicit principal takes any credential", p, r)
	}
	// A golem key is still checked: a live key on a standalone gateway is refused.
	if _, r := k.Authenticate(http.Header{"X-Api-Key": {key.New(key.Live)}}); r == nil || r.Code != contract.AuthKeyWrongInstallation {
		t.Fatalf("%+v", r)
	}
}
