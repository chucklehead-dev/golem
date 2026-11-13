package limit

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// Refusals, mapped to contract codes by the proxy (contracts.md §1).
var (
	ErrBudget      = errors.New("limit: the budget is spent")      // budget.pool_exhausted, 402
	ErrConcurrency = errors.New("limit: too many calls in flight") // limit.concurrency, 429
)

// Call is one request's admission: who, where, and its limits as the
// snapshot gives them.
type Call struct {
	ID, Principal, Scope string
	Budget               int64 // the scope's budget in micro-units; 0: none
	PrincipalLimit       int   // in-flight calls per principal; 0: none
	ScopeLimit           int   // in-flight calls per scope (ADR 0041 §7.6); 0: none
	// Strict (a scope option): admit only if known spend plus Worst fits
	// (ADR 0031); else the soft check, known spend below the budget.
	Strict bool
	Worst  int64
}

// Known is a snapshot's known spend for one scope on this site (ADR 0042):
// the ledger's balance with this site's events numbered above Watermark
// left out, and Watermark, the counter number up to which every event of
// this site and scope is in the ledger.
type Known struct {
	Spend     int64
	Watermark int64
}

// Limiter is one gateway's admission check over the site store.
type Limiter struct {
	store Store
	// OnNumbered, if set, is told each event's counter number once it has
	// one: at completion, or when an increment held during an outage is
	// replayed. The event's ledger row carries the number (ADR 0042).
	OnNumbered func(scope string, e Event, seq int64)
	// Grace is how long after its own recovery a gateway records its
	// admissions as waivers: peers that have not recovered yet hold calls
	// Valkey lacks. Two renewal intervals by default (each peer renews, and
	// so notices, within one). Now is the clock.
	Grace time.Duration
	// StoreTimeout bounds each store call (default 250 ms).
	StoreTimeout time.Duration
	// Now is the clock (tests set it).
	Now func() time.Time

	mu         sync.Mutex
	known      map[string]Known   // by scope, from the snapshot
	last       map[string]int64   // by scope, known spend as last computed: all an outage leaves
	running    map[string]*Ticket // this gateway's calls the provider may still be working on
	pending    []held             // increments held while the store is down, in order
	down       bool
	graceUntil time.Time
	waived     map[string]bool // calls admitted while the store was down, or in grace
}

type held struct {
	scope string
	e     Event
}

// New returns a gateway's limiter over the site store.
func New(store Store) *Limiter {
	return &Limiter{store: store, known: map[string]Known{}, last: map[string]int64{},
		running: map[string]*Ticket{}, waived: map[string]bool{}, Grace: 60 * time.Second, StoreTimeout: 250 * time.Millisecond, Now: time.Now}
}

// Ticket is an admitted call. Exactly one of Complete or Cancel is called;
// after Cancel, Ended when the upstream has ended.
type Ticket struct {
	l    *Limiter
	call Call
	done bool
}

// Admit checks a call and takes its slots. While the store is reachable the
// limits are the site's and known spend is read afresh (one O(1) read);
// while it is not, this gateway's own counts and its last known spend, and
// the call is a waiver of the bound (ADR 0041 §7.3).
func (l *Limiter) Admit(ctx context.Context, c Call) (*Ticket, error) {
	if !l.isDown() {
		k, err := l.read(ctx, c.Scope)
		switch {
		case err == nil:
			l.mu.Lock()
			l.last[c.Scope] = k
			l.mu.Unlock()
		case ctx.Err() != nil:
			return nil, ctx.Err() // the caller went away: no outage
		default:
			l.markDown(err)
		}
	}
	l.mu.Lock()
	known := l.last[c.Scope]
	l.mu.Unlock()
	switch {
	case c.Budget == 0:
	case c.Strict && known+c.Worst > c.Budget:
		return nil, ErrBudget
	case !c.Strict && known >= c.Budget:
		return nil, ErrBudget
	}
	if !l.isDown() {
		sctx, cancel := l.storeContext(ctx)
		ok, err := l.store.Acquire(sctx, Slot{c.ID, c.Principal, c.Scope}, c.PrincipalLimit, c.ScopeLimit)
		cancel()
		switch {
		case err == nil && !ok:
			return nil, ErrConcurrency
		case err == nil:
			l.mu.Lock()
			defer l.mu.Unlock()
			if l.now().Before(l.graceUntil) {
				l.waived[c.ID] = true // peers may still hold calls Valkey lacks
			}
			return l.track(c), nil
		case ctx.Err() != nil:
			return nil, ctx.Err()
		}
		l.markDown(err) // and fall through to the local check
	}
	// The local check and the tracking are one step under the lock: no
	// store call between them.
	l.mu.Lock()
	defer l.mu.Unlock()
	byPrincipal, byScope := 0, 0
	for _, t := range l.running {
		if t.call.Principal == c.Principal {
			byPrincipal++
		}
		if t.call.Scope == c.Scope {
			byScope++
		}
	}
	if (c.PrincipalLimit > 0 && byPrincipal >= c.PrincipalLimit) || (c.ScopeLimit > 0 && byScope >= c.ScopeLimit) {
		return nil, ErrConcurrency
	}
	l.waived[c.ID] = true
	return l.track(c), nil
}

// storeContext bounds a store call: Valkey answers a site's gateways in a
// millisecond or two, and one that hangs must not hold a request (or, under
// context.WithoutCancel, a completion) for ever.
func (l *Limiter) storeContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, l.StoreTimeout)
}

func (l *Limiter) isDown() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.down
}

