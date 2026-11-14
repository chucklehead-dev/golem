package limit

import (
	"cmp"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/valkey-io/valkey-go"
)

// Valkey is the site store (ADR 0012, 0031, 0048): one Valkey (ElastiCache
// for Valkey, or the site's own) shared by the site's gateways. Each
// operation is one Lua script, so it is atomic. Every key carries the hash
// tag {golem:<site>}: in cluster mode they share a slot, as the scripts
// need.
//
// Slots are sorted sets per principal and per scope, scored by their expiry
// on the server's clock (TIME), so a gateway's clock cannot extend one.
//
// The counter, per scope and budget period, is one hash of minute to spend
// (ADR 0048): a completion is an HINCRBY of its minute, and known spend
// reads the minutes from the snapshot's boundary to now, one HGET each: a
// few for a gateway on a current snapshot (its boundary an ingestion
// allowance back), at most a retention's for one on the oldest. The hash's
// "total" field is the period's whole spend, read alone for a snapshot
// with no boundary (it counts every minute, retained or not). Minutes
// older than Retain are deleted by the adds once the hash holds more than
// a retention's. Retain must exceed the longest authorization lease a live
// gateway can hold (an internal pool's four hours, ADR 0032) plus the
// ingestion allowance, since a gateway partitioned from the bucket but not
// from its Valkey keeps an old snapshot and needs the minutes from its
// boundary on.
type Valkey struct {
	c      valkey.Client
	prefix string
	// TTL is a slot's life without renewal (ADR 0038: 90 seconds).
	TTL time.Duration
	// Retain is how long the counter keeps a minute (default 6 hours).
	Retain time.Duration
	// ReplicaAcks, if positive, makes each add wait for that many replicas
	// to acknowledge it (WAIT), so a failover cannot lose it before the
	// ledger has it (ADR 0048). An add is at completion, after
	// the response's last byte but before its handler returns, so the
	// caller's end of stream (HTTP/2's END_STREAM, the last chunk) waits
	// for it: a round trip, and with ReplicaAcks up to WAIT's 100 ms.
	ReplicaAcks int
	// Unreplicated counts adds that fewer than ReplicaAcks replicas
	// acknowledged in time: the exposure a failover could still lose.
	Unreplicated atomic.Int64

	keys   sync.Map // keyOf -> *scopeKeys, built once
	cached atomic.Int64
	args   atomic.Pointer[durArgs]
}

// durArgs are the scripts' duration arguments, formatted once for the
// durations they were formatted from.
type durArgs struct {
	ttl, retain time.Duration
	ttlMs       string
	lifeMs      string // past the longest month, plus retention
}

func (v *Valkey) durations() *durArgs {
	if d := v.args.Load(); d != nil && d.ttl == v.TTL && d.retain == v.Retain {
		return d
	}
	d := &durArgs{ttl: v.TTL, retain: v.Retain, ttlMs: ms(v.TTL), lifeMs: ms(31*24*time.Hour + v.Retain)}
	v.args.Store(d)
	return d
}

// NewValkey returns the store for site over a client.
func NewValkey(c valkey.Client, site string) *Valkey {
	return &Valkey{c: c, prefix: "{golem:" + site + "}:", TTL: 90 * time.Second, Retain: 6 * time.Hour}
}

// maxCachedKeys bounds the key cache: scopes times periods in use.
const maxCachedKeys = 1 << 14

// scopeKeys are a scope and period's keys. Built once and shared
// (read-only), so a call builds no strings for them.
type scopeKeys struct {
	counter string // the period counter: minute -> spend
	slots   string // the scope's slots
}

func (v *Valkey) keysOf(scope, period string) *scopeKeys {
	k := keyOf{scope, period}
	if ks, ok := v.keys.Load(k); ok {
		return ks.(*scopeKeys)
	}
	ks := &scopeKeys{counter: v.prefix + "spend:" + scope + ":" + period, slots: v.scopeKey(scope)}
	if v.cached.Add(1) > maxCachedKeys { // a new month, or many scopes: start again
		v.keys.Clear()
		v.cached.Store(1)
	}
	v.keys.Store(k, ks)
	return ks
}

// script is a Lua script with what a pipelined EVALSHA, or EVAL after
// NOSCRIPT, needs: Lua.Exec does that fallback itself, one command at a
// time.
type script struct {
	*valkey.Lua
	src, sha string
}

