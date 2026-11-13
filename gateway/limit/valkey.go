package limit

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/valkey-io/valkey-go"
)

// Valkey is the site store (ADR 0012, 0031, 0042): one Valkey (ElastiCache
// for Valkey, or the site's own) shared by the site's gateways. Each
// operation is one Lua script, so it is atomic. Every key carries the hash
// tag {golem:<site>}: in cluster mode they share a slot, as the scripts
// need.
//
// Slots are sorted sets per principal and per scope, scored by their expiry
// on the server's clock (TIME), so a gateway's clock cannot extend one.
//
// The counter, per scope and budget period, numbers its events (ADR 0042): a sequence, a
// running total, the total as of each number, and the event of each number
// (so adding one twice returns its number). Known spend reads two of these,
// O(1). History older than Retain is trimmed, a little at each add; Retain
// must exceed the longest authorization lease a live gateway can hold (an
// internal pool's four hours, ADR 0032), since a gateway partitioned from
// the bucket but not from its Valkey keeps an old snapshot and needs its
// watermark's total.
type Valkey struct {
	c      valkey.Client
	prefix string
	// TTL is a slot's life without renewal (ADR 0038: 90 seconds).
	TTL time.Duration
	// Retain is how long the counter keeps its history (default 5 hours).
	Retain time.Duration
	// ReplicaAcks, if positive, makes each add wait for that many replicas
	// to acknowledge it (WAIT), so a failover cannot lose it and hand its
	// number to another event (ADR 0042). Adds are off the request's
	// critical path: at completion.
	ReplicaAcks int
}

// NewValkey returns the store for site over a client.
func NewValkey(c valkey.Client, site string) *Valkey {
	return &Valkey{c: c, prefix: "{golem:" + site + "}:", TTL: 90 * time.Second, Retain: 5 * time.Hour}
}

const nowMillis = `local t = redis.call('TIME'); local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
`

// slotsTake trims expired slots and, if the limits allow, takes one: KEYS
// principal and scope slots, ARGV call, principal limit, scope limit, TTL.
// It is a Lua function shared by the admit script.
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

// afterOf is the period counter's total after w: KEYS seq, total, cum.
// Returns the total and 1 if the total as of w was trimmed (then the whole).
const afterOf = `
local function after(seqk, totalk, cumk, w)
  local n = tonumber(redis.call('GET', seqk) or '0')
  local total = tonumber(redis.call('GET', totalk) or '0')
  if w >= n then return 0, 0 end
  if w == 0 then return total, 0 end
  local at = redis.call('HGET', cumk, w)
  if not at then return total, 1 end
  return total - tonumber(at), 0
end
`

// admit: KEYS principal slots, scope slots, seq, total, cum; ARGV call,
// principal limit, scope limit, TTL, watermark, spend, budget, strict (0 or
// 1), worst. Returns {outcome, known, over}: the check and the slot in one
// atomic step, so two gateways cannot both pass the check and then both
// take the last slot or the last of the budget.
var admit = valkey.NewLuaScript(nowMillis + slotsTake + afterOf + `
local a, over = after(KEYS[3], KEYS[4], KEYS[5], tonumber(ARGV[5]))
local known = tonumber(ARGV[6]) + a
local budget, worst = tonumber(ARGV[7]), tonumber(ARGV[9])
if budget > 0 then
  if ARGV[8] == '1' and known + worst > budget then return {1, known, over} end
  if ARGV[8] ~= '1' and known >= budget then return {1, known, over} end
end
if not take(KEYS[1], KEYS[2], ARGV[1], tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4])) then
  return {2, known, over}
end
return {0, known, over}`)

// register adds slots without checking limits: KEYS pairs (principal,
// scope), ARGV[1] the TTL, then the calls.
var register = valkey.NewLuaScript(nowMillis + `
local exp = now + tonumber(ARGV[1])
for i = 2, #ARGV do
  redis.call('ZADD', KEYS[2*i-3], exp, ARGV[i])
  redis.call('ZADD', KEYS[2*i-2], exp, ARGV[i])
  redis.call('PEXPIRE', KEYS[2*i-3], ARGV[1])
  redis.call('PEXPIRE', KEYS[2*i-2], ARGV[1])
end
return 1`)

// addTo numbers an event in a period's counter: KEYS seq, total, cum (number
// -> total), ev (event -> number), ids (number -> event), times (number by
// time); the event, its cost, the retention and the keys' life (a period's
// counter outlives its period by the retention, then goes).
const addTo = `
local function add(k, id, cost, retain, life)
  local had = redis.call('HGET', k[4], id)
  if had then return tonumber(had) end
  local n = redis.call('INCR', k[1])
  local total = redis.call('INCRBY', k[2], cost)
  redis.call('HSET', k[3], n, total)
  redis.call('HSET', k[4], id, n)
  redis.call('HSET', k[5], n, id)
  redis.call('ZADD', k[6], now, n)
  for i = 1, 6 do redis.call('PEXPIRE', k[i], life) end
  -- trim a little history past retention, never the newest number
  local old = redis.call('ZRANGEBYSCORE', k[6], '-inf', now - retain, 'LIMIT', 0, 8)
  for _, m in ipairs(old) do
    if tonumber(m) < n then
      local was = redis.call('HGET', k[5], m)
      if was then redis.call('HDEL', k[4], was) end
      redis.call('HDEL', k[5], m)
      redis.call('HDEL', k[3], m)
      redis.call('ZREM', k[6], m)
    end
  end
  return n
end
`

