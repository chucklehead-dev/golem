package controller

import (
	"errors"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/control/publish"
	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// TestFailingPoolBacksOff: while one pool keeps failing (its checkpoint
// tampered), the healthy pool publishes once a renewal interval, so the
// state document gains about one version a minute rather than one a
// second; the failing pool is retried with backoff, at least once a
// renewal interval, and at once when there is a change to publish.
func TestFailingPoolBacksOff(t *testing.T) {
	w := newWorld(t)
	r1 := w.replicas["r1"]
	edge := *r1.Publishers["internal"]
	edge.Pool = "edge"
	r1.Publishers["edge"] = &edge
	w.lead("r1")
	if p, err := r1.Step(t.Context()); !p || err != nil {
		t.Fatalf("first publication: %v %v", p, err)
	}
	if _, err := w.bucket.Put(t.Context(), snapshot.CheckpointKey("edge"), []byte("not a checkpoint"), ""); err != nil {
		t.Fatal(err)
	}
	r1.clock = r1.clock.Add(time.Minute) // the renewal: the edge pool's first failure
	w.lead("r1")
	if _, err := r1.Step(t.Context()); !errors.Is(err, publish.ErrTampered) || r1.Publications()["internal"].Seq != 2 {
		t.Fatalf("the renewal: %v, internal pool at %d; want it published, the edge pool tampered", err, r1.Publications()["internal"].Seq)
	}
	st := r1.Elector.State
	version := func() uint64 {
		if err := st.Refresh(t.Context()); err != nil {
			t.Fatal(err)
		}
		return st.Current().StateVersion
	}
	attempts := func() int64 { return r1.Failures()[Failure{"edge", ReasonTampered}] }
	v0, seq0 := version(), r1.Publications()["internal"].Seq

	const seconds = 300 // five renewal intervals, a step a second
	for i := range seconds {
		r1.clock = r1.clock.Add(time.Second)
		if i%5 == 0 {
			w.lead("r1")
		}
		if _, err := r1.Step(t.Context()); !errors.Is(err, publish.ErrTampered) {
			t.Fatalf("second %d: %v; want the edge pool's ErrTampered reported", i+1, err)
		}
	}
	renewals := r1.Publications()["internal"].Seq - seq0
	if renewals < 4 || renewals > 6 {
		t.Fatalf("the healthy pool published %d times in %d s; want about one a minute", renewals, seconds)
	}
	if grew := version() - v0; grew != renewals {
		t.Fatalf("the document gained %d versions for %d publications: want one each", grew, renewals)
	}
	// Two, four, eight, 16, 32 seconds, then a minute, each less up to a
	// quarter: from 7 to 12 attempts in five minutes.
	if n := attempts(); n < 5 || n > 12 {
		t.Fatalf("the failing pool was tried %d times in %d s; want it backed off, but tried at least once a minute", n, seconds)
	}
	if _, ok := r1.Publications()["edge"]; ok && r1.Publications()["edge"].Seq != 1 {
		t.Fatalf("the tampered pool published: %+v", r1.Publications()["edge"])
	}

	// A change to publish tries the failing pool at once, whatever its wait.
	before := attempts()
	if _, err := st.Update(t.Context(), func(d *state.Document) error {
		d.Scopes = map[string]state.Scope{"team-a": {Models: []string{"claude"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r1.clock = r1.clock.Add(time.Second)
	if p, err := r1.Step(t.Context()); !p || !errors.Is(err, publish.ErrTampered) {
		t.Fatalf("the change: published %v, %v; want the healthy pool published and the edge pool's failure", p, err)
	}
	if attempts() != before+1 {
		t.Fatalf("attempts %d after the change, were %d: want the failing pool tried at once", attempts(), before)
	}
}
