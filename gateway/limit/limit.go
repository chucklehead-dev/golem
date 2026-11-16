package limit

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chucklehead-dev/golem/gateway/admission"
	"github.com/chucklehead-dev/golem/lib/contract"
	"github.com/chucklehead-dev/golem/lib/period"
)

// The limiter's refusals, matched with errors.Is (contracts.md).
var (
	ErrBudget = &admission.Refusal{Code: contract.BudgetPoolExhausted,
		Msg: "the scope's budget for this period is spent; its managers or a budget manager can add to it"}
	ErrConcurrency = &admission.Refusal{Code: contract.LimitConcurrency,
		Msg: "too many calls in flight for this key's principal or scope; retry when one finishes"}
)

// Call is the call the limiter admits, as admission defines it.
type Call = admission.Call

// Known is one scope's known spend for one budget period, from the
// snapshot. It is the ledger's spend as golem-control last published it.
type Known struct {
	Period string
	Spend  int64
}

// Limiter is one gateway's admission check over the site store. Its lock
// guards only local state. Store calls run outside it, each bounded by
// StoreTimeout.
type Limiter struct {
	store Store
	// Grace is how long after its own recovery a gateway records its
	// admissions as waivers. Peers that have not recovered yet still run
	// calls whose slots Valkey lacks. The default is two renewal intervals,
	// since each peer renews, and so notices, within one.
	Grace time.Duration
	// StoreTimeout bounds each store call (default 250 ms).
	StoreTimeout time.Duration
	// Location is the budget periods' time zone (default UTC). A period is
	// a calendar month there, and a call belongs to the period it starts in.
	Location *time.Location
	// Now is the clock (tests set it).
	Now func() time.Time
	// Log is where outages and failed renewals are logged; nil logs to
	// slog.Default().
	Log *slog.Logger
	// Waived counts the calls admitted as waivers (Ticket.Waived).
	Waived atomic.Int64

	// down is read on every request, so it is read without the lock. It is
	// written under mu together with the state it goes with.
	down       atomic.Bool
	periods    atomic.Pointer[periodName] // the latest month's name, for period
	openLogged atomic.Int64               // when a rate limit last failed open, logged, in Unix ns

	mu          sync.Mutex
	spend       map[string]Known        // known spend by scope, from the snapshot
	running     map[string]*Ticket      // this gateway's calls the provider may still be working on
	tails       map[*Ticket]*time.Timer // cancelled calls' slots, held until their timer fires
	byPrincipal map[principalIn]int     // running calls counted, so an outage's local check is O(1)
	byScope     map[string]int
	graceUntil  time.Time
	// A renewal copies the running calls' slots and registers them outside
	// the lock. A call that ended meanwhile would be registered again, a
	// phantom slot until its TTL. So calls that end during a renewal are
	// released after it.
	reacquiring int
	endedDuring []Slot
	// A call that ends while the store is down cannot release its slot, so
	// a short outage leaves the slot at the store. It waits here for
	// Recover to release it. Past maxEndedDown, the TTL frees the rest.
	endedDown []Slot
	// downGen counts MarkDown's calls. Recover marks the limiter up only if
	// none landed while it re-registered the running calls.
	downGen uint64
	// downed wakes Run when an outage begins, so it probes for recovery
	// soon rather than at the next renewal.
	downed chan struct{}
	// probeFirst is the first recovery probe's wait. Tests shorten it.
	probeFirst time.Duration
}

// periodName is a month's period name, formatted once for the month.
type periodName struct {
	year  int
	month time.Month
	name  string
}

// renewInterval is how often Run renews the running calls' slots, well
// inside their TTL. ADR 0031.
const renewInterval = 30 * time.Second

// probeFirst is the wait before an outage's first recovery probe. Each
// failed probe doubles it, up to renewInterval. So a store back after a
// blip is found within about a second, and a long outage costs one probe
// per renewal interval.
const probeFirst = time.Second

// maxEndedDown bounds the slots kept for release on recovery, of calls
// that ended during an outage. A long outage's slots expire by TTL anyway.
const maxEndedDown = 1 << 16

