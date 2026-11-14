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
	// Now is the gateway's clock, for a read's last minute and an
	// admission that names none (default time.Now): Mem's, so the two
	// stores read the same window.
	Now func() time.Time
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
// durations they were formatted from: an admission or a completion builds
// no strings for them.
type durArgs struct {
	ttl, retain   time.Duration
	ttlMs         string // a slot's life
	lifeMs        string // a counter's: past the longest month, plus retention
	retainMinutes string
}

func (v *Valkey) durations() *durArgs {
	if d := v.args.Load(); d != nil && d.ttl == v.TTL && d.retain == v.Retain {
		return d
	}
	d := &durArgs{ttl: v.TTL, retain: v.Retain, ttlMs: ms(v.TTL), lifeMs: ms(31*24*time.Hour + v.Retain),
		retainMinutes: i64(int64(v.Retain / time.Minute))}
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

// The scripts are built from the Lua fragments below, each a local
// function (or, nowMillis, a local) a script's body calls. Each script's
// comment lists its KEYS and ARGV; numbers travel as decimal strings,
// minutes are Unix minutes (minuteOf), and a counter's fields are its
// minutes and "total".

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

// sinceOf is the Lua function since: the counter's spend in the minutes
// from from to the caller's minute plus clockSkewMinutes, and no further
// back than the retention (Mem.since reads the same window). It reads
// only those minutes, one HGET each: the cost is the boundary's age, not
// the counter's size. A from of 0 (no boundary) reads the total alone.
var sinceOf = `
local function since(counter, from, minute, retain)
  if from == 0 then return tonumber(redis.call('HGET', counter, 'total') or '0') end
  local total = 0
  for m = math.max(from, minute - retain), minute + ` + strconv.Itoa(clockSkewMinutes) + ` do
    local v = redis.call('HGET', counter, m)
    if v then total = total + tonumber(v) end
  end
  return total
end
`

// clockSkewMinutes is how far past the caller's minute a read looks: a
// minute stamped by a peer gateway whose clock is a little ahead.
const clockSkewMinutes = 1

// addTo is the Lua functions trim and add. add adds a cost to a minute of
// the counter and to its total, and extends the counter's life (a period's
// counter outlives its period by the retention, then goes); minute is the
// field as sent. trim, once the counter holds more than a retention's
// minutes and a margin, deletes those older than the retention, by the
// increment's minute: amortised, it runs about once a margin's minutes.
const addTo = `
local function trim(counter, minute, retain)
  if redis.call('HLEN', counter) <= retain + 60 then return end
  for _, field in ipairs(redis.call('HKEYS', counter)) do
    local m = tonumber(field) -- nil for 'total'
    if m and m < minute - retain then redis.call('HDEL', counter, field) end
  end
end

local function add(counter, minute, cost, life, retain)
  redis.call('HINCRBY', counter, minute, cost)
  redis.call('HINCRBY', counter, 'total', cost)
  redis.call('PEXPIRE', counter, life)
  trim(counter, tonumber(minute), retain)
end
`

// admitScript is one admission: the budget check and the slots in one
// atomic step, so two gateways cannot both pass the check and then both
// take the last slot or the last of the budget. The check is overBudget's.
//
//	KEYS: principal slots, scope slots, counter
//	ARGV: call, principal limit, scope limit, TTL (ms), from minute,
//	      the snapshot's spend, budget, worst-case check ("1" or "0"),
//	      worst, retention (minutes), the gateway's minute
//
// It returns {outcome, known}, outcome an Outcome.
var admitScript = valkey.NewLuaScript(nowMillis + slotsTake + sinceOf + `
local principalSlots, scopeSlots, counter = KEYS[1], KEYS[2], KEYS[3]
local call, principalLimit, scopeLimit, ttl = ARGV[1], tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4])
local from, spend, budget = tonumber(ARGV[5]), tonumber(ARGV[6]), tonumber(ARGV[7])
local worstCaseCheck, worst = ARGV[8] == '1', tonumber(ARGV[9])
local retain, minute = tonumber(ARGV[10]), tonumber(ARGV[11])

local known = spend + since(counter, from, minute, retain)
if budget ~= 0 then -- 0 is none; a negative one refuses, as overBudget does
  local over
  if worstCaseCheck then over = known + worst > budget else over = known >= budget end
  if over then return {1, known} end
end
if not take(principalSlots, scopeSlots, call, principalLimit, scopeLimit, ttl) then return {2, known} end
return {0, known}`)

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

// addScript adds increments in one script, so a batch is added whole or
// not at all.
//
//	KEYS: the counter, per increment
//	ARGV: the counters' life (ms), retention (minutes), then minute and
//	      cost per increment
var addScript = newScript(addTo + `
local life, retain = ARGV[1], tonumber(ARGV[2])
for i = 1, #KEYS do
  add(KEYS[i], ARGV[2*i+1], ARGV[2*i+2], life, retain)
end
return 1`)

// completeScript is a completion: the add, then the slot freed.
//
//	KEYS: counter, principal slots, scope slots
//	ARGV: the counter's life (ms), minute, cost, call, retention (minutes)
var completeScript = newScript(addTo + `
local counter, principalSlots, scopeSlots = KEYS[1], KEYS[2], KEYS[3]
local life, minute, cost, call, retain = ARGV[1], ARGV[2], ARGV[3], ARGV[4], tonumber(ARGV[5])
add(counter, minute, cost, life, retain)
redis.call('ZREM', principalSlots, call)
redis.call('ZREM', scopeSlots, call)
return 1`)

// sinceScript reads the counter's spend from a minute on (Store.Since).
//
//	KEYS: counter
//	ARGV: from minute, the gateway's minute, retention (minutes)
var sinceScript = valkey.NewLuaScript(sinceOf + `
return since(KEYS[1], tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3]))`)

func (v *Valkey) principalKey(p string) string           { return v.prefix + "slots:p:" + p }
func (v *Valkey) scopeKey(s string) string               { return v.prefix + "slots:s:" + s }
func (v *Valkey) counterKey(scope, period string) string { return v.keysOf(scope, period).counter }

func (v *Valkey) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func ms(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }

func i64(n int64) string { return strconv.FormatInt(n, 10) }

func (v *Valkey) Admit(ctx context.Context, a Admission) (Outcome, int64, error) {
	check := "0"
	if a.DeclaredWorstCaseCheck {
		check = "1"
	}
	d, ks := v.durations(), v.keysOf(a.Scope, a.Period)
	keys := []string{v.principalKey(a.Principal), ks.slots, ks.counter}
	args := []string{a.Call, strconv.Itoa(a.PrincipalLimit), strconv.Itoa(a.ScopeLimit), d.ttlMs,
		i64(fromMinute(a.Since)), i64(a.Spend), i64(a.Budget), check, i64(a.Worst), d.retainMinutes, i64(minuteOf(cmp.Or(a.Now, v.now())))}
	r, err := admitScript.Exec(ctx, v.c, keys, args).AsIntSlice()
	if err != nil {
		return 0, 0, wrap(err)
	}
	if len(r) != 2 {
		return 0, 0, fmt.Errorf("limit: the admit script returned %d values", len(r))
	}
	return Outcome(r[0]), r[1], nil
}

func (v *Valkey) Complete(ctx context.Context, in Increment, s Slot) error {
	d, ks := v.durations(), v.keysOf(in.Scope, in.Period)
	keys := []string{ks.counter, v.principalKey(s.Principal), v.scopeKey(s.Scope)}
	return v.write(ctx, completeScript, keys, []string{d.lifeMs, i64(minuteOf(in.At)), i64(in.Cost), s.Call, d.retainMinutes}, 1)
}

// write runs an add script (n increments). With ReplicaAcks it sends the
// script and WAIT together, on one connection: WAIT counts only its own
// connection's writes, and valkey-go sends a blocking command such as WAIT
// alone on a dedicated connection, where it would acknowledge nothing. A
// replica's acknowledgement only narrows what a failover can lose, so a
// short one is counted (Unreplicated), not an error.
func (v *Valkey) write(ctx context.Context, s *script, keys, args []string, n int64) error {
	if v.ReplicaAcks <= 0 {
		return wrap(s.Exec(ctx, v.c, keys, args).Error())
	}
	send := func(eval bool) []valkey.ValkeyResult {
		// Built for each send: a built command is spent once sent.
		wait := v.c.B().Wait().Numreplicas(int64(v.ReplicaAcks)).Timeout(100).Build()
		return v.c.DoMulti(ctx, v.run(s, eval, keys, args), wait)
	}
	// Lua.Exec falls back from EVALSHA to EVAL itself, one command at a
	// time; a pipeline has to do it by hand.
	rs := send(false)
	if err, ok := valkey.IsValkeyErr(rs[0].Error()); ok && err.IsNoScript() {
		rs = send(true)
	}
	if err := rs[0].Error(); err != nil {
		return wrap(err)
	}
	if !v.replicated(rs[1]) {
		v.Unreplicated.Add(n)
	}
	return nil
}

// run is the command that runs a script: EVALSHA, or with eval EVAL of its
// source (the server lacked it: NOSCRIPT).
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
	d := v.durations()
	for chunk := range slices.Chunk(incs, addChunk) {
		keys := make([]string, 0, len(chunk))
		args := append(make([]string, 0, 2+2*len(chunk)), d.lifeMs, d.retainMinutes)
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
		[]string{i64(fromMinute(since)), i64(minuteOf(v.now())), v.durations().retainMinutes}).AsInt64()
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
