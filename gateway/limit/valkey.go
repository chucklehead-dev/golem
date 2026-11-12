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
// The counter, per scope, numbers its events (ADR 0042): a sequence, a
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

var acquire = valkey.NewLuaScript(nowMillis + `
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now)
if tonumber(ARGV[2]) > 0 and redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[2]) then return 0 end
if tonumber(ARGV[3]) > 0 and redis.call('ZCARD', KEYS[2]) >= tonumber(ARGV[3]) then return 0 end
local exp = now + tonumber(ARGV[4])
redis.call('ZADD', KEYS[1], exp, ARGV[1])
redis.call('ZADD', KEYS[2], exp, ARGV[1])
redis.call('PEXPIRE', KEYS[1], ARGV[4])
redis.call('PEXPIRE', KEYS[2], ARGV[4])
return 1`)

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

// add: KEYS seq, total, cum (number -> total), ev (event -> number),
// ids (number -> event), times (number by time); ARGV event, cost, retain.
var add = valkey.NewLuaScript(nowMillis + `
local had = redis.call('HGET', KEYS[4], ARGV[1])
if had then return tonumber(had) end
local n = redis.call('INCR', KEYS[1])
local total = redis.call('INCRBY', KEYS[2], ARGV[2])
redis.call('HSET', KEYS[3], n, total)
redis.call('HSET', KEYS[4], ARGV[1], n)
redis.call('HSET', KEYS[5], n, ARGV[1])
redis.call('ZADD', KEYS[6], now, n)
-- trim a little history past retention, never the newest number
local old = redis.call('ZRANGEBYSCORE', KEYS[6], '-inf', now - tonumber(ARGV[3]), 'LIMIT', 0, 8)
for _, m in ipairs(old) do
  if tonumber(m) < n then
    local id = redis.call('HGET', KEYS[5], m)
    if id then redis.call('HDEL', KEYS[4], id) end
    redis.call('HDEL', KEYS[5], m)
    redis.call('HDEL', KEYS[3], m)
    redis.call('ZREM', KEYS[6], m)
  end
end
return n`)

// after: KEYS seq, total, cum; ARGV w. Returns {the total after w, 1 if
// the total as of w was trimmed (then the whole total)}.
var after = valkey.NewLuaScript(`
local n = tonumber(redis.call('GET', KEYS[1]) or '0')
local total = tonumber(redis.call('GET', KEYS[2]) or '0')
local w = tonumber(ARGV[1])
if w >= n then return {0, 0} end
if w == 0 then return {total, 0} end
local at = redis.call('HGET', KEYS[3], w)
if not at then return {total, 1} end
return {total - tonumber(at), 0}`)

func (v *Valkey) principalKey(p string) string { return v.prefix + "slots:p:" + p }
func (v *Valkey) scopeKey(s string) string     { return v.prefix + "slots:s:" + s }
func (v *Valkey) counterKeys(scope string) []string {
	p := v.prefix + "counter:" + scope + ":"
	return []string{p + "seq", p + "total", p + "cum", p + "ev", p + "ids", p + "times"}
}

func ms(d time.Duration) string { return strconv.FormatInt(d.Milliseconds(), 10) }

func (v *Valkey) Acquire(ctx context.Context, s Slot, principalLimit, scopeLimit int) (bool, error) {
	n, err := acquire.Exec(ctx, v.c, []string{v.principalKey(s.Principal), v.scopeKey(s.Scope)},
		[]string{s.Call, strconv.Itoa(principalLimit), strconv.Itoa(scopeLimit), ms(v.TTL)}).AsInt64()
	return n == 1, wrap(err)
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
	keys := make([]string, 0, 2*len(slots))
	args := []string{ms(v.TTL)}
	for _, s := range slots {
		keys = append(keys, v.principalKey(s.Principal), v.scopeKey(s.Scope))
		args = append(args, s.Call)
	}
	return wrap(register.Exec(ctx, v.c, keys, args).Error())
}

func (v *Valkey) Add(ctx context.Context, scope string, e Event) (int64, error) {
	seq, err := add.Exec(ctx, v.c, v.counterKeys(scope),
		[]string{e.ID, strconv.FormatInt(e.Cost, 10), ms(v.Retain)}).AsInt64()
	if err != nil {
		return 0, wrap(err)
	}
	if v.ReplicaAcks > 0 {
		// The add is done; a replica's acknowledgement only narrows what a
		// failover can lose. Its failure is not the add's.
		v.c.Do(ctx, v.c.B().Wait().Numreplicas(int64(v.ReplicaAcks)).Timeout(100).Build())
	}
	return seq, nil
}

func (v *Valkey) After(ctx context.Context, scope string, w int64) (int64, bool, error) {
	k := v.counterKeys(scope)
	r, err := after.Exec(ctx, v.c, k[:3], []string{strconv.FormatInt(w, 10)}).ToArray()
	if err != nil {
		return 0, false, wrap(err)
	}
	if len(r) != 2 {
		return 0, false, fmt.Errorf("limit: the after script returned %d values", len(r))
	}
	total, err := r[0].AsInt64()
	if err != nil {
		return 0, false, err
	}
	over, err := r[1].AsInt64()
	return total, over == 1, err
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