func newScript(src string) *script {
	sum := sha1.Sum([]byte(src))
	return &script{valkey.NewLuaScript(src), src, hex.EncodeToString(sum[:])}
}

const nowMillis = `local t = redis.call('TIME'); local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
`

// slotsTake is the Lua function take: it trims expired slots from a
// principal's and a scope's slot sets and, if the limits allow, takes one
// in each for the call, for the TTL. The admit script calls it.
const slotsTake = `
local function take(pk, sk, call, plimit, slimit, ttl)
  redis.call('ZREMRANGEBYSCORE', pk, '-inf', now)
  redis.call('ZREMRANGEBYSCORE', sk, '-inf', now)
  if plimit > 0 and redis.call('ZCARD', pk) >= plimit then return false end
  if slimit > 0 and redis.call('ZCARD', sk) >= slimit then return false end
  redis.call('ZADD', pk, now + ttl, call)
  redis.call('ZADD', sk, now + ttl, call)
  redis.call('PEXPIRE', pk, ttl)
  redis.call('PEXPIRE', sk, ttl)
  return true
end
`

// sinceOf is the Lua function since: the counter's spend in the minutes
// from from to now (the caller's minute) plus clockSkew, and no further
// back than the retention. It reads only those minutes: the cost is the
// boundary's age, not the counter's size.
const sinceOf = `
local function since(k, from, now, retain)
  if from == 0 then return tonumber(redis.call('HGET', k, 'total') or '0') end
  local lo = math.max(from, now - retain)
  local total = 0
  for m = lo, now + ` + clockSkewMinutes + ` do
    local v = redis.call('HGET', k, m)
    if v then total = total + tonumber(v) end
  end
  return total
end
`

// clockSkewMinutes is how far past the caller's minute a read looks: a
// minute stamped by a peer gateway whose clock is a little ahead.
const clockSkewMinutes = "1"

// trimOf is the Lua function trim: once a counter holds more than a
// retention's minutes (and a margin), delete those older than it, by the
// increment's minute. Amortised: it runs about once a margin's minutes.
const trimOf = `
local function trim(k, now, retain)
  if redis.call('HLEN', k) <= retain + 60 then return end
  for _, f in ipairs(redis.call('HKEYS', k)) do
    local m = tonumber(f) -- nil for "total"
    if m and m < now - retain then redis.call('HDEL', k, f) end
  end
end
`

// admit: KEYS principal slots, scope slots, counter; ARGV call, principal
// limit, scope limit, TTL, from minute, spend, budget, worst-case check (0
// or 1), worst, retention in minutes, the gateway's minute. Returns
// {outcome, known}: the check and the slot in one atomic step, so two
// gateways cannot both pass the check and then both take the last slot or
// the last of the budget.
var admitScript = valkey.NewLuaScript(nowMillis + slotsTake + sinceOf + `
local known = tonumber(ARGV[6]) + since(KEYS[3], tonumber(ARGV[5]), tonumber(ARGV[11]), tonumber(ARGV[10]))
local budget, worst = tonumber(ARGV[7]), tonumber(ARGV[9])
if budget > 0 then
  if ARGV[8] == '1' and known + worst > budget then return {1, known} end
  if ARGV[8] ~= '1' and known >= budget then return {1, known} end
end
if not take(KEYS[1], KEYS[2], ARGV[1], tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4])) then
  return {2, known}
end
return {0, known}`)

// register adds slots without checking limits: KEYS pairs (principal,
// scope), ARGV[1] the TTL, then the calls.
var registerScript = valkey.NewLuaScript(nowMillis + `
local exp = now + tonumber(ARGV[1])
for i = 2, #ARGV do
  redis.call('ZADD', KEYS[2*i-3], exp, ARGV[i])
  redis.call('ZADD', KEYS[2*i-2], exp, ARGV[i])
  redis.call('PEXPIRE', KEYS[2*i-3], ARGV[1])
  redis.call('PEXPIRE', KEYS[2*i-2], ARGV[1])
end
return 1`)

// addScript: KEYS counters, one per increment; ARGV the keys' life, the
// retention in minutes, then (minute, cost) per increment. A period's
// counter outlives its period by the retention, then goes. One script, so
// a batch is added whole or not at all.
var addScript = newScript(trimOf + `
local retain = tonumber(ARGV[2])
for i = 1, #KEYS do
  redis.call('HINCRBY', KEYS[i], ARGV[2*i+1], ARGV[2*i+2])
  redis.call('HINCRBY', KEYS[i], 'total', ARGV[2*i+2])
  redis.call('PEXPIRE', KEYS[i], ARGV[1])
  trim(KEYS[i], tonumber(ARGV[2*i+1]), retain)
end
return 1`)

