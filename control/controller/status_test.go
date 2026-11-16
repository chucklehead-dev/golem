package controller

import (
	"errors"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/control/audit"
	"github.com/chucklehead-dev/golem/control/entrasync"
	"github.com/chucklehead-dev/golem/control/publish"
	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/bucket"
)

// TestJobStatusForMetrics checks what the leader's jobs report for their
// metrics: the last audit step and group sync that succeeded, and the
// sync's removal guard tripping.
func TestJobStatusForMetrics(t *testing.T) {
	w := newWorld(t)
	r1 := w.replicas["r1"]
	w.lead("r1")
	archive, err := bucket.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { archive.Close() })
	st := r1.State
	r1.Audit = &audit.Shipper{State: st, Archive: archive}
	g := &groupDir{users: []entrasync.User{{ID: "oid-a", UPN: "a@example.com", Enabled: true}, {ID: "oid-b", UPN: "b@example.com", Enabled: true}}}
	r1.Sync = &entrasync.Sync{State: st, Directory: g, Now: r1.Now, MaxRemovals: 1}
	w.policy("team-a", "{models: [claude]}")
	if _, err := st.Update(t.Context(), func(d *state.Document) error {
		d.Scopes = map[string]state.Scope{"team-a": {Groups: []string{"g1"}}}
		d.Audit = append(d.Audit, state.AuditEvent{At: r1.clock, Actor: "root", Action: "scope.create", Target: "team-a"})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !r1.ShippedAt().IsZero() || !r1.SyncedAt().IsZero() {
		t.Fatal("a job reported success before it ran")
	}
	if _, err := r1.Step(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(r1.auditStep(t.Context()), r1.syncStep(t.Context())); err != nil {
		t.Fatal(err)
	}
	if !r1.ShippedAt().Equal(r1.clock) || !r1.SyncedAt().Equal(r1.clock) || r1.GuardTrips() != 0 {
		t.Fatalf("shipped %v, synced %v, guard %d; want both at %v and no trip", r1.ShippedAt(), r1.SyncedAt(), r1.GuardTrips(), r1.clock)
	}
	p, ok := r1.Publications()["internal"]
	if !ok || p.Seq != 1 || !p.At.Equal(r1.clock) || !p.ValidUntil.Equal(r1.clock.Add(4*time.Hour)) {
		t.Fatalf("publication %+v, %v", p, ok)
	}

	// Both members leave: two removals, over the guard's one.
	synced := r1.clock
	g.users = nil
	r1.clock = r1.clock.Add(5 * time.Minute)
	w.lead("r1")
	if err := r1.syncStep(t.Context()); !errors.Is(err, entrasync.ErrTooManyRemovals) {
		t.Fatalf("the sync: %v, want the removal guard", err)
	}
	if r1.GuardTrips() != 1 || !r1.SyncedAt().Equal(synced) {
		t.Fatalf("guard %d, synced %v; want one trip and the earlier sync", r1.GuardTrips(), r1.SyncedAt())
	}
}

// TestFailureReason: each sentinel is its reason, however wrapped.
func TestFailureReason(t *testing.T) {
	for err, want := range map[error]string{
		publish.ErrTampered: ReasonTampered,
		errors.Join(errors.New("x"), publish.ErrRegressed): ReasonRegressed,
		state.ErrRegressed:               ReasonRegressed,
		state.ErrMissing:                 ReasonRegressed,
		publish.ErrFenced:                ReasonFenced,
		state.ErrFenced:                  ReasonFenced,
		publish.ErrConflict:              ReasonConflict,
		state.ErrContended:               ReasonConflict,
		ErrUnmatched:                     ReasonUnmatched,
		errors.New("kms: access denied"): ReasonOther,
	} {
		if got := FailureReason(err); got != want {
			t.Errorf("FailureReason(%v) = %s, want %s", err, got, want)
		}
	}
}