// New returns a gateway's limiter over the site store.
func New(store Store) *Limiter {
	return &Limiter{store: store, spend: map[string]Known{},
		running: map[string]*Ticket{}, tails: map[*Ticket]*time.Timer{}, byPrincipal: map[principalIn]int{}, byScope: map[string]int{},
		Grace: 2 * renewInterval, StoreTimeout: 250 * time.Millisecond, Location: time.UTC,
		downed: make(chan struct{}, 1), probeFirst: probeFirst}
}

// Ticket is an admitted call. It ends once, by Complete or Cancel, and a
// later end is ignored. After Cancel, its slot is held until Ended runs or
// the time EndAt names.
type Ticket struct {
	l          *Limiter
	call       Call
	period     string
	waived     bool
	over       bool  // admitted over the budget, in a mode that admits one
	knownSpend int64 // the known spend admission read
	ended      bool  // Complete or Cancel has run, under l.mu
	freed      bool  // its slot is freed, under l.mu
}

// claimEnd claims the ticket's one end. It reports false if Complete or
// Cancel has already run.
func (l *Limiter) claimEnd(t *Ticket) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t.ended {
		return false
	}
	t.ended = true
	return true
}

// Waived reports whether the call was admitted without the store's check,
// during an outage or the grace after one. Its ledger row records the
// waiver. ADR 0041.
func (t *Ticket) Waived() bool { return t.waived }

// Period is the budget period the call is counted in, as its ledger row
// records it.
func (t *Ticket) Period() string { return t.period }

// OverBudget reports whether the call was admitted over its scope's
// budget, which only warn and observe modes allow. It also returns the
// known spend admission read.
func (t *Ticket) OverBudget() (bool, int64) { return t.over, t.knownSpend }

func (l *Limiter) log() *slog.Logger { return cmp.Or(l.Log, slog.Default()) }

