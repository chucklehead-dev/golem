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
	s := &Service{State: &state.Store{Bucket: b}, Now: func() time.Time { return time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC) }}
	ctx := t.Context()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.CreateScope(ctx, admin, Options{}, "team-a", state.Scope{Models: []string{"claude", "gpt"}}))
	must(s.CreateScope(ctx, admin, Options{}, "team-b", state.Scope{Models: []string{"claude"}}))
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
			return s.CreateScope(t.Context(), c, Options{}, "new", state.Scope{})
		},
		"add a member to team-a": func(s *Service, c Caller) error {
			return s.AddMember(t.Context(), c, Options{}, "team-a", Member{ID: "zed", Kind: "user"})
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
			if got := err == nil; got != want || (!want && !errors.Is(err, ErrPermission)) {
				t.Errorf("%s by %s: %v, want allowed=%v", name, c.Principal, err, want)
			}
		}
	}
}

// TestRevokeKey: a key is revoked by its owner, its scope's managers or
// admins, and by no one else.
func TestRevokeKey(t *testing.T) {
	for _, tc := range []struct {
		c    Caller
		want bool
	}{{admin, true}, {mgrA, true}, {alice, true}, {mgrB, false}, {eve, false}} {
		s := setup(t)
		k, err := s.MintKey(t.Context(), alice, Options{}, KeyRequest{Owner: "alice", Scope: "team-a"})
		if err != nil {
			t.Fatal(err)
		}
		err = s.RevokeKey(t.Context(), tc.c, Options{}, k.Hash)
		if (err == nil) != tc.want || (!tc.want && !errors.Is(err, ErrPermission)) {
			t.Errorf("revoke by %s: %v, want allowed=%v", tc.c.Principal, err, tc.want)
		}
		if revoked := !s.State.Current().Keys[k.Hash].RevokedAt.IsZero(); revoked != tc.want {
			t.Errorf("revoke by %s: revoked %v", tc.c.Principal, revoked)
		}
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
