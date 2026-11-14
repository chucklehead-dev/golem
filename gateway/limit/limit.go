package limit

import (
	"cmp"
	"context"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chucklehead-dev/golem/gateway/admission"
	"github.com/chucklehead-dev/golem/lib/contract"
	budget "github.com/chucklehead-dev/golem/lib/period"
)

// Refusals (contracts.md §1), matched with errors.Is.
var (
	ErrBudget = &admission.Refusal{Code: contract.BudgetPoolExhausted,
		Msg: "the scope's budget for this period is spent; its managers or a budget manager can add to it"}
	ErrConcurrency = &admission.Refusal{Code: contract.LimitConcurrency,
		Msg: "too many calls in flight for this key's principal or scope; retry when one finishes"}
)

// Call is the call the limiter admits: admission's.
type Call = admission.Call

// Known is a snapshot's known spend for one scope on this site (ADR 0042),
// for one budget period: the ledger's balance with this site's events
// numbered above Watermark left out, and Watermark, the counter number up
// to which every event of this site, scope and period is in the ledger.
type Known struct {
	Period    string
	Spend     int64
	Watermark int64
}

// Limiter is one gateway's admission check over the site store. Its lock
// guards only local state; store calls run outside it, each bounded by
// StoreTimeout.
type Limiter struct {
	store Store
	// OnNumbered, if set, is told each event's counter number once it has
	// one: at completion, or when an increment held during an outage is
	// replayed (or 0, when it never will be). The ledger writes it as the
	// event's numbering record (ADR 0043).
	OnNumbered func(Numbered)
	// Grace is how long after its own recovery a gateway records its
	// admissions as waivers: peers that have not recovered yet hold calls
	// Valkey lacks. Two renewal intervals by default (each peer renews, and
	// so notices, within one).
	Grace time.Duration
	// StoreTimeout bounds each store call (default 250 ms).
	StoreTimeout time.Duration
	// Location is the budget periods' time zone (default UTC); a period is
	// a calendar month there, and a call belongs to the period it starts in
	// (contracts.md §3).
	Location *time.Location
	// Now is the clock (tests set it).
	Now func() time.Time

	// down is read on every request, so without the lock; it is written
	// under mu with the state it goes with.
	down    atomic.Bool
	periods atomic.Pointer[periodName]

	mu          sync.Mutex
	known       map[string]Known        // by scope, from the snapshot
	last        map[keyOf]int64         // known spend as last computed: all an outage leaves
	overLogged  map[keyOf]bool          // over-counting reported once per scope and period
	running     map[string]*Ticket      // this gateway's calls the provider may still be working on
	tails       map[*Ticket]*time.Timer // cancelled calls' slots, held until their timer
	byPrincipal map[string]int          // running calls, counted: an outage's local check is O(1)
	byScope     map[string]int
	pending     []held // increments held while the store is down, in order
	graceUntil  time.Time
	// A renewal copies the running calls' slots, then registers them
	// outside the lock: a call that ends meanwhile would be registered
	// again, a phantom slot for the TTL. Calls ending while one is in
	// flight are released after it.
	reacquiring int
	endedDuring []Slot
	// MaxPending bounds the increments held during an outage (default
	// 100,000); past it the oldest is given to OnNumbered with number 0, so
	// its ledger row is written unnumbered (ADR 0042: never numbered).
	MaxPending int
}

// keyOf is a scope in a budget period.
type keyOf struct{ scope, period string }

// periodName is a month's period name, formatted once for the month.
type periodName struct {
	year  int
	month time.Month
	name  string
}

type held struct {
	scope, period string
	e             Event
}

// New returns a gateway's limiter over the site store.
func New(store Store) *Limiter {
	return &Limiter{store: store, known: map[string]Known{}, last: map[keyOf]int64{}, overLogged: map[keyOf]bool{},
		running: map[string]*Ticket{}, byPrincipal: map[string]int{}, byScope: map[string]int{}, MaxPending: 100_000,
		Grace: 60 * time.Second, StoreTimeout: 250 * time.Millisecond, Location: time.UTC}
}

// Numbered is an event's number in its scope's counter for a period: the
// ledger's numbering record (ADR 0043). Seq 0: the event never reached the
// counter (an increment dropped during an outage), so its row counts as
// it is.
type Numbered struct {
	Scope, Period string
	Event
	Seq int64
}

// Ticket is an admitted call. It ends once, by Complete or Cancel (a
// later end is ignored); after Cancel, its slot is held until Ended, or
// the time EndAt names.
type Ticket struct {
	l       *Limiter
	call    Call
	period  string
	waived  bool
	charged bool // Complete or Cancel has run (under l.mu)
	done    bool // its slot is freed (under l.mu)
}

