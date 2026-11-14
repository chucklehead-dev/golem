package state

import (
	"errors"
	"slices"
	"testing"
	"time"
)

func seeded(t *testing.T) *Store {
	t.Helper()
	s := &Store{Bucket: open(t)}
	if _, err := s.Update(t.Context(), func(d *Document) error {
		d.Scopes = map[string]Scope{"a": {Models: []string{"claude"}}, "b": {Models: []string{"claude", "gpt"}}}
		d.Principals = map[string]Principal{
			"alice": {Kind: "user", Entra: "oid-1", Scopes: []string{"a", "b"}, Default: "b"},
			"ci":    {Kind: "service", Entra: "app-1", Scopes: []string{"b"}},
			"bot":   {Kind: "user", Scopes: []string{"a"}},
		}
		d.Keys = map[string]Key{"h1": {Display: "glm_live_1", Owner: "alice", Scope: "a"}, "h2": {Display: "glm_live_2", Owner: "bot", Scope: "a"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

// TestEntitlementInvariants: each change below is refused, and the state
// is left as it was.
func TestEntitlementInvariants(t *testing.T) {
	s := seeded(t)
	ctx := t.Context()
	if _, err := s.Update(ctx, func(d *Document) error { return d.Revoke("h1", time.Unix(1, 0)) }); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Document){
		"a key's owner missing":        func(d *Document) { d.Keys["h3"] = Key{Owner: "nobody", Scope: "a"} },
		"a key outside its owner":      func(d *Document) { d.Keys["h3"] = Key{Owner: "bot", Scope: "b"} },
		"a member of no such scope":    func(d *Document) { p := d.Principals["bot"]; p.Scopes = []string{"z"}; d.Principals["bot"] = p },
		"a default outside its scopes": func(d *Document) { p := d.Principals["ci"]; p.Default = "a"; d.Principals["ci"] = p },
		"an Entra ID twice": func(d *Document) {
			d.Principals["eve"] = Principal{Kind: "user", Entra: "oid-1", Scopes: []string{"a"}}
		},
		"a kind that is neither": func(d *Document) { d.Principals["x"] = Principal{Kind: "robot", Scopes: []string{"a"}} },
		"a revocation undone":    func(d *Document) { k := d.Keys["h1"]; k.RevokedAt = time.Time{}; d.Keys["h1"] = k },
		"a revocation dropped":   func(d *Document) { d.Revocations = nil },
		"a revocation rewritten": func(d *Document) { d.Revocations = []Revocation{{Key: "h2", At: time.Unix(1, 0)}} },
	} {
		before := s.Current().StateVersion
		if _, err := s.Update(ctx, func(d *Document) error { change(d); return nil }); !errors.Is(err, ErrInvariant) {
			t.Errorf("%s: %v, want ErrInvariant", name, err)
		}
		if s.Current().StateVersion != before {
			t.Errorf("%s: written anyway", name)
		}
	}
}

// TestEntitlements: the snapshot's view: scopes' models limited to the
// routes that exist, users and services by Entra ID, every principal a
// key owner of its kind, a revoked key listed revoked.
func TestEntitlementsView(t *testing.T) {
	s := seeded(t)
	s.Update(t.Context(), func(d *Document) error { return d.Revoke("h2", time.Unix(1, 0)) })
	e := s.Current().Entitlements([]string{"claude"})
	if len(e.Scopes) != 2 || !slices.Equal(e.Scopes[1].Models, []string{"claude"}) || e.Scopes[0].PrincipalInFlight != DefaultPrincipalInFlight {
		t.Fatalf("scopes %+v", e.Scopes)
	}
	if len(e.Users) != 1 || e.Users[0].ID != "alice" || e.Users[0].Default != "b" || len(e.Services) != 1 || e.Services[0].Default != "b" {
		t.Fatalf("users %+v services %+v", e.Users, e.Services)
	}
	if len(e.KeyOwners) != 3 || e.KeyOwners[1].ID != "bot" || e.KeyOwners[1].Kind != "user" {
		t.Fatalf("key owners %+v", e.KeyOwners)
	}
	if len(e.Keys) != 2 || e.Keys[0].Revoked || !e.Keys[1].Revoked || e.Keys[0].PrincipalKind != "user" {
		t.Fatalf("keys %+v", e.Keys)
	}
}
