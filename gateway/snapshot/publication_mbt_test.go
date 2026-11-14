package snapshot

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/chucklehead-dev/gotogether/connect"
	"github.com/chucklehead-dev/gotogether/itf"

	"github.com/chucklehead-dev/golem/control/publish"
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
// restart with an empty disk takes its floor from the publisher's record
// (the design's TRUSTED_FLOOR). Leadership and the authoritative state
// (leases, revocations, restores, replicas' reads) remain the
// environment: their effect reaches the publisher as the epoch and state
// version the model's publish used.

// pub is the model's Pub record.
type pub struct {
	Seq        int
	ValidUntil int
}

// epoch is the model's clock zero; one model tick is one minute.
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func at(tick int) time.Time { return epoch.Add(time.Duration(tick) * time.Minute) }

type pubDriver struct {
	t        testing.TB
	clock    int
	bucket   *bucket.Dir
	pub      *publish.Publisher
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
	rec, _ := json.Marshal(publish.Record{Epoch: 1, Seq: 0, Source: 0})
	ptr, err := snapshot.SignPointer(ctx, snapshot.Pointer{Pool: "pool", PublishSeq: 0, Epoch: 1, Hash: hash}, d.pub.Signer)
	if err != nil {
		return err
	}
	for k, b := range map[string][]byte{snapshot.ObjectKey("pool", hash): body, publish.RecordKey("pool"): rec, snapshot.PointerKey("pool"): ptr} {
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
			d.pub = &publish.Publisher{Bucket: d.bucket, Signer: sign.Ed25519{ID: "k", Key: sk}, Pool: "pool",
				Now: func() time.Time { return at(d.clock) }}
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
			ptr, err := d.pub.Publish(context.Background(), uint64(p.Epoch), uint64(p.Source), func(seq uint64) (*snapshot.Snapshot, error) {
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
			rec, _, err := d.pub.LoadRecord(context.Background())
			if err != nil {
				return err
			}
			h := d.holder()
			h.RaiseFloor(rec.Seq)
			d.gateways[gateway()] = d.source(h)
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
		"revoke": connect.Skip, "restore": connect.Skip, "takeLease": connect.Skip,
		"noticeLoss": connect.Skip, "forgeStatus": connect.Skip, "partition": connect.Skip, "heal": connect.Skip,
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
	Gws     map[string]gwState
	Record  recState
	Pointer ptrState
}

func (d *pubDriver) State() (any, error) {
	st := pubState{Gws: map[string]gwState{}}
	ctx := context.Background()
	rec, _, err := d.pub.LoadRecord(ctx)
	if err != nil {
		return nil, err
	}
	st.Record = recState{Epoch: int(rec.Epoch), Seq: int(rec.Seq), Source: int(rec.Source)}
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
	for _, g := range slices.Sorted(mapsKeys(d.gateways)) {
		h := d.gateways[g].Holder
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
		func(t testing.TB) connect.Driver { return &pubDriver{t: t} })
}
