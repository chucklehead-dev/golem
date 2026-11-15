package state

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/lib/snapshot"
)

func seeded(t *testing.T) *Store {
	t.Helper()
	s := &Store{Bucket: open(t)}
	if _, err := s.Update(t.Context(), func(d *Document) error {
		d.Scopes = map[string]Scope{"a": {}, "b": {Groups: []string{"g1"}}}
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
		"a principal in the reserved namespace": func(d *Document) {
			d.Principals["entra:oid-9"] = Principal{Kind: "user", Scopes: []string{"a"}}
		},
		"a synced membership of a service": func(d *Document) { p := d.Principals["bot"]; p.Synced = []string{"a"}; d.Principals["bot"] = p },
		"a synced membership of no scope": func(d *Document) {
			d.Principals["syn"] = Principal{Kind: "user", Entra: "oid-9", Synced: []string{"z"}}
		},
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

// TestEntitlements: the snapshot's view: scopes by ID only (their policy
// is their ScopePolicy's, which the compiler joins), users and services by
// Entra ID, every principal a key owner of its kind, a revoked key listed
// revoked.
func TestEntitlementsView(t *testing.T) {
	s := seeded(t)
	s.Update(t.Context(), func(d *Document) error { return d.Revoke("h2", time.Unix(1, 0)) })
	e := s.Current().Entitlements([]string{"claude"})
	if !reflect.DeepEqual(e.Scopes, []snapshot.Scope{{ID: "a"}, {ID: "b"}}) {
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

// TestRolesAndAudit: a role binds an existing principal to an existing
// scope as a manager; the audit outbox loses only its oldest events.
func TestRolesAndAudit(t *testing.T) {
	s := seeded(t)
	ctx := t.Context()
	ev := func(a string) AuditEvent { return AuditEvent{At: time.Unix(1, 0), Actor: "root", Action: a} }
	if _, err := s.Update(ctx, func(d *Document) error {
		d.Roles = append(d.Roles, RoleBinding{"alice", "manager", "a"})
		d.Audit = append(d.Audit, ev("one"), ev("two"), ev("three"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Document){
		"a role for no one":       func(d *Document) { d.Roles = append(d.Roles, RoleBinding{"nobody", "manager", "a"}) },
		"a role on no scope":      func(d *Document) { d.Roles = append(d.Roles, RoleBinding{"alice", "manager", "z"}) },
		"a role golem has not":    func(d *Document) { d.Roles = append(d.Roles, RoleBinding{"alice", "owner", "a"}) },
		"an event rewritten":      func(d *Document) { d.Audit[1].Actor = "eve" },
		"an event dropped midway": func(d *Document) { d.Audit = []AuditEvent{d.Audit[0], d.Audit[2]} },
		"the newest dropped":      func(d *Document) { d.Audit = d.Audit[:2] },
		"a number skipped": func(d *Document) {
			d.Audit = append(d.Audit, AuditEvent{Seq: d.AuditSeq + 2, Action: "x"})
		},
		"the counter set back": func(d *Document) { d.AuditSeq = 0; d.Audit = nil },
	} {
		if _, err := s.Update(ctx, func(d *Document) error { change(d); return nil }); !errors.Is(err, ErrInvariant) {
			t.Errorf("%s: %v, want ErrInvariant", name, err)
		}
	}
	if _, err := s.Update(ctx, func(d *Document) error { d.Audit = append(d.Audit[2:], ev("four")); return nil }); err != nil {
		t.Fatalf("the oldest shipped and trimmed, a new one added: %v", err)
	}
	if !s.Current().Has("alice", "manager", "a") || len(s.Current().Audit) != 2 {
		t.Fatalf("roles %v, audit %v", s.Current().Roles, s.Current().Audit)
	}
}
