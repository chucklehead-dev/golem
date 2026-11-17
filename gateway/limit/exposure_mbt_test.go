package limit

import (
	"fmt"
	"maps"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/chucklehead-dev/gotogether/connect"
	"github.com/chucklehead-dev/gotogether/itf"
)

// The limiter against model/exposure.md's design: the model's traces drive
// one Limiter per gateway over one shared store, and after every step the
// slots, the waivers and each gateway's known spend must be the model's.
// The ledger and golem-control (publishSnapshot) are the environment;
// applySnapshot gives a gateway the model's snapshot.

// The exposureDesign instance's constants.
var owner = map[string]string{"a1": "p1", "a2": "p1", "b1": "p2", "b2": "p2", "c1": "p3"}

const scope = "s"

// The model has one budget period: the driver's clock stays in it.
var (
	inPeriod  = time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	thePeriod = "2026-11"
)

func call(request string) Call {
	return Call{ID: request, Principal: owner[request], Scope: scope, Budget: 4, PrincipalLimit: 1, ScopeLimit: 2}
}

// faultyStore is a Store the test can fault as the model's environment does.
type faultyStore interface {
	Store
	SetDown(bool)
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

func (d *expDriver) newLimiter() *Limiter {
	l := New(d.store)
	l.Now = func() time.Time { return inPeriod }
	// The model ends a grace by graceEnds, not by a clock.
	l.Grace = 1000 * time.Hour
	return l
}

func (d *expDriver) Step(s *connect.Step) error {
	ctx := d.t.Context()
	req := func() string { return connect.MustPick[string](s, "request") }
	gw := func() string { return connect.MustPick[string](s, "gateway") }
	cost := func() int64 { return connect.MustPick[int64](s, "cost") }
	return connect.Switch{
		"init": func(*connect.Step) error {
			d.store = d.newSt(d.t)
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
		// The ledger and golem-control: the environment.
		"publishSnapshot": connect.Skip,
		"applySnapshot": func(s *connect.Step) error {
			var snaps map[string]int64
			if err := itf.Decode(s.Get("snaps"), &snaps); err != nil {
				return err
			}
			d.gws[gw()].ApplySnapshot(ctx, scope, ScopeSpend{Period: thePeriod, Spend: snaps[gw()]})
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
		"recover":    func(*connect.Step) error { return d.gws[gw()].Recover(ctx) },
		"graceEnds":  func(*connect.Step) error { d.gws[gw()].endGrace(); return nil },
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
	Slots  []string
	Waived []string
}

func (d *expDriver) State() (any, error) {
	st := expState{Slots: []string{}, Waived: slices.Sorted(maps.Keys(d.waived))}
	if st.Waived == nil {
		st.Waived = []string{}
	}
	for _, s := range d.store.Slots() {
		st.Slots = append(st.Slots, s.Call)
	}
	return st, nil
}

// CheckStep compares each gateway's known spend (the model's def known:
// its snapshot's) with the limiter's.
func (d *expDriver) CheckStep(s *connect.Step) error {
	var snaps map[string]int64
	if err := itf.Decode(s.Get("snaps"), &snaps); err != nil {
		return err
	}
	for _, g := range slices.Sorted(maps.Keys(d.gws)) {
		if got := d.gws[g].KnownSpend(scope); got != snaps[g] {
			return fmt.Errorf("gateway %s: known spend %d, the model's %d", g, got, snaps[g])
		}
	}
	return nil
}

//quint:checks exposure.limitsHonoured, exposure.withinBound
func TestExposureModel(t *testing.T) {
	if _, err := exec.LookPath("quint"); err != nil {
		t.Skip("quint not installed")
	}
	// Require: every scenario check-exposure.sh shows the design can reach
	// is reached by the traces that drive the limiter (or steered to), so
	// the limiter is tested in each, not only where sampling happens to go.
	connect.Run(t, connect.Config{Spec: "../../model/exposure.qnt", Main: "exposureDesign", MaxSamples: 100, MaxSteps: 30,
		Require: []string{"overshoots", "waiverExceedsLimit", "cancelledBilledMore", "snapshotLags"}},
		func(tb testing.TB) connect.Driver {
			return &expDriver{t: tb, newSt: func(testing.TB) faultyStore { return NewMem() }}
		})
}

// TestExposureModelValkey: the same, over the Valkey store, its outages
// injected by a wrapper.
//
//quint:checks exposure.limitsHonoured, exposure.withinBound
func TestExposureModelValkey(t *testing.T) {
	if _, err := exec.LookPath("quint"); err != nil {
		t.Skip("quint not installed")
	}
	addr := valkeyAddr(t)
	connect.Run(t, connect.Config{Spec: "../../model/exposure.qnt", Main: "exposureDesign", MaxSamples: 50, MaxSteps: 30},
		func(tb testing.TB) connect.Driver {
			return &expDriver{t: tb, newSt: func(tb testing.TB) faultyStore { return &valkeyFaults{Valkey: newValkey(tb, addr)} }}
		})
}
