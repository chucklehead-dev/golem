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
// counter (its minutes), the slots, waivers, held increments and each
// gateway's known spend (while it uses the store) must be the model's. The
// ledger and golem-control (deliver, publishSnapshot) are the environment;
// applySnapshot gives a gateway the model's snapshot, its boundary a model
// minute on the driver's clock.

// The exposureDesign instance's constants.
var owner = map[string]string{"a1": "p1", "a2": "p1", "b1": "p2", "b2": "p2", "c1": "p3"}

const scope = "s"

// The model has one budget period: the driver's clock, inPeriod plus the
// model's minutes, stays in it.
var (
	inPeriod = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	period   = "2026-11"
)

// mbtRetain keeps every minute of a trace: inPeriod is long past by the
// stores' own clocks, which trim by them.
const mbtRetain = 1000 * 24 * time.Hour

// modelMinute is a time's minute on the model's clock.
func modelMinute(t time.Time) int64 { return minuteOf(t) - minuteOf(inPeriod) }

func call(request string) Call {
	return Call{ID: request, Principal: owner[request], Scope: scope, Budget: 4, PrincipalLimit: 1, ScopeLimit: 2}
}

type event struct {
	ID   string `itf:",tuple"`
	Cost int64
	At   int64
}

// faultyStore is a Store the test can fault as the model's environment does.
type faultyStore interface {
	Store
	SetDown(bool)
	Failover(scope, period string, minute int64)
	Buckets(scope, period string) map[int64]int64
	Slots() []Slot
}

type expDriver struct {
	t       testing.TB
	newSt   func(testing.TB) faultyStore
	store   faultyStore
	clock   int64 // the model's minute
	gws     map[string]*Limiter
	tickets map[string]*Ticket
	waived  map[string]bool // the model's ghost: every call ever waived
}

type snapRec struct {
	B, Spend int64
}

func (d *expDriver) now() time.Time { return inPeriod.Add(time.Duration(d.clock) * time.Minute) }

func (d *expDriver) newLimiter() *Limiter {
	l := New(d.store)
	l.Now = d.now
	// The model ends a grace by graceEnds, not by its clock's minutes.
	l.Grace = mbtRetain
	return l
}

func (d *expDriver) Step(s *connect.Step) error {
	ctx := d.t.Context()
	req := func() string { return connect.MustPick[string](s, "request") }
	gw := func() string { return connect.MustPick[string](s, "gateway") }
	cost := func() int64 { return connect.MustPick[int64](s, "cost") }
	return connect.Switch{
		"init": func(*connect.Step) error {
			d.store, d.clock = d.newSt(d.t), 0
			d.gws = map[string]*Limiter{"g1": d.newLimiter(), "g2": d.newLimiter()}
			d.tickets, d.waived = map[string]*Ticket{}, map[string]bool{}
			return nil
		},
		"admit": func(s *connect.Step) error {
			c := call(req())
			if b, ok := s.Picks["budget"]; ok { // a varied budget (TestStoresAgreeVaried)
				if err := itf.Decode(b, &c.Budget); err != nil {
					return err
				}
			}
			t, err := d.gws[gw()].Admit(ctx, c)
			if err != nil {
				return err // the model admitted it: so must the gateway
			}
			d.tickets[req()] = t
			if t.Waived() {
				d.waived[req()] = true
			}
			return nil
		},
		"complete":     func(*connect.Step) error { return d.ticket(req(), func(t *Ticket) { t.Complete(ctx, cost()) }) },
		"cancel":       func(*connect.Step) error { return d.ticket(req(), func(t *Ticket) { t.Cancel(ctx, cost()) }) },
		"providerEnds": func(*connect.Step) error { return d.ticket(req(), func(t *Ticket) { t.Ended(ctx) }) },
		"tick":         func(*connect.Step) error { d.clock++; return nil },
		// The ledger and golem-control: the environment.
		"deliver": connect.Skip, "publishSnapshot": connect.Skip,
		"applySnapshot": func(s *connect.Step) error {
			var snaps map[string]snapRec
			if err := itf.Decode(s.Get("snaps"), &snaps); err != nil {
				return err
			}
			sn := snaps[gw()]
			d.gws[gw()].ApplySnapshot(ctx, scope, Known{Period: period, Spend: sn.Spend,
				Since: inPeriod.Add(time.Duration(sn.B) * time.Minute)})
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
			// its own order, so bring the model's to the front (the seqs
			// stay in place: they order what is held).
			e := connect.MustPick[event](s, "e")
			l.mu.Lock()
			i := slices.IndexFunc(l.pending, func(h held) bool { return h.Call == e.ID })
			if i >= 0 {
				l.pending[0].Increment, l.pending[i].Increment = l.pending[i].Increment, l.pending[0].Increment
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
			d.store.Failover(scope, period, minuteOf(inPeriod)+connect.MustPick[int64](s, "t"))
			return nil
		},
	}.Step(s)
}

// ticket runs f on the request's ticket; a request with none (its
// admission refused, under a varied budget) is an error, not a panic.
func (d *expDriver) ticket(req string, f func(*Ticket)) error {
	t := d.tickets[req]
	if t == nil {
		return fmt.Errorf("request %s was not admitted", req)
	}
	f(t)
	return nil
}

// expState projects the model's variables the limiter has.
type expState struct {
	Counter map[int64]int64
	Slots   []string
	Waived  []string
	Pending map[string][]event
}

func (d *expDriver) State() (any, error) {
	st := expState{Counter: map[int64]int64{}, Slots: []string{}, Waived: slices.Sorted(maps.Keys(d.waived)), Pending: map[string][]event{}}
	if st.Waived == nil {
		st.Waived = []string{}
	}
	for m, spend := range d.store.Buckets(scope, period) {
		st.Counter[m-minuteOf(inPeriod)] = spend
	}
	for _, s := range d.store.Slots() {
		st.Slots = append(st.Slots, s.Call)
	}
	for _, g := range slices.Sorted(maps.Keys(d.gws)) {
		ps := []event{}
		for _, in := range d.gws[g].Pending(scope) {
			ps = append(ps, event{in.Call, in.Cost, modelMinute(in.At)})
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
	var counter map[int64]int64
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
		for m, spend := range counter {
			if m >= snaps[g].B {
				want += spend
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
	// Require: every scenario check-exposure.sh shows the design can reach
	// is reached by the traces that drive the limiter (or steered to), so
	// the limiter is tested in each, not only where sampling happens to go.
	connect.Run(t, connect.Config{Spec: "../../model/exposure.qnt", Main: "exposureDesign", MaxSamples: 100, MaxSteps: 30,
		Require: []string{"overshoots", "waiverExceedsLimit", "cancelledBilledMore", "handoffInFlight", "snapshotSkew"}},
		func(tb testing.TB) connect.Driver {
			return &expDriver{t: tb, newSt: func(testing.TB) faultyStore { m := NewMem(); m.Retain = mbtRetain; return m }}
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
				v := newValkey(tb, addr)
				v.Retain = mbtRetain
				return &valkeyFaults{Valkey: v}
			}}
		})
}
