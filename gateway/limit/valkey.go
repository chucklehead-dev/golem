package limit

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/valkey-io/valkey-go"
)

// Valkey is the site store, one Valkey shared by the site's gateways. Each
// operation is one Lua script or pipeline, so it is atomic. Every key
// carries the hash tag {golem:<site>}, so in cluster mode all keys share
// the hash slot the scripts need.
//
// Slots are sorted sets per principal in a scope and per scope. They are
// scored by their expiry on the server's clock (TIME), so a gateway's
// clock cannot extend one. The store keeps no spend. ADR 0031.
type Valkey struct {
	// cl is the client, nil until DialValkey's dial has made one. Until
	// then every operation fails as ErrUnavailable, an outage to the limiter.
	cl     atomic.Pointer[valkey.Client]
	dial   func() (valkey.Client, error)
	prefix string
	// TTL is how long a slot lives without renewal (default 90 seconds).
	TTL time.Duration
	// Log is where Connect logs its dials; nil logs to slog.Default().
	Log *slog.Logger

	afterFn func(time.Duration) <-chan time.Time // Connect's waits (tests)

	ttlArg atomic.Pointer[ttlArg]
}

// ttlArg is a slot's life as the scripts take it, formatted once for the
// TTL it was formatted from: an admission builds no string for it.
type ttlArg struct {
	ttl time.Duration
	ms  string
}

func (v *Valkey) ttlMs() string {
	if d := v.ttlArg.Load(); d != nil && d.ttl == v.TTL {
		return d.ms
	}
	d := &ttlArg{v.TTL, ms(v.TTL)}
	v.ttlArg.Store(d)
	return d.ms
}

// NewValkey returns the store for site over a client.
func NewValkey(c valkey.Client, site string) *Valkey {
	v := siteStore(site)
	v.cl.Store(&c)
	return v
}

// siteStore is the store for site with its defaults, and no client.
func siteStore(site string) *Valkey {
	return &Valkey{prefix: "{golem:" + site + "}:", TTL: 90 * time.Second}
}

// DialValkey returns the store for site over a client that dial makes. It
// dials once now. If that fails, it still returns the store, whose every
// operation fails as ErrUnavailable. The limiter then admits as in an
// outage until Connect makes a client and Recover finds the store.
//
// A gateway that refused to start without Valkey would turn an outage plus
// pod churn into lost capacity, where the limiter should fail open. The
// error is the first dial's, for the caller to log. The store is never
// nil. ADR 0041.
func DialValkey(site string, dial func() (valkey.Client, error)) (*Valkey, error) {
	v := siteStore(site)
	v.dial = dial
	c, err := dial()
	if err != nil {
		return v, err
	}
	v.cl.Store(&c)
	return v, nil
}

// errNotConnected is every operation's error before the first client:
// built once, so a call refused for it allocates nothing.
var errNotConnected = fmt.Errorf("%w: not connected yet (the gateway retries in the background; check the Valkey address, the network policy, and -valkey-tls, -valkey-user and VALKEY_PASSWORD)", ErrUnavailable)

// client is the store's client, or errNotConnected before the first.
func (v *Valkey) client() (valkey.Client, error) {
	if c := v.cl.Load(); c != nil {
		return *c, nil
	}
	return nil, errNotConnected
}

// Connected reports whether the store has a client (its calls may still
// fail).
func (v *Valkey) Connected() bool { return v.cl.Load() != nil }

// Connect retries DialValkey's dial until it makes a client or ctx ends,
// a second after the last failure, doubling to 30 seconds. It returns at
// once for a store that has a client.
func (v *Valkey) Connect(ctx context.Context) {
	wait := time.Second
	for !v.Connected() && v.dial != nil {
		select {
		case <-ctx.Done():
			return
		case <-v.after(wait):
		}
		c, err := v.dial()
		if err != nil {
			wait = min(2*wait, 30*time.Second)
			v.log().Warn("limit: Valkey still unreachable; admitting on local counts, recorded as waivers", "retry_in", wait, "err", err)
			continue
		}
		if ctx.Err() != nil {
			c.Close() // made as the gateway stopped: nothing else would close it
			return
		}
		v.cl.Store(&c)
		v.log().Info("limit: connected to Valkey; the limiter recovers at its next renewal")
	}
}

