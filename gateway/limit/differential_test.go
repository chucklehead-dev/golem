package limit

import (
	"maps"
	"math/rand/v2"
	"os/exec"
	"slices"
	"testing"

	"github.com/chucklehead-dev/gotogether/connect"
	"github.com/chucklehead-dev/gotogether/itf"
)

// The two stores are two implementations of one contract: whatever the
// limiter asks, they must answer alike. Each is checked against the model
// on its own (TestExposureModel, TestExposureModelValkey), but two that
// each agree with the model can still disagree where the model never goes:
// a budget of zero, below zero or past what a Lua number holds exactly.

// observed is what a gateway sees beyond the model's state: each
// gateway's known spend.
type observed struct {
	State expState
	Known map[string]int64
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
		o.Known[g] = d.gws[g].Known(scope)
	}
	return o, nil
}

// stores makes a driver per store over the same trace.
func stores(tb testing.TB, addr string) map[string]connect.Driver {
	mem := &expDriver{t: tb, newSt: func(testing.TB) faultyStore { return NewMem() }}
	vk := &expDriver{t: tb, newSt: func(tb testing.TB) faultyStore { return &valkeyFaults{Valkey: newValkey(tb, addr)} }}
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
			return &connect.Differential{Impls: stores(tb, addr)}
		})
}

// TestStoresAgreeVaried: the same traces with budgets the model never
// picks, each step's drawn from a seed of its trace and index (so a
// failure replays): zero and below, and past 2^53 (where a Lua number is
// no longer an integer). The model's state no longer applies: the stores
// are compared with each other only.
func TestStoresAgreeVaried(t *testing.T) {
	if _, err := exec.LookPath("quint"); err != nil {
		t.Skip("quint not installed")
	}
	addr := valkeyAddr(t)
	budgets := []int64{-1, 0, 1, 4, 1 << 40, 1<<53 + 1}
	connect.Run(t, connect.Config{Spec: "../../model/exposure.qnt", Main: "exposureDesign", MaxSamples: 100, MaxSteps: 30},
		func(tb testing.TB) connect.Driver {
			return &connect.Differential{Impls: stores(tb, addr), IgnoreModel: true,
				Vary: func(s *connect.Step) error {
					if s.Action != "admit" {
						return nil
					}
					r := rand.New(rand.NewPCG(uint64(s.Trace), uint64(s.Index)))
					v, err := itf.Encode(budgets[r.IntN(len(budgets))])
					s.Picks["budget"] = v
					return err
				}}
		})
}
