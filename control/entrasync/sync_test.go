package entrasync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/bucket"
)

// dir is a directory in memory: users by group; a group absent is gone.
// An account is as accounts has it, else as a group lists it, else live
// and enabled (a user in no group).
type dir struct {
	groups   map[string][]User
	accounts map[string]User
	fail     error
	lookups  int
}

func (d *dir) User(_ context.Context, id string) (User, error) {
	d.lookups++
	if d.fail != nil {
		return User{}, d.fail
	}
	if u, ok := d.accounts[id]; ok {
		if u == (User{}) {
			return User{}, ErrNoUser
		}
		return u, nil
	}
	for _, us := range d.groups {
		for _, u := range us {
			if u.ID == id {
				return u, nil
			}
		}
	}
	return User{ID: id, Enabled: true}, nil
}

func (d *dir) Members(_ context.Context, g string) ([]User, error) {
	if d.fail != nil {
		return nil, d.fail
	}
	us, ok := d.groups[g]
	if !ok {
		return nil, ErrNoGroup
	}
	return us, nil
}

var at = time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)

// lead1 is the leadership the tests' syncs run under.
var lead1 = state.Fence{Holder: "r1", Epoch: 1}

func setup(t *testing.T, d *dir) *Sync {
	t.Helper()
	b, err := bucket.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	st := &state.Store{Bucket: b}
	if _, err := st.Update(t.Context(), func(doc *state.Document) error {
		doc.Scopes = map[string]state.Scope{"team-a": {Groups: []string{"g-a"}}, "team-b": {Groups: []string{"g-b", "g-nested"}}, "team-c": {}}
		// carol is a member of team-c in golem, and of team-a by group.
		doc.Principals = map[string]state.Principal{
			"carol": {Kind: "user", Entra: "oid-c", Scopes: []string{"team-c"}, Synced: []string{"team-a"}},
			"ci":    {Kind: "service", Scopes: []string{"team-c"}},
		}
		doc.Keys = map[string]state.Key{"kc": {Owner: "carol", Scope: "team-a"}, "kc2": {Owner: "carol", Scope: "team-c"}}
		doc.Fence = &state.Fence{Holder: "r1", Epoch: 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return &Sync{State: st, Directory: d, Now: func() time.Time { return at }}
}

func alive(d *state.Document, h string) bool { return d.Keys[h].RevokedAt.IsZero() }

// TestSync: members of bound groups become members of their scopes, are
// created if new, keep memberships granted in golem, and lose synced ones
// (and the keys in them) when they leave; a disabled account loses all.
func TestSync(t *testing.T) {
	d := &dir{groups: map[string][]User{
		"g-a":      {{ID: "oid-a", UPN: "Alice@Example.com", Enabled: true}, {ID: "oid-c", UPN: "carol@example.com", Enabled: true}},
		"g-b":      {{ID: "oid-a", UPN: "Alice@Example.com", Enabled: true}},
		"g-nested": {{ID: "oid-x", UPN: "x@example.com", Enabled: false}},
	}}
	s := setup(t, d)
	res, err := s.Step(t.Context(), lead1)
	if err != nil {
		t.Fatal(err)
	}
	doc := s.State.Current()
	alice := doc.Principals["alice@example.com"]
	if alice.Entra != "oid-a" || alice.Kind != "user" || strings.Join(alice.Synced, ",") != "team-a,team-b" {
		t.Fatalf("alice %+v", alice)
	}
	if _, ok := doc.Principals["x@example.com"]; ok || res.Created != 1 {
		t.Fatalf("a disabled account was created (%+v)", res)
	}
	if v := doc.StateVersion; func() bool {
		r, _ := s.Step(t.Context(), lead1)
		return s.State.Current().StateVersion != v || r != (Result{})
	}() {
		t.Fatal("a step with nothing to change wrote")
	}

	// carol leaves g-a: her synced team-a ends, with its key; team-c,
	// granted in golem, stays, with its key.
	d.groups["g-a"] = d.groups["g-a"][:1]
	if _, err := s.Step(t.Context(), lead1); err != nil {
		t.Fatal(err)
	}
	doc = s.State.Current()
	if c := doc.Principals["carol"]; len(c.Synced) != 0 || !c.MemberOf("team-c") || alive(doc, "kc") || !alive(doc, "kc2") {
		t.Fatalf("carol %+v, keys kc %v kc2 %v", c, alive(doc, "kc"), alive(doc, "kc2"))
	}

	// alice is disabled: every synced membership and key ends.
	if _, err := s.State.Update(t.Context(), func(doc *state.Document) error {
		doc.Keys["ka"] = state.Key{Owner: "alice@example.com", Scope: "team-b"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for g := range d.groups {
		for i := range d.groups[g] {
			if d.groups[g][i].ID == "oid-a" {
				d.groups[g][i].Enabled = false
			}
		}
	}
	if _, err := s.Step(t.Context(), lead1); err != nil {
		t.Fatal(err)
	}
	doc = s.State.Current()
	if a := doc.Principals["alice@example.com"]; len(a.Synced) != 0 || alive(doc, "ka") {
		t.Fatalf("disabled alice %+v, key alive %v", a, alive(doc, "ka"))
	}
	var actions []string
	for _, e := range doc.Audit {
		if e.Actor == Actor {
			actions = append(actions, e.Action+" "+e.Target)
		}
	}
	if len(actions) < 3 {
		t.Fatalf("audit %v: each change by the sync", actions)
	}
}

// TestSyncSafety: a directory error changes nothing; a deleted group's
// memberships end; a step ending more than MaxRemovals is refused whole.
func TestSyncSafety(t *testing.T) {
	d := &dir{groups: map[string][]User{"g-a": {{ID: "oid-c", UPN: "carol@example.com", Enabled: true}}, "g-b": {}, "g-nested": {}}}
	s := setup(t, d)
	if _, err := s.Step(t.Context(), lead1); err != nil {
		t.Fatal(err)
	}
	before := s.State.Current().StateVersion
	d.fail = errors.New("Graph: 503")
	if _, err := s.Step(t.Context(), lead1); err == nil || s.State.Current().StateVersion != before {
		t.Fatalf("a directory error: %v, written %v", err, s.State.Current().StateVersion != before)
	}
	d.fail = nil

	s.MaxRemovals = 0       // the default, 50
	delete(d.groups, "g-a") // deleted in Entra
	if _, err := s.Step(t.Context(), lead1); err != nil {
		t.Fatal(err)
	}
	if c := s.State.Current().Principals["carol"]; len(c.Synced) != 0 {
		t.Fatalf("a deleted group's membership kept: %+v", c)
	}

	var many []User
	for i := range 60 {
		many = append(many, User{ID: "oid-" + string(rune('A'+i%26)) + string(rune('a'+i/26)), UPN: "u" + string(rune('A'+i%26)) + string(rune('a'+i/26)) + "@example.com", Enabled: true})
	}
	d.groups["g-b"] = many
	if _, err := s.Step(t.Context(), lead1); err != nil {
		t.Fatal(err)
	}
	d.groups["g-b"] = nil // emptied by mistake
	before = s.State.Current().StateVersion
	if _, err := s.Step(t.Context(), lead1); !errors.Is(err, ErrTooManyRemovals) || s.State.Current().StateVersion != before {
		t.Fatalf("60 removals at once: %v", err)
	}
	s.MaxRemovals = 100
	if r, err := s.Step(t.Context(), lead1); err != nil || r.Removed != 60 {
		t.Fatalf("with the limit raised: %+v %v", r, err)
	}
}

// TestSyncOffboarding: the removal guard holds back bulk removals of users
// still active, never offboarding: 60 users disabled at once, or deleted
// at once (gone from their groups, in the deleted items), lose every
// synced membership and key despite the guard of 50; 60 active users
// dropped from a group trip it and nothing is written; in a step that
// trips it, only the offboarding is written; and an account whose state
// the directory does not report is not taken for offboarding.
func TestSyncOffboarding(t *testing.T) {
	cohort := func(name string, n int) []User {
		var us []User
		for i := range n {
			id := fmt.Sprintf("oid-%s-%02d", name, i)
			us = append(us, User{ID: id, UPN: fmt.Sprintf("%s%02d@example.com", name, i), Enabled: true})
		}
		return us
	}
	active, disabled, deleted, few := cohort("a", 60), cohort("b", 60), cohort("c", 60), cohort("d", 5)
	d := &dir{groups: map[string][]User{"g-a": {}, "g-nested": {},
		"g-b": slices.Concat(active, disabled, deleted, few)}, accounts: map[string]User{}}
	s := setup(t, d)
	if r, err := s.Step(t.Context(), lead1); err != nil || r.Created != 185 {
		t.Fatalf("first sync: %+v %v", r, err)
	}
	// Each user gets a key in team-b.
	if _, err := s.State.Update(t.Context(), func(doc *state.Document) error {
		for _, u := range d.groups["g-b"] {
			doc.Keys["k-"+u.ID] = state.Key{Owner: u.UPN, Scope: "team-b"}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	set := func(us []User) {
		var keep []User
		for _, u := range d.groups["g-b"] {
			if !slices.ContainsFunc(us, func(x User) bool { return x.ID == u.ID }) {
				keep = append(keep, u)
			}
		}
		d.groups["g-b"] = keep
	}
	offboarded := func(us []User) bool {
		doc := s.State.Current()
		for _, u := range us {
			if p := doc.Principals[u.UPN]; len(p.Synced) != 0 || alive(doc, "k-"+u.ID) {
				return false
			}
		}
		return true
	}
	untouched := func(us []User) bool {
		doc := s.State.Current()
		for _, u := range us {
			if p := doc.Principals[u.UPN]; strings.Join(p.Synced, ",") != "team-b" || !alive(doc, "k-"+u.ID) {
				return false
			}
		}
		return true
	}

	// 60 active users dropped from the group: the guard trips, nothing is
	// written.
	set(active)
	before := s.State.Current().StateVersion
	if _, err := s.Step(t.Context(), lead1); !errors.Is(err, ErrTooManyRemovals) || s.State.Current().StateVersion != before {
		t.Fatalf("60 active users dropped: %v, written %v; want the guard, nothing written", err, s.State.Current().StateVersion != before)
	}
	d.groups["g-b"] = append(d.groups["g-b"], active...)

	// 60 disabled at once, still in the group: all offboarded.
	for i, u := range d.groups["g-b"] {
		if slices.ContainsFunc(disabled, func(x User) bool { return x.ID == u.ID }) {
			d.groups["g-b"][i].Enabled, d.groups["g-b"][i].Disabled = false, true
		}
	}
	if r, err := s.Step(t.Context(), lead1); err != nil || r.Offboarded != 60 || r.Removed != 60 || r.Revoked != 60 || !offboarded(disabled) {
		t.Fatalf("60 disabled: %+v %v; want all offboarded despite the guard", r, err)
	}

	// 60 deleted at once: Entra drops them from the group; their accounts
	// are in the deleted items.
	set(deleted)
	for _, u := range deleted {
		d.accounts[u.ID] = User{ID: u.ID, Deleted: true}
	}
	if r, err := s.Step(t.Context(), lead1); err != nil || r.Offboarded != 60 || !offboarded(deleted) {
		t.Fatalf("60 deleted: %+v %v; want all offboarded despite the guard", r, err)
	}

	// A step that trips the guard still offboards: 60 active users dropped
	// and five deleted.
	set(active)
	set(few)
	for _, u := range few {
		d.accounts[u.ID] = User{ID: u.ID, Deleted: true}
	}
	r, err := s.Step(t.Context(), lead1)
	if !errors.Is(err, ErrTooManyRemovals) || r.Offboarded != 5 || !offboarded(few) || !untouched(active) {
		t.Fatalf("mixed: %+v %v; want the guard, the five offboarded and the 60 untouched", r, err)
	}
	if !strings.Contains(err.Error(), "only the offboarding of 5") {
		t.Fatalf("the guard's error does not say what was written: %v", err)
	}

	// Accounts whose state the directory does not report (neither enabled
	// nor disabled, nor deleted) are removals the guard counts.
	d.groups["g-b"] = nil
	for _, u := range active {
		d.accounts[u.ID] = User{ID: u.ID}
		d.groups["g-b"] = append(d.groups["g-b"], User{ID: u.ID, UPN: u.UPN})
	}
	before = s.State.Current().StateVersion
	if _, err := s.Step(t.Context(), lead1); !errors.Is(err, ErrTooManyRemovals) || s.State.Current().StateVersion != before {
		t.Fatalf("60 accounts of unreported state: %v; want the guard, nothing written", err)
	}
}

// TestSyncNames: a new user's ID is their sign-in name, unless taken or
// reserved; a principal granted in golem is never taken over by name.
func TestSyncNames(t *testing.T) {
	d := &dir{groups: map[string][]User{
		"g-a":      {{ID: "oid-ci-person", UPN: "ci", Enabled: true}, {ID: "oid-r1234567", UPN: "entra:someone", Enabled: true}},
		"g-b":      {},
		"g-nested": {},
	}}
	s := setup(t, d)
	if _, err := s.Step(t.Context(), lead1); err != nil {
		t.Fatal(err)
	}
	doc := s.State.Current()
	if ci := doc.Principals["ci"]; ci.Kind != "service" || ci.Entra != "" {
		t.Fatalf("the service ci was taken over: %+v", ci)
	}
	if p, ok := doc.Principals["ci#oid-ci-p"]; !ok || p.Entra != "oid-ci-person" {
		t.Fatalf("principals %v", keys(doc.Principals))
	}
	if p, ok := doc.Principals["user-oid-r1234567"]; !ok || p.UPN != "entra:someone" {
		t.Fatalf("a sign-in name in the reserved namespace: principals %v", keys(doc.Principals))
	}
}

func keys[V any](m map[string]V) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// TestGraph: Graph's pages are followed, users' fields read, a 404 is a
// missing group, and a next page off Graph is refused (the token stays).
func TestGraph(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no token", 401)
			return
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1.0/groups/gone/"):
			http.NotFound(w, r)
		case strings.HasPrefix(r.URL.Path, "/v1.0/groups/evil/"):
			json.NewEncoder(w).Encode(map[string]any{"value": []any{}, "@odata.nextLink": "https://elsewhere.example/steal"})
		case r.URL.Path == "/v1.0/users/live" && r.URL.Query().Get("$select") == "id,userPrincipalName,accountEnabled":
			json.NewEncoder(w).Encode(map[string]any{"id": "live", "userPrincipalName": "l@x", "accountEnabled": false})
		case r.URL.Path == "/v1.0/directory/deletedItems/deleted":
			json.NewEncoder(w).Encode(map[string]any{"id": "deleted", "userPrincipalName": "deleteddeleted@x"})
		case strings.HasPrefix(r.URL.Path, "/v1.0/users/"), strings.HasPrefix(r.URL.Path, "/v1.0/directory/deletedItems/"):
			http.NotFound(w, r)
		case r.URL.Query().Get("page") == "2":
			json.NewEncoder(w).Encode(map[string]any{"value": []any{map[string]any{"id": "o2", "userPrincipalName": "b@x", "accountEnabled": false}}})
		default:
			if r.URL.Path != "/v1.0/groups/g1/transitiveMembers/microsoft.graph.user" || r.URL.Query().Get("$select") != "id,userPrincipalName,accountEnabled" {
				http.Error(w, "path "+r.URL.Path, 400)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"value": []any{map[string]any{"id": "o1", "userPrincipalName": "a@x", "accountEnabled": true},
				map[string]any{"id": "o3", "userPrincipalName": "c@x"}}, "@odata.nextLink": srv.URL + "/v1.0/groups/g1/next?page=2"})
		}
	}))
	defer srv.Close()
	g := &Graph{Base: srv.URL + "/v1.0", Token: func(context.Context) (string, error) { return "tok", nil }}
	us, err := g.Members(t.Context(), "g1")
	if err != nil || len(us) != 3 || us[0] != (User{ID: "o1", UPN: "a@x", Enabled: true}) ||
		us[1] != (User{ID: "o3", UPN: "c@x"}) || us[2] != (User{ID: "o2", UPN: "b@x", Disabled: true}) {
		t.Fatalf("%+v %v: want enabled, unreported (neither) and disabled", us, err)
	}
	// An account: live (disabled here), deleted (in the deleted items), or
	// neither (a 404 from both is not taken for a deletion).
	if u, err := g.User(t.Context(), "live"); err != nil || u != (User{ID: "live", UPN: "l@x", Disabled: true}) {
		t.Fatalf("a live account: %+v %v", u, err)
	}
	if u, err := g.User(t.Context(), "deleted"); err != nil || !u.Deleted || u.ID != "deleted" {
		t.Fatalf("a deleted account: %+v %v", u, err)
	}
	if _, err := g.User(t.Context(), "purged"); !errors.Is(err, ErrNoUser) {
		t.Fatalf("an account in neither: %v, want ErrNoUser", err)
	}
	if _, err := g.Members(t.Context(), "gone"); !errors.Is(err, ErrNoGroup) {
		t.Fatalf("a missing group: %v", err)
	}
	if _, err := g.Members(t.Context(), "evil"); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("a next page elsewhere: %v", err)
	}
}

// TestFederatedToken: the projected token is the client assertion, and
// the answer is cached.
func TestFederatedToken(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		calls++
		if r.URL.Path != "/t1/oauth2/v2.0/token" || r.Form.Get("client_assertion") != "sa-token" || r.Form.Get("client_id") != "app" ||
			r.Form.Get("scope") != "https://graph.microsoft.com/.default" {
			json.NewEncoder(w).Encode(map[string]any{"error": "invalid_client"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"access_token": "graph-tok", "expires_in": 3600})
	}))
	defer srv.Close()
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte("sa-token\n"), 0o600)
	f := &FederatedToken{Tenant: "t1", ClientID: "app", TokenFile: file, Authority: srv.URL}
	for range 2 {
		if tok, err := f.Token(t.Context()); err != nil || tok != "graph-tok" {
			t.Fatalf("%q %v", tok, err)
		}
	}
	if calls != 1 {
		t.Fatalf("%d token requests, want 1 (cached)", calls)
	}
}