func (v *Valkey) log() *slog.Logger { return cmp.Or(v.Log, slog.Default()) }

// after is time.After, or a test's.
func (v *Valkey) after(d time.Duration) <-chan time.Time {
	if v.afterFn != nil {
		return v.afterFn(d)
	}
	return time.After(d)
}

// Close closes the client, if there is one.
func (v *Valkey) Close() {
	if c := v.cl.Load(); c != nil {
		(*c).Close()
	}
}

// Ping checks the store answers. Limiter.Recover asks it before it marks
// the store up: a gateway with nothing to renew would otherwise
// find an unreachable store answering.
func (v *Valkey) Ping(ctx context.Context) error {
	c, err := v.client()
	if err != nil {
		return err
	}
	return wrap(c.Do(ctx, c.B().Ping().Build()).Error())
}

// The scripts are built from the Lua fragments below. Each script's
// comment lists its KEYS and ARGV; numbers travel as decimal strings.

// nowMillis is the server's clock (TIME) in milliseconds: slots expire on
// it, so no gateway's clock can extend one.
const nowMillis = `local t = redis.call('TIME'); local nowMs = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
`

// slotsTake is the Lua function take: it trims expired slots from a
// principal's and a scope's slot sets and, if the limits allow (0: none),
// takes one in each for the call, for the TTL.
const slotsTake = `
local function take(principalSlots, scopeSlots, call, principalLimit, scopeLimit, ttl)
  redis.call('ZREMRANGEBYSCORE', principalSlots, '-inf', nowMs)
  redis.call('ZREMRANGEBYSCORE', scopeSlots, '-inf', nowMs)
  if principalLimit > 0 and redis.call('ZCARD', principalSlots) >= principalLimit then return false end
  if scopeLimit > 0 and redis.call('ZCARD', scopeSlots) >= scopeLimit then return false end
  redis.call('ZADD', principalSlots, nowMs + ttl, call)
  redis.call('ZADD', scopeSlots, nowMs + ttl, call)
  redis.call('PEXPIRE', principalSlots, ttl)
  redis.call('PEXPIRE', scopeSlots, ttl)
  return true
end
`

// admitScript is one admission: the budget check and the slots in one
// atomic step, so two gateways cannot both take the last slot. The check
// is overBudget's, on the snapshot's known spend the gateway sends.
//
//	KEYS: principal slots, scope slots
//	ARGV: call, principal limit, scope limit, TTL (ms), known spend, budget
//
// It returns an Outcome.
//
//quint:enforces exposure.limitsHonoured
var admitScript = valkey.NewLuaScript(nowMillis + slotsTake + `
local principalSlots, scopeSlots = KEYS[1], KEYS[2]
local call, principalLimit, scopeLimit, ttl = ARGV[1], tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4])
local known, budget = tonumber(ARGV[5]), tonumber(ARGV[6])
if budget ~= 0 and known >= budget then return 1 end -- 0 is none; a negative one refuses, as overBudget does
if not take(principalSlots, scopeSlots, call, principalLimit, scopeLimit, ttl) then return 2 end
return 0`)

// registerScript registers (or renews) slots without checking limits.
//
//	KEYS: principal slots, scope slots, per call
//	ARGV: TTL (ms), then the calls
var registerScript = valkey.NewLuaScript(nowMillis + `
local ttl = ARGV[1]
local expiry = nowMs + tonumber(ttl)
for i = 1, #ARGV - 1 do
  local call, principalSlots, scopeSlots = ARGV[i+1], KEYS[2*i-1], KEYS[2*i]
  redis.call('ZADD', principalSlots, expiry, call)
  redis.call('ZADD', scopeSlots, expiry, call)
  redis.call('PEXPIRE', principalSlots, ttl)
  redis.call('PEXPIRE', scopeSlots, ttl)
end
return 1`)

