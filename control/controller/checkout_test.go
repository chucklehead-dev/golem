package controller

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// gitSync is a checkout as git-sync lays it out under its --root: a
// worktree per commit in .worktrees/<hash>, the link current pointing at
// one (relative, swapped atomically), and .git/FETCH_HEAD rewritten at
// every fetch.
type gitSync struct {
	t    *testing.T
	root string
}

func newGitSync(t *testing.T) *gitSync {
	g := &gitSync{t: t, root: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(g.root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return g
}

func (g *gitSync) link() string    { return filepath.Join(g.root, "current") }
func (g *gitSync) fetched() string { return filepath.Join(g.root, ".git", "FETCH_HEAD") }

// sync checks out hash with files, points the link at it, and records a
// fetch at at.
func (g *gitSync) sync(hash string, files map[string]string, at time.Time) {
	wt := filepath.Join(g.root, ".worktrees", hash)
	if err := os.MkdirAll(wt, 0o755); err != nil {
		g.t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(wt, name), []byte(body), 0o644); err != nil {
			g.t.Fatal(err)
		}
	}
	tmp := filepath.Join(g.root, "tmp-link")
	if err := os.Symlink(filepath.Join(".worktrees", hash), tmp); err != nil {
		g.t.Fatal(err)
	}
	if err := os.Rename(tmp, g.link()); err != nil {
		g.t.Fatal(err)
	}
	g.fetch(at)
}

// fetch records a fetch at at that found the branch where it was.
func (g *gitSync) fetch(at time.Time) {
	if err := os.WriteFile(g.fetched(), []byte("deadbeef\t\tbranch 'main'\n"), 0o644); err != nil {
		g.t.Fatal(err)
	}
	if err := os.Chtimes(g.fetched(), at, at); err != nil {
		g.t.Fatal(err)
	}
}

// TestCheckout: golem-control reads the config through git-sync's link,
// publishes the commit as the snapshot's configRevision, follows the link
// to the next commit, and stops renewing leases once the checkout has not
// been fetched for -max-config-age: the gateway's lease runs out at the
// last renewal's ValidUntil. A change of the entitlements meanwhile is
// still published, its lease not extended; a fetch again resumes the
// renewals.
func TestCheckout(t *testing.T) {
	const a, b = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	w := newWorld(t)
	r1 := w.replicas["r1"]
	g := newGitSync(t)
	g.sync(a, map[string]string{"backend.yaml": backend, "claude.yaml": route("claude")}, r1.clock)
	r1.Dir = g.link()
	r1.Checkout = &Checkout{Link: g.link(), Fetched: g.fetched()}
	r1.MaxConfigAge = 30 * time.Minute
	w.lead("r1")

	current := func() *snapshot.Snapshot {
		t.Helper()
		c, err := w.gateway.Holder.Current()
		if err != nil {
			t.Fatal(err)
		}
		return c.Snapshot
	}
	if p, err := w.step("r1"); !p || err != nil {
		t.Fatalf("first publication: %v %v", p, err)
	}
	if s := current(); s.Source.ConfigRevision != a {
		t.Fatalf("configRevision %q, want the checkout's commit %s", s.Source.ConfigRevision, a)
	}

	// git-sync moves to the next commit: a route added.
	r1.clock = r1.clock.Add(time.Minute)
	g.sync(b, map[string]string{"backend.yaml": backend, "claude.yaml": route("claude"), "haiku.yaml": route("haiku")}, r1.clock)
	if p, err := w.step("r1"); !p || err != nil {
		t.Fatalf("the next commit: %v %v", p, err)
	}
	if s := current(); s.Source.ConfigRevision != b || len(s.Routes) != 2 {
		t.Fatalf("configRevision %q with %d routes, want %s with 2", s.Source.ConfigRevision, len(s.Routes), b)
	}
	if age, ok := r1.ConfigAge(); !ok || age != 0 {
		t.Fatalf("config age %v %v, want 0 just after a fetch", age, ok)
	}

	// Fetched every period: renewals go on past -max-config-age's span.
	for range 40 {
		r1.clock = r1.clock.Add(time.Minute)
		g.fetch(r1.clock)
		if _, err := w.step("r1"); err != nil {
			t.Fatalf("a renewal with the checkout fetched: %v", err)
		}
	}
	renewed := current()
	if renewed.ValidUntil != r1.clock.Add(4*time.Hour) {
		t.Fatalf("valid until %v, want %v: renewed every minute while fetched", renewed.ValidUntil, r1.clock.Add(4*time.Hour))
	}

	// git-sync stalls: no fetch for longer than -max-config-age.
	stalled := r1.clock
	r1.clock = stalled.Add(31 * time.Minute)
	p, err := w.step("r1")
	if p || !errors.Is(err, ErrStale) || !strings.Contains(err.Error(), "-max-config-age 30m0s") {
		t.Fatalf("a stale checkout: published %v, %v; want no renewal and ErrStale", p, err)
	}
	if age, ok := r1.ConfigAge(); !ok || age != 31*time.Minute {
		t.Fatalf("config age %v %v, want 31m", age, ok)
	}
	if s := current(); s.PublishSeq != renewed.PublishSeq || s.ValidUntil != renewed.ValidUntil {
		t.Fatalf("the gateway holds %d to %v: want %d to %v, the lease not renewed", s.PublishSeq, s.ValidUntil, renewed.PublishSeq, renewed.ValidUntil)
	}

	// A change of the entitlements is published, within the lease granted.
	if _, err := r1.State.Update(t.Context(), func(d *state.Document) error {
		d.Principals = map[string]state.Principal{"bob": {Kind: "user"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r1.clock = r1.clock.Add(time.Minute)
	if p, err := w.step("r1"); !p || !errors.Is(err, ErrStale) {
		t.Fatalf("an entitlements change while stale: published %v, %v; want it published and ErrStale", p, err)
	}
	if s := current(); s.PublishSeq != renewed.PublishSeq+1 || s.ValidUntil != renewed.ValidUntil {
		t.Fatalf("the gateway holds %d to %v: want %d to %v, the lease not extended", s.PublishSeq, s.ValidUntil, renewed.PublishSeq+1, renewed.ValidUntil)
	}
	// Hours on, nothing renews it: the lease runs out.
	r1.clock = renewed.ValidUntil.Add(-time.Minute)
	if p, _ := w.step("r1"); p {
		t.Fatal("renewed a stale checkout's config")
	}
	r1.clock = renewed.ValidUntil
	if _, err := w.gateway.Holder.Current(); err == nil {
		t.Fatal("the gateway's lease outlived a stale checkout")
	}

	// git-sync fetches again: renewals resume.
	g.fetch(r1.clock)
	if p, err := r1.Step(t.Context()); !p || err != nil {
		t.Fatalf("after a fetch: published %v, %v; want the renewal", p, err)
	}
}

// TestCheckoutAgeUnknown: a checkout whose last fetch cannot be read is
// treated as stale, failing closed: no renewal.
func TestCheckoutAgeUnknown(t *testing.T) {
	w := newWorld(t)
	r1 := w.replicas["r1"]
	r1.Checkout = &Checkout{Fetched: filepath.Join(t.TempDir(), "FETCH_HEAD")}
	r1.MaxConfigAge = 30 * time.Minute
	w.lead("r1")
	if p, err := w.step("r1"); p || !errors.Is(err, ErrStale) || !strings.Contains(err.Error(), "age is unknown") {
		t.Fatalf("published %v, %v; want nothing published and ErrStale", p, err)
	}
	if _, ok := r1.ConfigAge(); ok {
		t.Fatal("an unreadable fetch has an age")
	}
	if n := r1.Failures()[Failure{"internal", ReasonConfigStale}]; n != 1 {
		t.Fatalf("%d config_stale failures, want 1: a first publication would grant a lease", n)
	}
}
