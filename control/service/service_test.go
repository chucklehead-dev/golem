package service

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/bucket"
)

var (
	admin = Caller{Principal: "root", Admin: true}
	mgrA  = Caller{Principal: "mona"} // manager of team-a
	mgrB  = Caller{Principal: "bob"}  // manager of team-b
	alice = Caller{Principal: "alice"}
	eve   = Caller{Principal: "eve"} // no role, no membership
)

func setup(t *testing.T) *Service {
	t.Helper()
	b, err := bucket.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	// The config's ScopePolicies, as golem-control reads them.
	policies := map[string][]string{"team-a": {"claude", "gpt"}, "team-b": {"claude"}}
	s := &Service{State: &state.Store{Bucket: b}, Now: func() time.Time { return time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC) },
		ScopeModels: func(scope string) ([]string, bool) { return policies[scope], true }}
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.CreateScope(ctx, admin, Options{}, "team-a"))
	must(s.CreateScope(ctx, admin, Options{}, "team-b"))
	for _, m := range []struct{ scope, id, kind string }{{"team-a", "mona", "user"}, {"team-b", "bob", "user"}, {"team-a", "alice", "user"}, {"team-a", "ci", "service"}} {
		must(s.AddMember(ctx, admin, Options{}, m.scope, Member{ID: m.id, Kind: m.kind}))
	}
	must(s.BindManager(ctx, admin, Options{}, "team-a", "mona"))
	must(s.BindManager(ctx, admin, Options{}, "team-b", "bob"))
	return s
}

// TestAuthorization: who may do what (org-model.md §2), for each method.
func TestAuthorization(t *testing.T) {
	type op func(s *Service, c Caller) error
	mint := func(owner, scope string) op {
		return func(s *Service, c Caller) error {
			_, err := s.MintKey(t.Context(), c, Options{}, KeyRequest{Owner: owner, Scope: scope})
			return err
		}
	}
	ops := map[string]op{
		"create a scope": func(s *Service, c Caller) error {
			return s.CreateScope(t.Context(), c, Options{}, "new")
		},
		"add a member to team-a": func(s *Service, c Caller) error {
			return s.AddMember(t.Context(), c, Options{}, "team-a", Member{ID: "zed", Kind: "service"})
		},
		"remove alice from team-a":  func(s *Service, c Caller) error { return s.RemoveMember(t.Context(), c, Options{}, "team-a", "alice") },
		"bind a manager":            func(s *Service, c Caller) error { return s.BindManager(t.Context(), c, Options{}, "team-a", "alice") },
		"mint alice's key":          mint("alice", "team-a"),
		"mint team-a's service key": mint("ci", "team-a"),
	}
	allowed := map[string][]string{ // the callers each op allows
		"create a scope":            {"root"},
		"add a member to team-a":    {"root", "mona"},
		"remove alice from team-a":  {"root", "mona"},
		"bind a manager":            {"root"},
		"mint alice's key":          {"root", "alice"},
		"mint team-a's service key": {"root", "mona"},
	}
	for name, do := range ops {
		for _, c := range []Caller{admin, mgrA, mgrB, alice, eve} {
			err := do(setup(t), c)
			want := false
			for _, p := range allowed[name] {
				want = want || p == c.Principal
			}
			// A refusal is denied to whoever can see team-a, not found to
			// whoever cannot (its existence is not theirs to learn).
			refusal := ErrPermission
			if (c == mgrB || c == eve) && name != "create a scope" && name != "bind a manager" {
				refusal = ErrNotFound
			}
			if got := err == nil; got != want || (!want && !errors.Is(err, refusal)) {
				t.Errorf("%s by %s: %v, want allowed=%v (refused: %v)", name, c.Principal, err, want, refusal)
			}
		}
	}
}

