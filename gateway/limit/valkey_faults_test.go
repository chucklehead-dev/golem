package limit

import (
	"context"
	"slices"
	"strconv"
	"sync/atomic"
	"time"
)

// valkeyFaults injects the model's environment into a Valkey store: an
// outage (every call fails, and the slots' TTLs lapse) and a failover (a
// minute of the counter lost).
type valkeyFaults struct {
	*Valkey
	down atomic.Bool
}

func (f *valkeyFaults) Admit(ctx context.Context, a Admission) (Outcome, int64, error) {
	if f.down.Load() {
		return 0, 0, ErrUnavailable
	}
	return f.Valkey.Admit(ctx, a)
}

func (f *valkeyFaults) Complete(ctx context.Context, in Increment, s Slot) error {
	if f.down.Load() {
		return ErrUnavailable
	}
	return f.Valkey.Complete(ctx, in, s)
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

func (f *valkeyFaults) Add(ctx context.Context, in Increment) error {
	if f.down.Load() {
		return ErrUnavailable
	}
	return f.Valkey.Add(ctx, in)
}

func (f *valkeyFaults) AddAll(ctx context.Context, incs []Increment) error {
	if f.down.Load() {
		return ErrUnavailable
	}
	return f.Valkey.AddAll(ctx, incs)
}

func (f *valkeyFaults) Since(ctx context.Context, scope, period string, since time.Time) (int64, error) {
	if f.down.Load() {
		return 0, ErrUnavailable
	}
	return f.Valkey.Since(ctx, scope, period, since)
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

// Failover loses a minute of the counter, and its share of the total, as a
// replica missing those writes would.
func (f *valkeyFaults) Failover(sc, period string, minute int64) {
	ctx := context.Background()
	k, field := f.counterKey(sc, period), strconv.FormatInt(minute, 10)
	lost, _ := f.c.Do(ctx, f.c.B().Hget().Key(k).Field(field).Build()).AsInt64()
	f.c.Do(ctx, f.c.B().Hdel().Key(k).Field(field).Build())
	f.c.Do(ctx, f.c.B().Hincrby().Key(k).Field("total").Increment(-lost).Build())
}

// Buckets is the counter, minute to spend.
func (f *valkeyFaults) Buckets(sc, period string) map[int64]int64 {
	m, _ := f.c.Do(context.Background(), f.c.B().Hgetall().Key(f.counterKey(sc, period)).Build()).AsStrMap()
	out := map[int64]int64{}
	for k, v := range m {
		minute, err := strconv.ParseInt(k, 10, 64)
		if err != nil {
			continue // "total"
		}
		spend, _ := strconv.ParseInt(v, 10, 64)
		out[minute] = spend
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
