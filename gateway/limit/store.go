// Package limit is the gateway's admission check (ADR 0031, 0038, 0041
// §7.3 and §7.6, 0052): the snapshot's known spend against the scope's
// budget, in-flight limits per principal and per scope, and
// requests-per-minute rate limits (rate.go), over the site's shared store
// (Valkey in production). Its rules are the ones model/exposure.md shows
// necessary, and its connect test checks it against that model.
//
// Known spend is the snapshot's alone (ADR 0052): the ledger's spend for
// the scope's period, which golem-control publishes about every minute.
// The site store keeps slots and rate buckets, no spend.
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

// ErrUnavailable is a store that cannot be reached.
var ErrUnavailable = errors.New("limit: the site store is unreachable")

// Admission is one admission's check, done atomically by the store: the
// scope's known spend against its budget, then the slots.
type Admission struct {
	Slot
	Known          int64 // the snapshot's known spend for the call's period
	Budget         int64 // the budget refused over; 0: none
	PrincipalLimit int   // 0: none
	ScopeLimit     int   // 0: none
}

// Outcome is an admission's result.
type Outcome int

const (
	Admitted Outcome = iota
	OverBudget
	OverConcurrency
)

// overBudget is admission's budget check, as Mem, the admit script, an
// outage's local check and a ticket's over-budget mark make it: known
// spend must be below the budget, whatever the call then costs (ADR 0031,
// 0044). A budget of 0 is none.
func overBudget(known, budget int64) bool { return budget != 0 && known >= budget }

// atLimit reports whether n calls in flight fill an in-flight limit (0:
// none).
func atLimit(n, limit int) bool { return limit > 0 && n >= limit }

// Store is the site's shared state: concurrency slots and the rate
// buckets. Each method is one atomic operation and one round trip.
type Store interface {
	// Admit checks known spend and the limits and, if both allow, takes
	// the slot.
	Admit(ctx context.Context, a Admission) (Outcome, error)
	// Release frees a call's slot.
	Release(ctx context.Context, s Slot) error
	// Reacquire registers (or renews) slots for calls still running: the
	// renewal every 30 seconds (ADR 0038), and after an outage in which
	// their TTLs lapsed. It does not check limits: the calls are already
	// running.
	Reacquire(ctx context.Context, slots []Slot) error
	// Take takes a request from every rate bucket of ms or from none
	// (rate.go), reporting the first without room and the wait until it
	// has.
	Take(ctx context.Context, ms []Meter) (ok bool, over int, wait time.Duration, err error)
}

// Mem is an in-memory Store: the standalone gateway's, and the tests'. Its
// fault models Valkey's: SetDown makes it unreachable (and, as an outage
// longer than the slots' TTL would, loses every slot).
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

func (m *Mem) Admit(_ context.Context, a Admission) (Outcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return 0, ErrUnavailable
	}
	if overBudget(a.Known, a.Budget) {
		return OverBudget, nil
	}
	if !m.acquire(a.Slot, a.PrincipalLimit, a.ScopeLimit) {
		return OverConcurrency, nil
	}
	return Admitted, nil
}

// acquire takes a slot if the principal's and the scope's limits allow;
// mu is held.
func (m *Mem) acquire(s Slot, principalLimit, scopeLimit int) bool {
	byPrincipal, byScope := 0, 0
	for _, o := range m.slots {
		if o.Principal == s.Principal {
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