// charge claims the ticket's one end.
func (l *Limiter) charge(t *Ticket) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t.charged {
		return false
	}
	t.charged = true
	return true
}

// Waived reports whether the call was admitted outside the bound: while the
// store was unreachable, or in the grace after a recovery. Its ledger row
// says so, the exposure ADR 0041 §7.3 reports.
func (t *Ticket) Waived() bool { return t.waived }

// Period is the budget period the call is counted in: its ledger row's.
func (t *Ticket) Period() string { return t.period }

func (l *Limiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// period is the budget period a time falls in (lib/period's name, cached
// for the month).
func (l *Limiter) period(t time.Time) string {
	y, m, _ := t.In(l.Location).Date()
	if p := l.periods.Load(); p != nil && p.year == y && p.month == m {
		return p.name
	}
	p := &periodName{y, m, budget.Of(t, l.Location)}
	l.periods.Store(p)
	return p.name
}

// snapshotFor is the snapshot's known spend for a scope in a period: none
// if the snapshot is of another period (a new month starts from nothing).
func (l *Limiter) snapshotFor(scope, period string) Known {
	l.mu.Lock()
	defer l.mu.Unlock()
	if k := l.known[scope]; k.Period == period {
		return k
	}
	return Known{Period: period}
}

// Admit checks a call and takes its slots. While the store is reachable it
// is one atomic step at the store, one round trip: known spend against the
// budget, then the slots. While it is not, this gateway's own counts and its
// last known spend decide, and the call is a waiver of the bound (ADR 0041
// §7.3).
func (l *Limiter) Admit(ctx context.Context, c Call) (*Ticket, error) {
	period := l.period(cmp.Or(c.Started, l.now()))
	lastKey := keyOf{c.Scope, period}
	if !l.isDown() {
		k := l.snapshotFor(c.Scope, period)
		sctx, cancel := l.storeContext(ctx)
		o, known, over, err := l.store.Admit(sctx, Admission{Slot: Slot{c.ID, c.Principal, c.Scope}, Period: period,
			Watermark: k.Watermark, Spend: k.Spend, Budget: c.Budget, DeclaredWorstCaseCheck: c.DeclaredWorstCaseCheck, Worst: c.Worst,
			PrincipalLimit: c.PrincipalLimit, ScopeLimit: c.ScopeLimit})
		cancel()
		switch {
		case err == nil:
			l.mu.Lock()
			defer l.mu.Unlock()
			if over && !l.overLogged[lastKey] {
				// Once per scope and period: after a failover it holds for
				// every admission until the next snapshot.
				l.overLogged[lastKey] = true
				slog.Warn("limit: the counter no longer holds this snapshot's watermark; counting all of it (over-counting)",
					"scope", c.Scope, "period", period, "watermark", k.Watermark)
			}
			l.last[lastKey] = known
			switch o {
			case OverBudget:
				return nil, ErrBudget
			case OverConcurrency:
				return nil, ErrConcurrency
			}
			// In grace, peers may still hold calls Valkey lacks.
			return l.track(c, period, l.now().Before(l.graceUntil)), nil
		case ctx.Err() != nil:
			return nil, ctx.Err() // the caller went away: no outage
		}
		l.markDown(err) // and fall through to the local check
	}
	// The local check and the tracking are one step under the lock.
	l.mu.Lock()
	defer l.mu.Unlock()
	known := l.last[lastKey]
	switch {
	case c.Budget == 0:
	case c.DeclaredWorstCaseCheck && known+c.Worst > c.Budget, !c.DeclaredWorstCaseCheck && known >= c.Budget:
		return nil, ErrBudget
	}
	if (c.PrincipalLimit > 0 && l.byPrincipal[c.Principal] >= c.PrincipalLimit) || (c.ScopeLimit > 0 && l.byScope[c.Scope] >= c.ScopeLimit) {
		return nil, ErrConcurrency
	}
	return l.track(c, period, true), nil
}

// storeContext bounds a store call: Valkey answers a site's gateways in a
// millisecond or two, and one that hangs must not hold a request (or, under
// context.WithoutCancel, a completion) for ever.
// An in-process store (Mem) has no I/O to bound, and a deadline costs two
// allocations per call.
func (l *Limiter) storeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := l.store.(inProcess); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, l.StoreTimeout)
}

// inProcess marks a store that answers in-process, never over a network.
type inProcess interface{ inProcess() }

