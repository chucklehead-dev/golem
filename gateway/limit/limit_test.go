package limit

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/gateway/admission"
)

func scopeCall(id, principal string) Call {
	return Call{ID: id, Principal: principal, Scope: "s", Budget: 10, PrincipalLimit: 1, ScopeLimit: 2}
}

// TestSoft: known spend is the snapshot's (ADR 0052), checked below the
// budget whatever the call then costs (the accepted risk); a call's cost
// is its ledger row's, not counted here.
func TestSoft(t *testing.T) {
	ctx := t.Context()
	l := New(NewMem())
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }
	tk, err := l.Admit(ctx, scopeCall("a", "p1"))
	if err != nil {
		t.Fatal(err)
	}
	tk.Complete(ctx, 80)
	if got := l.Known("s"); got != 0 {
		t.Fatalf("known %d before a snapshot counts the call: the gateway counts no spend", got)
	}
	l.ApplySnapshot(ctx, "s", Known{Period: "2026-11", Spend: 9})
	if _, err := l.Admit(ctx, scopeCall("b", "p1")); err != nil {
		t.Fatalf("9 < 10 admits: %v", err)
	}
	l.ApplySnapshot(ctx, "s", Known{Period: "2026-11", Spend: 10})
	if _, err := l.Admit(ctx, scopeCall("c", "p2")); !errors.Is(err, ErrBudget) {
		t.Fatalf("at the budget: %v, want ErrBudget", err)
	}
	if got := l.Known("s"); got != 10 {
		t.Fatalf("known %d, want the snapshot's 10", got)
	}
}

func TestLimits(t *testing.T) {
	ctx := t.Context()
	l := New(NewMem())
	a, _ := l.Admit(ctx, scopeCall("a", "p1"))
	if _, err := l.Admit(ctx, scopeCall("b", "p1")); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("a second call for p1: %v", err)
	}
	if _, err := l.Admit(ctx, scopeCall("c", "p2")); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Admit(ctx, scopeCall("d", "p3")); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("a third call in the scope: %v (the scope limit is 2)", err)
	}
	// A cancelled call keeps its slot until its upstream ends.
	a.Cancel(ctx, 1)
	if _, err := l.Admit(ctx, scopeCall("e", "p1")); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("after cancel: %v; the provider may still be working", err)
	}
	a.Ended(ctx)
	if _, err := l.Admit(ctx, scopeCall("e", "p1")); err != nil {
		t.Fatalf("after the upstream ended: %v", err)
	}
}

// TestOutage: during an outage each gateway admits against its own counts
// and the snapshot's known spend, as waivers; on recovery each re-acquires
// its running calls' slots.
func TestOutage(t *testing.T) {
	ctx := t.Context()
	st := NewMem()
	g1, g2 := New(st), New(st)
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	g1.Now, g2.Now = func() time.Time { return now }, func() time.Time { return now }
	st.SetDown(true)
	// Each gateway admits against its own counts: p1 has two calls running.
	a, err1 := g1.Admit(ctx, scopeCall("a", "p1"))
	b, err2 := g2.Admit(ctx, scopeCall("b", "p1"))
	if err1 != nil || err2 != nil || !a.Waived() || !b.Waived() {
		t.Fatalf("%v %v: both admitted and both waivers", err1, err2)
	}
	// The snapshot still refuses: it needs no store.
	g1.ApplySnapshot(ctx, "s", Known{Period: "2026-11", Spend: 10})
	if _, err := g1.Admit(ctx, scopeCall("c", "p2")); !errors.Is(err, ErrBudget) {
		t.Fatalf("over the budget during an outage: %v, want ErrBudget", err)
	}
	a.Complete(ctx, 3)
	st.SetDown(false)
	if err := g1.Recover(ctx); err != nil || g1.Down() {
		t.Fatalf("Recover = %v, down %v", err, g1.Down())
	}
	g2.Recover(ctx)
	if got := st.Slots(); len(got) != 1 || got[0].Call != "b" {
		t.Fatalf("slots %v: b's re-acquired, a's not (it ended)", got)
	}
	// In its grace a recovered gateway's admissions are waivers: a peer may
	// not have re-registered its slots yet.
	d, err := g2.Admit(ctx, scopeCall("d", "p2"))
	if err != nil || !d.Waived() {
		t.Fatalf("in grace: %v, want a waiver", err)
	}
	d.Complete(ctx, 1)
	g2.endGrace()
	if tk, err := g2.Admit(ctx, scopeCall("e", "p3")); err != nil || tk.Waived() {
		t.Fatalf("after the grace: %v, want admitted, no waiver", err)
	}
}

// blocking is a store whose calls wait for their context.
type blocking struct{ Store }

func (blocking) Admit(ctx context.Context, _ Admission) (Outcome, error) {
	<-ctx.Done()
	return 0, ctx.Err()
}

