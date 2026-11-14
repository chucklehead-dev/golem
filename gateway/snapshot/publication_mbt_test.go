package snapshot

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"maps"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/chucklehead-dev/gotogether/connect"
	"github.com/chucklehead-dev/gotogether/itf"

	"github.com/chucklehead-dev/golem/control/lead"
	"github.com/chucklehead-dev/golem/control/publish"
	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/bucket"
	"github.com/chucklehead-dev/golem/lib/sign"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// The gateway's half of model/publication.md, checked against the real
// Holder: traces of the model's design drive it, and after every step each
// gateway's applied publication and floor must be the model's.
//
// Publication goes through the real code too: publish is control/publish's
// Publisher (fenced by the record, as the model's FENCE), the pointer and
// snapshots live in a real bucket, poll is each gateway's Bucket source,
// and replayPointer is an attacker copying an old signed pointer back. A
// restart with an empty disk takes its floor from the trusted checkpoint
// (the design's TRUSTED_FLOOR). Leadership is real too: each replica is a
// control/lead Elector with its own view of the state and its own clock
// (a paused replica's stands still), takeLease is a replica taking the
// lapsed lease, noticeLoss its next try, and publish uses the epoch its
// elector believes. The authoritative state (revocations, restores,
// replicas' reads) remains the environment: it reaches the publisher as
// the state version the model's publish used.

// pub is the model's Pub record.
type pub struct {
	Seq        int
	ValidUntil int
}

// epoch is the model's clock zero; one model tick is one minute.
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(tick int) time.Time { return epoch.Add(time.Duration(tick) * time.Minute) }

// ctlReplica is a golem-control replica: its elector, its publisher and
// its clock.
type ctlReplica struct {
	el    *lead.Elector
	pub   *publish.Publisher
	clock time.Time
}

type pubDriver struct {
	t        testing.TB
	clock    int
	bucket   *bucket.Dir
	pub      *publish.Publisher // r1's: what the driver reads the state through
	replicas map[string]*ctlReplica
	keys     sign.Ed25519Keys
	signed   map[int][]byte // every pointer signed, by sequence: what a replay copies back
	gateways map[string]*Bucket
}

func (d *pubDriver) holder() *Holder {
	return &Holder{Now: func() time.Time { return at(d.clock) }}
}

func (d *pubDriver) source(h *Holder) *Bucket {
	return &Bucket{Bucket: d.bucket, Keys: d.keys, Pool: "pool", Holder: h}
}

