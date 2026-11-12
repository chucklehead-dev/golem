package limit

import (
	"context"
	"slices"
	"sync/atomic"
)

// valkeyFaults injects the model's environment into a Valkey store: an
// outage (every call fails, and the slots' TTLs lapse) and a failover (the
// counter loses events).
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

func (f *valkeyFaults) Add(ctx context.Context, scope string, es ...Event) error {
	if f.down.Load() {
		return ErrUnavailable
	}
	return f.Valkey.Add(ctx, scope, es...)
}

func (f *valkeyFaults) Events(ctx context.Context, scope string) ([]Event, error) {
	if f.down.Load() {
		return nil, ErrUnavailable
	}
	return f.Valkey.Events(ctx, scope)
}

func (f *valkeyFaults) Forget(ctx context.Context, scope string, ids []string) error {
	if f.down.Load() {
		return ErrUnavailable
	}
	return f.Valkey.Forget(ctx, scope, ids)
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

func (f *valkeyFaults) Failover(sc string, keep func(string) bool) {
	ctx := context.Background()
	es, _ := f.Valkey.Events(ctx, sc)
	var lost []string
	for _, e := range es {
		if !keep(e.ID) {
			lost = append(lost, e.ID)
		}
	}
	f.Valkey.Forget(ctx, sc, lost)
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
