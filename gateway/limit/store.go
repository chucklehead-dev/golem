// Package limit is the gateway's admission check (ADR 0031, 0038, 0041
// §7.3 and §7.6, 0048): known spend against a budget, and in-flight limits
// per principal and per scope, over the site's shared store (Valkey in
// production). Its rules are the ones model/exposure.md shows necessary,
// and its connect test checks it against that model.
//
// Known spend is the snapshot's spend, the ledger's for calls that ended
// before the snapshot's boundary, plus the site counter's minutes from the
// boundary on (ADR 0048): one read of at most a retention's minutes,
// whatever the traffic.
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

// Increment is a call's cost for its scope's counter in its budget period,
// added to the minute the call ended in (At): the minute its ledger row
// names, so a snapshot's boundary puts the call on one side only. The
// counter keeps amounts, not events; Call is for logs and tests.
type Increment struct {
	Scope, Period string
	Call          string
	Cost          int64
	At            time.Time
}

// minuteOf is a time's minute: the counter's grain.
func minuteOf(t time.Time) int64 { return t.Unix() / 60 }

// fromMinute is the first minute admission counts for a snapshot's
// boundary: the boundary's own minute (golem-control places boundaries
// on minutes, so nothing in it is in the snapshot too; one off a minute
// counts that minute's earlier part twice, erring high). A zero boundary
// counts every minute retained.
func fromMinute(since time.Time) int64 {
	if since.IsZero() {
		return 0
	}
	return minuteOf(since)
}

// Admission is one admission's check, done atomically by the store: known
// spend (the snapshot's plus the counter's minutes from its boundary)
// against the budget, then the slots.
type Admission struct {
	Slot
	Period                 string    // the budget period, e.g. "2026-11": counters are per period
	Since                  time.Time // the snapshot's boundary, for this period (zero if it is of another)
	Spend                  int64     // the snapshot's known spend for this period
	Now                    time.Time // the gateway's clock: the read's last minute (zero: the store's)
	Budget                 int64     // 0: none
	DeclaredWorstCaseCheck bool
	Worst                  int64
	PrincipalLimit         int // 0: none
	ScopeLimit             int // 0: none
}

// Outcome is an admission's result.
type Outcome int

const (
	Admitted Outcome = iota
	OverBudget
	OverConcurrency
)

// overBudget is admission's budget check, as Mem and an outage's local
// check make it (the admit script makes the same in Lua). Under the
// declared-worst-case check known spend plus the call's declared worst
// case must fit the budget; under the soft check known spend must be below
// it, whatever the call then costs (ADR 0031, 0044). A budget of 0 is none.
func overBudget(known, budget int64, worstCaseCheck bool, worst int64) bool {
	switch {
	case budget == 0:
		return false
	case worstCaseCheck:
		return known+worst > budget
	default:
		return known >= budget
	}
}

// atLimit reports whether n calls in flight fill an in-flight limit (0:
// none).
func atLimit(n, limit int) bool { return limit > 0 && n >= limit }

// Store is the site's shared state: concurrency slots and the spend
// counter, per scope, budget period and minute. Each method is one atomic
// operation and one round trip (one Valkey script).
type Store interface {
	// Admit checks known spend and the limits and, if both allow, takes
	// the slot. It returns the outcome and the known spend it computed.
	Admit(ctx context.Context, a Admission) (o Outcome, known int64, err error)
	// Complete adds a call's increment and frees its slot: Add and Release
	// in one step.
	Complete(ctx context.Context, in Increment, s Slot) error
	// Release frees a call's slot.
	Release(ctx context.Context, s Slot) error
	// Reacquire registers (or renews) slots for calls still running: the
	// renewal every 30 seconds (ADR 0038), and after an outage in which
	// their TTLs lapsed. It does not check limits: the calls are already
	// running.
	Reacquire(ctx context.Context, slots []Slot) error
	// Add adds an increment (a cancellation's) to its minute.
	Add(ctx context.Context, in Increment) error
	// AddAll adds several held increments in one step, all or none: an
	// outage's replay. An increment is an amount, not an event: one added
	// twice (a replay whose reply was lost, sent again) is counted twice,
	// erring high.
	AddAll(ctx context.Context, incs []Increment) error
	// Since returns the period's spend in the minutes from since's on (for
	// reports; admission computes it inside Admit).
	Since(ctx context.Context, scope, period string, since time.Time) (int64, error)
}

