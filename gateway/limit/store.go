// Package limit is the gateway's admission check. It checks the snapshot's
// known spend against the scope's budget, the in-flight limits per
// principal and per scope, and the rate limits (rate.go). Its shared state
// lives in the site's store, Valkey in production. model/exposure.md shows
// why each rule is needed, and a connect test checks the code against it.
//
// Known spend comes from the snapshot alone. It is the ledger's spend for
// the scope's period, which golem-control publishes about every minute.
// The site store keeps slots and rate buckets, never spend. ADR 0031.
package limit

import (
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// Slot is a call holding a concurrency slot.
type Slot struct {
	Call, Principal, Scope string
}

// ErrUnavailable means the store cannot be reached.
var ErrUnavailable = errors.New("limit: the site store is unreachable")

// Admission is one admission's check, which the store makes atomically. It
// checks the scope's known spend against its budget, then takes the slot.
type Admission struct {
	Slot
	KnownSpend     int64 // the snapshot's known spend for the call's period
	Budget         int64 // budget to refuse at; 0 skips the budget check (observe and warn modes)
	PrincipalLimit int   // the principal's in-flight limit in the scope; 0 is none
	ScopeLimit     int   // the scope's in-flight limit; 0 is none
}

// Outcome is an admission's result.
type Outcome int

const (
	Admitted Outcome = iota
	OverBudget
	OverConcurrency
)

// overBudget is admission's budget check. Mem, the admit script, an
// outage's local check and a ticket's over-budget mark all use it. Known
// spend must be below the budget, whatever the call then costs. A budget
// of 0 is none. ADR 0031.
//
//quint:enforces exposure.withinBound
func overBudget(spend, budget int64) bool { return budget != 0 && spend >= budget }

// atLimit reports whether n calls in flight fill an in-flight limit. A
// limit of 0 is none.
func atLimit(n, limit int) bool { return limit > 0 && n >= limit }

// Store is the site's shared state, the concurrency slots and the rate
// buckets. Each method is one atomic operation and one round trip.
type Store interface {
	// Admit checks known spend and the limits and, if both allow, takes
	// the slot.
	Admit(ctx context.Context, a Admission) (Outcome, error)
	// Release frees a call's slot.
	Release(ctx context.Context, s Slot) error
	// Reacquire registers or renews the slots of calls still running. The
	// limiter calls it every 30 seconds, and after an outage in which the
	// slots' TTLs lapsed. It checks no limits, since the calls already run.
	Reacquire(ctx context.Context, slots []Slot) error
	// Take takes a request from every rate bucket of ms or from none
	// (rate.go). It reports the first bucket without room and the wait
	// until it has some.
	Take(ctx context.Context, ms []Meter) (ok bool, over int, wait time.Duration, err error)
}

// Mem is an in-memory Store, for the standalone gateway and the tests. Its
// fault models Valkey's. SetDown makes it unreachable and loses every
// slot, as an outage longer than the slots' TTL would.
type Mem struct {
	mu    sync.Mutex
	down  bool
	slots map[string]Slot
	// Now is the rate buckets' clock.
	Now func() time.Time

	buckets map[string]*rateBucket // rate buckets (rate.go)
}

// NewMem returns an empty store.
func NewMem() *Mem {
	return &Mem{slots: map[string]Slot{}, buckets: map[string]*rateBucket{}, Now: time.Now}
}

//quint:enforces exposure.limitsHonoured
func (m *Mem) Admit(_ context.Context, a Admission) (Outcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return 0, ErrUnavailable
	}
	if overBudget(a.KnownSpend, a.Budget) {
		return OverBudget, nil
	}
	if !m.acquire(a.Slot, a.PrincipalLimit, a.ScopeLimit) {
		return OverConcurrency, nil
	}
	return Admitted, nil
}

// acquire takes a slot if the principal's and the scope's limits allow.
// The caller holds mu.
func (m *Mem) acquire(s Slot, principalLimit, scopeLimit int) bool {
	byPrincipal, byScope := 0, 0
	for _, o := range m.slots {
		if o.Principal == s.Principal && o.Scope == s.Scope { // a principal's limit is per scope
			byPrincipal++
		}
		if o.Scope == s.Scope {
			byScope++
		}
	}
	if atLimit(byPrincipal, principalLimit) || atLimit(byScope, scopeLimit) {
		return false
	}
	m.slots[s.Call] = s
	return true
}

func (m *Mem) Release(_ context.Context, s Slot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return ErrUnavailable
	}
	delete(m.slots, s.Call)
	return nil
}

func (m *Mem) Reacquire(_ context.Context, slots []Slot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return ErrUnavailable
	}
	for _, s := range slots {
		m.slots[s.Call] = s
	}
	return nil
}

// inProcess: Mem has no I/O, so the limiter sets it no deadline.
func (*Mem) inProcess() {}

// SetDown makes the store unreachable (true) or reachable again. Going down
// loses every slot: their TTLs lapse during the outage.
func (m *Mem) SetDown(down bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.down = down
	if down {
		clear(m.slots)
	}
}

// Slots lists the slots held, sorted by call (tests).
func (m *Mem) Slots() []Slot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.SortedFunc(maps.Values(m.slots), func(a, b Slot) int { return strings.Compare(a.Call, b.Call) })
}