func (l *Limiter) isDown() bool { return l.down.Load() }

// markDown records an outage: the store failed and the caller is still
// there. The gateway admits on its own counts until Recover.
func (l *Limiter) markDown(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.down.Load() {
		slog.Warn("limit: the site store is unreachable; admitting on local counts, recorded as waivers", "err", err)
	}
	l.down.Store(true)
}

// track records a running call; mu is held.
func (l *Limiter) track(c Call, period string, waived bool) *Ticket {
	t := &Ticket{l: l, call: c, period: period, waived: waived}
	l.running[c.ID] = t
	l.byPrincipal[c.Principal]++
	l.byScope[c.Scope]++
	return t
}

// finish marks a ticket done and forgets the call; it reports whether it
// was still running.
func (l *Limiter) finish(t *Ticket) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t.done {
		return false
	}
	t.done = true
	delete(l.running, t.call.ID)
	if l.reacquiring > 0 {
		l.endedDuring = append(l.endedDuring, t.slot())
	}
	decr(l.byPrincipal, t.call.Principal)
	decr(l.byScope, t.call.Scope)
	return true
}

// decr counts one fewer, forgetting a count at zero so the maps stay as
// small as the calls running.
func decr(m map[string]int, k string) {
	if m[k] <= 1 {
		delete(m, k)
		return
	}
	m[k]--
}

func (t *Ticket) slot() Slot { return Slot{t.call.ID, t.call.Principal, t.call.Scope} }

// Complete records the call's cost and frees its slot: one round trip.
func (t *Ticket) Complete(ctx context.Context, cost int64) {
	l, e := t.l, Event{t.call.ID, cost}
	if !l.charge(t) || !l.finish(t) {
		return
	}
	if !l.isDown() {
		sctx, cancel := l.storeContext(ctx)
		seq, err := l.store.Complete(sctx, t.period, t.slot(), e)
		cancel()
		if err == nil {
			l.numbered(t.call.Scope, t.period, e, seq)
			return
		}
		l.markDown(err) // completions run detached from the caller: an error is the store's
	}
	l.hold(t.call.Scope, t.period, e) // the slot's TTL frees it
}

// Cancel records what the gateway saw of a call the client abandoned. The
// slot is kept until Ended: the provider may still be working (and billing)
// on it, and freeing the slot would let cancel-and-resend escape the
// in-flight limit (ADR 0041 §7.6).
func (t *Ticket) Cancel(ctx context.Context, seen int64) { t.cancel(ctx, seen) }

func (t *Ticket) cancel(ctx context.Context, seen int64) bool {
	l, e := t.l, Event{t.call.ID, seen}
	if !l.charge(t) {
		return false
	}
	if !l.isDown() {
		sctx, cancel := l.storeContext(ctx)
		seq, err := l.store.Add(sctx, t.call.Scope, t.period, e)
		cancel()
		if err == nil {
			l.numbered(t.call.Scope, t.period, e, seq)
			return true
		}
		l.markDown(err)
	}
	l.hold(t.call.Scope, t.period, e)
	return true
}

// EndAt frees a cancelled call's slot at a time to come: the provider may
// go on working (and billing) after its response was cut or abandoned, so
// the slot is held a while longer (ADR 0041 §7.6; golem#4).
func (t *Ticket) EndAt(at time.Time) {
	l := t.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if t.done || l.tails[t] != nil {
		return
	}
	if l.tails == nil {
		l.tails = map[*Ticket]*time.Timer{}
	}
	l.tails[t] = time.AfterFunc(time.Until(at), func() { t.Ended(context.Background()) })
}

// Ended frees a cancelled call's slot once its upstream has ended (the
// connection closed, or the deployment's maximum duration passed).
func (t *Ticket) Ended(ctx context.Context) {
	l := t.l
	l.mu.Lock()
	delete(l.tails, t)
	l.mu.Unlock()
	if !l.finish(t) || l.isDown() {
		return // when down, the slot's TTL frees it
	}
	sctx, cancel := l.storeContext(ctx)
	defer cancel()
	if err := l.store.Release(sctx, t.slot()); err != nil {
		l.markDown(err)
	}
}

func (l *Limiter) hold(scope, period string, e Event) {
	l.mu.Lock()
	l.pending = append(l.pending, held{scope, period, e}) // replayed by Recover
	var dropped held
	over := len(l.pending) > cmp.Or(l.MaxPending, 100_000)
	if over {
		dropped, l.pending = l.pending[0], l.pending[1:]
	}
	l.mu.Unlock()
	if over {
		// The oldest goes to the ledger unnumbered: its row still counts
		// once ingested; only the counter (this site's fast view) loses it.
		l.numbered(dropped.scope, dropped.period, dropped.e, 0)
	}
}

