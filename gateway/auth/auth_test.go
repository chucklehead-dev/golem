package auth

import (
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/chucklehead-dev/golem/lib/contract"
	"github.com/chucklehead-dev/golem/lib/entra"
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
	orphan, departed := key.New(key.Live), key.New(key.Live)
	devListed := key.New(key.Dev)
	e := ent(
		snapshot.Key{Hash: hashOf(good), Principal: "alice", Scope: "team-a"},
		snapshot.Key{Hash: hashOf(narrow), Principal: "bob", Scope: "team-a", Models: []string{"claude"}},
		snapshot.Key{Hash: hashOf(revoked), Principal: "carol", Scope: "team-a", Revoked: true},
		snapshot.Key{Hash: hashOf(expired), Principal: "dan", Scope: "team-a", ExpiresAt: now},
		snapshot.Key{Hash: hashOf(orphan), Principal: "erin", Scope: "team-gone"},
		snapshot.Key{Hash: hashOf(departed), Principal: "frank", Scope: "team-a"},
		snapshot.Key{Hash: hashOf(devListed), Principal: "gina", Scope: "team-a"},
	)
	e.Users = []snapshot.Member{{ID: "frank", Entra: "f-oid", Scopes: []string{"team-b"}, Default: "team-b"}}
	unowned, wrongKind := key.New(key.Live), key.New(key.Live)
	e.Keys = append(e.Keys, snapshot.Key{Hash: hashOf(unowned), Principal: "zed", Scope: "team-a"},
		snapshot.Key{Hash: hashOf(wrongKind), Principal: "svc", PrincipalKind: "user", Scope: "team-a"})
	for _, id := range []string{"alice", "bob", "carol", "dan", "gina"} {
		e.KeyOwners = append(e.KeyOwners, snapshot.Owner{ID: id, Kind: "user", Scopes: []string{"team-a"}})
	}
	e.KeyOwners = append(e.KeyOwners, snapshot.Owner{ID: "svc", Kind: "service", Scopes: []string{"team-a"}})
	k := Compile(e, key.Live, func() time.Time { return now }, nil)

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
		// A dev key in a production snapshot (a publisher's mistake) is
		// still refused: the prefix is checked before the hash.
		{"a dev key the snapshot lists", http.Header{"X-Api-Key": {devListed}}, contract.AuthKeyWrongInstallation.Name},
		// Fail closed: never a zero (unlimited) budget for a missing scope.
		{"its scope is gone", http.Header{"X-Api-Key": {orphan}}, contract.AuthzScopeNotMember.Name},
		{"its principal left the scope", http.Header{"X-Api-Key": {departed}}, contract.AuthzScopeNotMember.Name},
		// golem#5: a key is honoured only for a listed owner of its kind.
		{"its owner is not listed", http.Header{"X-Api-Key": {unowned}}, contract.AuthKeyRevoked.Name},
		{"its owner is of another kind", http.Header{"X-Api-Key": {wrongKind}}, contract.AuthKeyRevoked.Name},
		{"two different credentials", http.Header{"Authorization": {"Bearer " + good}, "X-Api-Key": {narrow}}, contract.AuthMissing.Name},
		{"the same credential twice", http.Header{"Authorization": {"Bearer " + good}, "X-Api-Key": {good}}, "alice"},
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

// TestImplicit: standalone's implicit principal takes the gateway's own
// local key and nothing else; it never exists for a live installation
// (golem#5).
func TestImplicit(t *testing.T) {
	local := key.New(key.Dev)
	k := Compile(snapshot.Entitlements{Implicit: true}, key.Dev, time.Now, nil)
	if _, r := k.Authenticate(http.Header{"X-Api-Key": {local}}); r == nil {
		t.Fatal("an implicit principal without its local key admitted a request")
	}
	k.RequireLocalKey(local)
	p, r := k.Authenticate(http.Header{"X-Api-Key": {local}})
	if r != nil || p.ID != "local" || !p.Allows([]byte("whatever")) {
		t.Fatalf("%+v %+v: the local key is the local principal", p, r)
	}
	for _, other := range []string{"", "sk-anything-the-sdk-wants", key.New(key.Dev)} {
		if _, r := k.Authenticate(http.Header{"X-Api-Key": {other}}); r == nil {
			t.Fatalf("%q was admitted: only the local key is", other)
		}
	}
	live := Compile(snapshot.Entitlements{Implicit: true}, key.Live, time.Now, nil)
	live.RequireLocalKey(local)
	if _, r := live.Authenticate(http.Header{"X-Api-Key": {local}}); r == nil {
		t.Fatal("a live installation has no implicit principal")
	}
}

// TestDenylist: an entry refuses its credential or caller whatever the
// snapshot allows; a bad edit keeps the list in force; clearing it lets
// them in again.
func TestDenylist(t *testing.T) {
	e := &entra.Verifier{Tenant: tenant, Audiences: []string{apiID}, Now: func() time.Time { return now }}
	e.SetKeys(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &signingKey.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	plain := key.New(key.Live)
	hash := key.Hash(plain)
	ents := snapshot.Entitlements{
		Scopes:    []snapshot.Scope{{ID: "team-a", Models: []string{"claude"}}},
		Users:     []snapshot.Member{{ID: "alice", Entra: alice, Scopes: []string{"team-a"}, Default: "team-a"}},
		KeyOwners: []snapshot.Owner{{ID: "alice", Kind: "user", Scopes: []string{"team-a"}}},
		Keys:      []snapshot.Key{{Hash: hex.EncodeToString(hash[:]), Display: key.Display(plain), Principal: "alice", PrincipalKind: "user", Scope: "team-a"}},
	}
	k := Compile(ents, key.Live, func() time.Time { return now }, e)
	path := filepath.Join(t.TempDir(), "denylist")
	f := &DenyFile{Path: path}
	k.Deny(f)
	write := func(body string) error {
		os.WriteFile(path, []byte(body), 0o600)
		return f.Load()
	}
	refusal := func(h http.Header) string {
		if _, r := k.Authenticate(h); r != nil {
			return r.Code.Name
		}
		return ""
	}
	byKey := http.Header{"X-Api-Key": {plain}}
	byToken := bearer(sign(t, signingKey, "k1", userClaims()), "")
	if got := refusal(byKey) + refusal(byToken); got != "" {
		t.Fatalf("no denylist: %s", got)
	}
	for name, tc := range map[string]struct {
		body             string
		keyDenied, token bool
	}{
		"by hash":      {"key " + hex.EncodeToString(hash[:]) + "  # leaked in a gist\n", true, false},
		"by prefix":    {"key-prefix " + plain[:20] + "\n", true, false},
		"by principal": {"# incident 42\nprincipal alice\n", true, true},
		"by Entra ID":  {"entra " + alice + "\n", false, true},
	} {
		if err := write(tc.body); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := refusal(byKey) == "auth.denied"; got != tc.keyDenied {
			t.Errorf("%s: the key denied %v, want %v", name, got, tc.keyDenied)
		}
		if got := refusal(byToken) == "auth.denied"; got != tc.token {
			t.Errorf("%s: the token denied %v, want %v", name, got, tc.token)
		}
	}
	if err := write("principal alice\n"); err != nil {
		t.Fatal(err)
	}
	if err := write("prinicpal typo\n"); err == nil || refusal(byToken) != "auth.denied" {
		t.Fatalf("a bad edit: %v, %q; want an error and the previous list kept", err, refusal(byToken))
	}
	if err := write("key-prefix glm_\n"); err == nil {
		t.Fatal("a prefix that would deny every key was accepted")
	}
	if err := write(""); err != nil || refusal(byKey)+refusal(byToken) != "" {
		t.Fatalf("cleared: %v", err)
	}
}

// TestCaps: a key's principal carries its own cap in the scope, or the
// scope's default, and its key's cap; a token's principal its cap alone.
func TestCaps(t *testing.T) {
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	capped, uncapped := key.New(key.Live), key.New(key.Live)
	e := snapshot.Entitlements{
		Scopes: []snapshot.Scope{{ID: "team-a", Models: []string{"claude"}, DefaultMemberCapMicros: 50,
			Members: []snapshot.MemberAccount{{Principal: "alice", CapMicros: 80}, {Principal: "bob", KnownSpend: 3}}}},
		Keys: []snapshot.Key{{Hash: hashOf(capped), Display: key.Display(capped), Principal: "alice", Scope: "team-a", CapMicros: 20},
			{Hash: hashOf(uncapped), Display: key.Display(uncapped), Principal: "bob", Scope: "team-a"}},
		Users: []snapshot.Member{{ID: "carol", Entra: "c-oid", Scopes: []string{"team-a"}, Default: "team-a"}},
		KeyOwners: []snapshot.Owner{{ID: "alice", Kind: "user", Scopes: []string{"team-a"}},
			{ID: "bob", Kind: "user", Scopes: []string{"team-a"}}},
	}
	k := Compile(e, key.Live, func() time.Time { return now }, nil)
	for _, tc := range []struct {
		key           string
		member, keyed int64
	}{{capped, 80, 20}, {uncapped, 50, 0}} {
		p, no := k.Authenticate(http.Header{"X-Api-Key": {tc.key}})
		if no != nil {
			t.Fatal(no.Msg)
		}
		if p.Limits.MemberCapMicros != tc.member || p.Limits.KeyCapMicros != tc.keyed {
			t.Errorf("%s: member cap %d key cap %d, want %d and %d", p.ID, p.Limits.MemberCapMicros, p.Limits.KeyCapMicros, tc.member, tc.keyed)
		}
	}
	if p := k.byEntra["c-oid"]["team-a"]; p.Limits.MemberCapMicros != 50 || p.Limits.KeyCapMicros != 0 {
		t.Errorf("carol (a token): %+v, want the scope's default cap and no key cap", p.Limits)
	}
}