// TestRevokeKey: a member's key is revoked by its owner or admins; a
// service's key also by its scope's managers; nobody else, and an outsider
// is not told the key exists.
func TestRevokeKey(t *testing.T) {
	for _, tc := range []struct {
		owner   string
		c       Caller
		want    bool
		refusal error
	}{
		{"alice", admin, true, nil}, {"alice", alice, true, nil}, {"alice", mgrA, false, ErrPermission},
		{"alice", mgrB, false, ErrNotFound}, {"alice", eve, false, ErrNotFound},
		{"ci", mgrA, true, nil}, {"ci", mgrB, false, ErrNotFound}, {"ci", alice, false, ErrPermission},
	} {
		s := setup(t)
		k, err := s.MintKey(t.Context(), admin, Options{}, KeyRequest{Owner: tc.owner, Scope: "team-a"})
		if err != nil {
			t.Fatal(err)
		}
		err = s.RevokeKey(t.Context(), tc.c, Options{}, k.Hash)
		if (err == nil) != tc.want || (!tc.want && !errors.Is(err, tc.refusal)) {
			t.Errorf("revoke %s's key by %s: %v, want allowed=%v", tc.owner, tc.c.Principal, err, tc.want)
		}
		if revoked := !s.State.Current().Keys[k.Hash].RevokedAt.IsZero(); revoked != tc.want {
			t.Errorf("revoke %s's key by %s: revoked %v", tc.owner, tc.c.Principal, revoked)
		}
	}
}