// markDown records an outage: the store failed and the caller is still
// there. The gateway admits on its own counts until Recover.
func (l *Limiter) markDown(err error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.down {
		slog.Warn("limit: the site store is unreachable; admitting on local counts, recorded as waivers", "err", err)
	}
	l.down = true
}

// read is known spend from the store: the snapshot's, plus the counter's
// total after its watermark.
func (l *Limiter) read(ctx context.Context, scope string) (int64, error) {
	l.mu.Lock()
	k := l.known[scope]
	l.mu.Unlock()
	sctx, cancel := l.storeContext(ctx)
	defer cancel()
	after, over, err := l.store.After(sctx, scope, k.Watermark)
	if err != nil {
		return 0, err
	}
	if over {
		slog.Warn("limit: the counter no longer holds this snapshot's watermark; counting all of it (over-counting)",
			"scope", scope, "watermark", k.Watermark)
	}
	return k.Spend + after, nil
}

// track records a running call; mu is held.
func (l *Limiter) track(c Call) *Ticket {
	t := &Ticket{l: l, call: c}
	l.running[c.ID] = t
	return t
}

// Complete records the call's cost and frees its slot.
func (t *Ticket) Complete(ctx context.Context, cost int64) {
	t.l.emit(ctx, t.call.Scope, Event{t.call.ID, cost})
	t.l.release(ctx, t)
}

// Cancel records what the gateway saw of a call the client abandoned. The
// slot is kept until Ended: the provider may still be working (and billing)
// on it, and freeing the slot would let cancel-and-resend escape the
// in-flight limit (ADR 0041 §7.6).
func (t *Ticket) Cancel(ctx context.Context, seen int64) {
	t.l.emit(ctx, t.call.Scope, Event{t.call.ID, seen})
}

// Ended frees a cancelled call's slot once its upstream has ended (the
// connection closed, or the deployment's maximum duration passed).
func (t *Ticket) Ended(ctx context.Context) { t.l.release(ctx, t) }

func (l *Limiter) emit(ctx context.Context, scope string, e Event) {
	if !l.isDown() {
		sctx, cancel := l.storeContext(ctx)
		seq, err := l.store.Add(sctx, scope, e)
		cancel()
		if err == nil {
			l.numbered(scope, e, seq)
			return
		}
		l.markDown(err) // completions run detached from the caller: an error is the store's
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pending = append(l.pending, held{scope, e}) // replayed by Recover
}

func (l *Limiter) numbered(scope string, e Event, seq int64) {
	if l.OnNumbered != nil {
		l.OnNumbered(scope, e, seq)
	}
}

func (l *Limiter) release(ctx context.Context, t *Ticket) {
	l.mu.Lock()
	if t.done {
		l.mu.Unlock()
		return
	}
	t.done = true
	delete(l.running, t.call.ID)
	down := l.down
	l.mu.Unlock()
	if !down {
		sctx, cancel := l.storeContext(ctx)
		err := l.store.Release(sctx, Slot{t.call.ID, t.call.Principal, t.call.Scope})
		cancel()
		if err != nil {
			l.markDown(err) // the slot's TTL frees it
		}
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
// request path calls it every 30 seconds (ADR 0038). A failed renewal is an
// outage: the gateway admits on its own counts until Recover (ADR 0041 §7.3).
func (l *Limiter) Renew(ctx context.Context) error {
	l.mu.Lock()
	if l.down {
		l.mu.Unlock()
		return ErrUnavailable
	}
	slots := l.slots()
	l.mu.Unlock()
	sctx, cancel := l.storeContext(ctx)
	defer cancel()
	if err := l.store.Reacquire(sctx, slots); err != nil {
		l.markDown(err)
		return err
	}
	return nil
}

// slots lists this gateway's running calls' slots; mu is held.
func (l *Limiter) slots() []Slot {
	var slots []Slot
	for _, t := range l.running {
		slots = append(slots, Slot{t.call.ID, t.call.Principal, t.call.Scope})
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
		_, ok, err := l.ReplayOne(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
	}
	l.mu.Lock()
	slots := l.slots()
	l.mu.Unlock()
	sctx, cancel := l.storeContext(ctx)
	defer cancel()
	if err := l.store.Reacquire(sctx, slots); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.pending) > 0 {
		return ErrUnavailable // more were held meanwhile: the next Recover replays them
	}
	l.down = false
	l.graceUntil = l.now().Add(l.Grace)
	return nil
}

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
	seq, err := l.store.Add(sctx, h.scope, h.e)
	cancel()
	if err != nil {
		return h.e, false, err
	}
	l.mu.Lock()
	if len(l.pending) > 0 && l.pending[0] == h {
		l.pending = l.pending[1:]
	}
	l.mu.Unlock()
	l.numbered(h.scope, h.e, seq)
	return h.e, true, nil
}

func (l *Limiter) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

// EndGrace ends the grace after a recovery at once: for an operator who
// knows every gateway has recovered, and for tests (the model's graceEnds).
func (l *Limiter) EndGrace() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.graceUntil = time.Time{}
}

// Known is a scope's known spend now, read afresh when the store answers
// (else the last computed). It does not change what admission decides
// from: only an admission's own read does, as in model/exposure.md.
func (l *Limiter) Known(ctx context.Context, scope string) int64 {
	if !l.isDown() {
		if k, err := l.read(ctx, scope); err == nil {
			return k
		}
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.last[scope]
}

// Waived lists the calls admitted while the store was unreachable: the
// exposure the bound does not cover, to be reported (ADR 0041 §7.3).
func (l *Limiter) Waived() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Sorted(keys(l.waived))
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
func (l *Limiter) Down() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.down
}

func keys[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
