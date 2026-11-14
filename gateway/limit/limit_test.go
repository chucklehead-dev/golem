package limit

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

func scopeCall(id, principal string) Call {
	return Call{ID: id, Principal: principal, Scope: "s", Budget: 10, PrincipalLimit: 1, ScopeLimit: 2}
}

func TestSoftAndWorstCase(t *testing.T) {
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
	// The declared-worst-case check: 8 + 5 > 10 refuses (ADR 0031).
	checked := scopeCall("c", "p2")
	checked.DeclaredWorstCaseCheck, checked.Worst = true, 5
	if _, err := l.Admit(ctx, checked); !errors.Is(err, ErrBudget) {
		t.Fatalf("checked: %v, want ErrBudget", err)
	}
	checked.Worst = 2
	if _, err := l.Admit(ctx, checked); err != nil {
		t.Fatalf("checked within the budget: %v", err)
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
	st.SetDown(true)
	// Each gateway admits against its own counts: p1 has two calls running.
	a, err1 := g1.Admit(ctx, scopeCall("a", "p1"))
	b, err2 := g2.Admit(ctx, scopeCall("b", "p1"))
	if err1 != nil || err2 != nil || !a.Waived() || !b.Waived() {
		t.Fatalf("%v %v: both admitted and both waivers", err1, err2)
	}
	a.Complete(ctx, 3)
	if p := g1.Pending("s"); len(p) != 1 || len(st.Buckets("s", g1.period(g1.now()))) != 0 {
		t.Fatalf("pending %v: the increment is held, the counter without it", p)
	}
	st.SetDown(false)
	g1.Recover(ctx)
	g2.Recover(ctx)
	if g1.Known(ctx, "s") != 3 || len(st.Slots()) != 1 || len(g1.Pending("s")) != 0 {
		t.Fatalf("known %d, slots %v: the increment replayed, b's slot re-acquired", g1.Known(ctx, "s"), st.Slots())
	}
}

// TestBoundary: known spend is the snapshot's (the ledger's, for calls
// that ended before its boundary) plus the counter's minutes from the
// boundary on, on each gateway whichever snapshot it holds.
func TestBoundary(t *testing.T) {
	ctx := t.Context()
	st := NewMem()
	g1, g2 := New(st), New(st)
	now := time.Date(2026, 11, 13, 12, 0, 30, 0, time.UTC)
	g1.Now, g2.Now, st.Now = func() time.Time { return now }, func() time.Time { return now }, func() time.Time { return now }
	for i, c := range []int64{3, 5, 2} { // in 12:00, 12:01 and 12:02
		tk, _ := g1.Admit(ctx, scopeCall(fmt.Sprint(i), "p1"))
		tk.Complete(ctx, c)
		now = now.Add(time.Minute)
	}
	// The ledger has the calls that ended before 12:02 (8); g1 applies that
	// snapshot, g2 has none.
	g1.ApplySnapshot(ctx, "s", Known{Period: "2026-11", Spend: 8, Since: time.Date(2026, 11, 13, 12, 2, 0, 0, time.UTC)})
	if a, b := g1.Known(ctx, "s"), g2.Known(ctx, "s"); a != 10 || b != 10 {
		t.Fatalf("known %d on the new snapshot, %d on none: both must be 10", a, b)
	}
	// A minute past retention is gone, and the counter stays bounded: a
	// snapshot that old no longer reads it, but known spend is never below
	// the period's whole total, which keeps it (as a gateway with no
	// boundary reads it).
	now = now.Add(st.Retain)
	late := scopeCall("late", "p1")
	late.Budget = 0
	tk, err := g2.Admit(ctx, late)
	if err != nil {
		t.Fatal(err)
	}
	tk.Complete(ctx, 1)
	if a, b := g1.Known(ctx, "s"), g2.Known(ctx, "s"); a != 11 || b != 11 || len(st.Buckets("s", "2026-11")) != 1 {
		t.Fatalf("known %d (want 11: 12:02's minute gone, the total kept), %d (want 11, the total), minutes %v", a, b, st.Buckets("s", "2026-11"))
	}
}

// blocking is a store whose calls wait for their context.
type blocking struct{ Store }

func (blocking) Admit(ctx context.Context, _ Admission) (Outcome, int64, error) {
	<-ctx.Done()
	return 0, 0, ctx.Err()
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
// new month's, so the old month's spend cannot refuse every call.
func TestPeriodRollover(t *testing.T) {
	ctx := t.Context()
	st := NewMem()
	l := New(st)
	now := time.Date(2026, 11, 30, 23, 59, 0, 0, time.UTC)
	l.Now = func() time.Time { return now }
	for i := range 3 {
		tk, err := l.Admit(ctx, scopeCall(fmt.Sprint("nov", i), "p1"))
		if err != nil {
			t.Fatal(err)
		}
		tk.Complete(ctx, 4)
	}
	l.ApplySnapshot(ctx, "s", Known{Period: "2026-11", Spend: 12, Since: now.Add(time.Minute)})
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
	if len(st.Buckets("s", "2026-12")) != 1 {
		t.Fatalf("December's counter %v", st.Buckets("s", "2026-12"))
	}
	// A call admitted in November and completed in December is November's.
	now = time.Date(2027, 1, 31, 23, 59, 0, 0, time.UTC)
	jan, _ := l.Admit(ctx, scopeCall("jan", "p1"))
	now = now.Add(time.Hour)
	jan.Complete(ctx, 1)
	if len(st.Buckets("s", "2027-01")) != 1 || len(st.Buckets("s", "2027-02")) != 0 {
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

func (c *counting) Add(ctx context.Context, in Increment) error {
	c.adds++
	return c.Mem.Add(ctx, in)
}

func (c *counting) AddAll(ctx context.Context, incs []Increment) error {
	c.batches++
	return c.Mem.AddAll(ctx, incs)
}

// TestRecoverBatches: an outage's held increments are replayed in batches,
// and the held ones are bounded.
func TestRecoverBatches(t *testing.T) {
	ctx := t.Context()
	st := &counting{Mem: NewMem()}
	l := New(st)
	l.MaxPending = 600
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
	if len(l.Pending("s")) != 600 || l.Dropped.Load() != 100 {
		t.Fatalf("pending %d, dropped %d: past MaxPending the oldest are dropped, and counted", len(l.Pending("s")), l.Dropped.Load())
	}
	st.SetDown(false)
	if err := l.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if st.batches != 3 || st.adds != 0 || l.Known(ctx, "s") != 600 {
		t.Fatalf("%d batches, %d single adds, known %d: 600 replayed in three round trips", st.batches, st.adds, l.Known(ctx, "s"))
	}
}

// busy is a store at which, at every add and every renewal, another of
// the limiter's calls completes first: a gateway whose calls keep ending
// while it recovers.
type busy struct {
	*Mem
	l *Limiter
	n int
}

func (b *busy) complete(ctx context.Context) {
	b.n++
	c := scopeCall(fmt.Sprint("busy", b.n), fmt.Sprint("busy", b.n))
	c.ScopeLimit, c.Budget = 0, 0
	if tk, err := b.l.Admit(ctx, c); err == nil {
		tk.Complete(ctx, 1)
	}
}

func (b *busy) AddAll(ctx context.Context, incs []Increment) error {
	b.complete(ctx)
	return b.Mem.AddAll(ctx, incs)
}

func (b *busy) Reacquire(ctx context.Context, slots []Slot) error {
	b.complete(ctx)
	return b.Mem.Reacquire(ctx, slots)
}

// TestRecoverUnderLoad: a gateway whose calls keep completing while it
// replays and re-registers its slots (each completion held, since it is
// still down) recovers in one Recover, and loses none of them. A recovery
// that waited for the held increments to run out before marking itself up
// would never finish: one is always held by then.
func TestRecoverUnderLoad(t *testing.T) {
	ctx := t.Context()
	st := &busy{Mem: NewMem()}
	l := New(st)
	st.l = l
	st.SetDown(true)
	tk, _ := l.Admit(ctx, scopeCall("a", "p1"))
	tk.Complete(ctx, 1) // held
	st.SetDown(false)
	if err := l.Recover(ctx); err != nil || l.Down() {
		t.Fatalf("Recover = %v, down %v: it must converge under load", err, l.Down())
	}
	if held, known := len(l.Pending("s")), l.Known(ctx, "s"); held != 0 || known != int64(1+st.n) {
		t.Fatalf("held %d, known %d: want all %d completions counted", held, known, 1+st.n)
	}
}

// TestCompletionRacingRecovery: a completion that found the limiter down,
// and holds its increment only after a recovery has marked it up and
// drained what was held, must not be held where no replay will find it:
// hold refuses it, and it goes to the store itself.
func TestCompletionRacingRecovery(t *testing.T) {
	ctx := t.Context()
	st := NewMem()
	l := New(st)
	st.SetDown(true)
	tk, _ := l.Admit(ctx, scopeCall("a", "p1")) // the limiter is down now
	ran := false
	l.beforeHold = func() {
		if !ran { // the recovery runs between the completion's look and its hold
			ran = true
			st.SetDown(false)
			if err := l.Recover(ctx); err != nil {
				t.Fatal(err)
			}
		}
	}
	tk.Complete(ctx, 4)
	if held, known := len(l.Pending("s")), l.Known(ctx, "s"); !ran || l.Down() || held != 0 || known != 4 {
		t.Fatalf("ran %v, down %v, held %d, known %d: the increment must reach the counter", ran, l.Down(), held, known)
	}
}

// TestStaleBoundary: a snapshot whose boundary is older than the counter's
// retention (golem-control's known-spend job unable to read the ledger for
// hours, its last result stamped into each renewed snapshot) reads only
// the minutes retained; the spend between its boundary and the retention's
// start would vanish, and the budget stop refusing. Known spend is never
// below this site's counter total.
func TestStaleBoundary(t *testing.T) {
	ctx := t.Context()
	st := NewMem()
	l := New(st)
	// One fixed clock for the limiter and the store, mid-month, so the
	// call seven hours back and the stale boundary are of this period.
	now := time.Date(2026, 11, 14, 12, 0, 30, 0, time.UTC)
	clock := now.Add(-7 * time.Hour)
	l.Now, st.Now = func() time.Time { return clock }, func() time.Time { return clock }
	l.ApplySnapshot(ctx, "s", Known{Period: "2026-11", Since: now.Add(-8 * time.Hour).Truncate(time.Minute)})
	c := scopeCall("a", "p1")
	c.Started = clock
	tk, err := l.Admit(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	tk.Complete(ctx, 10) // seven hours ago: the budget spent
	clock = now
	if k := l.snapshotFor("s", l.period(now)); k.Since.IsZero() {
		t.Fatal("the stale snapshot is not this period's: the test would not read from its boundary")
	}
	c = scopeCall("b", "p2")
	c.Started = now
	if _, err := l.Admit(ctx, c); !errors.Is(err, ErrBudget) || l.Known(ctx, "s") != 10 {
		t.Fatalf("admit %v, known %d: a boundary older than the retention must not hide this site's 10", err, l.Known(ctx, "s"))
	}
}

// TestPeriodOfStart: a call belongs to the period it started in, though
// admitted after midnight (its body still arriving): the counter it is
// added to is the one its ledger row names.
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
	if tk.Period() != "2026-11" || len(st.Buckets("s", "2026-11")) != 1 || len(st.Buckets("s", "2026-12")) != 0 {
		t.Fatalf("period %s: the call started in November is counted there", tk.Period())
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

// TestTicketEndsOnce: a ticket's second end, in either order, charges
// nothing more and frees nothing twice.
func TestTicketEndsOnce(t *testing.T) {
	ctx := t.Context()
	l := New(NewMem())
	a, _ := l.Admit(ctx, scopeCall("a", "p1"))
	a.Cancel(ctx, 3)
	a.Complete(ctx, 5) // after Cancel: ignored
	a.Cancel(ctx, 7)   // twice: ignored
	b, _ := l.Admit(ctx, scopeCall("b", "p2"))
	b.Complete(ctx, 2)
	b.Complete(ctx, 2)
	b.Cancel(ctx, 4)
	if l.Known(ctx, "s") != 5 {
		t.Fatalf("known %d: each ticket charged once (3 and 2)", l.Known(ctx, "s"))
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

// overlapping runs another replay inside its first Add, as a replay running
// at the same time would finish first.
type overlapping struct {
	*Mem
	l    *Limiter
	once bool
}

func (o *overlapping) Add(ctx context.Context, in Increment) error {
	if !o.once {
		o.once = true
		if _, _, err := o.l.ReplayOne(ctx); err != nil {
			return err
		}
	}
	return o.Mem.Add(ctx, in)
}

// TestOverlappingReplays: two replays of the same held increment (a
// ReplayOne racing another) take it off once: the later finds it gone and
// leaves the next one held, rather than dropping an increment no replay
// has sent.
func TestOverlappingReplays(t *testing.T) {
	ctx := t.Context()
	st := &overlapping{Mem: NewMem()}
	l := New(st)
	st.l = l
	st.SetDown(true)
	for _, id := range []string{"a", "b"} {
		c := scopeCall(id, "p-"+id)
		c.ScopeLimit = 0
		tk, err := l.Admit(ctx, c)
		if err != nil {
			t.Fatal(err)
		}
		tk.Complete(ctx, 1)
	}
	st.SetDown(false)
	if _, ok, err := l.ReplayOne(ctx); !ok || err != nil {
		t.Fatalf("replay: %v, %v", ok, err)
	}
	if p := l.Pending("s"); len(p) != 1 || p[0].Call != "b" {
		t.Fatalf("held %+v after two replays of a: want b still held", p)
	}
}

// atCap is a store at which, at every replayed batch while the limiter is
// still down, more of its calls complete first (each held, the oldest
// dropped at MaxPending), and which counts each call's increments.
type atCap struct {
	*Mem
	l       *Limiter
	perAdd  int // completions arriving during each replayed batch
	left    int // completions still to arrive (negative: no end)
	n       int
	batches int
	counted map[string]int
}

func (s *atCap) arrive(ctx context.Context) {
	for range s.perAdd {
		if s.left == 0 || !s.l.Down() {
			return
		}
		s.left--
		s.n++
		c := scopeCall(fmt.Sprint("busy", s.n), fmt.Sprint("busy", s.n))
		c.ScopeLimit, c.Budget = 0, 0
		if tk, err := s.l.Admit(ctx, c); err == nil {
			tk.Complete(ctx, 1)
		}
	}
}

func (s *atCap) AddAll(ctx context.Context, incs []Increment) error {
	if s.batches++; s.batches > 1000 {
		return errors.New("the replay does not converge")
	}
	s.arrive(ctx)
	if err := s.Mem.AddAll(ctx, incs); err != nil {
		return err
	}
	for _, in := range incs {
		s.counted[in.Call]++
	}
	return nil
}

func (s *atCap) Complete(ctx context.Context, in Increment, sl Slot) error {
	if err := s.Mem.Complete(ctx, in, sl); err != nil {
		return err
	}
	s.counted[in.Call]++
	return nil
}

// TestRecoverAtTheCap: completions held while a replay is in flight, at
// MaxPending, drop the oldest held increments; the batch in flight is
// still taken off once sent, so no increment is counted twice, and the
// recovery converges however many complete meanwhile. Taking a batch off
// only while it was still the held list's prefix re-sent it every round
// (1580 counted of 350 completed) and, under steady traffic, never ended.
func TestRecoverAtTheCap(t *testing.T) {
	for _, tc := range []struct {
		name         string
		perAdd, left int
	}{
		{"a burst during the replay", 10, 50},
		{"steady traffic past a batch per replay", 300, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			st := &atCap{Mem: NewMem(), perAdd: tc.perAdd, left: tc.left, counted: map[string]int{}}
			l := New(st)
			st.l = l
			l.MaxPending = 300
			st.SetDown(true)
			for i := range 300 {
				c := scopeCall(fmt.Sprint(i), fmt.Sprint("p", i))
				c.ScopeLimit = 0
				tk, err := l.Admit(ctx, c)
				if err != nil {
					t.Fatal(err)
				}
				tk.Complete(ctx, 1)
			}
			st.SetDown(false)
			if err := l.Recover(ctx); err != nil || l.Down() {
				t.Fatalf("Recover = %v, down %v: it must converge at the cap", err, l.Down())
			}
			twice := 0
			for _, n := range st.counted {
				if n > 1 {
					twice++
				}
			}
			completed := int64(300 + st.n)
			held, known := len(l.Pending("s")), l.Known(ctx, "s")
			if twice > 0 || held != 0 || known != int64(len(st.counted)) || known > completed {
				t.Fatalf("held %d, known %d, %d calls counted (%d more than once), of %d completed (%d dropped): each must be counted at most once",
					held, known, len(st.counted), twice, completed, l.Dropped.Load())
			}
			if known+l.Dropped.Load() < completed {
				t.Fatalf("known %d + dropped %d < %d completed: an increment lost uncounted", known, l.Dropped.Load(), completed)
			}
		})
	}
}
