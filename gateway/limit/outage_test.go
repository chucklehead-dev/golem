package limit

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// flaky is a store that can fail its calls for a while without losing its
// slots: an outage shorter than their TTL, which a reachable store keeps.
type flaky struct {
	*Mem
	fail atomic.Bool
	// duringReacquire, if set, runs inside each Reacquire.
	duringReacquire func()
}

func (f *flaky) Admit(ctx context.Context, a Admission) (Outcome, error) {
	if f.fail.Load() {
		return 0, ErrUnavailable
	}
	return f.Mem.Admit(ctx, a)
}

func (f *flaky) Release(ctx context.Context, s Slot) error {
	if f.fail.Load() {
		return ErrUnavailable
	}
	return f.Mem.Release(ctx, s)
}

func (f *flaky) Reacquire(ctx context.Context, slots []Slot) error {
	if f.duringReacquire != nil {
		f.duringReacquire()
	}
	if f.fail.Load() {
		return ErrUnavailable
	}
	return f.Mem.Reacquire(ctx, slots)
}

func (f *flaky) Ping(context.Context) error {
	if f.fail.Load() {
		return ErrUnavailable
	}
	return nil
}

// TestEndedDuringOutage: calls that end while the store is unreachable
// cannot release their slots, which a short outage leaves at the store; on
// recovery they are released, or the scope would be refused
// limit.concurrency site-wide until their TTLs lapsed.
func TestEndedDuringOutage(t *testing.T) {
	ctx := t.Context()
	st := &flaky{Mem: NewMem()}
	l := New(st)
	a, _ := l.Admit(ctx, scopeCall("a", "p1"))
	b, _ := l.Admit(ctx, scopeCall("b", "p2"))
	st.fail.Store(true)
	a.Complete(ctx, 0) // its release fails: the outage begins
	if !l.Down() {
		t.Fatal("a failed release: the limiter is not down")
	}
	b.Complete(ctx, 0) // ended while down
	st.fail.Store(false)
	if err := l.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if got := st.Slots(); len(got) != 0 {
		t.Fatalf("slots %v after recovery: the calls that ended in the outage still hold theirs", got)
	}
	l.endGrace()
	for _, id := range []string{"c", "d"} { // the scope's limit of two, whole again
		if _, err := l.Admit(ctx, scopeCall(id, "p"+id)); err != nil {
			t.Fatalf("admitting %s after recovery: %v", id, err)
		}
	}
}

// TestMarkDownDuringRecover: a store call that fails while Recover is
// re-registering the running calls is an outage Recover must not end.
func TestMarkDownDuringRecover(t *testing.T) {
	ctx := t.Context()
	st := &flaky{Mem: NewMem()}
	l := New(st)
	l.MarkDown(ErrUnavailable)
	st.duringReacquire = func() { l.MarkDown(ErrUnavailable) } // another call's failure, meanwhile
	if err := l.Recover(ctx); err == nil || !l.Down() {
		t.Fatalf("Recover = %v, down %v: a failure during it must keep the limiter down", err, l.Down())
	}
	st.duringReacquire = nil
	if err := l.Recover(ctx); err != nil || l.Down() {
		t.Fatalf("Recover = %v, down %v: want it up", err, l.Down())
	}
}

// TestRecoveryProbed: an outage is probed soon after it begins, not at the
// next renewal, and again with backoff while the store stays down.
func TestRecoveryProbed(t *testing.T) {
	st := &flaky{Mem: NewMem()}
	l := New(st)
	l.probeFirst = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	st.fail.Store(true)
	l.MarkDown(ErrUnavailable)
	time.Sleep(50 * time.Millisecond) // a few failed probes
	st.fail.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	for l.Down() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if l.Down() {
		t.Fatal("still down 5 s after the store came back: recovery waits for the 30 s renewal")
	}
}
