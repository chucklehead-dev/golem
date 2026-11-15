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
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// Refusals (contracts.md §1), matched with errors.Is.
var (
	ErrBudget = &admission.Refusal{Code: contract.BudgetPoolExhausted,
		Msg: "the scope's budget for this period is spent; its managers or a budget manager can add to it"}
	ErrConcurrency = &admission.Refusal{Code: contract.LimitConcurrency,
		Msg: "too many calls in flight for this key's principal or scope; retry when one finishes"}
	ErrMemberCap = &admission.Refusal{Code: contract.BudgetMemberCap,
		Msg: "your cap in this scope for this period is spent; the scope's managers can raise it"}
	ErrKeyCap = &admission.Refusal{Code: contract.BudgetKeyCap,
		Msg: "this key's spend cap for this period is spent; its owner or the scope's managers can raise it, or use another key"}
)

// refusalAt is the refusal of a call over a level's limit.
func refusalAt(l admission.Level) *admission.Refusal {
	switch l {
	case admission.KeyLevel:
		return ErrKeyCap
	case admission.MemberLevel:
		return ErrMemberCap
	}
	return ErrBudget
}

// Call is the call the limiter admits: admission's.
type Call = admission.Call

// Known is a snapshot's known spend for one account (ADR 0048), for one
// budget period: the ledger's spend of the calls that ended before Since,
// a boundary golem-control places an ingestion allowance behind the
// snapshot's time. The site counter's minutes from Since on are added to
// it at admission.
type Known struct {
	Period string
	Spend  int64
	Since  time.Time
}

// Limiter is one gateway's admission check over the site store. Its lock
// guards only local state; store calls run outside it, each bounded by
// StoreTimeout.
type Limiter struct {
	store Store
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
	// MaxPending bounds the increments held during an outage (default
	// 100,000); past it the oldest is dropped (counted in Dropped). Its
	// ledger row was written at the call's end, so the snapshots count it
	// once their boundary passes its minute: the counter, this site's fast
	// view, loses it until then.
	MaxPending int
	// Dropped counts the increments dropped: held past MaxPending, or
	// abandoned at shutdown with the store unreachable.
	Dropped atomic.Int64
	// Waived counts the calls admitted as waivers (Ticket.Waived): while
	// the store was unreachable, or in the grace after a recovery.
	Waived atomic.Int64

	// down is read on every request, so without the lock; it is written
	// under mu with the state it goes with.
	down    atomic.Bool
	periods atomic.Pointer[periodName] // the latest month's, for period

	mu          sync.Mutex
	known       map[string]Known        // by account, from the snapshot
	last        map[keyOf]int64         // known spend as last computed: all an outage leaves
	running     map[string]*Ticket      // this gateway's calls the provider may still be working on
	tails       map[*Ticket]*time.Timer // cancelled calls' slots, held until their timer
	byPrincipal map[string]int          // running calls, counted: an outage's local check is O(1)
	byScope     map[string]int
	pending     []held // increments held while the store is down, in order of seq
	heldSeq     uint64 // the last held increment's seq
	graceUntil  time.Time
	// A renewal copies the running calls' slots, then registers them
	// outside the lock: a call that ends meanwhile would be registered
	// again, a phantom slot for the TTL. Calls ending while one is in
	// flight are released after it.
	reacquiring int
	endedDuring []Slot
	// beforeHold, if set, runs before an increment is held (tests: a
	// recovery between a completion's look and its hold).
	beforeHold func()
}

// held is an increment held for replay, with its seq: increasing with each
// increment held, so a replay takes off what it sent by identity, not by
// position (holds at MaxPending drop the oldest while a batch is in
// flight) nor by value (two equal increments are two).
type held struct {
	seq uint64
	Increment
}

// keyOf is an account in a budget period.
type keyOf struct{ account, period string }

// charged is a call's accounts (org-model.md §4.2), in the order admission
// checks them: its key's and its member's where they are capped, then its
// scope's. An uncapped key or member has no account: its spend is its
// scope's, and the site keeps no counter for it.
type charged struct {
	n      int
	ids    [MaxAccounts]string
	levels [MaxAccounts]admission.Level
	limits [MaxAccounts]int64 // each account's own: its cap, or the scope's budget
}