// completeScript: the add, then free the slot: KEYS counter, principal and
// scope slots; ARGV life, minute, cost, call, retention in minutes.
var completeScript = newScript(trimOf + `
redis.call('HINCRBY', KEYS[1], ARGV[2], ARGV[3])
redis.call('HINCRBY', KEYS[1], 'total', ARGV[3])
redis.call('PEXPIRE', KEYS[1], ARGV[1])
trim(KEYS[1], tonumber(ARGV[2]), tonumber(ARGV[5]))
redis.call('ZREM', KEYS[2], ARGV[4])
redis.call('ZREM', KEYS[3], ARGV[4])
return 1`)

// sinceScript: KEYS counter; ARGV from minute, now minute, retention in
// minutes.
var sinceScript = valkey.NewLuaScript(sinceOf + `
return since(KEYS[1], tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3]))`)

func (v *Valkey) principalKey(p string) string           { return v.prefix + "slots:p:" + p }
func (v *Valkey) scopeKey(s string) string               { return v.prefix + "slots:s:" + s }
func (v *Valkey) counterKey(scope, period string) string { return v.keysOf(scope, period).counter }

// retainMinutes is the retention in whole minutes, for the scripts.
func (v *Valkey) retainMinutes() string { return strconv.FormatInt(int64(v.Retain/time.Minute), 10) }

func ms(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }

func (v *Valkey) Admit(ctx context.Context, a Admission) (Outcome, int64, error) {
	check := "0"
	if a.DeclaredWorstCaseCheck {
		check = "1"
	}
	ks := v.keysOf(a.Scope, a.Period)
	keys := []string{v.principalKey(a.Principal), ks.slots, ks.counter}
	r, err := admitScript.Exec(ctx, v.c, keys, []string{a.Call, strconv.Itoa(a.PrincipalLimit), strconv.Itoa(a.ScopeLimit), v.durations().ttlMs,
		i64(fromMinute(a.Since)), i64(a.Spend), i64(a.Budget), check, i64(a.Worst), v.retainMinutes(), i64(minuteOf(cmp.Or(a.Now, time.Now())))}).AsIntSlice()
	if err != nil {
		return 0, 0, wrap(err)
	}
	if len(r) != 2 {
		return 0, 0, fmt.Errorf("limit: the admit script returned %d values", len(r))
	}
	return Outcome(r[0]), r[1], nil
}

func (v *Valkey) Complete(ctx context.Context, in Increment, s Slot) error {
	ks := v.keysOf(in.Scope, in.Period)
	keys := []string{ks.counter, v.principalKey(s.Principal), v.scopeKey(s.Scope)}
	return v.write(ctx, completeScript, keys, []string{v.durations().lifeMs, i64(minuteOf(in.At)), i64(in.Cost), s.Call, v.retainMinutes()}, 1)
}

// write runs an add script. With ReplicaAcks it sends the script and WAIT
// together, on one connection: WAIT counts only its own connection's
// writes, and valkey-go sends a blocking command such as WAIT alone on a
// dedicated connection, where it would acknowledge nothing. A replica's
// acknowledgement only narrows what a failover can lose, so a short one
// is counted (Unreplicated: n increments), not an error.
func (v *Valkey) write(ctx context.Context, s *script, keys, args []string, n int64) error {
	if v.ReplicaAcks <= 0 {
		return wrap(s.Exec(ctx, v.c, keys, args).Error())
	}
	rs := v.pipeline(ctx, func(eval bool) valkey.Commands { return valkey.Commands{v.run(s, eval, keys, args)} })
	if err := rs[0].Error(); err != nil {
		return wrap(err)
	}
	if !v.replicated(rs[1]) {
		v.Unreplicated.Add(n)
	}
	return nil
}