// Mem is an in-memory Store: the standalone gateway's, and the tests'. Its
// faults model Valkey's: SetDown makes it unreachable (and, as an outage
// longer than the slots' TTL would, loses every slot); Failover loses a
// minute.
type Mem struct {
	mu      sync.Mutex
	down    bool
	slots   map[string]Slot
	counter map[keyOf]*memCounter
	// Retain is how long the counter keeps a minute, as Valkey's: at least
	// the longest authorization lease a live gateway can hold, plus the
	// ingestion allowance (ADR 0048: six hours). Now is its clock.
	Retain time.Duration
	Now    func() time.Time
}

// memCounter is a scope's counter for a period: its minutes, and its
// whole total, which a snapshot with no boundary reads (Valkey's "total"
// field).
type memCounter struct {
	minutes map[int64]int64
	total   int64
}

// NewMem returns an empty store.
func NewMem() *Mem {
	return &Mem{slots: map[string]Slot{}, counter: map[keyOf]*memCounter{}, Retain: 6 * time.Hour, Now: time.Now}
}

func (m *Mem) Admit(_ context.Context, a Admission) (Outcome, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return 0, 0, ErrUnavailable
	}
	known := a.Spend + m.since(a.Scope, a.Period, a.Since)
	if overBudget(known, a.Budget, a.DeclaredWorstCaseCheck, a.Worst) {
		return OverBudget, known, nil
	}
	if !m.acquire(a.Slot, a.PrincipalLimit, a.ScopeLimit) {
		return OverConcurrency, known, nil
	}
	return Admitted, known, nil
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

func (m *Mem) Complete(_ context.Context, in Increment, s Slot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return ErrUnavailable
	}
	m.add(in)
	delete(m.slots, s.Call)
	return nil
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

func (m *Mem) Add(_ context.Context, in Increment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return ErrUnavailable
	}
	m.add(in)
	return nil
}

func (m *Mem) AddAll(_ context.Context, incs []Increment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return ErrUnavailable
	}
	for _, in := range incs {
		m.add(in)
	}
	return nil
}

// add counts an increment in its minute and the total; mu is held.
func (m *Mem) add(in Increment) {
	k := keyOf{in.Scope, in.Period}
	c := m.counter[k]
	if c == nil {
		c = &memCounter{minutes: map[int64]int64{}}
		m.counter[k] = c
	}
	c.minutes[minuteOf(in.At)] += in.Cost
	c.total += in.Cost
	// Minutes past retention go, as Valkey's do.
	cutoff := minuteOf(m.Now().Add(-m.Retain))
	maps.DeleteFunc(c.minutes, func(minute, _ int64) bool { return minute < cutoff })
}

func (m *Mem) Since(_ context.Context, scope, period string, since time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return 0, ErrUnavailable
	}
	return m.since(scope, period, since), nil
}

// since is the counter's spend from since's minute on, in the window the
// Valkey script's since reads: no further back than the retention (a
// minute past it may not be trimmed yet) and no further ahead than a
// peer's clock skew. No boundary reads the whole total.
func (m *Mem) since(scope, period string, since time.Time) int64 {
	c := m.counter[keyOf{scope, period}]
	switch from := fromMinute(since); {
	case c == nil:
		return 0
	case from == 0:
		return c.total
	default:
		now := minuteOf(m.Now())
		lo, hi := max(from, now-int64(m.Retain/time.Minute)), now+clockSkewMinutes
		total := int64(0)
		for minute, spend := range c.minutes {
			if minute >= lo && minute <= hi {
				total += spend
			}
		}
		return total
	}
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

// Failover loses a minute of a scope's counter for a period: an
// asynchronous replica promoted without the latest writes.
func (m *Mem) Failover(scope, period string, minute int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.counter[keyOf{scope, period}]; c != nil {
		c.total -= c.minutes[minute]
		delete(c.minutes, minute)
	}
}

// Buckets is a scope's counter for a period, minute to spend (tests).
func (m *Mem) Buckets(scope, period string) map[int64]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.counter[keyOf{scope, period}]; c != nil {
		return maps.Clone(c.minutes)
	}
	return nil
}

// Slots lists the slots held, sorted by call (tests).
func (m *Mem) Slots() []Slot {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.SortedFunc(maps.Values(m.slots), func(a, b Slot) int { return strings.Compare(a.Call, b.Call) })
}