func (l *Limiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// period is the name of the budget period a time falls in, cached for the
// month.
func (l *Limiter) period(t time.Time) string {
	y, m, _ := t.In(l.Location).Date()
	if p := l.periods.Load(); p != nil && p.year == y && p.month == m {
		return p.name
	}
	p := &periodName{y, m, period.Of(t, l.Location)}
	l.periods.Store(p)
	return p.name
}

// knownSpend is the snapshot's known spend for a scope in a period. It is
// zero if the snapshot is of another period, so a new month starts empty.
func (l *Limiter) knownSpend(scope, period string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.knownSpendLocked(scope, period)
}

// knownSpendLocked is knownSpend with mu held.
func (l *Limiter) knownSpendLocked(scope, period string) int64 {
	if k := l.spend[scope]; k.Period == period {
		return k.Spend
	}
	return 0
}

// Admit checks a call and takes its slots. While the store is reachable,
// that is one atomic step at the store, in one round trip. It checks known
// spend against the budget, then takes the slots. During an outage this
// gateway's own counts decide, and the call is a waiver. ADR 0041.
//
// Only an enforced budget refuses. Under warn or observe the store checks
// the slots alone, and the ticket says whether the call is over budget.
func (l *Limiter) Admit(ctx context.Context, c Call) (*Ticket, error) {
	period := l.period(cmp.Or(c.Started, l.now()))
	spend := l.knownSpend(c.Scope, period)
	if !l.Down() {
		a := Admission{Slot: slotOf(c), KnownSpend: spend, Budget: refusing(c),
			PrincipalLimit: c.PrincipalLimit, ScopeLimit: c.ScopeLimit}
		// The store call runs to its own timeout, not the caller's. If the
		// caller's leaving cut it, the store might hold a slot this gateway
		// does not track until its TTL. Run whole, the answer is known, and
		// an admission nobody waits for is ended at once.
		bg := context.WithoutCancel(ctx)
		sctx, cancel := l.storeContext(bg)
		o, err := l.store.Admit(sctx, a)
		cancel()
		switch {
		case err == nil:
			t, err := l.admitted(c, period, spend, o)
			if t != nil && ctx.Err() != nil {
				t.Complete(bg, 0) // frees the slot: the call never ran
				return nil, ctx.Err()
			}
			return t, err
		case ctx.Err() != nil:
			return nil, ctx.Err() // the caller went away: no outage
		}
		l.MarkDown(err) // then fall through to the local check
	}
	return l.admitLocally(c, period, spend)
}

// refusing is the budget to refuse a call at. It is the scope's budget if
// the scope enforces it, and otherwise 0.
func refusing(c Call) int64 {
	if c.BudgetMode != admission.Enforce {
		return 0
	}
	return c.Budget
}

// admitted takes the store's answer to an admission, and tracks an
// admitted call.
func (l *Limiter) admitted(c Call, period string, spend int64, o Outcome) (*Ticket, error) {
	switch o {
	case OverBudget:
		return nil, ErrBudget
	case OverConcurrency:
		return nil, ErrConcurrency
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	// In grace, peers may still run calls whose slots Valkey lacks.
	return l.track(c, period, l.now().Before(l.graceUntil), spend), nil
}

// admitLocally is the outage's check. The snapshot's known spend and this
// gateway's own running calls decide, and the call is a waiver. The check
// and the tracking are one step under the lock.
func (l *Limiter) admitLocally(c Call, period string, spend int64) (*Ticket, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if overBudget(spend, refusing(c)) {
		return nil, ErrBudget
	}
	if atLimit(l.byPrincipal[principalOf(c)], c.PrincipalLimit) || atLimit(l.byScope[c.Scope], c.ScopeLimit) {
		return nil, ErrConcurrency
	}
	return l.track(c, period, true, spend), nil
}

// storeContext bounds a store call. Valkey answers in a millisecond or
// two, and a hung call must not hold a request or a completion forever. An
// in-process store has no I/O to bound, and a deadline costs two
// allocations per call, so it gets none.
func (l *Limiter) storeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := l.store.(inProcess); ok {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, l.StoreTimeout)
}

// inProcess marks a store that answers in-process, never over a network.
type inProcess interface{ inProcess() }

// MarkDown records an outage. Either a store call failed while its caller
// waited, or the store was unreachable when the gateway started. Calls are
// then admitted on local counts as waivers, until Recover finds the store
// answering.
//
// One failed call marks the store down. After a failure the store's state
// is unknown, since an admit may have taken the slot or a release may not
// have landed. Trusting it would admit against counts known to be wrong.
// So a blip costs local counting until the first recovery probe, about a
// second later, and the grace's waivers after it.
func (l *Limiter) MarkDown(err error) { l.markDown(err) }

// markDown is MarkDown, and also keeps the slots whose release failed, for
// Recover to release.
func (l *Limiter) markDown(err error, unreleased ...Slot) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keepEnded(unreleased...)
	l.downGen++
	if !l.down.Swap(true) {
		l.log().Warn("limit: the site store is unreachable; admitting on local counts, recorded as waivers", "err", err)
		select {
		case l.downed <- struct{}{}:
		default: // Run has already been told
		}
	}
}

// keepEnded keeps ended calls' slots for release on recovery, up to
// maxEndedDown. The caller holds mu.
func (l *Limiter) keepEnded(slots ...Slot) {
	n := min(len(slots), maxEndedDown-len(l.endedDown))
	l.endedDown = append(l.endedDown, slots[:max(n, 0)]...)
}

// track records a running call and the known spend it was admitted at. The
// caller holds mu.
func (l *Limiter) track(c Call, period string, waived bool, spend int64) *Ticket {
	t := &Ticket{l: l, call: c, period: period, waived: waived, over: overBudget(spend, c.Budget), knownSpend: spend}
	if waived {
		l.Waived.Add(1)
	}
	l.running[c.ID] = t
	l.byPrincipal[principalOf(c)]++
	l.byScope[c.Scope]++
	return t
}

