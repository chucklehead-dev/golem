package snapshot

import (
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/chucklehead-dev/gotogether/connect"
	"github.com/chucklehead-dev/gotogether/itf"

	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// The gateway's half of model/publication.md, checked against the real
// Holder: traces of the model's design drive it, and after every step each
// gateway's applied publication and floor must be the model's.
//
// The publisher side (leases, fencing, the pointer, replays and forged
// status) is the environment here, since golem-control is not built yet:
// its actions are skipped, and poll hands the gateway the publication the
// model's publisher put in the pointer.

// pub is the model's Pub record.
type pub struct {
	Seq        int
	ValidUntil int
}

// epoch is the model's clock zero; one model tick is one minute.
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(tick int) time.Time { return epoch.Add(time.Duration(tick) * time.Minute) }

type pubDriver struct {
	clock    int
	gateways map[string]*Holder
}

func (d *pubDriver) holder() *Holder {
	return &Holder{Now: func() time.Time { return at(d.clock) }}
}

func (d *pubDriver) Step(s *connect.Step) error {
	gateway := func() string { return connect.MustPick[string](s, "gateway") }
	return connect.Switch{
		"init": func(s *connect.Step) error {
			// Every gateway starts holding the genesis publication.
			var gws map[string]struct{ Applied pub }
			if err := connectDecode(s, &gws, "gws"); err != nil {
				return err
			}
			d.clock = 0
			d.gateways = map[string]*Holder{}
			for g, gw := range gws {
				h := d.holder()
				if err := h.Apply(publication(gw.Applied), "genesis", nil); err != nil {
					return err
				}
				d.gateways[g] = h
			}
			return nil
		},
		"tick": func(*connect.Step) error { d.clock++; return nil },
		"poll": func(s *connect.Step) error {
			var p pub
			if err := connectDecode(s, &p, "pointer"); err != nil {
				return err
			}
			return d.gateways[gateway()].Apply(publication(p), fmt.Sprint("seq-", p.Seq), nil)
		},
		"restartEmpty": func(s *connect.Step) error {
			// A restart with an empty disk: the floor comes from the trusted
			// publisher checkpoint (the design's TRUSTED_FLOOR).
			var rec struct{ Seq int }
			if err := connectDecode(s, &rec, "record"); err != nil {
				return err
			}
			h := d.holder()
			h.RaiseFloor(uint64(rec.Seq))
			d.gateways[gateway()] = h
			return nil
		},
		"serve": func(s *connect.Step) error {
			// The model serves; the gateway must too. (Which keys are revoked is
			// the entitlements' part of a snapshot, not built yet: the lease is
			// what the Holder decides.)
			_, err := d.gateways[gateway()].Current()
			return err
		},
		// The environment: golem-control and the attacker.
		"revoke": connect.Skip, "restore": connect.Skip, "takeLease": connect.Skip,
		"noticeLoss": connect.Skip, "publish": connect.Skip, "replayPointer": connect.Skip,
		"forgeStatus": connect.Skip, "partition": connect.Skip, "heal": connect.Skip, "loadState": connect.Skip,
	}.Step(s)
}

func publication(p pub) *snapshot.Snapshot {
	return &snapshot.Snapshot{Schema: snapshot.Schema, PublishSeq: uint64(p.Seq), ValidUntil: at(p.ValidUntil)}
}

func connectDecode(s *connect.Step, dst any, path ...string) error {
	return itf.Decode(s.Get(path...), dst)
}

// gwState is the model's Gw record, projected: what the gateway applied and
// its floor.
type gwState struct {
	Applied pub
	Floor   int
}

type pubState struct {
	Gws map[string]gwState
}

func (d *pubDriver) State() (any, error) {
	st := pubState{Gws: map[string]gwState{}}
	for _, g := range slices.Sorted(mapsKeys(d.gateways)) {
		h := d.gateways[g]
		gs := gwState{Applied: pub{Seq: -1}, Floor: int(h.Floor())}
		if c := h.cur.Load(); c != nil {
			gs.Applied = pub{Seq: int(c.Snapshot.PublishSeq), ValidUntil: int(c.Snapshot.ValidUntil.Sub(epoch) / time.Minute)}
		}
		st.Gws[g] = gs
	}
	return st, nil
}

func mapsKeys[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func TestPublicationModel(t *testing.T) {
	if os.Getenv("QUINT_BIN") == "" {
		if _, err := lookQuint(); err != nil {
			t.Skip("quint not installed (or set QUINT_BIN)")
		}
	}
	connect.Run(t, connect.Config{Spec: "../../model/publication.qnt", Main: "design", MaxSamples: 50, MaxSteps: 30},
		func(testing.TB) connect.Driver { return &pubDriver{} })
}