// seed publishes the genesis publication (sequence 0, epoch 1) as the
// model's init has it: the record, the snapshot and its signed pointer.
func (d *pubDriver) seed(g pub) error {
	ctx := context.Background()
	s := publication(g)
	body, err := s.Encode()
	if err != nil {
		return err
	}
	hash := snapshot.Hash(body)
	if _, err := d.pub.State.Update(ctx, func(doc *state.Document) error {
		doc.Publications = map[string]state.Record{"pool": {Epoch: 1, Seq: 0, Source: 0}}
		return nil
	}); err != nil {
		return err
	}
	cp, err := snapshot.SignCheckpoint(ctx, snapshot.Checkpoint{Pool: "pool", PublishSeq: 0}, d.pub.Signer)
	if err != nil {
		return err
	}
	ptr, err := snapshot.SignPointer(ctx, snapshot.Pointer{Pool: "pool", PublishSeq: 0, Epoch: 1, Hash: hash}, d.pub.Signer)
	if err != nil {
		return err
	}
	for k, b := range map[string][]byte{snapshot.ObjectKey("pool", hash): body, snapshot.CheckpointKey("pool"): cp, snapshot.PointerKey("pool"): ptr} {
		if _, err := d.bucket.Put(ctx, k, b, ""); err != nil {
			return err
		}
	}
	d.signed[0] = ptr
	return nil
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
			var genesis pub
			if err := connectDecode(s, &genesis, "pointer"); err != nil {
				return err
			}
			d.clock = 0
			if d.bucket != nil {
				d.bucket.Close()
			}
			dir, err := os.MkdirTemp("", "publication-mbt-")
			if err != nil {
				return err
			}
			d.t.Cleanup(func() { os.RemoveAll(dir) })
			if d.bucket, err = bucket.OpenDir(dir); err != nil {
				return err
			}
			pk, sk, _ := ed25519.GenerateKey(nil)
			d.keys = sign.Ed25519Keys{"k": pk}
			var believes map[string]int
			if err := connectDecode(s, &believes, "believes"); err != nil {
				return err
			}
			d.replicas = map[string]*ctlReplica{}
			for r := range believes {
				c := &ctlReplica{clock: at(0)}
				st := &state.Store{Bucket: d.bucket}
				c.el = &lead.Elector{State: st, ID: r, Duration: 30 * time.Second, Now: func() time.Time { return c.clock }}
				c.pub = &publish.Publisher{State: st, Bucket: d.bucket, Signer: sign.Ed25519{ID: "k", Key: sk}, Pool: "pool",
					Now: func() time.Time { return at(d.clock) }}
				d.replicas[r] = c
			}
			d.pub = d.replicas["r1"].pub
			// The first leader, r1, holds the lease at epoch 1 (the model's init).
			if e, err := d.replicas["r1"].el.Try(context.Background()); err != nil || e != 1 {
				return fmt.Errorf("r1's first lease: epoch %d, %v", e, err)
			}
			d.signed = map[int][]byte{}
			if err := d.seed(genesis); err != nil {
				return err
			}
			d.gateways = map[string]*Bucket{}
			for g := range gws {
				src := d.source(d.holder())
				if err := src.Poll(context.Background()); err != nil {
					return fmt.Errorf("gateway %s, genesis: %w", g, err)
				}
				d.gateways[g] = src
			}
			return nil
		},
		"tick": func(*connect.Step) error { d.clock++; return nil },
		"poll": func(s *connect.Step) error {
			var p pub
			if err := connectDecode(s, &p, "pointer"); err != nil {
				return err
			}
			if err := d.gateways[gateway()].Poll(context.Background()); err != nil {
				return fmt.Errorf("the model applies publication %d; the gateway: %w", p.Seq, err)
			}
			return nil
		},
		"publish": func(s *connect.Step) error {
			// The model's publish, as it came out: its epoch, state version and
			// lease end are what the real publisher is given.
			var p struct{ Seq, Epoch, Source, ValidUntil int }
			if err := connectDecode(s, &p, "pointer"); err != nil {
				return err
			}
			r := d.replicas[connect.MustPick[string](s, "replica")]
			believed := r.el.Epoch()
			if int(believed) != p.Epoch {
				return fmt.Errorf("the model publishes at epoch %d; the replica's elector believes %d", p.Epoch, believed)
			}
			ptr, err := r.pub.Publish(context.Background(), publish.Lease{Holder: r.el.ID, Epoch: believed}, uint64(p.Source), func(seq uint64) (*snapshot.Snapshot, error) {
				return publication(pub{Seq: int(seq), ValidUntil: p.ValidUntil}), nil
			})
			if err != nil {
				return fmt.Errorf("the model publishes %d; the publisher: %w", p.Seq, err)
			}
			if int(ptr.PublishSeq) != p.Seq {
				return fmt.Errorf("the publisher numbered %d, the model %d", ptr.PublishSeq, p.Seq)
			}
			raw, _, err := d.bucket.Get(context.Background(), snapshot.PointerKey("pool"), "")
			if err != nil {
				return err
			}
			d.signed[p.Seq] = raw
			return nil
		},
		"replayPointer": func(s *connect.Step) error {
			old := connect.MustPick[pub](s, "old")
			raw, ok := d.signed[old.Seq]
			if !ok {
				return fmt.Errorf("replay of publication %d, never signed", old.Seq)
			}
			_, err := d.bucket.Put(context.Background(), snapshot.PointerKey("pool"), raw, "")
			return err
		},
		"restartEmpty": func(s *connect.Step) error {
			// A restart with an empty disk: the floor comes from the trusted
			// publisher checkpoint (the design's TRUSTED_FLOOR).
			// The restarted gateway's floor is the trusted checkpoint
			// golem-control wrote, read by the source as it starts.
			src := d.source(d.holder())
			if err := src.Start(context.Background()); err != nil {
				return err
			}
			d.gateways[gateway()] = src
			return nil
		},
		"serve": func(s *connect.Step) error {
			// The model serves; the gateway must too. (Which keys are revoked is
			// the entitlements' part of a snapshot, not built yet: the lease is
			// what the Holder decides.)
			_, err := d.gateways[gateway()].Holder.Current()
			return err
		},
		// The environment: golem-control and the attacker.
		"takeLease": func(s *connect.Step) error {
			// The replica sees the lease, then its own clock moves past the
			// lease's duration with the lease unchanged (its holder paused):
			// it judges it lapsed, takes it at the next epoch and claims the
			// fence. The time guard holds because the driver arranges it.
			r := d.replicas[connect.MustPick[string](s, "replica")]
			if err := r.el.State.Load(context.Background()); err != nil {
				return err
			}
			if _, err := r.el.Try(context.Background()); err != nil {
				return err
			}
			r.clock = r.clock.Add(r.el.Duration + time.Second)
			var want int
			if err := connectDecode(s, &want, "leaseEpoch"); err != nil {
				return err
			}
			if e, err := r.el.Try(context.Background()); err != nil || int(e) != want {
				return fmt.Errorf("the model's lease is epoch %d; the elector took %d (%v)", want, e, err)
			}
			return nil
		},
		"noticeLoss": func(s *connect.Step) error {
			r := d.replicas[connect.MustPick[string](s, "replica")]
			if e, err := r.el.Try(context.Background()); err != nil || e != 0 {
				return fmt.Errorf("a replica that lost the lease tried and kept epoch %d (%v)", e, err)
			}
			return nil
		},
		"revoke": connect.Skip, "restore": connect.Skip,
		"forgeStatus": connect.Skip, "partition": connect.Skip, "heal": connect.Skip,
		"loadState": connect.Skip,
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

