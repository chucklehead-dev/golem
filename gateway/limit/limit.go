package limit

import (
	"cmp"
	"context"
	"log/slog"
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

// Known is a snapshot's known spend for one scope (ADR 0052), for one
// budget period: the ledger's spend, as golem-control last published it.
type Known struct {
	Period string
	Spend  int64
}

// Limiter is one gateway's admission check over the site store. Its lock
// guards only local state; store calls run outside it, each bounded by
// StoreTimeout.
type Limiter struct {
	store Store
	// Grace is how long after its own recovery a gateway records its
	// admissions as waivers: peers that have not recovered yet run calls
	// whose slots Valkey lacks. Two renewal intervals by default (each
	// peer renews, and so notices, within one).
	Grace time.Duration
	// StoreTimeout bounds each store call (default 250 ms).
	StoreTimeout time.Duration
	// Location is the budget periods' time zone (default UTC); a period is
	// a calendar month there, and a call belongs to the period it starts in
	// (contracts.md §3).
	Location *time.Location
	// Now is the clock (tests set it).
	Now func() time.Time
	// Waived counts the calls admitted as waivers (Ticket.Waived): while
	// the store was unreachable, or in the grace after a recovery.
	Waived atomic.Int64

	// down is read on every request, so without the lock; it is written
	// under mu with the state it goes with.
	down       atomic.Bool
	periods    atomic.Pointer[periodName] // the latest month's, for period
	openLogged atomic.Int64               // when a rate limit last failed open, logged (Unix ns)

	mu          sync.Mutex
	known       map[string]Known        // by scope, from the snapshot
	running     map[string]*Ticket      // this gateway's calls the provider may still be working on
	tails       map[*Ticket]*time.Timer // cancelled calls' slots, held until their timer
	byPrincipal map[string]int          // running calls, counted: an outage's local check is O(1)
	byScope     map[string]int
	graceUntil  time.Time
	// A renewal copies the running calls' slots, then registers them
	// outside the lock: a call that ends meanwhile would be registered
	// again, a phantom slot for the TTL. Calls ending while one is in
	// flight are released after it.
	reacquiring int
	endedDuring []Slot
}

// periodName is a month's period name, formatted once for the month.
type periodName struct {
	year  int
	month time.Month
	name  string
}

// renewInterval is how often Run renews the running calls' slots (ADR
// 0038), well inside their TTL.
const renewInterval = 30 * time.Second

// New returns a gateway's limiter over the site store.
func New(store Store) *Limiter {
	return &Limiter{store: store, known: map[string]Known{},
		running: map[string]*Ticket{}, tails: map[*Ticket]*time.Timer{}, byPrincipal: map[string]int{}, byScope: map[string]int{},
		Grace: 2 * renewInterval, StoreTimeout: 250 * time.Millisecond, Location: time.UTC}
}

// Ticket is an admitted call. It ends once, by Complete or Cancel (a
// later end is ignored); after Cancel, its slot is held until Ended, or
// the time EndAt names.
type Ticket struct {
	l      *Limiter
	call   Call
	period string
	waived bool
	over   bool  // admitted over the budget, in a mode that admits one
	known  int64 // the known spend admission read
	ended  bool  // Complete or Cancel has run (under l.mu)
	freed  bool  // its slot is freed (under l.mu)
}

// claimEnd claims the ticket's one end: false if Complete or Cancel has
// already run.
func (l *Limiter) claimEnd(t *Ticket) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t.ended {
		return false
	}
	t.ended = true
	return true
}

// Waived reports whether the call was admitted outside the bound: while the
// store was unreachable, or in the grace after a recovery. Its ledger row
// says so, the exposure ADR 0041 §7.3 reports.
func (t *Ticket) Waived() bool { return t.waived }

// Period is the budget period the call is counted in: its ledger row's.
func (t *Ticket) Period() string { return t.period }

// OverBudget reports whether the call was admitted over its scope's
// budget, in a mode that admits one (warn, observe): what enforce would
// have refused; and the known spend admission read.
func (t *Ticket) OverBudget() (bool, int64) { return t.over, t.known }

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

// knownFor is the snapshot's known spend for a scope in a period: none if
// the snapshot is of another period (a new month starts from nothing).
func (l *Limiter) knownFor(scope, period string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.knownLocked(scope, period)
}

// knownLocked is knownFor with mu held.
func (l *Limiter) knownLocked(scope, period string) int64 {
	if k := l.known[scope]; k.Period == period {
		return k.Spend
	}
	return 0
}

