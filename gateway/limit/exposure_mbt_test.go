package limit

import (
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/gotogether/connect"
	"github.com/chucklehead-dev/gotogether/itf"
)

// The limiter against model/exposure.md's design: the model's traces drive
// one Limiter per gateway over one shared store, and after every step the
// counter (its events in number order), the slots, waivers, held
// increments and each gateway's known spend (while it uses the store) must
// be the model's. The ledger and golem-control (deliver, publishSnapshot)
// are the environment; applySnapshot gives a gateway the model's snapshot.

// The exposureDesign instance's constants.
var owner = map[string]string{"a1": "p1", "a2": "p1", "b1": "p2", "b2": "p2", "c1": "p3"}

const scope = "s"

// The model has one budget period: the driver's clock stays in it.
var (
	inPeriod = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	period   = "2026-11"
)

func newLimiter(st Store) *Limiter {
	l := New(st)
	l.Now = func() time.Time { return inPeriod }
	return l
}

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
	Failover(scope, period string, keep int)
	Counter(scope, period string) []Event
	Slots() []Slot
}

type expDriver struct {
	t       testing.TB
	newSt   func(testing.TB) faultyStore
	store   faultyStore
	gws     map[string]*Limiter
	tickets map[string]*Ticket
	waived  map[string]bool // the model's ghost: every call ever waived
}

type snapRec struct {
	W, Spend int64
}

func (d *expDriver) Step(s *connect.Step) error {
	ctx := d.t.Context()
	req := func() string { return connect.MustPick[string](s, "request") }
	gw := func() string { return connect.MustPick[string](s, "gateway") }
	cost := func() int64 { return connect.MustPick[int64](s, "cost") }
	return connect.Switch{
		"init": func(*connect.Step) error {
			d.store = d.newSt(d.t)
			d.gws = map[string]*Limiter{"g1": newLimiter(d.store), "g2": newLimiter(d.store)}
			d.tickets, d.waived = map[string]*Ticket{}, map[string]bool{}
			return nil
		},
		"admit": func(s *connect.Step) error {
			t, err := d.gws[gw()].Admit(ctx, call(req()))
			if err != nil {
				return err // the model admitted it: so must the gateway
			}
			d.tickets[req()] = t
			if t.Waived() {
				d.waived[req()] = true
			}
			return nil
		},
		"complete":     func(*connect.Step) error { d.tickets[req()].Complete(ctx, cost()); return nil },
		"cancel":       func(*connect.Step) error { d.tickets[req()].Cancel(ctx, cost()); return nil },
		"providerEnds": func(*connect.Step) error { d.tickets[req()].Ended(ctx); return nil },
		// The ledger and golem-control: the environment.
		"deliver": connect.Skip, "publishSnapshot": connect.Skip,
		"applySnapshot": func(s *connect.Step) error {
			var snaps map[string]snapRec
			if err := itf.Decode(s.Get("snaps"), &snaps); err != nil {
				return err
			}
			sn := snaps[gw()]
			d.gws[gw()].ApplySnapshot(ctx, scope, Known{Period: period, Spend: sn.Spend, Watermark: sn.W})
			return nil
		},
		"valkeyDown": func(*connect.Step) error {
			d.store.SetDown(true)
			for _, g := range slices.Sorted(maps.Keys(d.gws)) {
				d.gws[g].Renew(ctx) // fails: each gateway notices
			}
			return nil
		},
		"valkeyBack": func(*connect.Step) error { d.store.SetDown(false); return nil },
		"replayOne": func(s *connect.Step) error {
			l := d.gws[gw()]
			// The model picks which held increment; the gateway replays in
			// its own order, so bring the model's to the front.
			e := connect.MustPick[event](s, "e")
			l.mu.Lock()
			i := slices.IndexFunc(l.pending, func(h held) bool { return h.e.ID == e.ID })
			if i >= 0 {
				l.pending[0], l.pending[i] = l.pending[i], l.pending[0]
			}
			l.mu.Unlock()
			if i < 0 {
				return fmt.Errorf("gateway %s holds no increment %s", gw(), e.ID)
			}
			_, _, err := l.ReplayOne(ctx)
			return err
		},
		"recover":   func(*connect.Step) error { return d.gws[gw()].Recover(ctx) },
		"graceEnds": func(*connect.Step) error { d.gws[gw()].EndGrace(); return nil },
		"failover": func(s *connect.Step) error {
			d.store.Failover(scope, period, connect.MustPick[int](s, "keep"))
			return nil
		},
	}.Step(s)
}

// expState projects the model's variables the limiter has.
type expState struct {
	Counter []event
	Slots   []string
	Waived  []string
	Pending map[string][]event
}

func (d *expDriver) State() (any, error) {
	st := expState{Counter: []event{}, Slots: []string{}, Waived: slices.Sorted(maps.Keys(d.waived)), Pending: map[string][]event{}}
	if st.Waived == nil {
		st.Waived = []string{}
	}
	for _, e := range d.store.Counter(scope, period) {
		st.Counter = append(st.Counter, event{e.ID, e.Cost})
	}
	for _, s := range d.store.Slots() {
		st.Slots = append(st.Slots, s.Call)
	}
	for _, g := range slices.Sorted(maps.Keys(d.gws)) {
		ps := []event{}
		for _, e := range d.gws[g].Pending(scope) {
			ps = append(ps, event{e.ID, e.Cost})
		}
		slices.SortFunc(ps, func(a, b event) int { return strings.Compare(a.ID, b.ID) })
		st.Pending[g] = ps
	}
	return st, nil
}

// CheckStep compares each gateway's known spend (a def of the model's) for
// the gateways that use the store.
func (d *expDriver) CheckStep(s *connect.Step) error {
	var up bool
	var down []string
	var snaps map[string]snapRec
	var counter []event
	for _, c := range []struct {
		path string
		dst  any
	}{{"valkeyUp", &up}, {"gwDown", &down}, {"snaps", &snaps}, {"counter", &counter}} {
		if err := itf.Decode(s.Get(c.path), c.dst); err != nil {
			return err
		}
	}
	for _, g := range slices.Sorted(maps.Keys(d.gws)) {
		if !up || slices.Contains(down, g) {
			continue // a gateway not using the store decides from its last view
		}
		want := snaps[g].Spend
		for i, e := range counter {
			if int64(i) >= snaps[g].W {
				want += e.Cost
			}
		}
		if got := d.gws[g].Known(d.t.Context(), scope); got != want {
			return fmt.Errorf("gateway %s: known spend %d, the model's %d", g, got, want)
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
				return &valkeyFaults{Valkey: newValkey(tb, addr)}
			}}
		})
}