func (l *Limiter) numbered(scope, period string, e Event, seq int64) {
	if l.OnNumbered != nil {
		l.OnNumbered(Numbered{scope, period, e, seq})
	}
}

// ApplySnapshot takes a snapshot's known spend for a scope. The counter is
// left as it is: a gateway still on an older snapshot needs its history
// (ADR 0042), which retention trims.
func (l *Limiter) ApplySnapshot(_ context.Context, scope string, k Known) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.known[scope] = k
}

// Renew re-registers the slots of this gateway's running calls; the
// gateway's renewal loop calls it every 30 seconds (ADR 0038). A failed
// renewal is an outage: the gateway admits on its own counts until Recover
// (ADR 0041 §7.3).
func (l *Limiter) Renew(ctx context.Context) error {
	l.mu.Lock()
	if l.down.Load() {
		l.mu.Unlock()
		return ErrUnavailable
	}
	l.mu.Unlock()
	if err := l.reacquire(ctx); err != nil {
		l.markDown(err)
		return err
	}
	return nil
}

// reacquire registers the running calls' slots, then releases those of
// calls that ended while it ran.
func (l *Limiter) reacquire(ctx context.Context) error {
	l.mu.Lock()
	slots := l.slots()
	l.reacquiring++
	l.mu.Unlock()
	sctx, cancel := l.storeContext(ctx)
	err := l.store.Reacquire(sctx, slots)
	cancel()
	l.mu.Lock()
	l.reacquiring--
	var ended []Slot
	if l.reacquiring == 0 {
		ended, l.endedDuring = l.endedDuring, nil
	}
	l.mu.Unlock()
	for _, s := range ended {
		sctx, cancel := l.storeContext(ctx)
		l.store.Release(sctx, s) // if it fails, the slot's TTL frees it
		cancel()
	}
	return err
}

// renewalGrace is how long past its deadline a call's slot is still
// renewed: time for its cut and its accounting.
const renewalGrace = time.Minute

// slots lists this gateway's running calls' slots; mu is held.
func (l *Limiter) slots() []Slot {
	var slots []Slot
	now := l.now()
	for _, t := range l.running {
		if d := t.call.Deadline; !d.IsZero() && now.After(d.Add(renewalGrace)) {
			continue // a call past its deadline holds no slot: its TTL ends it
		}
		slots = append(slots, t.slot())
	}
	return slots
}

// Recover is called when the store answers again: the held increments are
// replayed in order, each numbered (harmless if repeated), and every call
// still running re-registers its slot, so calls that streamed through the
// outage still count. No lock is held across a store call: completions may
// add to the held increments meanwhile, and Recover goes on until none is
// left.
func (l *Limiter) Recover(ctx context.Context) error {
	for {
		n, err := l.replayBatch(ctx)
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	if err := l.reacquire(ctx); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) > 0 {
		return ErrUnavailable // more were held meanwhile: the next Recover replays them
	}
	l.down.Store(false)
	l.graceUntil = l.now().Add(l.Grace)
	return nil
}

// replayBatch replays up to replayBatchSize of the oldest held increments
// in one round trip: one at a time, a busy gateway's completions (held
// while it replays) could outpace the replay.
func (l *Limiter) replayBatch(ctx context.Context) (int, error) {
	l.mu.Lock()
	batch := slices.Clone(l.pending[:min(len(l.pending), replayBatchSize)])
	l.mu.Unlock()
	if len(batch) == 0 {
		return 0, nil
	}
	incs := make([]Increment, len(batch))
	for i, h := range batch {
		incs[i] = Increment{h.scope, h.period, h.e}
	}
	sctx, cancel := l.storeContext(ctx)
	seqs, err := l.store.AddAll(sctx, incs)
	cancel()
	if err != nil {
		return 0, err
	}
	l.mu.Lock()
	// Only Recover and ReplayOne take from the front; completions append.
	if len(l.pending) >= len(batch) && slices.Equal(l.pending[:len(batch)], batch) {
		l.pending = l.pending[len(batch):]
	}
	l.mu.Unlock()
	for i, h := range batch {
		l.numbered(h.scope, h.period, h.e, seqs[i])
	}
	return len(batch), nil
}

const replayBatchSize = 256

