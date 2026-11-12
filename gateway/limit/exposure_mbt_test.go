package limit

import (
	"os/exec"
	"slices"
	"testing"

	"github.com/chucklehead-dev/gotogether/connect"
	"github.com/chucklehead-dev/gotogether/itf"
)

// The limiter against model/exposure.md's design: the model's traces drive
// one Limiter per gateway over one shared store, and after every step the
// slots, waivers, held increments and (while the store is reachable) each
// gateway's known spend must be the model's. The ledger (deliver) is the
// environment; a snapshot's known spend is the model's own.

// The exposureDesign instance's constants.
var owner = map[string]string{"a1": "p1", "a2": "p1", "b1": "p2", "b2": "p2", "c1": "p3"}

const scope = "s"

func call(request string) Call {
	return Call{ID: request, Principal: owner[request], Scope: scope, Budget: 4, PrincipalLimit: 1, ScopeLimit: 2}
}

type event struct {
	ID   string `itf:",tuple"`
	Cost int64
}

// faultyStore is a Store the test can fault as the model's environment does.
type faultyStore interface {
	Store
	SetDown(bool)
	Failover(scope string, keep func(id string) bool)
	Slots() []Slot
}

type expDriver struct {
	t       testing.TB
	newSt   func(testing.TB) faultyStore
	store   faultyStore
	gws     map[string]*Limiter
	tickets map[string]*Ticket
}

func (d *expDriver) Step(s *connect.Step) error {
	ctx := d.t.Context()
	req := func() string { return connect.MustPick[string](s, "request") }
	cost := func() int64 { return connect.MustPick[int64](s, "cost") }
	return connect.Switch{
		"init": func(*connect.Step) error {
			d.store = d.newSt(d.t)
			d.gws = map[string]*Limiter{"g1": New(d.store), "g2": New(d.store)}
			d.tickets = map[string]*Ticket{}
			return nil
		},
		"admit": func(s *connect.Step) error {
			t, err := d.gws[connect.MustPick[string](s, "gateway")].Admit(ctx, call(req()))
			d.tickets[req()] = t
			return err // the model admitted it: so must the gateway
		},
		"complete":     func(*connect.Step) error { d.tickets[req()].Complete(ctx, cost()); return nil },
		"cancel":       func(*connect.Step) error { d.tickets[req()].Cancel(ctx, cost()); return nil },
		"providerEnds": func(*connect.Step) error { d.tickets[req()].Ended(ctx); return nil },
		"deliver":      connect.Skip, // the ledger's ingestion: the environment
		"publishSnapshot": func(s *connect.Step) error {
			k, err := snapOf(s)
			for _, g := range d.gws {
				g.ApplySnapshot(ctx, scope, k)
			}
			return err
		},
		"valkeyDown": func(*connect.Step) error { d.store.SetDown(true); return nil },
		"valkeyBack": func(*connect.Step) error {
			d.store.SetDown(false)
			for _, g := range slices.Sorted(keys(d.gws)) {
				if err := d.gws[g].Recover(ctx); err != nil {
					return err
				}
			}
			return nil
		},
		"failover": func(s *connect.Step) error {
			k, err := snapOf(s)
			d.store.Failover(scope, func(id string) bool { return k.Included[id] })
			return err
		},
	}.Step(s)
}

func snapOf(s *connect.Step) (Known, error) {
	var snap struct {
		Events []event
		Spend  int64
	}
	if err := itf.Decode(s.Get("snap"), &snap); err != nil {
		return Known{}, err
	}
	k := Known{Spend: snap.Spend, Included: map[string]bool{}}
	for _, e := range snap.Events {
		k.Included[e.ID] = true
	}
	return k, nil
}

// expState projects the model's variables the limiter has.
type expState struct {
	Slots   []string
	Waived  []string
	Pending []event
}

func (d *expDriver) State() (any, error) {
	st := expState{Slots: []string{}, Waived: []string{}, Pending: []event{}}
	for _, s := range d.store.Slots() {
		st.Slots = append(st.Slots, s.Call)
	}
	for _, g := range slices.Sorted(keys(d.gws)) {
		st.Waived = append(st.Waived, d.gws[g].Waived()...)
		for _, e := range d.gws[g].Pending(scope) {
			st.Pending = append(st.Pending, event{e.ID, e.Cost})
		}
	}
	slices.Sort(st.Waived)
	st.Waived = slices.Compact(st.Waived)
	slices.SortFunc(st.Pending, func(a, b event) int { return compare(a.ID, b.ID) })
	st.Pending = slices.Compact(st.Pending)
	return st, nil
}

// CheckStep compares known spend, a def of the model's rather than a
// variable: snapshot plus the counter's events it does not include.
func (d *expDriver) CheckStep(s *connect.Step) error {
	var up bool
	if err := itf.Decode(s.Get("valkeyUp"), &up); err != nil || !up {
		return err // while the store is down a gateway's known spend is its last view
	}
	k, err := snapOf(s)
	if err != nil {
		return err
	}
	var counter []event
	if err := itf.Decode(s.Get("counter"), &counter); err != nil {
		return err
	}
	want := k.Spend
	for _, e := range counter {
		if !k.Included[e.ID] {
			want += e.Cost
		}
	}
	for _, g := range slices.Sorted(keys(d.gws)) {
		if got := d.gws[g].Known(d.t.Context(), scope); got != want {
			return errorf("gateway %s: known spend %d, the model's %d", g, got, want)
		}
	}
	return nil
}

func TestExposureModel(t *testing.T) {
	if _, err := exec.LookPath("quint"); err != nil {
		t.Skip("quint not installed")
	}
	connect.Run(t, connect.Config{Spec: "../../model/exposure.qnt", Main: "exposureDesign", MaxSamples: 100, MaxSteps: 30},
		func(tb testing.TB) connect.Driver {
			return &expDriver{t: tb, newSt: func(testing.TB) faultyStore { return NewMem() }}
		})
}

// TestExposureModelValkey: the same, over the Valkey store, its outages and
// failovers injected by a wrapper.
func TestExposureModelValkey(t *testing.T) {
	if _, err := exec.LookPath("quint"); err != nil {
		t.Skip("quint not installed")
	}
	addr := valkeyAddr(t)
	connect.Run(t, connect.Config{Spec: "../../model/exposure.qnt", Main: "exposureDesign", MaxSamples: 50, MaxSteps: 30},
		func(tb testing.TB) connect.Driver {
			return &expDriver{t: tb, newSt: func(tb testing.TB) faultyStore {
				return &valkeyFaults{Valkey: newValkey(tb.(*testing.T), addr)}
			}}
		})
}