func chargedOf(c Call) charged {
	var ch charged
	add := func(id string, lv admission.Level, limit int64) {
		ch.ids[ch.n], ch.levels[ch.n], ch.limits[ch.n] = id, lv, limit
		ch.n++
	}
	if c.KeyCap > 0 && c.Key != "" {
		add(KeyAccount(c.Key), admission.KeyLevel, c.KeyCap)
	}
	if c.MemberCap > 0 {
		add(MemberAccount(c.Scope, c.Principal), admission.MemberLevel, c.MemberCap)
	}
	add(c.Scope, admission.ScopeLevel, c.Budget)
	return ch
}

// over is the first account a call admitted at known is over the limit
// of, or none.
func (ch *charged) over(c Call, known *[MaxAccounts]int64) admission.Over {
	for i := range ch.n {
		if overBudget(known[i], ch.limits[i], c.DeclaredWorstCaseCheck, c.Worst) {
			return admission.Over{Level: ch.levels[i], Known: known[i], Limit: ch.limits[i]}
		}
	}
	return admission.Over{}
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

// defaultMaxPending is MaxPending's default.
const defaultMaxPending = 100_000

// New returns a gateway's limiter over the site store.
func New(store Store) *Limiter {
	return &Limiter{store: store, known: map[string]Known{}, last: map[keyOf]int64{},
		running: map[string]*Ticket{}, tails: map[*Ticket]*time.Timer{}, byPrincipal: map[string]int{}, byScope: map[string]int{},
		MaxPending: defaultMaxPending, Grace: 2 * renewInterval, StoreTimeout: 250 * time.Millisecond, Location: time.UTC}
}

// Ticket is an admitted call. It ends once, by Complete or Cancel (a
// later end is ignored); after Cancel, its slot is held until Ended, or
// the time EndAt names.
type Ticket struct {
	l      *Limiter
	call   Call
	period string
	waived bool
	ch     charged
	over   admission.Over // the account admitted over its limit, in a mode that admits one
	ended  bool           // Complete or Cancel has run (under l.mu)
	freed  bool           // its slot is freed (under l.mu)
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

// OverBudget reports the account the call was admitted over the limit of,
// in a mode that admits one (warn, observe): what enforce would have
// refused.
func (t *Ticket) OverBudget() admission.Over { return t.over }

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
func (l *Limiter) snapshotFor(account, period string) Known {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.snapshotLocked(account, period)
}

// snapshotLocked is snapshotFor with mu held.
func (l *Limiter) snapshotLocked(account, period string) Known {
	if k := l.known[account]; k.Period == period {
		return k
	}
	return Known{Period: period}
}

// Admit checks a call and takes its slots. While the store is reachable it
// is one atomic step at the store, one round trip: each account's known
// spend against its limit (the key's cap, the member's, the scope's
// budget), then the slots. While it is not, this gateway's own counts and its
// last known spend decide, and the call is a waiver of the bound (ADR 0041
// §7.3).
//
// Only an enforced budget refuses (c.BudgetMode, the scope's, for every
// level): under warn or observe the store checks the slots alone, and the
// known spends it returns are checked against the limits here, the ticket
// saying which account the call is over.
func (l *Limiter) Admit(ctx context.Context, c Call) (*Ticket, error) {
	period := l.period(cmp.Or(c.Started, l.now()))
	ch := chargedOf(c)
	if !l.Down() {
		var accts [MaxAccounts]Account
		l.mu.Lock()
		for i := range ch.n {
			k := l.snapshotLocked(ch.ids[i], period)
			accts[i] = Account{ID: ch.ids[i], Since: k.Since, Spend: k.Spend, Limit: refusing(c, ch.limits[i])}
		}
		l.mu.Unlock()
		a := Admission{Slot: slotOf(c), Period: period, Now: l.now(), Accounts: accts[:ch.n],
			DeclaredWorstCaseCheck: c.DeclaredWorstCaseCheck, Worst: c.Worst,
			PrincipalLimit: c.PrincipalLimit, ScopeLimit: c.ScopeLimit}
		sctx, cancel := l.storeContext(ctx)
		r, err := l.store.Admit(sctx, a)
		cancel()
		switch {
		case err == nil:
			return l.admitted(c, &ch, period, r)
		case ctx.Err() != nil:
			return nil, ctx.Err() // the caller went away: no outage
		}
		l.markDown(err) // and fall through to the local check
	}
	return l.admitLocally(c, &ch, period)
}

// refusing is the limit a call is refused over: the account's own if the
// scope enforces, else none.
func refusing(c Call, limit int64) int64 {
	if c.BudgetMode != admission.Enforce {
		return 0
	}
	return limit
}

// admitted takes the store's answer to an admission: the known spends it
// computed become the ones an outage would fall back on, and an admitted
// call is tracked.
func (l *Limiter) admitted(c Call, ch *charged, period string, r Result) (*Ticket, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := range ch.n {
		l.last[keyOf{ch.ids[i], period}] = r.Known[i]
	}
	switch r.Outcome {
	case OverBudget:
		if r.Over < 0 || r.Over >= ch.n {
			return nil, ErrBudget // a store's garbled answer: the scope's refusal
		}
		return nil, refusalAt(ch.levels[r.Over])
	case OverConcurrency:
		return nil, ErrConcurrency
	}
	// In grace, peers may still hold calls Valkey lacks.
	return l.track(c, ch, period, l.now().Before(l.graceUntil), &r.Known), nil
}

// admitLocally is the outage's check: the last known spends and this
// gateway's own running calls decide, and the call is a waiver. The check
// and the tracking are one step under the lock.
func (l *Limiter) admitLocally(c Call, ch *charged, period string) (*Ticket, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var known [MaxAccounts]int64
	for i := range ch.n {
		known[i] = l.last[keyOf{ch.ids[i], period}]
		if overBudget(known[i], refusing(c, ch.limits[i]), c.DeclaredWorstCaseCheck, c.Worst) {
			return nil, refusalAt(ch.levels[i])
		}
	}
	if atLimit(l.byPrincipal[c.Principal], c.PrincipalLimit) || atLimit(l.byScope[c.Scope], c.ScopeLimit) {
		return nil, ErrConcurrency
	}
	return l.track(c, ch, period, true, &known), nil
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

// markDown records an outage: the store failed and the caller is still
// there. The gateway admits on its own counts until Recover.
func (l *Limiter) markDown(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.down.Swap(true) {
		slog.Warn("limit: the site store is unreachable; admitting on local counts, recorded as waivers", "err", err)
	}
}

// MarkDown starts the limiter in its outage mode: the store was found
// unreachable before any call (a gateway starting during a Valkey outage).
// Calls are admitted on local counts as waivers, increments held, until
// Run's Recover finds the store answering.
func (l *Limiter) MarkDown(err error) { l.markDown(err) }

// pinger is a store that can tell whether it answers without changing
// anything (Valkey's PING).
type pinger interface{ Ping(context.Context) error }

// track records a running call admitted at its accounts' known spends; mu
// is held.
func (l *Limiter) track(c Call, ch *charged, period string, waived bool, known *[MaxAccounts]int64) *Ticket {
	t := &Ticket{l: l, call: c, period: period, waived: waived, ch: *ch, over: ch.over(c, known)}
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

// Complete records the call's cost and frees its slot: one round trip.
// If the store does not take it, the increment is held and the slot's TTL
// frees the slot.
func (t *Ticket) Complete(ctx context.Context, cost int64) {
	if !t.l.claimEnd(t) || !t.l.finish(t) {
		return
	}
	t.l.account(ctx, t, cost, true)
}

// Cancel records what the gateway saw of a call the client abandoned. The
// slot is kept until Ended: the provider may still be working (and billing)
// on it, and freeing the slot would let cancel-and-resend escape the
// in-flight limit (ADR 0041 §7.6).
func (t *Ticket) Cancel(ctx context.Context, seen int64) { t.cancel(ctx, seen) }

// cancel is Cancel, reporting whether it was the ticket's end.
func (t *Ticket) cancel(ctx context.Context, seen int64) bool {
	if !t.l.claimEnd(t) {
		return false
	}
	t.l.account(ctx, t, seen, false)
	return true
}

// account adds a ticket's cost to its accounts' counters, in the minute it
// ends, and with release frees its slot in the same step (Store.Complete);
// otherwise the slot stays (Store.Add). While the store is down, or if
// this call to it fails (an outage: accounting runs detached from the
// caller), the increment is held for Recover. An increment is held only
// while the limiter is down (hold refuses one otherwise), so one that
// races a recovery is sent again rather than held where no replay will
// find it.
func (l *Limiter) account(ctx context.Context, t *Ticket, cost int64, release bool) {
	// A cost is never negative (it would take spend back) nor past
	// MaxMicros (Valkey's sums would no longer be exact): only a garbled
	// usage report gives one, and it is counted as the bound.
	in := Increment{Accounts: t.ch.ids[:t.ch.n], Period: t.period, Call: t.call.ID, Cost: min(max(cost, 0), snapshot.MaxMicros), At: l.now()}
	for {
		if !l.Down() {
			err := l.send(ctx, in, t, release)
			if err == nil {
				return
			}
			l.markDown(err)
		}
		if l.beforeHold != nil {
			l.beforeHold()
		}
		if l.hold(in) {
			return
		}
	}
}

// send gives the store a ticket's increment: with release, freeing its slot
// in the same step (Store.Complete); without, keeping it (Store.Add).
func (l *Limiter) send(ctx context.Context, in Increment, t *Ticket, release bool) error {
	sctx, cancel := l.storeContext(ctx)
	defer cancel()
	if release {
		return l.store.Complete(sctx, in, t.slot())
	}
	return l.store.Add(sctx, in)
}

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

// Ended frees a cancelled call's slot once its upstream has ended (the
// connection closed, or the deployment's maximum duration passed).
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
		l.markDown(err)
	}
}

// hold keeps an increment for Recover to replay, dropping the oldest past
// MaxPending. It refuses (false) once the limiter is up again: Recover
// marks it up under the same lock and then drains what is held, so an
// increment held after that would be held for ever.
func (l *Limiter) hold(in Increment) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.down.Load() {
		return false
	}
	l.heldSeq++
	l.pending = append(l.pending, held{l.heldSeq, in})
	if len(l.pending) > cmp.Or(l.MaxPending, defaultMaxPending) {
		l.pending = l.pending[1:]
		l.Dropped.Add(1)
	}
	return true
}

// ApplySnapshot takes a snapshot's known spend for an account (a scope's
// ID, MemberAccount or KeyAccount). The counter is
// left as it is: a gateway still on an older snapshot needs the minutes
// from its own boundary (ADR 0048), which retention trims.
func (l *Limiter) ApplySnapshot(_ context.Context, account string, k Known) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.known[account] = k
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
		l.markDown(err)
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

// Recover is called when the store answers again: the held increments are
// replayed in order, each into its own minute, and every call still
// running re-registers its slot, so calls that streamed through the outage
// still count. No lock is held across a store call.
//
// It converges under load: while it replays, completions are still held,
// so it replays until little is left, or until it has replayed what was
// held when it began (completions at MaxPending can hold more than a batch
// per round), re-registers the slots, marks the limiter up (from then on
// hold refuses, and completions go to the store themselves), and drains
// the rest, which can only shrink.
func (l *Limiter) Recover(ctx context.Context) error {
	// With nothing held and nothing running, the replay and the renewal
	// touch no store: ask it first, or an unreachable one would be marked
	// up (and found down again by the next call).
	if p, ok := l.store.(pinger); ok {
		sctx, cancel := l.storeContext(ctx)
		err := p.Ping(sctx)
		cancel()
		if err != nil {
			return err
		}
	}
	l.mu.Lock()
	began := l.heldSeq
	l.mu.Unlock()
	for l.heldBefore(began) {
		if _, err := l.replayBatch(ctx); err != nil {
			return err
		}
	}
	if err := l.reacquire(ctx); err != nil {
		return err
	}
	l.mu.Lock()
	l.down.Store(false)
	l.graceUntil = l.now().Add(l.Grace)
	l.mu.Unlock()
	for {
		n, err := l.replayBatch(ctx)
		if err != nil {
			l.markDown(err) // what is left stays held for the next Recover
			return err
		}
		if n == 0 {
			return nil
		}
	}
}

// heldBefore reports whether more than a batch is held and the oldest was
// held by seq last: Recover's replay while down. Each round takes off the
// oldest (sent, or dropped at MaxPending), so it ends.
func (l *Limiter) heldBefore(last uint64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.pending) > replayBatchSize && l.pending[0].seq <= last
}

