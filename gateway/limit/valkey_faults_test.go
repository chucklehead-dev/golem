package limit

import (
	"context"
	"slices"
	"strconv"
	"sync/atomic"

	"github.com/valkey-io/valkey-go"
)

// valkeyFaults injects the model's environment into a Valkey store: an
// outage (every call fails, and the slots' TTLs lapse) and a failover (the
// counter loses its tail, and its numbers are given again).
type valkeyFaults struct {
	*Valkey
	down atomic.Bool
}

func (f *valkeyFaults) Acquire(ctx context.Context, s Slot, p, sc int) (bool, error) {
	if f.down.Load() {
		return false, ErrUnavailable
	}
	return f.Valkey.Acquire(ctx, s, p, sc)
}

func (f *valkeyFaults) Release(ctx context.Context, s Slot) error {
	if f.down.Load() {
		return ErrUnavailable
	}
	return f.Valkey.Release(ctx, s)
}

func (f *valkeyFaults) Reacquire(ctx context.Context, ss []Slot) error {
	if f.down.Load() {
		return ErrUnavailable
	}
	return f.Valkey.Reacquire(ctx, ss)
}

func (f *valkeyFaults) Add(ctx context.Context, scope string, e Event) (int64, error) {
	if f.down.Load() {
		return 0, ErrUnavailable
	}
	return f.Valkey.Add(ctx, scope, e)
}

func (f *valkeyFaults) After(ctx context.Context, scope string, w int64) (int64, bool, error) {
	if f.down.Load() {
		return 0, false, ErrUnavailable
	}
	return f.Valkey.After(ctx, scope, w)
}

// SetDown: going down, every slot lapses (the outage outlasts the TTL).
func (f *valkeyFaults) SetDown(down bool) {
	ctx := context.Background()
	if down {
		var keys []string
		for _, p := range []string{"p1", "p2", "p3"} {
			keys = append(keys, f.principalKey(p))
		}
		keys = append(keys, f.scopeKey(scope))
		f.c.Do(ctx, f.c.B().Del().Key(keys...).Build())
	}
	f.down.Store(down)
}

var truncate = valkey.NewLuaScript(`
local n = tonumber(redis.call('GET', KEYS[1]) or '0')
local keep = tonumber(ARGV[1])
for m = keep + 1, n do
  local id = redis.call('HGET', KEYS[5], m)
  if id then redis.call('HDEL', KEYS[4], id) end
  redis.call('HDEL', KEYS[5], m)
  redis.call('HDEL', KEYS[3], m)
  redis.call('ZREM', KEYS[6], m)
end
redis.call('SET', KEYS[1], keep)
redis.call('SET', KEYS[2], keep == 0 and 0 or (redis.call('HGET', KEYS[3], keep) or 0))
return 1`)

// Failover loses the counter after its first keep events.
func (f *valkeyFaults) Failover(sc string, keep int) {
	truncate.Exec(context.Background(), f.c, f.counterKeys(sc), []string{strconv.Itoa(keep)})
}

// Counter lists a scope's events in number order.
func (f *valkeyFaults) Counter(sc string) []Event {
	ctx := context.Background()
	k := f.counterKeys(sc)
	n, _ := f.c.Do(ctx, f.c.B().Get().Key(k[0]).Build()).AsInt64()
	var out []Event
	var prev int64
	for m := int64(1); m <= n; m++ {
		ms := strconv.FormatInt(m, 10)
		id, _ := f.c.Do(ctx, f.c.B().Hget().Key(k[4]).Field(ms).Build()).ToString()
		cum, _ := f.c.Do(ctx, f.c.B().Hget().Key(k[2]).Field(ms).Build()).AsInt64()
		out = append(out, Event{id, cum - prev})
		prev = cum
	}
	return out
}

func (f *valkeyFaults) Slots() []Slot {
	calls, _ := f.ScopeSlots(context.Background(), scope)
	slices.Sort(calls) // by expiry in Valkey; the model's is a set
	out := make([]Slot, len(calls))
	for i, c := range calls {
		out[i] = Slot{Call: c}
	}
	return out
}