// TestCallerCancelIsNoOutage: a caller going away mid-admission returns
// its own error; the gateway does not switch to outage mode.
func TestCallerCancelIsNoOutage(t *testing.T) {
	l := New(blocking{NewMem()})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := l.Admit(ctx, scopeCall("a", "p1")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Admit = %v, want the caller's context.Canceled", err)
	}
	if l.Down() {
		t.Fatal("a caller's cancellation was taken for an outage")
	}
	// A store that does not answer within StoreTimeout is one.
	l.StoreTimeout = 10 * time.Millisecond
	if tk, err := l.Admit(t.Context(), scopeCall("b", "p1")); err != nil || !l.Down() || !tk.Waived() {
		t.Fatalf("Admit = %v, down %v: a hung store is an outage, admitted on local counts as a waiver", err, l.Down())
	}
}

// TestPeriodRollover: a new month starts from nothing. The snapshot of the
// old month is not counted against the new one, so the old month's spend
// cannot refuse every call.
func TestPeriodRollover(t *testing.T) {
	ctx := t.Context()
	l := New(NewMem())
	now := time.Date(2026, 11, 30, 23, 59, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }
	l.ApplySnapshot(ctx, "s", Known{Period: "2026-11", Spend: 12})
	if _, err := l.Admit(ctx, scopeCall("late", "p1")); !errors.Is(err, ErrBudget) {
		t.Fatalf("November is spent (12 of 10): %v", err)
	}
	now = now.Add(2 * time.Minute) // December
	tk, err := l.Admit(ctx, scopeCall(fmt.Sprint("dec", 0), "p1"))
	if err != nil {
		t.Fatalf("a new period: %v", err)
	}
	if got := l.Known("s"); got != 0 || tk.Period() != "2026-12" {
		t.Fatalf("December's known spend %d, period %s: want 0, 2026-12", got, tk.Period())
	}
}

