// Package limit is the gateway's admission check (ADR 0031, 0038, 0041
// §7.3 and §7.6, 0042): known spend against a budget, and in-flight limits
// per principal and per scope, over the site's shared store (Valkey in
// production). Its rules are the ones model/exposure.md shows necessary,
// and its connect test checks it against that model.
//
// Known spend is the snapshot's spend plus the site counter's total after
// the snapshot's watermark (ADR 0042): one O(1) read, whatever the traffic.
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

// Event is a spend event: a call's cost as the gateway knows it, keyed by
// the call, so counting it twice is impossible.
type Event struct {
	ID   string
	Cost int64
}

// Slot is a call holding a concurrency slot.
type Slot struct {
	Call, Principal, Scope string
}

// ErrUnavailable is a store that cannot be reached.
var ErrUnavailable = errors.New("limit: the site store is unreachable")

// Admission is one admission's check, done atomically by the store: known
// spend (the snapshot's plus the counter's total after the snapshot's
// watermark) against the budget, then the slots.
type Admission struct {
	Slot
	Period         string // the budget period, e.g. "2026-11": counters are per period
	Watermark      int64  // the snapshot's, for this period (0 if it is of another)
	Spend          int64  // the snapshot's known spend for this period
	Budget         int64  // 0: none
	Strict         bool
	Worst          int64
	PrincipalLimit int // 0: none
	ScopeLimit     int // 0: none
}

// Outcome is an admission's result.
type Outcome int

const (
	Admitted Outcome = iota
	OverBudget
	OverConcurrency
)

// Store is the site's shared state: concurrency slots and the spend
// counter, per scope and budget period. Each method is one atomic operation
// and one round trip (one Valkey script).
type Store interface {
	// Admit checks known spend and the limits and, if both allow, takes
	// the slot. It returns the outcome and the known spend it computed;
	// over reports that the counter no longer held the total as of the
	// watermark, so all of it was counted (never too little).
	Admit(ctx context.Context, a Admission) (o Outcome, known int64, over bool, err error)
	// Complete records a call's event and frees its slot: Add and Release
	// in one step.
	Complete(ctx context.Context, period string, s Slot, e Event) (seq int64, err error)
	// Release frees a call's slot.
	Release(ctx context.Context, s Slot) error
	// Reacquire registers (or renews) slots for calls still running: the
	// renewal every 30 seconds (ADR 0038), and after an outage in which
	// their TTLs lapsed. It does not check limits: the calls are already
	// running.
	Reacquire(ctx context.Context, slots []Slot) error
	// Add records a spend event (a cancellation's, or a replay) and returns
	// its number in the scope's counter for the period: the next one, or
	// the one it already has.
	Add(ctx context.Context, scope, period string, e Event) (seq int64, err error)
	// After returns the total of the period's events numbered above w (for
	// reports; admission computes it inside Admit).
	After(ctx context.Context, scope, period string, w int64) (total int64, over bool, err error)
}

// Mem is an in-memory Store: the standalone gateway's, and the tests'. Its
// faults model Valkey's: SetDown makes it unreachable (and, as an outage
// longer than the slots' TTL would, loses every slot); Failover loses the
// counter's tail.
type Mem struct {
	mu      sync.Mutex
	down    bool
	slots   map[string]Slot
	counter map[keyOf]*memCounter
	// Retain is how long the counter keeps its history, as Valkey's: at
	// least the longest authorization lease a live gateway can hold
	// (ADR 0042: five hours). Now is its clock.
	Retain time.Duration
	Now    func() time.Time
}

type memCounter struct {
	events []memEvent // numbered from 1
	cum    []int64    // cum[i]: the total of events 1..i (cum[0] = 0)
	byID   map[string]int64
	base   int64 // numbers up to base have been trimmed
}

type memEvent struct {
	Event
	at time.Time
}

// NewMem returns an empty store.
func NewMem() *Mem {
	return &Mem{slots: map[string]Slot{}, counter: map[keyOf]*memCounter{}, Retain: 5 * time.Hour, Now: time.Now}
}

func (m *Mem) Admit(_ context.Context, a Admission) (Outcome, int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return 0, 0, false, ErrUnavailable
	}
	after, over := m.after(a.Scope, a.Period, a.Watermark)
	known := a.Spend + after
	switch {
	case a.Budget == 0:
	case a.Strict && known+a.Worst > a.Budget, !a.Strict && known >= a.Budget:
		return OverBudget, known, over, nil
	}
	if !m.acquire(a.Slot, a.PrincipalLimit, a.ScopeLimit) {
		return OverConcurrency, known, over, nil
	}
	return Admitted, known, over, nil
}

func (m *Mem) Complete(_ context.Context, period string, s Slot, e Event) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return 0, ErrUnavailable
	}
	seq := m.add(s.Scope, period, e)
	delete(m.slots, s.Call)
	return seq, nil
}

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
	if (principalLimit > 0 && byPrincipal >= principalLimit) || (scopeLimit > 0 && byScope >= scopeLimit) {
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

func (*Mem) inProcess() {}

func (m *Mem) counterOf(scope, period string) *memCounter {
	k := keyOf{scope, period}
	c := m.counter[k]
	if c == nil {
		c = &memCounter{cum: []int64{0}, byID: map[string]int64{}}
		m.counter[k] = c
	}
	return c
}

func (m *Mem) Add(_ context.Context, scope, period string, e Event) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return 0, ErrUnavailable
	}
	return m.add(scope, period, e), nil
}

func (m *Mem) add(scope, period string, e Event) int64 {
	c := m.counterOf(scope, period)
	if seq, ok := c.byID[e.ID]; ok {
		return seq
	}
	c.events = append(c.events, memEvent{e, m.Now()})
	c.cum = append(c.cum, c.cum[len(c.cum)-1]+e.Cost)
	seq := int64(len(c.events))
	c.byID[e.ID] = seq
	// Trim history past retention, oldest first.
	cutoff := m.Now().Add(-m.Retain)
	for c.base < seq-1 && c.events[c.base].at.Before(cutoff) {
		delete(c.byID, c.events[c.base].ID)
		c.base++
	}
	return seq
}

func (m *Mem) After(_ context.Context, scope, period string, w int64) (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return 0, false, ErrUnavailable
	}
	after, over := m.after(scope, period, w)
	return after, over, nil
}

func (m *Mem) after(scope, period string, w int64) (int64, bool) {
	c := m.counterOf(scope, period)
	total := c.cum[len(c.cum)-1]
	switch {
	case w >= int64(len(c.events)):
		return 0, false // nothing after w (or the counter lost its tail)
	case w < c.base:
		return total, true
	}
	return total - c.cum[w], false
}

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

// Failover loses a scope's counter for a period after its first keep
// events: an asynchronous replica promoted without the latest writes. Their
// numbers will be given again.
func (m *Mem) Failover(scope, period string, keep int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.counterOf(scope, period)
	if keep >= len(c.events) {
		return
	}
	for _, e := range c.events[keep:] {
		delete(c.byID, e.ID)
	}
	c.events, c.cum = c.events[:keep], c.cum[:keep+1]
	c.base = min(c.base, int64(keep))
}

// Counter lists a scope's events for a period in number order (tests).
func (m *Mem) Counter(scope, period string) []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Event
	for _, e := range m.counterOf(scope, period).events {
		out = append(out, e.Event)
	}
	return out
}

// Slots lists the slots held, sorted by call (tests).
func (m *Mem) Slots() []Slot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.SortedFunc(maps.Values(m.slots), func(a, b Slot) int { return strings.Compare(a.Call, b.Call) })
}