// finish marks a ticket done, forgets the call, and reports whether to
// release the slot now. A call that ends during a renewal is released
// after it. One that ends while the store is down is released on recovery.
func (l *Limiter) finish(t *Ticket) (release bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t.freed {
		return false
	}
	t.freed = true
	delete(l.running, t.call.ID)
	decr(l.byPrincipal, principalOf(t.call))
	decr(l.byScope, t.call.Scope)
	switch {
	case l.reacquiring > 0:
		l.endedDuring = append(l.endedDuring, t.slot())
	case l.Down():
		l.keepEnded(t.slot())
	default:
		return true
	}
	return false
}

// decr counts one fewer, deleting a count that reaches zero, so the maps
// stay as small as the calls running.
func decr[K comparable](m map[K]int, k K) {
	if m[k] <= 1 {
		delete(m, k)
		return
	}
	m[k]--
}

// principalIn is a principal in one scope, which a principal's in-flight
// limit counts. The limit is its scope's policy, so the principal's calls
// in another scope do not count against it.
type principalIn struct{ scope, principal string }

func principalOf(c Call) principalIn { return principalIn{c.Scope, c.Principal} }

// slotOf is the slot a call holds while it runs.
func slotOf(c Call) Slot { return Slot{c.ID, c.Principal, c.Scope} }

func (t *Ticket) slot() Slot { return slotOf(t.call) }

// Complete frees the call's slot, in one round trip. The cost is ignored,
// since the ledger row records it and the snapshots count it. A release
// the store does not take is retried on recovery. ADR 0031.
func (t *Ticket) Complete(ctx context.Context, _ int64) {
	if !t.l.claimEnd(t) {
		return
	}
	t.Ended(ctx)
}

// Cancel ends a call the client abandoned, keeping its slot until Ended.
// The provider may still be working and billing. Freeing the slot would let
// cancel-and-resend escape the in-flight limit. The ledger row records the
// cost seen. ADR 0041.
func (t *Ticket) Cancel(_ context.Context, _ int64) { t.cancel() }

// cancel is Cancel, and reports whether it ended the ticket.
func (t *Ticket) cancel() bool { return t.l.claimEnd(t) }

// EndAt frees a cancelled call's slot at a later time. The provider may go
// on working and billing after its response was cut or abandoned, so the
// slot is held a while longer. ADR 0041.
func (t *Ticket) EndAt(at time.Time) {
	l := t.l
	l.mu.Lock()
	defer l.mu.Unlock()
	if t.freed || l.tails[t] != nil {
		return
	}
	l.tails[t] = time.AfterFunc(time.Until(at), func() { t.Ended(context.Background()) })
}

// Ended frees a call's slot once its upstream has ended, because the
// connection closed or the route's maximum duration passed.
func (t *Ticket) Ended(ctx context.Context) {
	l := t.l
	l.mu.Lock()
	delete(l.tails, t)
	l.mu.Unlock()
	if !l.finish(t) {
		return // released later, after a renewal or on recovery
	}
	sctx, cancel := l.storeContext(ctx)
	defer cancel()
	if err := l.store.Release(sctx, t.slot()); err != nil {
		l.markDown(err, t.slot())
	}
}

// ApplySnapshot takes a snapshot's known spend for a scope.
func (l *Limiter) ApplySnapshot(_ context.Context, scope string, k Known) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.spend[scope] = k
}

// Renew re-registers the slots of this gateway's running calls. Run calls
// it every 30 seconds. A failed renewal is an outage, so the gateway admits
// on its own counts until Recover. ADR 0041.
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
	l.release(ctx, l.reacquired())
	return err
}

// release frees ended calls' slots at the store. If one fails, the store
// is down: it and the rest wait for Recover.
func (l *Limiter) release(ctx context.Context, slots []Slot) {
	for i, s := range slots {
		sctx, cancel := l.storeContext(ctx)
		err := l.store.Release(sctx, s)
		cancel()
		if err != nil {
			l.markDown(err, slots[i:]...)
			return
		}
	}
}

// reacquired ends a renewal. The last renewal in flight returns the slots
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

