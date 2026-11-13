package limit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

func scopeCall(id, principal string) Call {
	return Call{ID: id, Principal: principal, Scope: "s", Budget: 10, PrincipalLimit: 1, ScopeLimit: 2}
}

func TestSoftAndStrict(t *testing.T) {
	ctx := t.Context()
	l := New(NewMem())
	tk, err := l.Admit(ctx, scopeCall("a", "p1"))
	if err != nil {
		t.Fatal(err)
	}
	tk.Complete(ctx, 8)
	// Soft: 8 < 10 admits, though the call may cost more (the accepted risk).
	soft := scopeCall("b", "p1")
	soft.Worst = 5
	if _, err := l.Admit(ctx, soft); err != nil {
		t.Fatalf("soft: %v", err)
	}
	// Strict: 8 + 5 > 10 refuses (ADR 0031).
	strict := scopeCall("c", "p2")
	strict.Strict, strict.Worst = true, 5
	if _, err := l.Admit(ctx, strict); !errors.Is(err, ErrBudget) {
		t.Fatalf("strict: %v, want ErrBudget", err)
	}
	strict.Worst = 2
	if _, err := l.Admit(ctx, strict); err != nil {
		t.Fatalf("strict within the budget: %v", err)
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

func TestOutage(t *testing.T) {
	ctx := t.Context()
	st := NewMem()
	g1, g2 := New(st), New(st)
	var numbered []int64
	g1.OnNumbered = func(_ string, _ Event, seq int64) { numbered = append(numbered, seq) }
	st.SetDown(true)
	// Each gateway admits against its own counts: p1 has two calls running.
	a, err1 := g1.Admit(ctx, scopeCall("a", "p1"))
	b, err2 := g2.Admit(ctx, scopeCall("b", "p1"))
	if err1 != nil || err2 != nil || !a.Waived() || !b.Waived() {
		t.Fatalf("%v %v: both admitted and both waivers", err1, err2)
	}
	a.Complete(ctx, 3)
	if p := g1.Pending("s"); len(p) != 1 || len(numbered) != 0 {
		t.Fatalf("pending %v numbered %v: the increment is held, and has no number yet", p, numbered)
	}
	st.SetDown(false)
	g1.Recover(ctx)
	g2.Recover(ctx)
	if g1.Known(ctx, "s") != 3 || len(st.Slots()) != 1 || !slices.Equal(numbered, []int64{1}) {
		t.Fatalf("known %d, slots %v, numbered %v: the increment replayed and numbered, b's slot re-acquired",
			g1.Known(ctx, "s"), st.Slots(), numbered)
	}
}

// TestWatermark: known spend is the snapshot's plus the counter after its
// watermark, on each gateway whichever snapshot it holds.
func TestWatermark(t *testing.T) {
	ctx := t.Context()
	st := NewMem()
	g1, g2 := New(st), New(st)
	for i, c := range []int64{3, 5, 2} {
		tk, _ := g1.Admit(ctx, scopeCall(fmt.Sprint(i), "p1"))
		tk.Complete(ctx, c)
	}
	// The ledger has events 1 and 2 (8); g1 applies that snapshot, g2 not.
	g1.ApplySnapshot(ctx, "s", Known{Spend: 8, Watermark: 2})
	if a, b := g1.Known(ctx, "s"), g2.Known(ctx, "s"); a != 10 || b != 10 {
		t.Fatalf("known %d on the new snapshot, %d on the old: both must be 10", a, b)
	}
}

// blocking is a store whose calls wait for their context.
type blocking struct{ Store }

func (blocking) Admit(ctx context.Context, _ Admission) (Outcome, int64, bool, error) {
	<-ctx.Done()
	return 0, 0, false, ctx.Err()
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
// old month is not counted against the new one, and the counter is the
// new month's (numbered from 1), so last month's watermark cannot make the
// new month's events disappear or the old month's spend refuse every call.
func TestPeriodRollover(t *testing.T) {
	ctx := t.Context()
	st := NewMem()
	l := New(st)
	now := time.Date(2026, 11, 30, 23, 59, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }
	var seqs []int64
	l.OnNumbered = func(_ string, _ Event, seq int64) { seqs = append(seqs, seq) }
	for i := range 3 {
		tk, err := l.Admit(ctx, scopeCall(fmt.Sprint("nov", i), "p1"))
		if err != nil {
			t.Fatal(err)
		}
		tk.Complete(ctx, 4)
	}
	l.ApplySnapshot(ctx, "s", Known{Period: "2026-11", Spend: 12, Watermark: 3})
	if _, err := l.Admit(ctx, scopeCall("late", "p1")); !errors.Is(err, ErrBudget) {
		t.Fatalf("November is spent (12 of 10): %v", err)
	}
	now = now.Add(2 * time.Minute) // December
	tk, err := l.Admit(ctx, scopeCall("dec0", "p1"))
	if err != nil {
		t.Fatalf("a new period: %v", err)
	}
	tk.Complete(ctx, 2)
	if got := l.Known(ctx, "s"); got != 2 {
		t.Fatalf("December's known spend %d, want 2", got)
	}
	if !slices.Equal(seqs, []int64{1, 2, 3, 1}) || len(st.Counter("s", "2026-12")) != 1 {
		t.Fatalf("numbers %v: each period's counter numbers from 1", seqs)
	}
	// A call admitted in November and completed in December is November's.
	now = time.Date(2027, 1, 31, 23, 59, 0, 0, time.UTC)
	jan, _ := l.Admit(ctx, scopeCall("jan", "p1"))
	now = now.Add(time.Hour)
	jan.Complete(ctx, 1)
	if len(st.Counter("s", "2027-01")) != 1 || len(st.Counter("s", "2027-02")) != 0 {
		t.Fatal("a call's cost belongs to the period it started in")
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

// counting counts AddAll and Add calls.
type counting struct {
	*Mem
	adds, batches int
}

func (c *counting) Add(ctx context.Context, scope, period string, e Event) (int64, error) {
	c.adds++
	return c.Mem.Add(ctx, scope, period, e)
}

func (c *counting) AddAll(ctx context.Context, incs []Increment) ([]int64, error) {
	c.batches++
	return c.Mem.AddAll(ctx, incs)
}

// TestRecoverBatches: an outage's held increments are replayed in batches,
// each numbered in order, and the held ones are bounded.
func TestRecoverBatches(t *testing.T) {
	ctx := t.Context()
	st := &counting{Mem: NewMem()}
	l := New(st)
	l.MaxPending = 600
	var numbered []int64
	l.OnNumbered = func(_ string, _ Event, seq int64) { numbered = append(numbered, seq) }
	st.SetDown(true)
	for i := range 700 {
		c := scopeCall(fmt.Sprint(i), fmt.Sprint("p", i))
		c.ScopeLimit = 0
		tk, err := l.Admit(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		tk.Complete(ctx, 1)
	}
	if len(l.Pending("s")) != 600 || len(numbered) != 100 || slices.ContainsFunc(numbered, func(n int64) bool { return n != 0 }) {
		t.Fatalf("pending %d, numbered %d: past MaxPending the oldest go to the ledger unnumbered", len(l.Pending("s")), len(numbered))
	}
	st.SetDown(false)
	if err := l.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if st.batches != 3 || st.adds != 0 || len(st.Counter("s", l.period(l.now()))) != 600 {
		t.Fatalf("%d batches, %d single adds, counter %d: 600 replayed in three round trips",
			st.batches, st.adds, len(st.Counter("s", l.period(l.now()))))
	}
	if got := numbered[100:]; got[0] != 1 || got[599] != 600 {
		t.Fatalf("numbers %d..%d, want 1..600 in order", got[0], got[599])
	}
}

// TestPeriodOfStart: a call belongs to the period it started in, though
// admitted after midnight (its body still arriving): the counter it is
// numbered in is the one its ledger row names.
func TestPeriodOfStart(t *testing.T) {
	ctx := t.Context()
	st := NewMem()
	l := New(st)
	l.Now = func() time.Time { return time.Date(2026, 12, 1, 0, 0, 0, 2e8, time.UTC) }
	c := scopeCall("late", "p1")
	c.Started = time.Date(2026, 11, 30, 23, 59, 59, 9e8, time.UTC)
	tk, err := l.Admit(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	tk.Complete(ctx, 1)
	if tk.Period() != "2026-11" || len(st.Counter("s", "2026-11")) != 1 || len(st.Counter("s", "2026-12")) != 0 {
		t.Fatalf("period %s: the call started in November is counted there", tk.Period())
	}
}
