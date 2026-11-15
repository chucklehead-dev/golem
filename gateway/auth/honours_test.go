package auth

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/chucklehead-dev/golem/lib/entra"
	"github.com/chucklehead-dev/golem/lib/key"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// TestHonours: a grant made by one snapshot's keys is honoured by another's
// by the rules a new request is refused by: a key while it is listed, not
// revoked, its scope there and its principal a member of it; a token while
// its principal is a member of the scope it acted in; neither while the
// denylist names it. Expiry is not checked (it ends no call in flight).
func TestHonours(t *testing.T) {
	e := &entra.Verifier{Tenant: tenant, Audiences: []string{apiID}, Now: func() time.Time { return now }}
	e.SetKeys(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &signingKey.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	plain := key.New(key.Live)
	base := func() snapshot.Entitlements {
		return snapshot.Entitlements{
			Scopes:    []snapshot.Scope{{ID: "team-a", Models: []string{"claude"}}, {ID: "team-b"}},
			Users:     []snapshot.Member{{ID: "alice", Entra: alice, Scopes: []string{"team-a"}, Default: "team-a"}},
			KeyOwners: []snapshot.Owner{{ID: "alice", Kind: "user", Scopes: []string{"team-a"}}},
			Keys:      []snapshot.Key{{Hash: hashOf(plain), Principal: "alice", PrincipalKind: "user", Scope: "team-a"}},
		}
	}
	clock := func() time.Time { return now }
	first := Compile(base(), key.Live, clock, e)
	var byKey, byToken Grant
	if _, r := first.AuthenticateGrant(http.Header{"X-Api-Key": {plain}}, &byKey); r != nil {
		t.Fatal(r.Msg)
	}
	if _, r := first.AuthenticateGrant(bearer(sign(t, signingKey, "k1", userClaims()), ""), &byToken); r != nil {
		t.Fatal(r.Msg)
	}
	var refused Grant
	if _, r := first.AuthenticateGrant(http.Header{"X-Api-Key": {key.New(key.Live)}}, &refused); r == nil || refused != (Grant{}) {
		t.Fatal("a refused authentication left a grant")
	}
	for _, tc := range []struct {
		name       string
		change     func(*snapshot.Entitlements)
		key, token string // the refusal's code, "" if honoured
	}{
		{"unchanged", func(*snapshot.Entitlements) {}, "", ""},
		{"the key revoked", func(e *snapshot.Entitlements) { e.Keys[0].Revoked = true }, "auth.key_revoked", ""},
		{"the key removed", func(e *snapshot.Entitlements) { e.Keys = nil }, "auth.key_revoked", ""},
		{"the key expired", func(e *snapshot.Entitlements) { e.Keys[0].ExpiresAt = now.Add(-time.Hour) }, "", ""},
		{"the principal left the scope", func(e *snapshot.Entitlements) {
			e.KeyOwners[0].Scopes = []string{"team-b"}
			e.Users[0].Scopes, e.Users[0].Default = []string{"team-b"}, "team-b"
		}, "authz.scope_not_member", "authz.scope_not_member"},
		{"the principal is gone", func(e *snapshot.Entitlements) { e.KeyOwners, e.Users = nil, nil }, "auth.key_revoked", "authz.scope_not_member"},
		{"the scope is gone", func(e *snapshot.Entitlements) { e.Scopes = e.Scopes[1:] }, "authz.scope_not_member", "authz.scope_not_member"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ents := base()
			tc.change(&ents)
			k := Compile(ents, key.Live, clock, e)
			code := func(g *Grant) string {
				if r := k.Honours(g); r != nil {
					return r.Code.Name
				}
				return ""
			}
			if got := code(&byKey); got != tc.key {
				t.Errorf("the key's grant: %q, want %q", got, tc.key)
			}
			if got := code(&byToken); got != tc.token {
				t.Errorf("the token's grant: %q, want %q", got, tc.token)
			}
			if r := k.Honours(&Grant{}); r != nil {
				t.Error("no grant (no entitlements) is always honoured")
			}
		})
	}
	// The denylist as it is now, over the same keys.
	f := &DenyFile{Path: filepath.Join(t.TempDir(), "denylist")}
	first.Deny(f)
	for body, want := range map[string]string{
		"principal alice\n":               "auth.denied auth.denied",
		"key " + hashOf(plain) + "\n":     "auth.denied ",
		"key-prefix " + plain[:20] + "\n": "auth.denied ",
		"entra " + alice + "\n":           " auth.denied",
		"principal someone-else\n":        " ",
	} {
		os.WriteFile(f.Path, []byte(body), 0o600)
		if err := f.Load(); err != nil {
			t.Fatal(err)
		}
		var got [2]string
		for i, g := range []*Grant{&byKey, &byToken} {
			if r := first.Honours(g); r != nil {
				got[i] = r.Code.Name
			}
		}
		if s := got[0] + " " + got[1]; s != want {
			t.Errorf("denylist %q: %q, want %q", body, s, want)
		}
	}
}
