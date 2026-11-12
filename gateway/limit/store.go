// Package limit is the gateway's admission check (ADR 0031, 0038, 0041
// §7.3 and §7.6): known spend against a budget, and in-flight limits per
// principal and per scope, over the site's shared store (Valkey in
// production). Its rules are the ones model/exposure.md shows necessary,
// and its connect test checks it against that model.
package limit

import (
	"context"
	"errors"
	"slices"
	"sync"
)

// Event is a spend event: a call's cost as the gateway knows it, keyed by
// the call so that counting it twice is impossible (a set, by event ID).
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
	// principalLimit and the scope fewer than scopeLimit (0: no scope
	// limit). It reports whether it did.
	Acquire(ctx context.Context, s Slot, principalLimit, scopeLimit int) (bool, error)
	// Release frees a call's slot.
	Release(ctx context.Context, call string) error
	// Reacquire registers slots for calls still running, after an outage
	// in which their TTLs lapsed. It does not check limits: the calls are
	// already running.
	Reacquire(ctx context.Context, slots []Slot) error
	// Add records spend events for a scope; adding one twice is harmless.
	Add(ctx context.Context, scope string, es ...Event) error
	// Events returns a scope's counter.
	Events(ctx context.Context, scope string) ([]Event, error)
	// Forget drops events a snapshot now includes (garbage collection;
	// known spend ignores them either way).
	Forget(ctx context.Context, scope string, ids []string) error
}

// Mem is an in-memory Store: the standalone gateway's, and the tests'. Its
// faults model Valkey's: Down makes it unreachable (and, as an outage
// longer than the slots' TTL would, loses every slot); Failover loses
// counter events.
type Mem struct {
	mu      sync.Mutex
	down    bool
	slots   map[string]Slot
	counter map[string]map[string]int64 // scope -> event ID -> cost
}

// NewMem returns an empty store.
func NewMem() *Mem { return &Mem{slots: map[string]Slot{}, counter: map[string]map[string]int64{}} }

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
	if byPrincipal >= principalLimit || (scopeLimit > 0 && byScope >= scopeLimit) {
		return false, nil
	}
	m.slots[s.Call] = s
	return true, nil
}

func (m *Mem) Release(_ context.Context, call string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return ErrUnavailable
	}
	delete(m.slots, call)
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

func (m *Mem) Add(_ context.Context, scope string, es ...Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return ErrUnavailable
	}
	c := m.counter[scope]
	if c == nil {
		c = map[string]int64{}
		m.counter[scope] = c
	}
	for _, e := range es {
		c[e.ID] = e.Cost
	}
	return nil
}

func (m *Mem) Events(_ context.Context, scope string) ([]Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return nil, ErrUnavailable
	}
	var es []Event
	for id, c := range m.counter[scope] {
		es = append(es, Event{id, c})
	}
	slices.SortFunc(es, func(a, b Event) int { return compare(a.ID, b.ID) })
	return es, nil
}

func (m *Mem) Forget(_ context.Context, scope string, ids []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return ErrUnavailable
	}
	for _, id := range ids {
		delete(m.counter[scope], id)
	}
	return nil
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

// Failover loses a scope's counter events except those kept.
func (m *Mem) Failover(scope string, keep func(id string) bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.counter[scope] {
		if !keep(id) {
			delete(m.counter[scope], id)
		}
	}
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