// add: KEYS the counter's six; ARGV event, cost, retain, life.
var add = valkey.NewLuaScript(nowMillis + addTo + `
return add(KEYS, ARGV[1], tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4]))`)

// complete: add, then free the slot (KEYS[7], KEYS[8]: principal and scope
// slots; ARGV[5] the call), in one step.
var complete = valkey.NewLuaScript(nowMillis + addTo + `
local n = add(KEYS, ARGV[1], tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4]))
redis.call('ZREM', KEYS[7], ARGV[5])
redis.call('ZREM', KEYS[8], ARGV[5])
return n`)

// after: KEYS seq, total, cum; ARGV w.
var after = valkey.NewLuaScript(afterOf + `
local a, over = after(KEYS[1], KEYS[2], KEYS[3], tonumber(ARGV[1]))
return {a, over}`)

func (v *Valkey) principalKey(p string) string { return v.prefix + "slots:p:" + p }
func (v *Valkey) scopeKey(s string) string     { return v.prefix + "slots:s:" + s }
func (v *Valkey) counterKeys(scope, period string) []string {
	p := v.prefix + "counter:" + scope + ":" + period + ":"
	return []string{p + "seq", p + "total", p + "cum", p + "ev", p + "ids", p + "times"}
}

func ms(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }

func (v *Valkey) Admit(ctx context.Context, a Admission) (Outcome, int64, bool, error) {
	strict := "0"
	if a.Strict {
		strict = "1"
	}
	keys := append([]string{v.principalKey(a.Principal), v.scopeKey(a.Scope)}, v.counterKeys(a.Scope, a.Period)[:3]...)
	r, err := admit.Exec(ctx, v.c, keys, []string{a.Call, strconv.Itoa(a.PrincipalLimit), strconv.Itoa(a.ScopeLimit), ms(v.TTL),
		i64(a.Watermark), i64(a.Spend), i64(a.Budget), strict, i64(a.Worst)}).AsIntSlice()
	if err != nil {
		return 0, 0, false, wrap(err)
	}
	if len(r) != 3 {
		return 0, 0, false, fmt.Errorf("limit: the admit script returned %d values", len(r))
	}
	return Outcome(r[0]), r[1], r[2] == 1, nil
}

// life is how long a period's counter keys live after their last add: past
// the longest month, plus retention.
func (v *Valkey) life() string { return ms(31*24*time.Hour + v.Retain) }

func (v *Valkey) Complete(ctx context.Context, period string, s Slot, e Event) (int64, error) {
	keys := append(v.counterKeys(s.Scope, period), v.principalKey(s.Principal), v.scopeKey(s.Scope))
	seq, err := complete.Exec(ctx, v.c, keys, []string{e.ID, i64(e.Cost), ms(v.Retain), v.life(), s.Call}).AsInt64()
	if err != nil {
		return 0, wrap(err)
	}
	v.wait(ctx)
	return seq, nil
}

// wait, if ReplicaAcks is set, waits for replicas to acknowledge an add.
// The add is done; a replica's acknowledgement only narrows what a failover
// can lose, so its failure is not the add's.
func (v *Valkey) wait(ctx context.Context) {
	if v.ReplicaAcks > 0 {
		v.c.Do(ctx, v.c.B().Wait().Numreplicas(int64(v.ReplicaAcks)).Timeout(100).Build())
	}
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
	keys := make([]string, 0, 2*len(slots))
	args := []string{ms(v.TTL)}
	for _, s := range slots {
		keys = append(keys, v.principalKey(s.Principal), v.scopeKey(s.Scope))
		args = append(args, s.Call)
	}
	return wrap(register.Exec(ctx, v.c, keys, args).Error())
}

func (v *Valkey) Add(ctx context.Context, scope, period string, e Event) (int64, error) {
	seq, err := add.Exec(ctx, v.c, v.counterKeys(scope, period), []string{e.ID, i64(e.Cost), ms(v.Retain), v.life()}).AsInt64()
	if err != nil {
		return 0, wrap(err)
	}
	v.wait(ctx)
	return seq, nil
}

func (v *Valkey) After(ctx context.Context, scope, period string, w int64) (int64, bool, error) {
	r, err := after.Exec(ctx, v.c, v.counterKeys(scope, period)[:3], []string{i64(w)}).AsIntSlice()
	if err != nil {
		return 0, false, wrap(err)
	}
	if len(r) != 2 {
		return 0, false, fmt.Errorf("limit: the after script returned %d values", len(r))
	}
	return r[0], r[1] == 1, nil
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
	return fmt.Errorf("%w: %v", ErrUnavailable, err)
}