// replayBatch replays up to replayBatchSize of the oldest held increments
// in one round trip, all or none: one at a time, a busy gateway's
// completions (held while it replays) could outpace the replay.
func (l *Limiter) replayBatch(ctx context.Context) (int, error) {
	l.mu.Lock()
	n := min(len(l.pending), replayBatchSize)
	batch := make([]Increment, n)
	for i, h := range l.pending[:n] {
		batch[i] = h.Increment
	}
	var last uint64
	if n > 0 {
		last = l.pending[n-1].seq
	}
	l.mu.Unlock()
	if n == 0 {
		return 0, nil
	}
	sctx, cancel := l.storeContext(ctx)
	err := l.store.AddAll(sctx, batch)
	cancel()
	if err != nil {
		return 0, err
	}
	l.dropReplayed(last)
	return n, nil
}

// dropReplayed takes a replayed batch off the held increments: every one
// held by seq last. A batch is the oldest held when it was sent, so those
// still held are at the front; those gone meanwhile were taken by another
// replay, or dropped at MaxPending (and counted in Dropped). Only Recover
// and replayOne take from the front; holds append.
func (l *Limiter) dropReplayed(last uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	i, _ := slices.BinarySearchFunc(l.pending, last, func(h held, seq uint64) int { return cmp.Compare(h.seq, seq) })
	if i < len(l.pending) && l.pending[i].seq == last {
		i++
	}
	l.pending = l.pending[i:]
}