// deposing is a directory during whose read another replica claims the fence.
type deposing struct {
	*dir
	st *state.Store
}

func (d deposing) Members(ctx context.Context, g string) ([]User, error) {
	d.st.Update(ctx, func(doc *state.Document) error {
		doc.Fence = &state.Fence{Holder: "r2", Epoch: 2}
		return nil
	})
	return d.dir.Members(ctx, g)
}

// TestSyncFenced: a sync whose replica was deposed while it read the
// directory writes nothing (model/leadership.md's noStaleSync), and one run
// at a stale epoch is refused outright.
func TestSyncFenced(t *testing.T) {
	d := &dir{groups: map[string][]User{"g-a": {{ID: "oid-z", UPN: "zed@example.com", Enabled: true}}, "g-b": {}, "g-nested": {}}}
	s := setup(t, d)
	other := &state.Store{Bucket: s.State.Bucket}
	s.Directory = deposing{d, other}
	before := s.State.Current().Principals
	if _, err := s.Step(t.Context(), lead1); !errors.Is(err, state.ErrFenced) {
		t.Fatalf("a sync deposed mid-read: %v, want state.ErrFenced", err)
	}
	s.State.Refresh(t.Context())
	if _, ok := s.State.Current().Principals["zed@example.com"]; ok || len(s.State.Current().Principals) != len(before) {
		t.Fatal("the deposed leader's sync wrote")
	}
	s.Directory = d
	if _, err := s.Step(t.Context(), lead1); !errors.Is(err, state.ErrFenced) {
		t.Fatalf("a sync at a stale epoch: %v", err)
	}
	if _, err := s.Step(t.Context(), state.Fence{Holder: "r2", Epoch: 2}); err != nil {
		t.Fatalf("the new leader's sync: %v", err)
	}
}

