package limit

import (
	"cmp"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"log/slog"
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
	// cl is the client, nil until DialValkey's dial has made one: every
	// operation fails as ErrUnavailable until then (the limiter's outage).
	cl     atomic.Pointer[valkey.Client]
	dial   func() (valkey.Client, error)
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

	afterFn func(time.Duration) <-chan time.Time // Connect's waits (tests)

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
	v := &Valkey{prefix: "{golem:" + site + "}:", TTL: 90 * time.Second, Retain: 6 * time.Hour}
	v.cl.Store(&c)
	return v
}

// DialValkey returns the store for site over a client dial makes. It dials
// once now; if that fails (Valkey unreachable as the gateway starts), the
// store is returned all the same, every operation failing as
// ErrUnavailable, so the limiter runs in its outage mode (waivers, held
// increments) until Connect's retries make a client and Limiter.Recover
// finds the store answering. A gateway that refused to start without its
// Valkey would turn an outage plus any pod churn into lost capacity, where
// the limiter is to fail open (ADR 0041 §7.3). The error is the first
// dial's, for the caller to log; the store is never nil.
func DialValkey(site string, dial func() (valkey.Client, error)) (*Valkey, error) {
	v := &Valkey{dial: dial, prefix: "{golem:" + site + "}:", TTL: 90 * time.Second, Retain: 6 * time.Hour}
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
			slog.Warn("limit: Valkey still unreachable; admitting on local counts, recorded as waivers", "retry_in", wait, "err", err)
			continue
		}
		if ctx.Err() != nil {
			c.Close() // made as the gateway stopped: nothing else would close it
			return
		}
		v.cl.Store(&c)
		slog.Info("limit: connected to Valkey; the limiter recovers at its next renewal")
	}
}

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
// the store up: a gateway with nothing to replay or renew would otherwise
// find an unreachable store answering.
func (v *Valkey) Ping(ctx context.Context) error {
	c, err := v.client()
	if err != nil {
		return err
	}
	return wrap(c.Do(ctx, c.B().Ping().Build()).Error())
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

// admitScript is one admission: every account's check and the slots in
// one atomic step, so two gateways cannot both pass the check and then
// both take the last slot or the last of a budget. The check is
// overBudget's, each account's in order (the first over its limit
// refuses); known spend is Mem.known's, never below the counter's total (a
// boundary older than the retention reads only the minutes retained).
//
//	KEYS: principal slots, scope slots, then a counter per account
//	ARGV: call, principal limit, scope limit, TTL (ms), worst-case check
//	      ("1" or "0"), worst, retention (minutes), the gateway's minute,
//	      then per account: from minute, the snapshot's spend, limit
//
// It returns {outcome, the account refused over (from 0), the known spend
// of each}, outcome an Outcome.
var admitScript = valkey.NewLuaScript(nowMillis + slotsTake + sinceOf + `
local principalSlots, scopeSlots = KEYS[1], KEYS[2]
local call, principalLimit, scopeLimit, ttl = ARGV[1], tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4])
local worstCaseCheck, worst = ARGV[5] == '1', tonumber(ARGV[6])
local retain, minute = tonumber(ARGV[7]), tonumber(ARGV[8])

local out = {0, 0}
for i = 3, #KEYS do
  local j = 9 + 3 * (i - 3)
  local counter, from, spend = KEYS[i], tonumber(ARGV[j]), tonumber(ARGV[j+1])
  local known = spend + since(counter, from, minute, retain)
  if from ~= 0 then known = math.max(known, tonumber(redis.call('HGET', counter, 'total') or '0')) end
  out[#out+1] = known
end
for i = 3, #KEYS do
  local limit, known = tonumber(ARGV[9 + 3 * (i - 3) + 2]), out[i]
  if limit ~= 0 then -- 0 is none; a negative one refuses, as overBudget does
    local over
    if worstCaseCheck then over = known + worst > limit else over = known >= limit end
    if over then out[1], out[2] = 1, i - 3; return out end
  end
end
if not take(principalSlots, scopeSlots, call, principalLimit, scopeLimit, ttl) then out[1] = 2 end
return out`)

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
//	KEYS: a counter, per increment's account
//	ARGV: the counters' life (ms), retention (minutes), then minute and
//	      cost per key
var addScript = newScript(addTo + `
local life, retain = ARGV[1], tonumber(ARGV[2])
for i = 1, #KEYS do
  add(KEYS[i], ARGV[2*i+1], ARGV[2*i+2], life, retain)
end
return 1`)

// completeScript is a completion: the add to each account's counter, then
// the slot freed.
//
//	KEYS: principal slots, scope slots, then a counter per account
//	ARGV: the counters' life (ms), minute, cost, call, retention (minutes)
var completeScript = newScript(addTo + `
local principalSlots, scopeSlots = KEYS[1], KEYS[2]
local life, minute, cost, call, retain = ARGV[1], ARGV[2], ARGV[3], ARGV[4], tonumber(ARGV[5])
for i = 3, #KEYS do add(KEYS[i], minute, cost, life, retain) end
redis.call('ZREM', principalSlots, call)
redis.call('ZREM', scopeSlots, call)
return 1`)

// sinceScript reads the counter's spend from a minute on (Store.Since).
//
//	KEYS: counter
//	ARGV: from minute, the gateway's minute, retention (minutes)
var sinceScript = valkey.NewLuaScript(sinceOf + `
return since(KEYS[1], tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3]))`)

func (v *Valkey) principalKey(p string) string { return v.prefix + "slots:p:" + p }
func (v *Valkey) scopeKey(s string) string     { return v.prefix + "slots:s:" + s }
func (v *Valkey) counterKey(account, period string) string {
	return v.keysOf(account, period).counter
}

func (v *Valkey) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func ms(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }

func i64(n int64) string { return strconv.FormatInt(n, 10) }

func (v *Valkey) Admit(ctx context.Context, a Admission) (Result, error) {
	if len(a.Accounts) > MaxAccounts {
		return Result{}, fmt.Errorf("limit: an admission names %d accounts (at most %d)", len(a.Accounts), MaxAccounts)
	}
	check := "0"
	if a.DeclaredWorstCaseCheck {
		check = "1"
	}
	d := v.durations()
	keys := append(make([]string, 0, 2+MaxAccounts), v.principalKey(a.Principal), v.scopeKey(a.Scope))
	args := append(make([]string, 0, 8+3*MaxAccounts), a.Call, strconv.Itoa(a.PrincipalLimit), strconv.Itoa(a.ScopeLimit), d.ttlMs,
		check, i64(a.Worst), d.retainMinutes, i64(minuteOf(cmp.Or(a.Now, v.now()))))
	for _, acc := range a.Accounts {
		keys = append(keys, v.counterKey(acc.ID, a.Period))
		args = append(args, i64(fromMinute(acc.Since)), i64(acc.Spend), i64(acc.Limit))
	}
	c, err := v.client()
	if err != nil {
		return Result{}, err
	}
	r, err := admitScript.Exec(ctx, c, keys, args).AsIntSlice()
	if err != nil {
		return Result{}, wrap(err)
	}
	if len(r) != 2+len(a.Accounts) {
		return Result{}, fmt.Errorf("limit: the admit script returned %d values for %d accounts", len(r), len(a.Accounts))
	}
	res := Result{Outcome: Outcome(r[0]), Over: int(r[1])}
	copy(res.Known[:], r[2:])
	return res, nil
}

func (v *Valkey) Complete(ctx context.Context, in Increment, s Slot) error {
	d := v.durations()
	keys := append(make([]string, 0, 2+len(in.Accounts)), v.principalKey(s.Principal), v.scopeKey(s.Scope))
	for _, acc := range in.Accounts {
		keys = append(keys, v.counterKey(acc, in.Period))
	}
	return v.write(ctx, completeScript, keys, []string{d.lifeMs, i64(minuteOf(in.At)), i64(in.Cost), s.Call, d.retainMinutes}, 1)
}

// write runs an add script (n increments). With ReplicaAcks it sends the
// script and WAIT together, on one connection: WAIT counts only its own
// connection's writes, and valkey-go sends a blocking command such as WAIT
// alone on a dedicated connection, where it would acknowledge nothing. A
// replica's acknowledgement only narrows what a failover can lose, so a
// short one is counted (Unreplicated), not an error.
func (v *Valkey) write(ctx context.Context, s *script, keys, args []string, n int64) error {
	c, err := v.client()
	if err != nil {
		return err
	}
	if v.ReplicaAcks <= 0 {
		return wrap(s.Exec(ctx, c, keys, args).Error())
	}
	send := func(eval bool) []valkey.ValkeyResult {
		// Built for each send: a built command is spent once sent.
		wait := c.B().Wait().Numreplicas(int64(v.ReplicaAcks)).Timeout(100).Build()
		return c.DoMulti(ctx, run(c, s, eval, keys, args), wait)
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
func run(c valkey.Client, s *script, eval bool, keys, args []string) valkey.Completed {
	if eval {
		return c.B().Eval().Script(s.src).Numkeys(int64(len(keys))).Key(keys...).Arg(args...).Build()
	}
	return c.B().Evalsha().Sha1(s.sha).Numkeys(int64(len(keys))).Key(keys...).Arg(args...).Build()
}

// replicated reports whether a WAIT's reply counts ReplicaAcks replicas.
func (v *Valkey) replicated(wait valkey.ValkeyResult) bool {
	n, err := wait.AsInt64()
	return err == nil && n >= int64(v.ReplicaAcks)
}

func (v *Valkey) Release(ctx context.Context, s Slot) error {
	c, err := v.client()
	if err != nil {
		return err
	}
	cmds := valkey.Commands{
		c.B().Zrem().Key(v.principalKey(s.Principal)).Member(s.Call).Build(),
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
	ttl := v.durations().ttlMs
	for chunk := range slices.Chunk(slots, renewChunk) {
		keys := make([]string, 0, 2*len(chunk))
		args := append(make([]string, 0, 1+len(chunk)), ttl)
		for _, s := range chunk {
			keys = append(keys, v.principalKey(s.Principal), v.scopeKey(s.Scope))
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
			minute, cost := i64(minuteOf(in.At)), i64(in.Cost)
			for _, acc := range in.Accounts {
				keys = append(keys, v.counterKey(acc, in.Period))
				args = append(args, minute, cost)
			}
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

func (v *Valkey) Since(ctx context.Context, account, period string, since time.Time) (int64, error) {
	c, err := v.client()
	if err != nil {
		return 0, err
	}
	n, err := sinceScript.Exec(ctx, c, []string{v.counterKey(account, period)},
		[]string{i64(fromMinute(since)), i64(minuteOf(v.now())), v.durations().retainMinutes}).AsInt64()
	return n, wrap(err)
}

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