// recState is the model's PubRecord; ptrState its pointer, projected.
type recState struct{ Epoch, Seq, Source int }

type ptrState struct{ Seq, Epoch, Source, ValidUntil int }

type pubState struct {
	Gws         map[string]gwState
	Record      recState
	Pointer     ptrState
	LeaseHolder string
	LeaseEpoch  int
	Believes    map[string]int
}

func (d *pubDriver) State() (any, error) {
	st := pubState{Gws: map[string]gwState{}}
	ctx := context.Background()
	rec, err := d.pub.Record(ctx)
	if err != nil {
		return nil, err
	}
	st.Record = recState{Epoch: int(rec.Epoch), Seq: int(rec.Seq), Source: int(rec.Source)}
	if l := d.pub.State.Current().Fence; l != nil {
		st.LeaseHolder, st.LeaseEpoch = l.Holder, int(l.Epoch)
	}
	st.Believes = map[string]int{}
	for r, c := range d.replicas {
		st.Believes[r] = int(c.el.Epoch())
	}
	raw, _, err := d.bucket.Get(ctx, snapshot.PointerKey("pool"), "")
	if err != nil {
		return nil, err
	}
	ptr, err := snapshot.OpenPointer(raw, d.keys)
	if err != nil {
		return nil, err
	}
	body, _, err := d.bucket.Get(ctx, snapshot.ObjectKey("pool", ptr.Hash), "")
	if err != nil {
		return nil, err
	}
	snap, err := snapshot.Decode(body)
	if err != nil {
		return nil, err
	}
	st.Pointer = ptrState{Seq: int(ptr.PublishSeq), Epoch: int(ptr.Epoch), Source: int(ptr.Source),
		ValidUntil: int(snap.ValidUntil.Sub(epoch) / time.Minute)}
	for _, g := range slices.Sorted(maps.Keys(d.gateways)) {
		h := d.gateways[g].Holder
		gs := gwState{Applied: pub{Seq: -1}, Floor: int(h.Floor())}
		if c := h.cur.Load(); c != nil {
			gs.Applied = pub{Seq: int(c.Snapshot.PublishSeq), ValidUntil: int(c.Snapshot.ValidUntil.Sub(epoch) / time.Minute)}
		}
		st.Gws[g] = gs
	}
	return st, nil
}

func TestPublicationModel(t *testing.T) {
	if os.Getenv("QUINT_BIN") == "" {
		if _, err := lookQuint(); err != nil {
			t.Skip("quint not installed (or set QUINT_BIN)")
		}
	}
	// Require: check.sh's scenarios, each reached (or steered to) by the
	// traces that drive the gateway.
	connect.Run(t, connect.Config{Spec: "../../model/publication.qnt", Main: "design", MaxSamples: 50, MaxSteps: 30,
		Require: []string{"revocationReachesGateway", "partitionedAndExpired", "restoredWithRevocations"}},
		func(t testing.TB) connect.Driver { return &pubDriver{t: t} })
}