// TestSyncOutboxBackpressure: past the audit outbox's reserve a step
// writes its removals and offboarding and holds back its creations and
// additions, reporting state.ErrOutboxFull; a user moved between groups
// loses the old membership and waits for the new. Once the outbox is
// trimmed, the next step writes the rest.
func TestSyncOutboxBackpressure(t *testing.T) {
	d := &dir{groups: map[string][]User{
		"g-a":      {{ID: "oid-c", UPN: "carol@example.com", Enabled: true}},
		"g-b":      {{ID: "oid-d", UPN: "dan@example.com", Enabled: true}},
		"g-nested": {},
	}}
	s := setup(t, d)
	if _, err := s.Step(t.Context(), lead1); err != nil {
		t.Fatal(err)
	}
	s.State.MaxOutbox = 20
	if _, err := s.State.Update(t.Context(), func(doc *state.Document) error {
		for len(doc.Audit) < state.Reserve(20) {
			doc.Audit = append(doc.Audit, state.AuditEvent{At: at, Actor: Actor, Action: "membership.sync.remove", Target: "none"})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// carol is disabled (offboarding), dan moves from g-b to g-a, and erin
	// joins g-b (a creation).
	d.groups["g-a"] = []User{{ID: "oid-c", UPN: "carol@example.com", Disabled: true}, {ID: "oid-d", UPN: "dan@example.com", Enabled: true}}
	d.groups["g-b"] = []User{{ID: "oid-e", UPN: "erin@example.com", Enabled: true}}
	res, err := s.Step(t.Context(), lead1)
	if !errors.Is(err, state.ErrOutboxFull) || !strings.Contains(err.Error(), "only removals and offboarding were written") {
		t.Fatalf("a step past the reserve: %+v %v, want ErrOutboxFull with the removals written", res, err)
	}
	doc := s.State.Current()
	if c := doc.Principals["carol"]; len(c.Synced) != 0 || alive(doc, "kc") {
		t.Fatalf("carol not offboarded: %+v, kc alive %v", c, alive(doc, "kc"))
	}
	if dan := doc.Principals["dan@example.com"]; len(dan.Synced) != 0 {
		t.Fatalf("dan %+v: his team-b membership should end, his team-a one wait", dan)
	}
	if _, ok := doc.Principals["erin@example.com"]; ok || res.Created != 0 || res.Added != 0 || res.Offboarded != 1 {
		t.Fatalf("held back: erin created %v, %+v", ok, res)
	}
	for _, e := range doc.Audit[state.Reserve(20):] {
		if c, _ := state.ClassOf(e.Action); c != state.Restrictive {
			t.Errorf("a %s event written past the reserve", e.Action)
		}
	}

	// The shipper trims the outbox: the next step writes the rest.
	if _, err := s.State.Update(t.Context(), func(doc *state.Document) error { doc.Audit = nil; return nil }); err != nil {
		t.Fatal(err)
	}
	if res, err := s.Step(t.Context(), lead1); err != nil || res.Created != 1 || res.Added != 2 {
		t.Fatalf("after the trim: %+v %v, want erin created and dan and erin added", res, err)
	}
	doc = s.State.Current()
	if !doc.Principals["dan@example.com"].MemberOf("team-a") || !doc.Principals["erin@example.com"].MemberOf("team-b") {
		t.Fatalf("after the trim: dan %+v, erin %+v", doc.Principals["dan@example.com"], doc.Principals["erin@example.com"])
	}
}
