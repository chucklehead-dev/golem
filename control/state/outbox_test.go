package state

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// bounded is a store over a document with scope a, alice a member of it
// with live keys ka and kb, bob a member of nothing, and an empty outbox
// bounded at 10 (its reserve 8).
func bounded(t *testing.T) *Store {
	t.Helper()
	s := &Store{Bucket: open(t)}
	if _, err := s.Update(t.Context(), func(d *Document) error {
		d.Scopes = map[string]Scope{"a": {}}
		d.Principals = map[string]Principal{"alice": {Kind: "user", Scopes: []string{"a"}}, "bob": {Kind: "service", Scopes: []string{}}}
		d.Keys = map[string]Key{"ka": {Display: "gk-a", Owner: "alice", Scope: "a"}, "kb": {Display: "gk-b", Owner: "alice", Scope: "a"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.MaxOutbox = 10
	return s
}

// event is an audit event of action, as a service writes it.
func event(action string) AuditEvent {
	return AuditEvent{At: time.Unix(1, 0).UTC(), Actor: "root", Action: action, Target: "t"}
}

// fill adds restrictive events that change nothing until the outbox holds n.
func fill(t *testing.T, s *Store, n int) {
	t.Helper()
	if _, err := s.Update(t.Context(), func(d *Document) error {
		for len(d.Audit) < n {
			d.Audit = append(d.Audit, event("membership.remove"))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// grant makes bob a member of a, audited.
func grant(d *Document) error {
	p := d.Principals["bob"]
	p.Scopes = append(slices.Clone(p.Scopes), "a")
	d.Principals["bob"] = p
	d.Audit = append(d.Audit, event("membership.add"))
	return nil
}

// revoke revokes key h, audited.
func revoke(h string) func(*Document) error {
	return func(d *Document) error {
		d.Audit = append(d.Audit, event("key.revoke"))
		return d.Revoke(h, time.Unix(2, 0))
	}
}

// TestOutboxBound: past the reserve a write that grants is refused and a
// restrictive one lands; at the bound every write that adds an event is
// refused and the outbox does not grow; a write that adds none lands; a
// trim makes room again.
func TestOutboxBound(t *testing.T) {
	ctx := t.Context()
	s := bounded(t)
	fill(t, s, Reserve(10)-1)
	if _, err := s.Update(ctx, grant); err != nil {
		t.Fatalf("a grant that brings the outbox to its reserve: %v, want it written", err)
	}
	// The outbox is at its reserve: a grant is refused, before and with
	// validate_only, naming the cause and the fix.
	v := s.Current().StateVersion
	grantKey := func(d *Document) error {
		d.Keys["kc"] = Key{Display: "gk-c", Owner: "alice", Scope: "a"}
		d.Audit = append(d.Audit, event("key.mint"))
		return nil
	}
	for name, do := range map[string]func() error{
		"update":   func() error { _, err := s.Update(ctx, grantKey); return err },
		"validate": func() error { return s.Validate(ctx, grantKey) },
	} {
		err := do()
		full, ok := errors.AsType[*OutboxFull](err)
		if !errors.Is(err, ErrOutboxFull) || !ok || full.Hard() || errors.Is(err, ErrInvariant) {
			t.Fatalf("%s: a mint past the reserve: %v, want ErrOutboxFull for a grant", name, err)
		}
		for _, want := range []string{"key.mint", "-audit-archive", "Audit outbox not draining", "revocations and removals still land"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the refusal %q does not say %q", name, err, want)
			}
		}
	}
	// A restrictive label on a change that grants is judged by the change.
	if _, err := s.Update(ctx, func(d *Document) error {
		d.Keys["kc"] = Key{Display: "gk-c", Owner: "alice", Scope: "a"}
		d.Audit = append(d.Audit, event("key.revoke"))
		return nil
	}); !errors.Is(err, ErrOutboxFull) || !strings.Contains(err.Error(), "key gk-c minted") {
		t.Fatalf("a mint audited as a revocation, past the reserve: %v, want ErrOutboxFull", err)
	}
	if d := s.Current(); d.StateVersion != v || len(d.Audit) != Reserve(10) {
		t.Fatalf("refused grants wrote: version %d (was %d), outbox %d", d.StateVersion, v, len(d.Audit))
	}

	// Restrictive changes land in the reserve, up to the bound.
	if _, err := s.Update(ctx, revoke("ka")); err != nil {
		t.Fatalf("a revocation past the reserve: %v, want it written", err)
	}
	fill(t, s, 10)
	v = s.Current().StateVersion
	err := func() error { _, err := s.Update(ctx, revoke("kb")); return err }()
	if full, ok := errors.AsType[*OutboxFull](err); !ok || !full.Hard() || !strings.Contains(err.Error(), "emergency denylist") {
		t.Fatalf("a revocation at the bound: %v, want the outbox full", err)
	}
	if _, err := s.Update(ctx, grant); !errors.Is(err, ErrOutboxFull) {
		t.Fatalf("a grant at the bound: %v", err)
	}
	if d := s.Current(); d.StateVersion != v || len(d.Audit) != 10 || !d.Keys["kb"].RevokedAt.IsZero() {
		t.Fatalf("at the bound: version %d (was %d), outbox %d, kb revoked %v: nothing may grow", d.StateVersion, v, len(d.Audit), !d.Keys["kb"].RevokedAt.IsZero())
	}
	// A write that adds no event is not the outbox's to refuse.
	if _, err := s.Update(ctx, put("internal", 1)); err != nil {
		t.Fatalf("a publication record at the bound: %v", err)
	}

	// The shipper trims the oldest: grants land again.
	if _, err := s.Update(ctx, func(d *Document) error { d.Audit = slices.Clone(d.Audit[5:]); return nil }); err != nil {
		t.Fatalf("a trim: %v", err)
	}
	if _, err := s.Update(ctx, grantKey); err != nil {
		t.Fatalf("a mint after the trim: %v", err)
	}
}

// TestOutboxDefault: a store with no bound takes DefaultMaxOutbox; a
// change that would pass it in one write is refused too.
func TestOutboxDefault(t *testing.T) {
	s := &Store{Bucket: open(t)}
	if s.OutboxMax() != DefaultMaxOutbox || Reserve(DefaultMaxOutbox) != 8_000 {
		t.Fatalf("bound %d, reserve %d", s.OutboxMax(), Reserve(DefaultMaxOutbox))
	}
	s.MaxOutbox = 4
	if _, err := s.Update(t.Context(), func(d *Document) error {
		for range 5 {
			d.Audit = append(d.Audit, event("key.revoke"))
		}
		return nil
	}); !errors.Is(err, ErrOutboxFull) {
		t.Fatalf("five events into an empty outbox of four: %v", err)
	}
}

// TestUnclassifiedAction: an event of an action the table does not class
// is refused at every write, whatever the outbox holds.
func TestUnclassifiedAction(t *testing.T) {
	s := &Store{Bucket: open(t)}
	if _, err := s.Update(t.Context(), func(d *Document) error {
		d.Audit = append(d.Audit, event("scope.delete"))
		return nil
	}); !errors.Is(err, ErrInvariant) || !strings.Contains(err.Error(), `"scope.delete" has no class`) {
		t.Fatalf("an unclassified action: %v, want ErrInvariant", err)
	}
}

// TestWidens: the effective diff finds each way a change grants, and
// none in a change that only narrows.
func TestWidens(t *testing.T) {
	day := time.Unix(86400, 0).UTC()
	base := func() *Document {
		return &Document{
			Scopes: map[string]Scope{"a": {Groups: []string{"g1"}}, "b": {}},
			Principals: map[string]Principal{
				"alice": {Kind: "user", Entra: "oid-a", UPN: "alice@x", Scopes: []string{"a"}, Synced: []string{"b"}, Default: "a"},
				"ci":    {Kind: "service", Scopes: []string{"a"}},
			},
			Roles: []RoleBinding{{"alice", "manager", "a"}},
			Keys: map[string]Key{
				"k1": {Display: "k1", Owner: "alice", Scope: "a", Models: []string{"m1", "m2"}, ExpiresAt: day},
				"k2": {Display: "k2", Owner: "ci", Scope: "a"},
				"k3": {Display: "k3", Owner: "ci", Scope: "a", RevokedAt: day},
			},
		}
	}
	for name, tc := range map[string]struct {
		change func(d *Document)
		widens bool
	}{
		"nothing":             {func(d *Document) {}, false},
		"a scope created":     {func(d *Document) { d.Scopes["c"] = Scope{} }, true},
		"a group bound":       {func(d *Document) { d.Scopes["b"] = Scope{Groups: []string{"g2"}} }, true},
		"a group unbound":     {func(d *Document) { d.Scopes["a"] = Scope{} }, false},
		"a scope removed":     {func(d *Document) { delete(d.Scopes, "b") }, false},
		"a principal created": {func(d *Document) { d.Principals["bob"] = Principal{Kind: "service"} }, true},
		"a principal removed": {func(d *Document) { delete(d.Principals, "ci") }, false},
		"a membership added":  {func(d *Document) { p := d.Principals["ci"]; p.Scopes = []string{"a", "b"}; d.Principals["ci"] = p }, true},
		"a synced one added": {func(d *Document) {
			p := d.Principals["alice"]
			p.Synced = []string{"a", "b"}
			d.Principals["alice"] = p
		}, false}, // a member of a already
		"a membership removed": {func(d *Document) { p := d.Principals["alice"]; p.Synced = nil; d.Principals["alice"] = p }, false},
		"an Entra ID bound":    {func(d *Document) { p := d.Principals["ci"]; p.Entra = "app-1"; d.Principals["ci"] = p }, true},
		"an Entra ID cleared":  {func(d *Document) { p := d.Principals["alice"]; p.Entra = ""; d.Principals["alice"] = p }, false},
		"a kind changed":       {func(d *Document) { p := d.Principals["ci"]; p.Kind = "user"; d.Principals["ci"] = p }, true},
		"a default and UPN": {func(d *Document) {
			p := d.Principals["alice"]
			p.Default = "b"
			p.UPN = "a@y"
			d.Principals["alice"] = p
		}, false},
		"a role bound":            {func(d *Document) { d.Roles = append(d.Roles, RoleBinding{"ci", "manager", "a"}) }, true},
		"a role removed":          {func(d *Document) { d.Roles = nil }, false},
		"a key minted":            {func(d *Document) { d.Keys["k4"] = Key{Display: "k4", Owner: "ci", Scope: "a"} }, true},
		"a revoked key minted":    {func(d *Document) { d.Keys["k4"] = Key{Display: "k4", Owner: "ci", Scope: "a", RevokedAt: day} }, false},
		"a key revoked":           {func(d *Document) { k := d.Keys["k2"]; k.RevokedAt = day; d.Keys["k2"] = k }, false},
		"a key revived":           {func(d *Document) { k := d.Keys["k3"]; k.RevokedAt = time.Time{}; d.Keys["k3"] = k }, true},
		"a key moved":             {func(d *Document) { k := d.Keys["k2"]; k.Scope = "b"; d.Keys["k2"] = k }, true},
		"a key's models widened":  {func(d *Document) { k := d.Keys["k1"]; k.Models = []string{"m1", "m3"}; d.Keys["k1"] = k }, true},
		"a key's models all":      {func(d *Document) { k := d.Keys["k1"]; k.Models = nil; d.Keys["k1"] = k }, true},
		"a key's models narrowed": {func(d *Document) { k := d.Keys["k1"]; k.Models = []string{"m2"}; d.Keys["k1"] = k }, false},
		"a key's models given":    {func(d *Document) { k := d.Keys["k2"]; k.Models = []string{"m1"}; d.Keys["k2"] = k }, false},
		"an expiry extended":      {func(d *Document) { k := d.Keys["k1"]; k.ExpiresAt = day.Add(time.Hour); d.Keys["k1"] = k }, true},
		"an expiry removed":       {func(d *Document) { k := d.Keys["k1"]; k.ExpiresAt = time.Time{}; d.Keys["k1"] = k }, true},
		"an expiry brought in":    {func(d *Document) { k := d.Keys["k2"]; k.ExpiresAt = day; d.Keys["k2"] = k }, false},
	} {
		old, next := base(), base()
		tc.change(next)
		if got := widens(old, next); (got != "") != tc.widens {
			t.Errorf("%s: widens %q, want %v", name, got, tc.widens)
		}
	}
}

// TestRestoreLandsPastReserve: a restore is restrictive, and lands where a
// grant is refused.
func TestRestoreLandsPastReserve(t *testing.T) {
	s := bounded(t)
	backup := s.Current()
	if _, err := s.Update(t.Context(), func(d *Document) error {
		d.Keys["kc"] = Key{Display: "gk-c", Owner: "alice", Scope: "a"}
		d.Audit = append(d.Audit, event("key.mint"))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fill(t, s, Reserve(10))
	if _, err := s.Restore(t.Context(), backup, "admin:ops", false); err != nil {
		t.Fatalf("a restore past the reserve: %v, want it written", err)
	}
	if s.Current().Keys["kc"].RevokedAt.IsZero() {
		t.Fatal("the restore did not revoke the key minted since the backup")
	}
}

// TestEveryActionClassified: every audit action written anywhere in
// control/ and cmd/ is in the class table, and every action in the table
// is written somewhere. An action is found where it is written: an
// AuditEvent's Action, or the argument of a function's parameter named
// action (control/service's apply, control/entrasync's audit); one that
// is not a literal must be such a parameter, passed on.
func TestEveryActionClassified(t *testing.T) {
	root := filepath.Join("..", "..")
	written := map[string]string{} // action to where
	for _, top := range []string{"control", "cmd"} {
		dirs := map[string][]*ast.File{}
		fset := token.NewFileSet()
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, e fs.DirEntry, err error) error {
			if err != nil || e.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			dirs[filepath.Dir(path)] = append(dirs[filepath.Dir(path)], f)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, files := range dirs {
			// The package's functions with a parameter named action, and its
			// position.
			takes := map[string]int{}
			for _, f := range files {
				for _, d := range f.Decls {
					fd, ok := d.(*ast.FuncDecl)
					if !ok {
						continue
					}
					i := 0
					for _, field := range fd.Type.Params.List {
						for _, n := range field.Names {
							if n.Name == "action" {
								takes[fd.Name.Name] = i
							}
							i++
						}
						if len(field.Names) == 0 {
							i++
						}
					}
				}
			}
			for _, f := range files {
				ast.Inspect(f, func(n ast.Node) bool {
					var at ast.Expr
					switch n := n.(type) {
					case *ast.CompositeLit:
						if !isAuditEvent(n.Type) {
							return true
						}
						for _, el := range n.Elts {
							if kv, ok := el.(*ast.KeyValueExpr); ok {
								if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Action" {
									at = kv.Value
								}
							}
						}
					case *ast.CallExpr:
						name := ""
						switch fn := n.Fun.(type) {
						case *ast.Ident:
							name = fn.Name
						case *ast.SelectorExpr:
							name = fn.Sel.Name
						}
						if i, ok := takes[name]; ok && i < len(n.Args) {
							at = n.Args[i]
						}
					}
					if at == nil {
						return true
					}
					switch v := at.(type) {
					case *ast.BasicLit:
						if s, err := strconv.Unquote(v.Value); err == nil && v.Kind == token.STRING {
							written[s] = fset.Position(v.Pos()).String()
							return true
						}
					case *ast.Ident:
						if v.Name == "action" {
							return true // a parameter, passed on: its callers' literals are found
						}
					}
					t.Errorf("%s: an audit action that is not a literal or a parameter named action: write it as a literal, so this test can class it", fset.Position(at.Pos()))
					return true
				})
			}
		}
	}
	for _, a := range slices.Sorted(func(yield func(string) bool) {
		for a := range written {
			if !yield(a) {
				return
			}
		}
	}) {
		if _, ok := ClassOf(a); !ok {
			t.Errorf("%s: audit action %q has no class: add it to the actions table in control/state/outbox.go (Widening unless it can only narrow access)", written[a], a)
		}
	}
	for _, a := range Actions() {
		if _, ok := written[a]; !ok {
			t.Errorf("the actions table classes %q, which nothing in control/ or cmd/ writes: remove it", a)
		}
	}
	// Not vacuous: the writers are found.
	for _, a := range []string{"key.mint", "key.revoke", "membership.sync.remove", "principal.create", "state.restore"} {
		if _, ok := written[a]; !ok {
			t.Errorf("%s not found where it is written: the search is broken", a)
		}
	}
}

// isAuditEvent reports whether a composite literal's type is AuditEvent,
// in this package or another's.
func isAuditEvent(e ast.Expr) bool {
	switch e := e.(type) {
	case *ast.Ident:
		return e.Name == "AuditEvent"
	case *ast.SelectorExpr:
		return e.Sel.Name == "AuditEvent"
	}
	return false
}