// replayBatchSize is the increments one replay sends: one add script
// (addChunk), so a batch is added whole or not at all.
const replayBatchSize = addChunk

// replayOne replays the oldest increment held during an outage and reports
// it; Recover replays them all. ok is false when none is held.
func (l *Limiter) replayOne(ctx context.Context) (in Increment, ok bool, err error) {
	l.mu.Lock()
	if len(l.pending) == 0 {
		l.mu.Unlock()
		return Increment{}, false, nil
	}
	in, seq := l.pending[0].Increment, l.pending[0].seq
	l.mu.Unlock()
	sctx, cancel := l.storeContext(ctx)
	err = l.store.Add(sctx, in)
	cancel()
	if err != nil {
		return in, false, err
	}
	l.dropReplayed(seq)
	return in, true, nil
}

// Abandon gives up the increments still held (a gateway leaving with its
// store unreachable). Their ledger rows were written at their calls' ends:
// the snapshots count them once their boundary passes; the counter never
// will.
func (l *Limiter) Abandon() int {
	l.mu.Lock()
	n := len(l.pending)
	l.pending = nil
	l.mu.Unlock()
	l.Dropped.Add(int64(n))
	return n
}

// endGrace ends the grace after a recovery at once, as the model's
// graceEnds does once every gateway has recovered: for tests (no operator
// path calls it).
func (l *Limiter) endGrace() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.graceUntil = time.Time{}
}