// TestManagerRules: a manager cannot unseat another manager; a new
// principal needs a kind; a personal key expires within the limit, by
// default at it.
func TestManagerRules(t *testing.T) {
	ctx := t.Context()
	s := setup(t)
	if err := s.BindManager(ctx, admin, Options{}, "team-a", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveMember(ctx, mgrA, Options{}, "team-a", "alice"); !errors.Is(err, ErrPermission) {
		t.Fatalf("a manager removing a fellow manager: %v", err)
	}
	if err := s.AddMember(ctx, admin, Options{}, "team-a", Member{ID: "nokind"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a new principal with no kind: %v", err)
	}
	k, err := s.MintKey(ctx, alice, Options{}, KeyRequest{Owner: "alice", Scope: "team-a"})
	if err != nil {
		t.Fatal(err)
	}
	if exp := s.State.Current().Keys[k.Hash].ExpiresAt; !exp.Equal(s.now().Add(MaxPersonalKeyLife)) {
		t.Fatalf("a personal key without an expiry expires %v", exp)
	}
	if _, err := s.MintKey(ctx, alice, Options{}, KeyRequest{Owner: "alice", Scope: "team-a", Expires: s.now().Add(MaxPersonalKeyLife + time.Hour)}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a personal key past the limit: %v", err)
	}
	ci, err := s.MintKey(ctx, admin, Options{}, KeyRequest{Owner: "ci", Scope: "team-a"})
	if err != nil || !s.State.Current().Keys[ci.Hash].ExpiresAt.IsZero() {
		t.Fatalf("a service key: %v, expiry %v (none required)", err, s.State.Current().Keys[ci.Hash].ExpiresAt)
	}
}

// TestValidateOnly: a validation checks everything and writes nothing.
func TestValidateOnly(t *testing.T) {
	s := setup(t)
	before := s.State.Current().StateVersion
	if _, err := s.MintKey(t.Context(), alice, Options{ValidateOnly: true}, KeyRequest{Owner: "alice", Scope: "team-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MintKey(t.Context(), alice, Options{ValidateOnly: true}, KeyRequest{Owner: "alice", Scope: "team-a", Models: []string{"opus"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a model outside the scope, validated: %v", err)
	}
	if d := s.State.Current(); d.StateVersion != before || len(d.Keys) != 0 {
		t.Fatalf("a validation wrote: version %d → %d, %d keys", before, d.StateVersion, len(d.Keys))
	}
}

// TestKeyRules: a key narrows its scope, needs a member owner and a future
// expiry; its plaintext is returned once and is in no audit event.
func TestKeyRules(t *testing.T) {
	s := setup(t)
	ctx := t.Context()
	for name, r := range map[string]KeyRequest{
		"a model outside the scope": {Owner: "alice", Scope: "team-a", Models: []string{"opus"}},
		"an owner not a member":     {Owner: "bob", Scope: "team-a"},
		"an expiry past":            {Owner: "alice", Scope: "team-a", Expires: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		if _, err := s.MintKey(ctx, admin, Options{}, r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v, want ErrInvalid", name, err)
		}
	}
	k, err := s.MintKey(ctx, alice, Options{}, KeyRequest{Owner: "alice", Scope: "team-a", Models: []string{"claude"}})
	if err != nil || !strings.HasPrefix(k.Key, "glm_live_") {
		t.Fatalf("minted %+v %v", k, err)
	}
	d := s.State.Current()
	ev := d.Audit[len(d.Audit)-1]
	if ev.Action != "key.mint" || ev.Actor != "alice" || ev.Target != k.Display {
		t.Fatalf("audit %+v", ev)
	}
	for _, e := range d.Audit {
		for _, v := range append([]string{e.Target}, mapValues(e.Detail)...) {
			if strings.Contains(v, k.Key) {
				t.Fatalf("a key's plaintext in the audit: %+v", e)
			}
		}
	}
	if e := d.Entitlements([]string{"claude", "gpt"}); len(e.Keys) != 1 || e.Keys[0].Hash != k.Hash || e.Keys[0].Principal != "alice" {
		t.Fatalf("the snapshot's view: %+v", e.Keys)
	}
}

// TestRemoveMember: a member removed loses its keys in the scope (revoked,
// kept as records) and its role there.
func TestRemoveMember(t *testing.T) {
	s := setup(t)
	ctx := t.Context()
	k, _ := s.MintKey(ctx, admin, Options{}, KeyRequest{Owner: "mona", Scope: "team-a"})
	if err := s.RemoveMember(ctx, admin, Options{}, "team-a", "mona"); err != nil {
		t.Fatal(err)
	}
	d := s.State.Current()
	if d.Keys[k.Hash].RevokedAt.IsZero() || d.Has("mona", "manager", "team-a") || len(d.Revocations) != 1 {
		t.Fatalf("after removal: key %+v, still manager %v, revocations %v", d.Keys[k.Hash], d.Has("mona", "manager", "team-a"), d.Revocations)
	}
}

func mapValues(m map[string]string) []string {
	var v []string
	for _, x := range m {
		v = append(v, x)
	}
	return v
}

// TestReads: who sees which scopes and keys. A scope is not found by an
// outsider (its existence is not theirs to learn); keys are seen by their
// owner, their scope's managers and admins.
func TestReads(t *testing.T) {
	s := setup(t)
	ctx := t.Context()
	own, _ := s.MintKey(ctx, alice, Options{}, KeyRequest{Scope: "team-a"})
	svc, _ := s.MintKey(ctx, mgrA, Options{}, KeyRequest{Owner: "ci", Scope: "team-a"})
	bobs, _ := s.MintKey(ctx, mgrB, Options{}, KeyRequest{Scope: "team-b"})
	if own.Hash == "" || svc.Hash == "" || bobs.Hash == "" {
		t.Fatal("setup keys")
	}
	for _, tc := range []struct {
		c      Caller
		scopes string
		keys   []string
	}{
		{admin, "team-a,team-b", []string{own.Hash, svc.Hash, bobs.Hash}},
		{mgrA, "team-a", []string{own.Hash, svc.Hash}},
		{alice, "team-a", []string{own.Hash}},
		{mgrB, "team-b", []string{bobs.Hash}},
		{eve, "", nil},
	} {
		ids, _ := s.ListScopes(ctx, tc.c)
		if got := strings.Join(ids, ","); got != tc.scopes {
			t.Errorf("%s's scopes: %q, want %q", tc.c.Principal, got, tc.scopes)
		}
		ks, _ := s.ListKeys(ctx, tc.c, "")
		var got []string
		for _, k := range ks {
			got = append(got, k.Hash)
		}
		if !sameSet(got, tc.keys) {
			t.Errorf("%s's keys: %d, want %d", tc.c.Principal, len(got), len(tc.keys))
		}
	}
	if _, err := s.GetScope(ctx, eve, "team-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an outsider reading a scope: %v, want not found", err)
	}
	if _, err := s.GetScope(ctx, mgrB, "team-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another scope's manager: %v, want not found", err)
	}
	v, err := s.GetScope(ctx, alice, "team-a")
	if err != nil || strings.Join(v.Members, ",") != "alice,ci,mona" || strings.Join(v.Managers, ",") != "mona" {
		t.Fatalf("team-a as alice: %+v %v", v, err)
	}
	me, _ := s.Whoami(ctx, mgrA)
	if me.Kind != "user" || strings.Join(me.Manages, ",") != "team-a" || strings.Join(me.Scopes, ",") != "team-a" {
		t.Fatalf("whoami %+v", me)
	}
	if ks, _ := s.ListKeys(ctx, admin, "team-b"); len(ks) != 1 || ks[0].Hash != bobs.Hash {
		t.Fatalf("keys in team-b: %+v", ks)
	}
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range b {
		found := false
		for _, y := range a {
			found = found || x == y
		}
		if !found {
			return false
		}
	}
	return true
}

// TestManagerReadsScope: a scope's manager reads it without being a
// member (org-model.md §2: managing is its own right).
func TestManagerReadsScope(t *testing.T) {
	s := setup(t)
	ctx := t.Context()
	if err := s.BindManager(ctx, admin, Options{}, "team-b", "mona"); err != nil {
		t.Fatal(err)
	}
	if ids, err := s.ListScopes(ctx, mgrA); err != nil || strings.Join(ids, ",") != "team-a,team-b" {
		t.Fatalf("mona's scopes: %v %v", ids, err)
	}
	if v, err := s.GetScope(ctx, mgrA, "team-b"); err != nil || strings.Join(v.Managers, ",") != "bob,mona" {
		t.Fatalf("team-b as its non-member manager: %+v %v", v, err)
	}
}

// TestIdentityBinding: who a principal is in Entra is bound by an admin or
// the group sync, never by a manager (ADR 0047): a manager cannot create a
// user, bind an Entra ID, or create a principal another caller would
// inherit; a membership from a group is removed in Entra, not here.
func TestIdentityBinding(t *testing.T) {
	ctx := t.Context()
	s := setup(t)
	for name, m := range map[string]Member{
		"a user":                          {ID: "zed", Kind: "user"},
		"a service with someone's app ID": {ID: "bot", Kind: "service", Entra: "app-of-someone-else"},
		"a user with someone's oid":       {ID: "victim", Kind: "user", Entra: "oid-victim"},
	} {
		if err := s.AddMember(ctx, mgrA, Options{}, "team-a", m); err == nil {
			t.Errorf("%s: a manager created it", name)
		}
	}
	if err := s.AddMember(ctx, admin, Options{}, "team-a", Member{ID: "entra:oid-victim", Kind: "service"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("a principal in the reserved namespace: %v", err)
	}
	if err := s.AddMember(ctx, mgrA, Options{}, "team-a", Member{ID: "ci-2", Kind: "service"}); err != nil {
		t.Errorf("a keys-only service by its manager: %v", err)
	}
	if err := s.AddMember(ctx, mgrA, Options{}, "team-a", Member{ID: "bob"}); err != nil {
		t.Errorf("an existing principal by a manager: %v", err)
	}
	if err := s.AddMember(ctx, admin, Options{}, "team-a", Member{ID: "zoe", Kind: "user", Entra: "oid-zoe"}); err != nil {
		t.Errorf("an admin binding a user: %v", err)
	}

	// A synced membership, as the sync writes it, is the group's.
	if _, err := s.State.Update(ctx, func(d *state.Document) error {
		p := d.Principals["zoe"]
		p.Synced = []string{"team-b"}
		d.Principals["zoe"] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	k, err := s.MintKey(ctx, admin, Options{}, KeyRequest{Owner: "zoe", Scope: "team-b"})
	if err != nil {
		t.Fatalf("a key in a synced scope: %v", err)
	}
	if err := s.RemoveMember(ctx, admin, Options{}, "team-b", "zoe"); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "Entra group") {
		t.Fatalf("removing a synced membership: %v", err)
	}
	// Both ways: removing the granted one keeps the synced one and its key.
	if err := s.AddMember(ctx, admin, Options{}, "team-b", Member{ID: "zoe"}); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveMember(ctx, admin, Options{}, "team-b", "zoe"); err != nil {
		t.Fatal(err)
	}
	if d := s.State.Current(); !d.Principals["zoe"].MemberOf("team-b") || !d.Keys[k.Hash].RevokedAt.IsZero() {
		t.Fatalf("zoe %+v, key revoked %v", d.Principals["zoe"], !d.Keys[k.Hash].RevokedAt.IsZero())
	}
}

// TestBindGroup: admins bind and unbind groups; nobody else.
func TestBindGroup(t *testing.T) {
	ctx := t.Context()
	s := setup(t)
	if err := s.BindGroup(ctx, mgrA, Options{}, "team-a", "g1"); !errors.Is(err, ErrPermission) {
		t.Fatalf("a manager binding a group: %v", err)
	}
	for range 2 {
		if err := s.BindGroup(ctx, admin, Options{}, "team-a", "g1"); err != nil {
			t.Fatal(err)
		}
	}
	if g := s.State.Current().Scopes["team-a"].Groups; len(g) != 1 || g[0] != "g1" {
		t.Fatalf("groups %v", g)
	}
	if err := s.UnbindGroup(ctx, admin, Options{}, "team-a", "g1"); err != nil || len(s.State.Current().Scopes["team-a"].Groups) != 0 {
		t.Fatalf("unbind: %v", err)
	}
	if err := s.UnbindGroup(ctx, admin, Options{}, "team-a", "g1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unbind again: %v", err)
	}
}

// TestCreateScope: a scope is created with no policy (its ScopePolicy is
// config): an ID, an empty record the group bindings will fill, an audit
// event; a second of the same ID, or none, is invalid.
func TestCreateScope(t *testing.T) {
	ctx := t.Context()
	s := setup(t)
	if err := s.CreateScope(ctx, admin, Options{ValidateOnly: true}, "team-c"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.State.Current().Scopes["team-c"]; ok {
		t.Fatal("a validate-only create was written")
	}
	if err := s.CreateScope(ctx, admin, Options{}, "team-c"); err != nil {
		t.Fatal(err)
	}
	d := s.State.Current()
	if sc, ok := d.Scopes["team-c"]; !ok || len(sc.Groups) != 0 {
		t.Fatalf("scope %+v, %v", sc, ok)
	}
	if last := d.Audit[len(d.Audit)-1]; last.Action != "scope.create" || last.Target != "team-c" {
		t.Fatalf("audit %+v", last)
	}
	for id, want := range map[string]string{"team-c": `scope.id: scope "team-c" exists`, "": "scope.id: required"} {
		if err := s.CreateScope(ctx, admin, Options{}, id); !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), want) {
			t.Errorf("create %q: %v, want %s", id, err, want)
		}
	}
}

// TestKeyModelsFromConfig: a key's models are checked against its scope's
// ScopePolicy as the config gives it (ScopeModels): a scope with no policy
// allows none; a config that cannot tell checks nothing.
func TestKeyModelsFromConfig(t *testing.T) {
	ctx := t.Context()
	s := setup(t)
	if err := s.CreateScope(ctx, admin, Options{}, "team-c"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddMember(ctx, admin, Options{}, "team-c", Member{ID: "alice"}); err != nil {
		t.Fatal(err)
	}
	req := KeyRequest{Owner: "alice", Scope: "team-c", Models: []string{"claude"}}
	if _, err := s.MintKey(ctx, alice, Options{}, req); !errors.Is(err, ErrInvalid) ||
		!strings.Contains(err.Error(), `key.models[0]: scope team-c does not allow "claude": a key can only narrow its scope's models, its ScopePolicy's spec.models in the config`) {
		t.Fatalf("a model of a scope with no ScopePolicy: %v", err)
	}
	s.ScopeModels = func(string) ([]string, bool) { return nil, false }
	if _, err := s.MintKey(ctx, alice, Options{}, req); err != nil {
		t.Fatalf("a config that cannot tell: %v", err)
	}
}

// TestBindManager: binding a manager on a scope or of a principal that
// does not exist is not found, dry run or not, and writes nothing (the
// store's invariant would refuse it, as failed_precondition, too late to
// say which). A principal need not be a member to manage
// (TestManagerReadsScope).
func TestBindManager(t *testing.T) {
	s := setup(t)
	for _, o := range []Options{{ValidateOnly: true}, {}} {
		for _, tc := range []struct{ scope, principal, want string }{
			{"team-z", "alice", "scope team-z"},
			{"team-a", "zed", "principal zed: add them to a scope first"},
		} {
			before := s.State.Current().StateVersion
			err := s.BindManager(t.Context(), admin, o, tc.scope, tc.principal)
			if !errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s on %s (dry run %v): %v, want not found: %s", tc.principal, tc.scope, o.ValidateOnly, err, tc.want)
			}
			if s.State.Current().StateVersion != before {
				t.Errorf("%s on %s: written", tc.principal, tc.scope)
			}
		}
	}
	if err := s.BindManager(t.Context(), admin, Options{}, "team-a", ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("no principal: %v, want invalid", err)
	}
}

// TestOutboxBackpressure: past the audit outbox's reserve every method
// that grants is refused with ErrOutboxFull, validate_only too, and the
// restrictive ones land; at its bound a revocation is refused too, and
// nothing grows; once the outbox is trimmed, grants land again.
func TestOutboxBackpressure(t *testing.T) {
	s := setup(t)
	ctx := t.Context()
	var keys []string
	for _, owner := range []string{"alice", "alice", "ci"} {
		k, err := s.MintKey(ctx, admin, Options{}, KeyRequest{Owner: owner, Scope: "team-a"})
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.Hash)
	}
	s.State.MaxOutbox = 20
	fill := func(n int) {
		t.Helper()
		if _, err := s.State.Update(ctx, func(d *state.Document) error {
			for len(d.Audit) < n {
				d.Audit = append(d.Audit, state.AuditEvent{Actor: "root", Action: "key.revoke", Target: "none"})
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	fill(state.Reserve(20))
	v := s.State.Current().StateVersion
	grants := map[string]func(Options) error{
		"mint a key": func(o Options) error {
			_, err := s.MintKey(ctx, alice, o, KeyRequest{Scope: "team-a"})
			return err
		},
		"add a member":   func(o Options) error { return s.AddMember(ctx, mgrA, o, "team-a", Member{ID: "bot", Kind: "service"}) },
		"create a scope": func(o Options) error { return s.CreateScope(ctx, admin, o, "team-c") },
		"bind a manager": func(o Options) error { return s.BindManager(ctx, admin, o, "team-a", "alice") },
		"bind a group":   func(o Options) error { return s.BindGroup(ctx, admin, o, "team-a", "g-1") },
	}
	for name, grant := range grants {
		for _, o := range []Options{{}, {ValidateOnly: true}} {
			if err := grant(o); !errors.Is(err, state.ErrOutboxFull) || !strings.Contains(err.Error(), "-audit-archive") {
				t.Errorf("%s past the reserve (validate only %v): %v, want ErrOutboxFull naming the fix", name, o.ValidateOnly, err)
			}
		}
	}
	if d := s.State.Current(); d.StateVersion != v {
		t.Fatalf("refused grants wrote version %d", d.StateVersion)
	}
	// Restrictive changes land in the reserve.
	if err := s.RevokeKey(ctx, alice, Options{}, keys[0]); err != nil {
		t.Fatalf("a revocation past the reserve: %v", err)
	}
	if err := s.RemoveMember(ctx, mgrA, Options{}, "team-a", "ci"); err != nil {
		t.Fatalf("a removal past the reserve: %v", err)
	}
	if d := s.State.Current(); d.Keys[keys[0]].RevokedAt.IsZero() || d.Keys[keys[2]].RevokedAt.IsZero() || d.Principals["ci"].MemberOf("team-a") {
		t.Fatal("the revocation and the removal did not take effect")
	}
	// At the bound, a revocation is refused too, and nothing grows.
	fill(20)
	v = s.State.Current().StateVersion
	err := s.RevokeKey(ctx, alice, Options{}, keys[1])
	if full, ok := errors.AsType[*state.OutboxFull](err); !ok || !full.Hard() {
		t.Fatalf("a revocation at the bound: %v, want the outbox full", err)
	}
	if d := s.State.Current(); d.StateVersion != v || len(d.Audit) != 20 || !d.Keys[keys[1]].RevokedAt.IsZero() {
		t.Fatalf("at the bound: version %d (was %d), outbox %d", d.StateVersion, v, len(d.Audit))
	}
	// Trimmed (as the shipper does once archived), grants land again.
	if _, err := s.State.Update(ctx, func(d *state.Document) error { d.Audit = d.Audit[len(d.Audit)-5:]; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := grants["mint a key"](Options{}); err != nil {
		t.Fatalf("a mint after the trim: %v", err)
	}
}

// TestCaps: a member's cap is set by its scope's managers or admins, not
// by the member nor another scope's manager, within 0 to 2^50, and only for
// a member; 0 removes it, and so does leaving the scope. A key's cap is set
// by its owner, its scope's managers or admins, on a live key; a key can be
// minted with one. Each change is audited.
func TestCaps(t *testing.T) {
	s := setup(t)
	ctx := t.Context()
	for _, tc := range []struct {
		name string
		c    Caller
		p    string
		cap  int64
		want error
	}{
		{"the member", alice, "alice", 5, ErrPermission},
		{"another scope's manager", mgrB, "alice", 5, ErrNotFound}, // a scope it cannot see
		{"not a member", mgrA, "bob", 5, ErrNotFound},
		{"negative", mgrA, "alice", -1, ErrInvalid},
		{"past exact sums", mgrA, "alice", 1<<50 + 1, ErrInvalid},
		{"the manager", mgrA, "alice", 5, nil},
		{"an admin", admin, "ci", 7, nil},
	} {
		if err := s.SetMemberCap(ctx, tc.c, Options{}, "team-a", tc.p, tc.cap); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		}
	}
	d := s.State.Current()
	if d.Principals["alice"].Caps["team-a"] != 5 || d.Principals["ci"].Caps["team-a"] != 7 {
		t.Fatalf("caps %v %v", d.Principals["alice"].Caps, d.Principals["ci"].Caps)
	}
	if ev := d.Audit[len(d.Audit)-1]; ev.Action != "member.cap" || ev.Target != "ci" || ev.Detail["cap_micros"] != "7" {
		t.Fatalf("audit %+v", ev)
	}
	if v, _ := s.GetScope(ctx, mgrA, "team-a"); v.Caps["alice"] != 5 || v.Caps["ci"] != 7 {
		t.Fatalf("GetScope's caps %v", v.Caps)
	}
	if err := s.SetMemberCap(ctx, mgrA, Options{}, "team-a", "alice", 0); err != nil || s.State.Current().Principals["alice"].Caps != nil {
		t.Fatalf("0 removes it: %v %v", err, s.State.Current().Principals["alice"].Caps)
	}
	if err := s.RemoveMember(ctx, admin, Options{}, "team-a", "ci"); err != nil || s.State.Current().Principals["ci"].Caps != nil {
		t.Fatalf("leaving the scope removes it: %v %v", err, s.State.Current().Principals["ci"].Caps)
	}

	k, err := s.MintKey(ctx, alice, Options{}, KeyRequest{Owner: "alice", Scope: "team-a", CapMicros: 3})
	if err != nil || s.State.Current().Keys[k.Hash].CapMicros != 3 {
		t.Fatalf("minted with a cap: %v %+v", err, s.State.Current().Keys[k.Hash])
	}
	if _, err := s.MintKey(ctx, alice, Options{}, KeyRequest{Owner: "alice", Scope: "team-a", CapMicros: -1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("minted with a negative cap: %v", err)
	}
	for _, tc := range []struct {
		name string
		c    Caller
		want error
	}{
		{"an outsider", eve, ErrNotFound},
		{"another scope's manager", mgrB, ErrNotFound},
		{"the owner", alice, nil},
		{"the scope's manager", mgrA, nil},
		{"an admin", admin, nil},
	} {
		if err := s.SetKeyCap(ctx, tc.c, Options{}, k.Hash, 4); !errors.Is(err, tc.want) {
			t.Errorf("key cap by %s: %v, want %v", tc.name, err, tc.want)
		}
	}
	if err := s.RevokeKey(ctx, alice, Options{}, k.Hash); err != nil {
		t.Fatal(err)
	}
	if err := s.SetKeyCap(ctx, admin, Options{}, k.Hash, 9); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a revoked key's cap: %v, want ErrInvalid", err)
	}
}
