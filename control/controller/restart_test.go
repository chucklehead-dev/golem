package controller

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// TestRestartOverBrokenConfig: a leader that starts while the config
// directory does not compile (here, r2 taking over from r1 after a broken
// edit) renews the published config, verified from the bucket, with the
// current entitlements and a new lease, and says so; once the directory
// compiles, that is published.
func TestRestartOverBrokenConfig(t *testing.T) {
	w := newWorld(t)
	r2 := w.replicas["r2"]
	w.lead("r1")
	if p, err := w.step("r1"); !p || err != nil {
		t.Fatalf("first publication: %v %v", p, err)
	}
	current := func() *snapshot.Snapshot {
		c, err := w.gateway.Holder.Current()
		if err != nil {
			t.Fatal(err)
		}
		return c.Snapshot
	}
	first := current()
	w.write("broken.yaml", "apiVersion: golem.dev/v1alpha1\nkind: ModelRoute\nmetadata: {name: x}\nspec: {alias: x, pool: [{backend: nowhere, deployment: d}]}\n")

	// r1 is gone; r2 starts and takes the lapsed lease.
	w.lead("r2")
	r2.clock = r2.clock.Add(time.Minute)
	if w.lead("r2") != 2 {
		t.Fatal("r2 takes the lease")
	}
	if _, err := r2.Elector.State.Update(t.Context(), func(d *state.Document) error {
		d.Scopes = map[string]state.Scope{"team-a": {Models: []string{"claude"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	validUntil := first.ValidUntil
	for i := range 2 {
		p, err := w.step("r2")
		if !p || err == nil || !strings.Contains(err.Error(), "renewing the published config") || !strings.Contains(err.Error(), `no Backend named "nowhere"`) {
			t.Fatalf("renewal %d: published %v, %v; want the published config renewed and the directory's error", i, p, err)
		}
		s := current()
		if s.PublishSeq != uint64(2+i) || !s.ValidUntil.After(validUntil) {
			t.Fatalf("renewal %d: publication %d valid until %v, was %v", i, s.PublishSeq, s.ValidUntil, validUntil)
		}
		if !reflect.DeepEqual(s.Backends, first.Backends) || !reflect.DeepEqual(s.Routes, first.Routes) || s.Source.ConfigDigest != first.Source.ConfigDigest {
			t.Fatalf("renewal %d changed the config part: %+v", i, s)
		}
		if len(s.Entitlements.Scopes) != 1 || s.Entitlements.Scopes[0].ID != "team-a" {
			t.Fatalf("renewal %d: entitlements %+v; want the state's as of now", i, s.Entitlements)
		}
		validUntil = s.ValidUntil
		r2.clock = r2.clock.Add(61 * time.Second)
		w.lead("r2")
	}
	// The directory is fixed, with a new route: that is published.
	w.write("broken.yaml", route("fast"))
	if p, err := w.step("r2"); !p || err != nil {
		t.Fatalf("after the fix: %v %v", p, err)
	}
	if s := current(); len(s.Routes) != 2 {
		t.Fatalf("routes %+v: want the fixed directory's", s.Routes)
	}
}