// TestPeriodOfStart: a call belongs to the period it started in, though
// admitted after midnight (its body still arriving): the period its ledger
// row names, and the snapshot's spend it is checked against.
func TestPeriodOfStart(t *testing.T) {
	ctx := t.Context()
	l := New(NewMem())
	l.Now = func() time.Time { return time.Date(2026, 12, 1, 0, 0, 0, 2e8, time.UTC) }
	l.ApplySnapshot(ctx, "s", Known{Period: "2026-11", Spend: 10})
	c := scopeCall("late", "p1")
	c.Started = time.Date(2026, 11, 30, 23, 59, 59, 9e8, time.UTC)
	if _, err := l.Admit(ctx, c); !errors.Is(err, ErrBudget) {
		t.Fatalf("a call started in November is checked against November: %v", err)
	}
	l.ApplySnapshot(ctx, "s", Known{Period: "2026-11", Spend: 0})
	tk, err := l.Admit(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	if tk.Period() != "2026-11" {
		t.Fatalf("period %s: the call started in November is counted there", tk.Period())
	}
}

// TestOutageCounts: during an outage the local check counts this gateway's
// running calls, and the counts follow admissions and completions exactly.
func TestOutageCounts(t *testing.T) {
	ctx := t.Context()
	st := NewMem()
	l := New(st)
	st.SetDown(true)
	a, err := l.Admit(ctx, scopeCall("a", "p1"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Admit(ctx, scopeCall("b", "p1")); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("a second call for p1: %v", err)
	}
	c, _ := l.Admit(ctx, scopeCall("c", "p2"))
	if _, err := l.Admit(ctx, scopeCall("d", "p3")); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("a third in the scope: %v", err)
	}
	a.Complete(ctx, 1)
	a.Complete(ctx, 1) // twice is once
	if _, err := l.Admit(ctx, scopeCall("b", "p1")); err != nil {
		t.Fatalf("after a completed: %v", err)
	}
	c.Cancel(ctx, 0)
	c.Ended(ctx)
	if len(l.byPrincipal) != 1 || l.byScope["s"] != 1 {
		t.Fatalf("counts %v %v: one call (b) running", l.byPrincipal, l.byScope)
	}
}

// gated is a store whose Reacquire waits until released, so a test can
// end a call while a renewal is in flight.
type gated struct {
	*Mem
	in, release chan struct{}
}

func (g gated) Reacquire(ctx context.Context, slots []Slot) error {
	g.in <- struct{}{}
	<-g.release
	return g.Mem.Reacquire(ctx, slots)
}

// TestRenewalDoesNotRevive: a call that ends while a renewal is in flight
// must not be registered again by it (a phantom slot for the TTL, refusing
// its principal's next call).
func TestRenewalDoesNotRevive(t *testing.T) {
	ctx := t.Context()
	st := gated{NewMem(), make(chan struct{}), make(chan struct{})}
	l := New(st)
	a, err := l.Admit(ctx, scopeCall("a", "p1"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error)
	go func() { done <- l.Renew(ctx) }()
	<-st.in            // the renewal has copied a's slot
	a.Complete(ctx, 1) // a ends and frees its slot
	close(st.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := st.Slots(); len(got) != 0 {
		t.Fatalf("slots %v after a completed during the renewal: none may be held", got)
	}
	if _, err := l.Admit(ctx, scopeCall("b", "p1")); err != nil {
		t.Fatalf("p1's next call: %v", err)
	}
}

// recording records the slots each Reacquire is given.
type recording struct {
	*Mem
	got [][]Slot
}

func (r *recording) Reacquire(ctx context.Context, slots []Slot) error {
	r.got = append(r.got, slots)
	return r.Mem.Reacquire(ctx, slots)
}

// TestRenewalStopsAtDeadline: a call past its route's maximum duration (and
// the grace for its cut) is renewed no more: its slot ends with its TTL,
// whatever became of the call.
func TestRenewalStopsAtDeadline(t *testing.T) {
	ctx := t.Context()
	st := &recording{Mem: NewMem()}
	l := New(st)
	now := time.Now()
	l.Now = func() time.Time { return now }
	c := scopeCall("long", "p1")
	c.Deadline = now.Add(time.Minute)
	if _, err := l.Admit(ctx, c); err != nil {
		t.Fatal(err)
	}
	l.Renew(ctx)
	now = now.Add(time.Minute + renewalGrace + time.Second)
	l.Renew(ctx)
	if len(st.got) != 2 || len(st.got[0]) != 1 || len(st.got[1]) != 0 {
		t.Fatalf("renewed %v: before its deadline, then not", st.got)
	}
}

// TestTicketEndsOnce: a ticket's second end, in either order, frees
// nothing: a cancelled call's slot stays held through a later Complete.
func TestTicketEndsOnce(t *testing.T) {
	ctx := t.Context()
	st := NewMem()
	l := New(st)
	a, _ := l.Admit(ctx, scopeCall("a", "p1"))
	a.Cancel(ctx, 3)
	a.Complete(ctx, 5) // after Cancel: ignored
	a.Cancel(ctx, 7)   // twice: ignored
	b, _ := l.Admit(ctx, scopeCall("b", "p2"))
	b.Complete(ctx, 2)
	b.Complete(ctx, 2)
	b.Cancel(ctx, 4)
	if got := st.Slots(); len(got) != 1 || got[0].Call != "a" || l.byScope["s"] != 1 {
		t.Fatalf("slots %v, counts %v: a's held (cancelled), b's freed once", got, l.byScope)
	}
}

// TestAdmitter: the request path's view. A nil limiter is no admitter at
// all; Cancel holds the slot until the time given, then frees it, and
// Finish stops the tails still held (their slots expire by TTL).
func TestAdmitter(t *testing.T) {
	var none *Limiter
	if none.Admitter() != nil {
		t.Fatal("a nil limiter's Admitter must be a nil interface (admit everything uncounted)")
	}
	ctx := t.Context()
	l := New(NewMem())
	a := l.Admitter()
	tk, err := a.Admit(ctx, scopeCall("a", "p1"))
	if err != nil {
		t.Fatal(err)
	}
	tk.Cancel(ctx, 1, time.Now().Add(50*time.Millisecond))
	if _, err := a.Admit(ctx, scopeCall("b", "p1")); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("during the tail: %v, want the slot still held", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := a.Admit(ctx, scopeCall("b", "p1")); err == nil {
			break
		} else if time.Now().After(deadline) {
			t.Fatalf("after the tail: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	held, _ := a.Admit(ctx, scopeCall("c", "p2"))
	held.Cancel(ctx, 1, time.Now().Add(time.Hour))
	l.Finish(ctx)
	l.mu.Lock()
	n := len(l.tails)
	l.mu.Unlock()
	if n != 0 {
		t.Fatalf("%d tails still timed after Finish", n)
	}
}

// TestMetersFailOpen: meters are taken and refused through the store; with
// the store unreachable a request is admitted (rate limits fail open) and
// the limiter is down.
func TestMetersFailOpen(t *testing.T) {
	ctx := t.Context()
	st := NewMem()
	l := New(st)
	q := []Meter{{Key: admission.ScopeRateMeter("s"), PerMinute: 1}}
	if ok, _, _ := l.Take(ctx, q); !ok {
		t.Fatal("the first request of the minute")
	}
	if ok, _, wait := l.Take(ctx, q); ok || wait <= 0 {
		t.Fatalf("the second: %v %v, want refused with a wait", ok, wait)
	}
	st.SetDown(true)
	if ok, _, _ := l.Take(ctx, q); !ok || !l.Down() {
		t.Fatalf("the store unreachable: %v, down %v; want admitted, and the limiter down", ok, l.Down())
	}
}