// Known is an account's known spend now, for the current period, read afresh
// when the store answers (else the last computed): as admission computes
// it, never below the counter's whole total. It does not change what
// admission decides from: only an admission's own read does, as in
// model/exposure.md.
func (l *Limiter) Known(ctx context.Context, account string) int64 {
	period := l.period(l.now())
	if !l.Down() {
		k := l.snapshotFor(account, period)
		sctx, cancel := l.storeContext(ctx)
		since, err := l.store.Since(sctx, account, period, k.Since)
		total := since
		if err == nil && !k.Since.IsZero() {
			total, err = l.store.Since(sctx, account, period, time.Time{})
		}
		cancel()
		if err == nil {
			return max(k.Spend+since, total)
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last[keyOf{account, period}]
}

// pendingIn lists the increments held for replay, for an account.
func (l *Limiter) pendingIn(account string) []Increment {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Increment
	for _, h := range l.pending {
		if slices.Contains(h.Accounts, account) {
			out = append(out, h.Increment)
		}
	}
	return out
}

// Held is the number of increments held for replay (pendingIn, every scope).
func (l *Limiter) Held() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.pending)
}

// Down reports whether this gateway believes the store unreachable.
func (l *Limiter) Down() bool { return l.down.Load() }

// Run keeps running calls' slots alive (ADR 0038: every 30 seconds) and,
// after an outage, replays held increments and re-acquires slots, until
// ctx ends.
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

// Finish is the last accounting, once every call has ended: increments
// held through an outage replayed if the store answers within ctx, else
// abandoned (their rows, already written, are counted by the snapshots
// once their boundary passes).
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
			slog.Warn("limit: increments held through the outage were not replayed", "err", err)
		}
	}
	if n := l.Abandon(); n > 0 {
		slog.Warn("limit: increments held through the outage abandoned; the ledger has their rows", "increments", n)
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
