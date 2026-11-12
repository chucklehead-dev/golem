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
	"slices"
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

// Store is the site's shared state: concurrency slots and the spend
// counter. Each method is one atomic operation (one Valkey script).
type Store interface {
	// Acquire takes a slot for s if the principal holds fewer than
	// principalLimit and the scope fewer than scopeLimit (0: no limit). It
	// reports whether it did.
	Acquire(ctx context.Context, s Slot, principalLimit, scopeLimit int) (bool, error)
	// Release frees a call's slot.
	Release(ctx context.Context, s Slot) error
	// Reacquire registers (or renews) slots for calls still running: the
	// renewal every 30 seconds (ADR 0038), and after an outage in which
	// their TTLs lapsed. It does not check limits: the calls are already
	// running.
	Reacquire(ctx context.Context, slots []Slot) error
	// Add records a spend event and returns its number in the scope's
	// counter: the next one, or the one it already has (a replay).
	Add(ctx context.Context, scope string, e Event) (seq int64, err error)
	// After returns the total of the scope's events numbered above w. If
	// the counter no longer holds its total as of w (trimmed: older than
	// any live snapshot should be), it returns the whole total and
	// over is true: counting too much, never too little.
	After(ctx context.Context, scope string, w int64) (total int64, over bool, err error)
}

// Mem is an in-memory Store: the standalone gateway's, and the tests'. Its
// faults model Valkey's: SetDown makes it unreachable (and, as an outage
// longer than the slots' TTL would, loses every slot); Failover loses the
// counter's tail.
type Mem struct {
	mu      sync.Mutex
	down    bool
	slots   map[string]Slot
	counter map[string]*memCounter
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
	return &Mem{slots: map[string]Slot{}, counter: map[string]*memCounter{}, Retain: 5 * time.Hour, Now: time.Now}
}

func (m *Mem) Acquire(_ context.Context, s Slot, principalLimit, scopeLimit int) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return false, ErrUnavailable
	}
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
		return false, nil
	}
	m.slots[s.Call] = s
	return true, nil
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

func (m *Mem) scope(scope string) *memCounter {
	c := m.counter[scope]
	if c == nil {
		c = &memCounter{cum: []int64{0}, byID: map[string]int64{}}
		m.counter[scope] = c
	}
	return c
}

func (m *Mem) Add(_ context.Context, scope string, e Event) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return 0, ErrUnavailable
	}
	c := m.scope(scope)
	if seq, ok := c.byID[e.ID]; ok {
		return seq, nil
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
	return seq, nil
}

func (m *Mem) After(_ context.Context, scope string, w int64) (int64, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return 0, false, ErrUnavailable
	}
	c := m.scope(scope)
	total := c.cum[len(c.cum)-1]
	switch {
	case w >= int64(len(c.events)):
		return 0, false, nil // nothing after w (or the counter lost its tail)
	case w < c.base:
		return total, true, nil
	}
	return total - c.cum[w], false, nil
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

// Failover loses a scope's counter after its first keep events: an
// asynchronous replica promoted without the latest writes. Their numbers
// will be given again.
func (m *Mem) Failover(scope string, keep int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := m.scope(scope)
	if keep >= len(c.events) {
		return
	}
	for _, e := range c.events[keep:] {
		delete(c.byID, e.ID)
	}
	c.events, c.cum = c.events[:keep], c.cum[:keep+1]
	c.base = min(c.base, int64(keep))
}

// Counter lists a scope's events in number order (tests).
func (m *Mem) Counter(scope string) []Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Event
	for _, e := range m.scope(scope).events {
		out = append(out, e.Event)
	}
	return out
}

// Slots lists the slots held, sorted by call (tests).
func (m *Mem) Slots() []Slot {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Slot
	for _, s := range m.slots {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Slot) int { return compare(a.Call, b.Call) })
	return out
}

func compare(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
