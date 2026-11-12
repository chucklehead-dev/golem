package limit

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/valkey-io/valkey-go"
)

// Valkey is the site store (ADR 0012, 0031): one Valkey (ElastiCache for
// Valkey, or the site's own) shared by the site's gateways. Each operation
// is one Lua script, so it is atomic. Every key carries the hash tag
// {golem:<site>}: in cluster mode they share a slot, as the scripts need.
//
// Slots are sorted sets per principal and per scope, scored by their expiry
// on the server's clock (TIME), so a gateway's clock cannot extend one. The
// counter is a hash of event to cost per scope, with a sorted set of the
// events' times: an event older than Retain is trimmed, so Retain must
// exceed the ledger's ingestion lag plus a snapshot interval (an event the
// ledger never ingests is left to reconciliation, ADR 0031).
type Valkey struct {
	c      valkey.Client
	prefix string
	// TTL is a slot's life without renewal (ADR 0038: 90 seconds).
	TTL time.Duration
	// Retain is how long the counter keeps an event (default 15 minutes).
	Retain time.Duration
}

// NewValkey returns the store for site over a client.
func NewValkey(c valkey.Client, site string) *Valkey {
	return &Valkey{c: c, prefix: "{golem:" + site + "}:", TTL: 90 * time.Second, Retain: 15 * time.Minute}
}

const nowMillis = `local t = redis.call('TIME'); local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
`

var acquire = valkey.NewLuaScript(nowMillis + `
redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now)
if redis.call('ZCARD', KEYS[1]) >= tonumber(ARGV[2]) then return 0 end
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

var add = valkey.NewLuaScript(nowMillis + `
for i = 2, #ARGV, 2 do
  redis.call('HSET', KEYS[1], ARGV[i], ARGV[i+1])
  redis.call('ZADD', KEYS[2], 'NX', now, ARGV[i])
end
redis.call('PEXPIRE', KEYS[1], ARGV[1])
redis.call('PEXPIRE', KEYS[2], ARGV[1])
return 1`)

var events = valkey.NewLuaScript(nowMillis + `
local old = redis.call('ZRANGEBYSCORE', KEYS[2], '-inf', now - tonumber(ARGV[1]))
for _, id in ipairs(old) do
  redis.call('HDEL', KEYS[1], id)
  redis.call('ZREM', KEYS[2], id)
end
return redis.call('HGETALL', KEYS[1])`)

func (v *Valkey) principalKey(p string) string { return v.prefix + "slots:p:" + p }
func (v *Valkey) scopeKey(s string) string     { return v.prefix + "slots:s:" + s }
func (v *Valkey) spendKey(s string) string     { return v.prefix + "spend:" + s }
func (v *Valkey) timesKey(s string) string     { return v.prefix + "spendt:" + s }

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

func (v *Valkey) Add(ctx context.Context, scope string, es ...Event) error {
	if len(es) == 0 {
		return nil
	}
	args := []string{ms(v.Retain)}
	for _, e := range es {
		args = append(args, e.ID, strconv.FormatInt(e.Cost, 10))
	}
	return wrap(add.Exec(ctx, v.c, []string{v.spendKey(scope), v.timesKey(scope)}, args).Error())
}

func (v *Valkey) Events(ctx context.Context, scope string) ([]Event, error) {
	m, err := events.Exec(ctx, v.c, []string{v.spendKey(scope), v.timesKey(scope)}, []string{ms(v.Retain)}).AsStrMap()
	if err != nil {
		return nil, wrap(err)
	}
	es := make([]Event, 0, len(m))
	for id, c := range m {
		n, err := strconv.ParseInt(c, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("limit: counter %s event %s holds %q", scope, id, c)
		}
		es = append(es, Event{id, n})
	}
	return es, nil
}

func (v *Valkey) Forget(ctx context.Context, scope string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	cmds := valkey.Commands{
		v.c.B().Hdel().Key(v.spendKey(scope)).Field(ids...).Build(),
		v.c.B().Zrem().Key(v.timesKey(scope)).Member(ids...).Build(),
	}
	for _, r := range v.c.DoMulti(ctx, cmds...) {
		if err := r.Error(); err != nil {
			return wrap(err)
		}
	}
	return nil
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