// Admit checks a call and takes its slots. While the store is reachable it
// is one atomic step at the store, one round trip: the scope's known
// spend (the snapshot's) against its budget, then the slots. While it is
// not, this gateway's own counts and the snapshot decide, and the call is
// a waiver of the bound (ADR 0041 §7.3).
//
// Only an enforced budget refuses (c.BudgetMode, the scope's): under warn
// or observe the store checks the slots alone, and the ticket says whether
// the call is over the budget.
func (l *Limiter) Admit(ctx context.Context, c Call) (*Ticket, error) {
	period := l.period(cmp.Or(c.Started, l.now()))
	known := l.knownFor(c.Scope, period)
	if !l.Down() {
		a := Admission{Slot: slotOf(c), Known: known, Budget: refusing(c),
			PrincipalLimit: c.PrincipalLimit, ScopeLimit: c.ScopeLimit}
		// The store call runs to its own bound, not the caller's: a call
		// cut by the caller leaving may have taken the slot at the store
		// without this gateway tracking it, a phantom for the slot's TTL.
		// Run whole, the answer is known, and an admission the caller no
		// longer waits for is ended at once.
		bg := context.WithoutCancel(ctx)
		sctx, cancel := l.storeContext(bg)
		o, err := l.store.Admit(sctx, a)
		cancel()
		switch {
		case err == nil:
			t, err := l.admitted(c, period, known, o)
			if t != nil && ctx.Err() != nil {
				t.Complete(bg, 0) // frees the slot: the call never ran
				return nil, ctx.Err()
			}
			return t, err
		case ctx.Err() != nil:
			return nil, ctx.Err() // the caller went away: no outage
		}
		l.MarkDown(err) // and fall through to the local check
	}
	return l.admitLocally(c, period, known)
}

// refusing is the budget a call is refused over: the scope's if it
// enforces, else none.
func refusing(c Call) int64 {
	if c.BudgetMode != admission.Enforce {
		return 0
	}
	return c.Budget
}