// pipeline sends the script commands build makes, and WAIT after them
// when ReplicaAcks is set, in one round trip on one connection. If the
// server lacks the script (NOSCRIPT, in the first reply) it sends them
// again with the scripts' source: build is called for each send, since a
// built command is spent once sent.
func (v *Valkey) pipeline(ctx context.Context, build func(eval bool) valkey.Commands) []valkey.ValkeyResult {
	send := func(eval bool) []valkey.ValkeyResult {
		cmds := build(eval)
		if v.ReplicaAcks > 0 {
			cmds = append(cmds, v.c.B().Wait().Numreplicas(int64(v.ReplicaAcks)).Timeout(100).Build())
		}
		return v.c.DoMulti(ctx, cmds...)
	}
	rs := send(false)
	if err, ok := valkey.IsValkeyErr(rs[0].Error()); ok && err.IsNoScript() {
		rs = send(true)
	}
	return rs
}

// run is the command that runs a script: EVALSHA, or with eval EVAL of its
// source.
func (v *Valkey) run(s *script, eval bool, keys, args []string) valkey.Completed {
	if eval {
		return v.c.B().Eval().Script(s.src).Numkeys(int64(len(keys))).Key(keys...).Arg(args...).Build()
	}
	return v.c.B().Evalsha().Sha1(s.sha).Numkeys(int64(len(keys))).Key(keys...).Arg(args...).Build()
}

// replicated reports whether a WAIT's reply counts ReplicaAcks replicas.
func (v *Valkey) replicated(wait valkey.ValkeyResult) bool {
	n, err := wait.AsInt64()
	return err == nil && n >= int64(v.ReplicaAcks)
}

func i64(n int64) string { return strconv.FormatInt(n, 10) }

func (v *Valkey) Release(ctx context.Context, s Slot) error {
	cmds := valkey.Commands{
		v.c.B().Zrem().Key(v.principalKey(s.Principal)).Member(s.Call).Build(),
		v.c.B().Zrem().Key(v.scopeKey(s.Scope)).Member(s.Call).Build(),
	}
	for _, r := range v.c.DoMulti(ctx, cmds...) {
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
	// In chunks: one script holds Valkey's only thread for its whole run,
	// and a gateway with 10,000 calls in flight would stall every gateway
	// of the site at each renewal.
	ttl := v.durations().ttlMs
	for chunk := range slices.Chunk(slots, renewChunk) {
		keys := make([]string, 0, 2*len(chunk))
		args := append(make([]string, 0, 1+len(chunk)), ttl)
		for _, s := range chunk {
			keys = append(keys, v.principalKey(s.Principal), v.scopeKey(s.Scope))
			args = append(args, s.Call)
		}
		if err := registerScript.Exec(ctx, v.c, keys, args).Error(); err != nil {
			return wrap(err)
		}
	}
	return nil
}

// renewChunk is the slots one renewal script registers: about a
// millisecond of Valkey's time.
const renewChunk = 256

func (v *Valkey) Add(ctx context.Context, in Increment) error { return v.AddAll(ctx, []Increment{in}) }

// AddAll adds the increments in scripts of addChunk, each whole or not at
// all. A failed chunk ends the batch; the caller keeps the increments not
// reported added and replays them.
func (v *Valkey) AddAll(ctx context.Context, incs []Increment) error {
	life := v.durations().lifeMs
	for chunk := range slices.Chunk(incs, addChunk) {
		keys := make([]string, 0, len(chunk))
		args := append(make([]string, 0, 2+2*len(chunk)), life, v.retainMinutes())
		for _, in := range chunk {
			keys = append(keys, v.counterKey(in.Scope, in.Period))
			args = append(args, i64(minuteOf(in.At)), i64(in.Cost))
		}
		if err := v.write(ctx, addScript, keys, args, int64(len(chunk))); err != nil {
			return err
		}
	}
	return nil
}

// addChunk is the increments one add script takes; a replay is batched
// by the limiter in chunks no larger.
const addChunk = 256

func (v *Valkey) Since(ctx context.Context, scope, period string, since time.Time) (int64, error) {
	n, err := sinceScript.Exec(ctx, v.c, []string{v.counterKey(scope, period)},
		[]string{i64(fromMinute(since)), i64(minuteOf(time.Now())), v.retainMinutes()}).AsInt64()
	return n, wrap(err)
}

// ScopeSlots lists the calls holding a scope's slots (status, tests).
func (v *Valkey) ScopeSlots(ctx context.Context, scope string) ([]string, error) {
	r, err := v.c.Do(ctx, v.c.B().Zrange().Key(v.scopeKey(scope)).Min("0").Max("-1").Build()).AsStrSlice()
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
