package limit

import (
	"context"
	"errors"
	"slices"
	"sync"
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
	Budget               int64 // the scope's budget in micro-units
	PrincipalLimit       int   // in-flight calls per principal
	ScopeLimit           int   // in-flight calls per scope (ADR 0041 §7.6)
	// Strict (a scope option): admit only if known spend plus Worst fits
	// (ADR 0031); else the soft check, known spend below the budget.
	Strict bool
	Worst  int64
}

// Known is a snapshot's known spend for one scope: the ledger's
// deduplicated balance, and which events it includes, so the site counter
// adds only the rest (handoff by event: model/exposure.md §5.5).
type Known struct {
	Spend    int64
	Included map[string]bool
}

// Limiter is one gateway's admission check over the site store.
type Limiter struct {
	store Store

	mu      sync.Mutex
	known   map[string]Known   // by scope, from the snapshot
	cache   map[string][]Event // by scope, the counter as last read
	running map[string]*Ticket // this gateway's calls the provider may still be working on
	pending map[string][]Event // by scope, increments held while the store is down
	down    bool
	waived  map[string]bool // calls admitted while the store was down
}

// New returns a gateway's limiter over the site store.
func New(store Store) *Limiter {
	return &Limiter{store: store, known: map[string]Known{}, cache: map[string][]Event{},
		running: map[string]*Ticket{}, pending: map[string][]Event{}, waived: map[string]bool{}}
}

// Ticket is an admitted call. Exactly one of Complete or Cancel is called;
// after Cancel, Ended when the upstream has ended.
type Ticket struct {
	l    *Limiter
	call Call
	done bool
}

// Admit checks a call and takes its slots. While the store is reachable the
// limits are the site's; while it is not, this gateway's own counts, and the
// call is a waiver of the bound (ADR 0041 §7.3).
func (l *Limiter) Admit(ctx context.Context, c Call) (*Ticket, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.down {
		if err := l.refresh(ctx, c.Scope); err != nil {
			l.down = true
		}
	}
	known := l.knownLocked(c.Scope)
	if c.Strict {
		if known+c.Worst > c.Budget {
			return nil, ErrBudget
		}
	} else if known >= c.Budget {
		return nil, ErrBudget
	}
	scopeLimit := c.ScopeLimit
	if !l.down {
		ok, err := l.store.Acquire(ctx, Slot{c.ID, c.Principal, c.Scope}, c.PrincipalLimit, scopeLimit)
		switch {
		case err == nil && !ok:
			return nil, ErrConcurrency
		case err == nil:
			return l.track(c), nil
		}
		l.down = true // and fall through to the local check
	}
	byPrincipal, byScope := 0, 0
	for _, t := range l.running {
		if t.call.Principal == c.Principal {
			byPrincipal++
		}
		if t.call.Scope == c.Scope {
			byScope++
		}
	}
	if byPrincipal >= c.PrincipalLimit || (scopeLimit > 0 && byScope >= scopeLimit) {
		return nil, ErrConcurrency
	}
	l.waived[c.ID] = true
	return l.track(c), nil
}

func (l *Limiter) track(c Call) *Ticket {
	t := &Ticket{l: l, call: c}
	l.running[c.ID] = t
	return t
}

// Complete records the call's cost and frees its slot.
func (t *Ticket) Complete(ctx context.Context, cost int64) {
	l := t.l
	l.mu.Lock()
	defer l.mu.Unlock()
	l.emit(ctx, t.call.Scope, Event{t.call.ID, cost})
	l.release(ctx, t)
}

// Cancel records what the gateway saw of a call the client abandoned. The
// slot is kept until Ended: the provider may still be working (and billing)
// on it, and freeing the slot would let cancel-and-resend escape the
// in-flight limit (ADR 0041 §7.6).
func (t *Ticket) Cancel(ctx context.Context, seen int64) {
	l := t.l
	l.mu.Lock()
	defer l.mu.Unlock()
	l.emit(ctx, t.call.Scope, Event{t.call.ID, seen})
}

// Ended frees a cancelled call's slot once its upstream has ended (the
// connection closed, or the deployment's maximum duration passed).
func (t *Ticket) Ended(ctx context.Context) {
	l := t.l
	l.mu.Lock()
	defer l.mu.Unlock()
	l.release(ctx, t)
}

func (l *Limiter) emit(ctx context.Context, scope string, e Event) {
	if !l.down {
		if err := l.store.Add(ctx, scope, e); err == nil {
			return
		}
		l.down = true
	}
	l.pending[scope] = append(l.pending[scope], e) // replayed by Recover
}

func (l *Limiter) release(ctx context.Context, t *Ticket) {
	if t.done {
		return
	}
	t.done = true
	delete(l.running, t.call.ID)
	if !l.down {
		if err := l.store.Release(ctx, t.call.ID); err != nil {
			l.down = true // the slot's TTL frees it
		}
	}
}

// ApplySnapshot takes a snapshot's known spend for a scope, and drops the
// events it includes from the counter (best effort: known spend ignores
// them either way).
func (l *Limiter) ApplySnapshot(ctx context.Context, scope string, k Known) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.known[scope] = k
	if !l.down {
		var ids []string
		for _, e := range l.cache[scope] {
			if k.Included[e.ID] {
				ids = append(ids, e.ID)
			}
		}
		if len(ids) > 0 && l.store.Forget(ctx, scope, ids) != nil {
			l.down = true
		}
	}
}

// Recover is called when the store answers again: the held increments are
// replayed (harmless if repeated) and every call still running re-registers
// its slot, so calls that streamed through the outage still count.
func (l *Limiter) Recover(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, scope := range slices.Sorted(keys(l.pending)) {
		if err := l.store.Add(ctx, scope, l.pending[scope]...); err != nil {
			return err
		}
	}
	var slots []Slot
	for _, t := range l.running {
		slots = append(slots, Slot{t.call.ID, t.call.Principal, t.call.Scope})
	}
	if err := l.store.Reacquire(ctx, slots); err != nil {
		return err
	}
	clear(l.pending)
	l.down = false
	return nil
}

// Known is a scope's known spend now: the snapshot's, plus the counter's
// events the snapshot does not include, read afresh when the store answers
// (else from the last read). It does not change what admission decides
// from: only an admission's own read does, as in model/exposure.md.
func (l *Limiter) Known(ctx context.Context, scope string) int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	es := l.cache[scope]
	if !l.down {
		if fresh, err := l.store.Events(ctx, scope); err == nil {
			es = fresh
		}
	}
	return l.sum(scope, es)
}

func (l *Limiter) refresh(ctx context.Context, scope string) error {
	es, err := l.store.Events(ctx, scope)
	if err == nil {
		l.cache[scope] = es
	}
	return err
}

func (l *Limiter) knownLocked(scope string) int64 { return l.sum(scope, l.cache[scope]) }

func (l *Limiter) sum(scope string, events []Event) int64 {
	k := l.known[scope]
	sum := k.Spend
	for _, e := range events {
		if !k.Included[e.ID] {
			sum += e.Cost
		}
	}
	return sum
}

// Waived lists the calls admitted while the store was unreachable: the
// exposure the bound does not cover, to be reported (ADR 0041 §7.3).
func (l *Limiter) Waived() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Sorted(keys(l.waived))
}

// Pending lists the increments held for replay, by scope (tests, status).
func (l *Limiter) Pending(scope string) []Event {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.pending[scope])
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
