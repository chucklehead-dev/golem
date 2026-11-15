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

// Increment is a call's cost for its accounts' counters in its budget
// period (its scope's, and its member's and key's where they are capped),
// added to the minute the call ended in (At): the minute its ledger row
// names, so a snapshot's boundary puts the call on one side only. The
// counters keep amounts, not events; Call is for logs and tests. One
// increment is one write: every account's counter or none.
type Increment struct {
	Accounts []string // Account.ID's; the scope's first
	Period   string
	Call     string
	Cost     int64
	At       time.Time
}

// MaxAccounts is the most accounts a call is charged to: its key, its
// member and its scope (org-model.md §4.2).
const MaxAccounts = 3

// MemberAccount and KeyAccount are the IDs of a member's account in a
// scope and of a key's (by its display ID, the ledger's key_id). A scope's
// account is its ID, which has no colon.
func MemberAccount(scope, principal string) string { return "m:" + scope + "/" + principal }
func KeyAccount(display string) string             { return "k:" + display }

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

// Account is one account a call is charged to, as an admission checks it:
// its known spend (the snapshot's plus the counter's minutes from its
// boundary) against its limit.
type Account struct {
	ID    string
	Since time.Time // the snapshot's boundary, for this period (zero if it is of another)
	Spend int64     // the snapshot's known spend for this period
	Limit int64     // the budget or cap refused over; 0: none
}

// Admission is one admission's check, done atomically by the store: each
// account's known spend against its limit, in order (the first over its
// limit refuses), then the slots.
type Admission struct {
	Slot
	Period                 string    // the budget period, e.g. "2026-11": counters are per period
	Now                    time.Time // the gateway's clock: the read's last minute (zero: the store's)
	Accounts               []Account // at most MaxAccounts; the scope's first
	DeclaredWorstCaseCheck bool
	Worst                  int64
	PrincipalLimit         int // 0: none
	ScopeLimit             int // 0: none
}

// Outcome is an admission's result.
type Outcome int

const (
	Admitted   Outcome = iota
	OverBudget         // an account's limit: Result.Over says which
	OverConcurrency
)

// Result is an admission's outcome, the account it was refused over
// (OverBudget), and each account's known spend as the store computed it,
// in Admission.Accounts' order.
type Result struct {
	Outcome Outcome
	Over    int
	Known   [MaxAccounts]int64
}

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
	// the slot. It returns the outcome and the known spends it computed.
	Admit(ctx context.Context, a Admission) (Result, error)
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
	// Since returns an account's spend for the period in the minutes from
	// since's on (for reports; admission computes it inside Admit).
	Since(ctx context.Context, account, period string, since time.Time) (int64, error)
}

// Mem is an in-memory Store: the standalone gateway's, and the tests'. Its
// faults model Valkey's: SetDown makes it unreachable (and, as an outage
// longer than the slots' TTL would, loses every slot); failover loses a
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

func (m *Mem) Admit(_ context.Context, a Admission) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return Result{}, ErrUnavailable
	}
	var r Result
	for i, acc := range a.Accounts {
		r.Known[i] = m.known(acc.ID, a.Period, acc.Spend, acc.Since)
	}
	for i, acc := range a.Accounts {
		if overBudget(r.Known[i], acc.Limit, a.DeclaredWorstCaseCheck, a.Worst) {
			r.Outcome, r.Over = OverBudget, i
			return r, nil
		}
	}
	if !m.acquire(a.Slot, a.PrincipalLimit, a.ScopeLimit) {
		r.Outcome = OverConcurrency
	}
	return r, nil
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

// add counts an increment in its minute and the total of each of its
// accounts; mu is held.
func (m *Mem) add(in Increment) {
	cutoff := minuteOf(m.Now().Add(-m.Retain))
	for _, acc := range in.Accounts {
		k := keyOf{acc, in.Period}
		c := m.counter[k]
		if c == nil {
			c = &memCounter{minutes: map[int64]int64{}}
			m.counter[k] = c
		}
		c.minutes[minuteOf(in.At)] += in.Cost
		c.total += in.Cost
		// Minutes past retention go, as Valkey's do.
		maps.DeleteFunc(c.minutes, func(minute, _ int64) bool { return minute < cutoff })
	}
}

func (m *Mem) Since(_ context.Context, account, period string, since time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.down {
		return 0, ErrUnavailable
	}
	return m.since(account, period, since), nil
}

// since is the counter's spend from since's minute on, in the window the
// Valkey script's since reads: no further back than the retention (a
// minute past it may not be trimmed yet) and no further ahead than a
// peer's clock skew. No boundary reads the whole total.
func (m *Mem) since(account, period string, since time.Time) int64 {
	c := m.counter[keyOf{account, period}]
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

// known is an account's known spend for a snapshot's spend and boundary: the
// spend plus the counter's minutes from the boundary on, and never less
// than the counter's whole total for the period (the admit script's). A
// boundary older than the retention reads only the minutes retained, so
// the spend between it and the retention's start would be lost (a
// known-spend job that cannot read the ledger for hours keeps publishing
// its last boundary); the site's own total still counts. mu is held.
func (m *Mem) known(account, period string, spend int64, since time.Time) int64 {
	known := spend + m.since(account, period, since)
	if c := m.counter[keyOf{account, period}]; c != nil {
		known = max(known, c.total)
	}
	return known
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

// failover loses a minute of every counter for a period: an asynchronous
// replica promoted without the latest writes, which a call's increment
// reaches every account of at once.
func (m *Mem) failover(period string, minute int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, c := range m.counter {
		if k.period == period {
			c.total -= c.minutes[minute]
			delete(c.minutes, minute)
		}
	}
}

// Buckets is an account's counter for a period, minute to spend (tests).
func (m *Mem) Buckets(account, period string) map[int64]int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c := m.counter[keyOf{account, period}]; c != nil {
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