// admitted takes the store's answer to an admission: an admitted call is
// tracked.
func (l *Limiter) admitted(c Call, period string, known int64, o Outcome) (*Ticket, error) {
	switch o {
	case OverBudget:
		return nil, ErrBudget
	case OverConcurrency:
		return nil, ErrConcurrency
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// In grace, peers may still run calls whose slots Valkey lacks.
	return l.track(c, period, l.now().Before(l.graceUntil), known), nil
}

// admitLocally is the outage's check: the snapshot's known spend and this
// gateway's own running calls decide, and the call is a waiver. The check
// and the tracking are one step under the lock.
func (l *Limiter) admitLocally(c Call, period string, known int64) (*Ticket, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if overBudget(known, refusing(c)) {
		return nil, ErrBudget
	}
	if atLimit(l.byPrincipal[c.Principal], c.PrincipalLimit) || atLimit(l.byScope[c.Scope], c.ScopeLimit) {
		return nil, ErrConcurrency
	}
	return l.track(c, period, true, known), nil
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

// MarkDown records an outage: a store call failed (the caller still there),
// or the store was found unreachable before any call (a gateway starting
// during a Valkey outage). Calls are admitted on local counts as waivers
// until Run's Recover finds the store answering.
func (l *Limiter) MarkDown(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.down.Swap(true) {
		slog.Warn("limit: the site store is unreachable; admitting on local counts, recorded as waivers", "err", err)
	}
}

// track records a running call, admitted at known spend known; mu is
// held.
func (l *Limiter) track(c Call, period string, waived bool, known int64) *Ticket {
	t := &Ticket{l: l, call: c, period: period, waived: waived, over: overBudget(known, c.Budget), known: known}
	if waived {
		l.Waived.Add(1)
	}
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
	if t.freed {
		return false
	}
	t.freed = true
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

// slotOf is the slot a call holds while it runs.
func slotOf(c Call) Slot { return Slot{c.ID, c.Principal, c.Scope} }

func (t *Ticket) slot() Slot { return slotOf(t.call) }

// Complete frees the call's slot: one round trip. Its cost is the ledger
// row's, which the snapshots count (ADR 0052); the site keeps no spend. If
// the store does not take the release, the slot's TTL frees it.
func (t *Ticket) Complete(ctx context.Context, _ int64) {
	if !t.l.claimEnd(t) {
		return
	}
	t.Ended(ctx)
}

// Cancel ends a call the client abandoned. The slot is kept until Ended:
// the provider may still be working (and billing) on it, and freeing the
// slot would let cancel-and-resend escape the in-flight limit (ADR 0041
// §7.6). What was seen of its cost is the ledger row's.
func (t *Ticket) Cancel(_ context.Context, _ int64) { t.cancel() }

// cancel is Cancel, reporting whether it was the ticket's end.
func (t *Ticket) cancel() bool { return t.l.claimEnd(t) }

// EndAt frees a cancelled call's slot at a time to come: the provider may
// go on working (and billing) after its response was cut or abandoned, so
// the slot is held a while longer (ADR 0041 §7.6; golem#4).
func (t *Ticket) EndAt(at time.Time) {
	l := t.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if t.freed || l.tails[t] != nil {
		return
	}
	l.tails[t] = time.AfterFunc(time.Until(at), func() { t.Ended(context.Background()) })
}

// Ended frees a call's slot once its upstream has ended (the connection
// closed, or the deployment's maximum duration passed).
func (t *Ticket) Ended(ctx context.Context) {
	l := t.l
	l.mu.Lock()
	delete(l.tails, t)
	l.mu.Unlock()
	if !l.finish(t) || l.Down() {
		return // when down, the slot's TTL frees it
	}
	sctx, cancel := l.storeContext(ctx)
	defer cancel()
	if err := l.store.Release(sctx, t.slot()); err != nil {
		l.MarkDown(err)
	}
}

// ApplySnapshot takes a snapshot's known spend for a scope.
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
	if l.Down() {
		return ErrUnavailable
	}
	if err := l.reacquire(ctx); err != nil {
		l.MarkDown(err)
		return err
	}
	return nil
}

// reacquire registers the running calls' slots, then releases those of
// calls that ended while it ran.
func (l *Limiter) reacquire(ctx context.Context) error {
	l.mu.Lock()
	slots := l.renewable()
	l.reacquiring++
	l.mu.Unlock()
	sctx, cancel := l.storeContext(ctx)
	err := l.store.Reacquire(sctx, slots)
	cancel()
	for _, s := range l.reacquired() {
		sctx, cancel := l.storeContext(ctx)
		l.store.Release(sctx, s) // if it fails, the slot's TTL frees it
		cancel()
	}
	return err
}

// reacquired ends a renewal: the last of those in flight takes the slots
// of the calls that ended meanwhile, for release.
func (l *Limiter) reacquired() []Slot {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reacquiring--
	if l.reacquiring > 0 {
		return nil
	}
	ended := l.endedDuring
	l.endedDuring = nil
	return ended
}

// renewalGrace is how long past its deadline a call's slot is still
// renewed: time for its cut and its accounting.
const renewalGrace = time.Minute

// renewable lists the slots of this gateway's running calls that are
// still renewed; mu is held.
func (l *Limiter) renewable() []Slot {
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

// pinger is a store that can tell whether it answers without changing
// anything (Valkey's PING).
type pinger interface{ Ping(context.Context) error }

// Recover is called when the store answers again: every call still
// running re-registers its slot, so calls that streamed through the
// outage still count, and the limiter is marked up, its admissions
// waivers for a grace while peers recover theirs. No lock is held across
// a store call.
func (l *Limiter) Recover(ctx context.Context) error {
	// With nothing running, the renewal touches no store: ask it first,
	// or an unreachable one would be marked up (and found down again by
	// the next call).
	if p, ok := l.store.(pinger); ok {
		sctx, cancel := l.storeContext(ctx)
		err := p.Ping(sctx)
		cancel()
		if err != nil {
			return err
		}
	}
	if err := l.reacquire(ctx); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.down.Store(false)
	l.graceUntil = l.now().Add(l.Grace)
	return nil
}

// endGrace ends the grace after a recovery at once, as the model's
// graceEnds does once every gateway has recovered: for tests (no operator
// path calls it).
func (l *Limiter) endGrace() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.graceUntil = time.Time{}
}

// Known is a scope's known spend for the current period: the snapshot's,
// as admission reads it.
func (l *Limiter) Known(scope string) int64 { return l.knownFor(scope, l.period(l.now())) }

// Down reports whether this gateway believes the store unreachable.
func (l *Limiter) Down() bool { return l.down.Load() }

// Run keeps running calls' slots alive (ADR 0038: every 30 seconds) and,
// after an outage, re-acquires slots, until ctx ends.
func (l *Limiter) Run(ctx context.Context) {
	t := time.NewTicker(renewInterval)
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

// Finish ends the limiter once every call has ended: tails still held are
// not released. Their timers stop, and their slots expire by their TTL,
// which is what a tail is for (the provider may still be working).
func (l *Limiter) Finish(context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for t, timer := range l.tails {
		timer.Stop()
		delete(l.tails, t)
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

func (h tailed) Cancel(_ context.Context, _ int64, until time.Time) {
	if h.cancel() {
		h.EndAt(until)
	}
}