// renewalGrace is how long a slot is still renewed past its call's
// deadline. It leaves time to cut the call and account for it.
const renewalGrace = time.Minute

// renewable lists the slots of this gateway's running calls that are
// still renewed. The caller holds mu.
func (l *Limiter) renewable() []Slot {
	var slots []Slot
	now := l.now()
	for _, t := range l.running {
		if d := t.call.Deadline; !d.IsZero() && now.After(d.Add(renewalGrace)) {
			continue // a call past its deadline is not renewed, so its TTL ends it
		}
		slots = append(slots, t.slot())
	}
	return slots
}

// pinger is a store that can check that it answers without changing
// anything, as Valkey's PING does.
type pinger interface{ Ping(context.Context) error }

// Recover runs when the store answers again. Every running call
// re-registers its slot, so calls that streamed through the outage still
// count. The limiter is then marked up, with its admissions waivers for a
// grace while peers recover. Last, the slots of calls that ended during
// the outage are released.
//
// A store call that fails during Recover keeps the limiter down. No lock
// is held across a store call.
func (l *Limiter) Recover(ctx context.Context) error {
	l.mu.Lock()
	gen := l.downGen
	l.mu.Unlock()
	// With nothing running, the renewal touches no store. So ping it
	// first, or an unreachable store would be marked up.
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
	if l.downGen != gen {
		l.mu.Unlock()
		return errFailedDuringRecovery
	}
	l.down.Store(false)
	l.graceUntil = l.now().Add(l.Grace)
	ended := l.endedDown
	l.endedDown = nil
	l.mu.Unlock()
	l.release(ctx, ended)
	return nil
}

// errFailedDuringRecovery is a recovery cut short because a store call
// failed while it ran.
var errFailedDuringRecovery = errors.New("limit: a site store call failed during recovery; still admitting on local counts")

// endGrace ends the grace after a recovery at once, as the exposure
// model's graceEnds does once every gateway has recovered. Only tests call
// it.
func (l *Limiter) endGrace() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.graceUntil = time.Time{}
}

// KnownSpend is a scope's known spend for the current period, from the
// snapshot, as admission reads it.
func (l *Limiter) KnownSpend(scope string) int64 { return l.knownSpend(scope, l.period(l.now())) }

// Down reports whether this gateway believes the store unreachable.
func (l *Limiter) Down() bool { return l.down.Load() }

// Run renews the running calls' slots every 30 seconds until ctx ends.
// During an outage it probes for recovery instead. The first probe comes
// probeFirst after the outage starts, and each failed probe doubles the
// wait, up to the renewal interval.
func (l *Limiter) Run(ctx context.Context) {
	wait := renewInterval
	t := time.NewTimer(wait)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-l.downed: // an outage began
			wait = l.probeFirst
			t.Reset(wait)
			continue
		case <-t.C:
		}
		switch {
		case l.Down():
			if err := l.Recover(ctx); err != nil {
				l.log().Warn("limiter store still unreachable; admitting on local counts, recorded as waivers", "err", err)
				wait = min(2*wait, renewInterval)
			} else {
				wait = renewInterval
			}
		default:
			if err := l.Renew(ctx); err != nil {
				l.log().Warn("slot renewal failed; admitting on local counts until the store answers", "err", err)
			}
			wait = renewInterval // a failed renewal signals downed
		}
		t.Reset(wait)
	}
}

// Finish ends the limiter once every call has ended. It stops the tails'
// timers without releasing their slots. Those expire by TTL, which is what
// a tail is for, since the provider may still be working.
func (l *Limiter) Finish(context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for t, timer := range l.tails {
		timer.Stop()
		delete(l.tails, t)
	}
}

// Admitter is the limiter as the request path's admission.Admitter. A nil
// limiter gives a nil Admitter, which admits every call uncounted.
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

// tailed is a Ticket whose Cancel holds its slot until a given time.
type tailed struct{ *Ticket }

func (h tailed) Cancel(_ context.Context, _ int64, until time.Time) {
	if h.cancel() {
		h.EndAt(until)
	}
}
