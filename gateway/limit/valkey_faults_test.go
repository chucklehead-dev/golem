package limit

import (
	"context"
	"slices"
	"sync/atomic"
	"time"

	"github.com/valkey-io/valkey-go"
)

// mustClient is the store's client, for a test's own commands.
func (v *Valkey) mustClient() valkey.Client {
	c, err := v.client()
	if err != nil {
		panic(err)
	}
	return c
}

// valkeyFaults injects the model's environment into a Valkey store: an
// outage (every call fails, and the slots' TTLs lapse).
type valkeyFaults struct {
	*Valkey
	down atomic.Bool
}

func (f *valkeyFaults) Admit(ctx context.Context, a Admission) (Outcome, error) {
	if f.down.Load() {
		return 0, ErrUnavailable
	}
	return f.Valkey.Admit(ctx, a)
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

func (f *valkeyFaults) Take(ctx context.Context, ms []Meter) (bool, int, time.Duration, error) {
	if f.down.Load() {
		return false, 0, 0, ErrUnavailable
	}
	return f.Valkey.Take(ctx, ms)
}

// SetDown: going down, every slot lapses (the outage outlasts the TTL).
func (f *valkeyFaults) SetDown(down bool) {
	ctx := context.Background()
	if down {
		var keys []string
		for _, p := range []string{"p1", "p2", "p3"} {
			keys = append(keys, f.principalKey(scope, p))
		}
		keys = append(keys, f.scopeKey(scope))
		f.mustClient().Do(ctx, f.mustClient().B().Del().Key(keys...).Build())
	}
	f.down.Store(down)
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