// ReplayOne replays the oldest increment held during an outage and reports
// it; Recover replays them all. ok is false when none is held.
func (l *Limiter) ReplayOne(ctx context.Context) (e Event, ok bool, err error) {
	l.mu.Lock()
	if len(l.pending) == 0 {
		l.mu.Unlock()
		return Event{}, false, nil
	}
	h := l.pending[0]
	l.mu.Unlock()
	sctx, cancel := l.storeContext(ctx)
	seq, err := l.store.Add(sctx, h.scope, h.period, h.e)
	cancel()
	if err != nil {
		return h.e, false, err
	}
	l.mu.Lock()
	if len(l.pending) > 0 && l.pending[0] == h {
		l.pending = l.pending[1:]
	}
	l.mu.Unlock()
	l.numbered(h.scope, h.period, h.e, seq)
	return h.e, true, nil
}

// Abandon gives up the increments still held (a gateway leaving with its
// store unreachable): each is told to OnNumbered with number 0, so its row
// counts as it is, the counter never having had it.
func (l *Limiter) Abandon() int {
	l.mu.Lock()
	held := l.pending
	l.pending = nil
	l.mu.Unlock()
	for _, h := range held {
		l.numbered(h.scope, h.period, h.e, 0)
	}
	return len(held)
}

// EndGrace ends the grace after a recovery at once: for an operator who
// knows every gateway has recovered, and for tests (the model's graceEnds).
func (l *Limiter) EndGrace() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.graceUntil = time.Time{}
}

// Known is a scope's known spend now, for the current period, read afresh
// when the store answers (else the last computed). It does not change what
// admission decides from: only an admission's own read does, as in
// model/exposure.md.
func (l *Limiter) Known(ctx context.Context, scope string) int64 {
	period := l.period(l.now())
	if !l.isDown() {
		k := l.snapshotFor(scope, period)
		sctx, cancel := l.storeContext(ctx)
		after, _, err := l.store.After(sctx, scope, period, k.Watermark)
		cancel()
		if err == nil {
			return k.Spend + after
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last[keyOf{scope, period}]
}

// Pending lists the increments held for replay, for a scope.
func (l *Limiter) Pending(scope string) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Event
	for _, h := range l.pending {
		if h.scope == scope {
			out = append(out, h.e)
		}
	}
	return out
}

// Down reports whether this gateway believes the store unreachable.
func (l *Limiter) Down() bool { return l.isDown() }

// Run keeps running calls' slots alive (ADR 0038: every 30 seconds) and,
// after an outage, replays held increments and re-acquires slots, until
// ctx ends.
func (l *Limiter) Run(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if l.Down() {
			if err := l.Recover(ctx); err != nil {
				slog.Warn("limiter store still unreachable; admitting on local counts, recorded as waivers", "err", err)
			}
			continue
		}
		if err := l.Renew(ctx); err != nil {
			slog.Warn("slot renewal failed; admitting on local counts until the store answers", "err", err)
		}
	}
}

// Finish is the last accounting, once every call has ended: increments
// held through an outage replayed if the store answers within ctx, else
// abandoned (numbered 0: their rows, already written, count as they are).
//
// Tails still held are not released: their timers stop, and their slots
// expire by their TTL, which is what a tail is for (the provider may still
// be working), and no release races the abandonment below.
func (l *Limiter) Finish(ctx context.Context) {
	l.mu.Lock()
	for t, timer := range l.tails {
		timer.Stop()
		delete(l.tails, t)
	}
	l.mu.Unlock()
	if l.Down() {
		if err := l.Recover(ctx); err != nil {
			slog.Warn("limit: increments held through the outage were not replayed; their rows go unnumbered", "err", err)
		}
	}
	if n := l.Abandon(); n > 0 {
		slog.Warn("limit: increments held through the outage abandoned; numbered 0", "increments", n)
	}
}

// Admitter is the limiter as the request path's admission.Admitter; nil
// for a nil limiter (no limits: every call admitted uncounted).
func (l *Limiter) Admitter() admission.Admitter {
	if l == nil {
		return nil
	}
	return admitter{l}
}

type admitter struct{ l *Limiter }

func (a admitter) Admit(ctx context.Context, c admission.Call) (admission.Ticket, error) {
	t, err := a.l.Admit(ctx, c)
	if t == nil { // a nil *Ticket is not a nil admission.Ticket
		return nil, err
	}
	return tailed{t}, err
}

// tailed is a Ticket whose Cancel holds its slot to a time given.
type tailed struct{ *Ticket }

func (h tailed) Cancel(ctx context.Context, seen int64, until time.Time) {
	if h.cancel(ctx, seen) {
		h.EndAt(until)
	}
}