// principalKey is a principal's slots in one scope: its limit is the
// scope's, so its calls elsewhere count against neither. The scope is
// length-prefixed, so no scope and principal IDs (either may hold a colon)
// name another pair's key.
func (v *Valkey) principalKey(scope, p string) string {
	return v.prefix + "slots:p:" + strconv.Itoa(len(scope)) + ":" + scope + ":" + p
}
func (v *Valkey) scopeKey(s string) string { return v.prefix + "slots:s:" + s }

func ms(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }

func i64(n int64) string { return strconv.FormatInt(n, 10) }

func (v *Valkey) Admit(ctx context.Context, a Admission) (Outcome, error) {
	c, err := v.client()
	if err != nil {
		return 0, err
	}
	o, err := admitScript.Exec(ctx, c, []string{v.principalKey(a.Scope, a.Principal), v.scopeKey(a.Scope)},
		[]string{a.Call, strconv.Itoa(a.PrincipalLimit), strconv.Itoa(a.ScopeLimit), v.ttlMs(), i64(a.KnownSpend), i64(a.Budget)}).AsInt64()
	if err != nil {
		return 0, wrap(err)
	}
	return Outcome(o), nil
}

func (v *Valkey) Release(ctx context.Context, s Slot) error {
	c, err := v.client()
	if err != nil {
		return err
	}
	cmds := valkey.Commands{
		c.B().Zrem().Key(v.principalKey(s.Scope, s.Principal)).Member(s.Call).Build(),
		c.B().Zrem().Key(v.scopeKey(s.Scope)).Member(s.Call).Build(),
	}
	for _, r := range c.DoMulti(ctx, cmds...) {
		if err := r.Error(); err != nil {
			return wrap(err)
		}
	}
	return nil
}

func (v *Valkey) Reacquire(ctx context.Context, slots []Slot) error {
	if len(slots) == 0 {
		return nil
	}
	c, err := v.client()
	if err != nil {
		return err
	}
	// In chunks: one script holds Valkey's only thread for its whole run,
	// and a gateway with 10,000 calls in flight would stall every gateway
	// of the site at each renewal.
	ttl := v.ttlMs()
	for chunk := range slices.Chunk(slots, renewChunk) {
		keys := make([]string, 0, 2*len(chunk))
		args := append(make([]string, 0, 1+len(chunk)), ttl)
		for _, s := range chunk {
			keys = append(keys, v.principalKey(s.Scope, s.Principal), v.scopeKey(s.Scope))
			args = append(args, s.Call)
		}
		if err := registerScript.Exec(ctx, c, keys, args).Error(); err != nil {
			return wrap(err)
		}
	}
	return nil
}

// renewChunk is the slots one renewal script registers: about a
// millisecond of Valkey's time.
const renewChunk = 256

// ScopeSlots lists the calls holding a scope's slots (status, tests).
func (v *Valkey) ScopeSlots(ctx context.Context, scope string) ([]string, error) {
	c, err := v.client()
	if err != nil {
		return nil, err
	}
	r, err := c.Do(ctx, c.B().Zrange().Key(v.scopeKey(scope)).Min("0").Max("-1").Build()).AsStrSlice()
	return r, wrap(err)
}

// wrap marks a connection failure as ErrUnavailable; a script error stays
// itself (a bug, not an outage).
func wrap(err error) error {
	if err == nil || valkey.IsValkeyNil(err) {
		return nil
	}
	if _, ok := valkey.IsValkeyErr(err); ok {
		return err
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, err) // both: a caller can tell a timeout from a refusal
}
