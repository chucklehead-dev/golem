package limit

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/chucklehead-dev/gotogether/connect"
	"github.com/chucklehead-dev/gotogether/itf"
)

// The two stores are two implementations of one contract: whatever the
// limiter asks, they must answer alike. Each is checked against the model
// on its own (TestExposureModel, TestExposureModelValkey), but two that
// each agree with the model can still disagree where the model never goes:
// a negative or huge cost, a budget of zero, history past retention.

// observed is what a gateway sees of the store beyond the model's state:
// each gateway's known spend, read afresh.
type observed struct {
	State expState
	Known map[string]int64
	// Admits is the known spend an admission computes from each of the
	// trace's minutes as its boundary (a varied run's): the admit script's
	// read and Mem's, compared where the boundary is older than the
	// retention too.
	Admits map[int64]int64
}

// Observe is the state and every gateway's known spend (connect.Observer):
// a Differential compares them across the stores.
func (d *expDriver) Observe() (any, error) {
	st, err := d.State()
	if err != nil {
		return nil, err
	}
	o := observed{State: st.(expState), Known: map[string]int64{}}
	for _, g := range slices.Sorted(maps.Keys(d.gws)) {
		o.Known[g] = d.gws[g].Known(d.t.Context(), scope)
	}
	return o, nil
}

// byReads is a driver whose stores are compared through what reads return,
// not their raw minutes (a varied run's: its short retention trims each
// store on its own schedule). Under IgnoreModel the Differential runs no
// driver's model checks, so nothing else needs hiding.
type byReads struct{ d *expDriver }

func (u byReads) Step(s *connect.Step) error { return u.d.Step(s) }

// Observe is what reads see: a store keeps minutes past retention until it
// trims them, and each trims on its own schedule, so the counter is
// compared as every read from each of the trace's minutes returns it.
func (u byReads) Observe() (any, error) {
	o, err := u.d.Observe()
	if err != nil {
		return nil, err
	}
	ob := o.(observed)
	ob.State.Counter, ob.Admits = map[int64]int64{}, map[int64]int64{}
	for b := int64(1); b <= u.d.clock+1; b++ {
		since := inPeriod.Add(time.Duration(b) * time.Minute)
		n, err := u.d.store.Since(u.d.t.Context(), scope, period, since)
		if err != nil {
			n = -1 // down: both stores must be
		}
		ob.State.Counter[b] = n
		// A budget below zero refuses whatever the known spend, so the
		// probe takes no slot; it still reports what it computed.
		r, err := u.d.store.Admit(u.d.t.Context(), Admission{Slot: Slot{"probe", "probe", scope}, Period: period,
			Accounts: []Account{{ID: scope, Since: since, Spend: 1, Limit: -1}}, Now: u.d.now()})
		k := r.Known[0]
		if err != nil {
			k = -1
		}
		ob.Admits[b] = k
	}
	return ob, nil
}

// stores makes a driver per store over the same trace; retain is both
// stores' retention, and both trim by the model's clock.
func stores(tb testing.TB, addr string, retain time.Duration, byRead bool) map[string]connect.Driver {
	var drivers []*expDriver
	clock := func() time.Time { // the trace's clock: the drivers step together
		return drivers[0].now()
	}
	mem := &expDriver{t: tb, newSt: func(testing.TB) faultyStore {
		m := NewMem()
		m.Retain, m.Now = retain, clock
		return m
	}}
	vk := &expDriver{t: tb, newSt: func(tb testing.TB) faultyStore {
		v := newValkey(tb, addr)
		v.Retain, v.Now = retain, clock
		return &valkeyFaults{Valkey: v}
	}}
	drivers = []*expDriver{mem, vk}
	if byRead {
		return map[string]connect.Driver{"mem": byReads{mem}, "valkey": byReads{vk}}
	}
	return map[string]connect.Driver{"mem": mem, "valkey": vk}
}

// TestStoresAgree: the model's traces on both stores at once; each must be
// the model's after every step, and they must see the same known spend.
func TestStoresAgree(t *testing.T) {
	if _, err := exec.LookPath("quint"); err != nil {
		t.Skip("quint not installed")
	}
	addr := valkeyAddr(t)
	connect.Run(t, connect.Config{Spec: "../../model/exposure.qnt", Main: "exposureDesign", MaxSamples: 50, MaxSteps: 30},
		func(tb testing.TB) connect.Driver {
			return &connect.Differential{Impls: stores(tb, addr, mbtRetain, false)}
		})
}

// TestStoresAgreeVaried: the same traces with inputs the model never picks,
// each step's drawn from a seed of its trace and index (so a failure
// replays): costs negative, zero and past 2^53 (where a Lua number is no
// longer an integer), budgets of zero and below, and a retention shorter
// than the trace, so its history is trimmed. The model's state no longer
// applies: the stores are compared with each other only.
func TestStoresAgreeVaried(t *testing.T) {
	if _, err := exec.LookPath("quint"); err != nil {
		t.Skip("quint not installed")
	}
	addr := valkeyAddr(t)
	costs := []int64{-3, -1, 0, 1, 2, 7, 1 << 40, 1<<53 + 1}
	budgets := []int64{-1, 0, 1, 4, 1 << 40, 1<<53 + 1}
	connect.Run(t, connect.Config{Spec: "../../model/exposure.qnt", Main: "exposureDesign", MaxSamples: 100, MaxSteps: 30},
		func(tb testing.TB) connect.Driver {
			return &connect.Differential{Impls: stores(tb, addr, 5*time.Minute, true), IgnoreModel: true,
				Vary: func(s *connect.Step) error {
					r := rand.New(rand.NewPCG(uint64(s.Trace), uint64(s.Index)))
					set := func(name string, from []int64) error {
						v, err := itf.Encode(from[r.IntN(len(from))])
						s.Picks[name] = v
						return err
					}
					switch s.Action {
					case "admit":
						return set("budget", budgets)
					case "complete", "cancel":
						if _, ok := s.Picks["cost"]; !ok {
							return fmt.Errorf("step %d (%s) has no cost pick", s.Index, s.Action)
						}
						return set("cost", costs)
					}
					return nil
				}}
		})
}
