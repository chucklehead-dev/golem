package lead

import (
	"context"
	"crypto/ed25519"
	"errors"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/control/publish"
	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/bucket"
	"github.com/chucklehead-dev/golem/lib/sign"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// replica is a golem-control replica: its own view of the state, and its
// own clock (a paused replica's does not move).
type replica struct {
	*Elector
	clock time.Time
}

func replicas(t *testing.T, ids ...string) (bucket.Bucket, map[string]*replica) {
	t.Helper()
	b, err := bucket.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	start := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	rs := map[string]*replica{}
	for _, id := range ids {
		r := &replica{clock: start}
		r.Elector = &Elector{State: &state.Store{Bucket: b}, ID: id, Duration: 15 * time.Second, Now: func() time.Time { return r.clock }}
		rs[id] = r
	}
	return b, rs
}

func try(t *testing.T, r *replica) uint64 {
	t.Helper()
	e, err := r.Try(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// TestLease: the first replica takes epoch 1 and keeps it by renewing;
// another cannot take a held lease, takes a lapsed one at the next epoch,
// and the first then knows it does not lead.
func TestLease(t *testing.T) {
	_, rs := replicas(t, "a", "b")
	a, b := rs["a"], rs["b"]
	if try(t, a) != 1 || try(t, a) != 1 {
		t.Fatal("a takes epoch 1 and renews it")
	}
	b.clock = a.clock.Add(14 * time.Second)
	if try(t, b) != 0 {
		t.Fatal("b took a lease that had not lapsed")
	}
	b.clock = a.clock.Add(16 * time.Second)
	if try(t, b) != 2 {
		t.Fatal("b takes the lapsed lease at epoch 2")
	}
	if try(t, a) != 0 || a.Epoch() != 0 {
		t.Fatal("a still believes it leads after seeing b's lease")
	}
}

// TestGivesUpByItsClock: a leader that cannot renew stops believing when
// its own clock passes its last renewal plus the duration, before another
// replica may take the lease; a store error shorter than that keeps it.
func TestGivesUpByItsClock(t *testing.T) {
	b, rs := replicas(t, "a")
	a := rs["a"]
	try(t, a)
	a.State.Bucket = broken{b}
	a.clock = a.clock.Add(10 * time.Second)
	if e, err := a.Try(t.Context()); err == nil || e != 1 {
		t.Fatalf("a store error within the lease: epoch %d, %v; want 1 and the error", e, err)
	}
	a.clock = a.clock.Add(5 * time.Second)
	if a.Epoch() != 0 {
		t.Fatal("a believes it leads past its lease with no renewal")
	}
}

// TestStaleLeaderFenced: a leader paused past its lease, which publishes
// after the new leader has, is refused by the publication record.
func TestStaleLeaderFenced(t *testing.T) {
	bkt, rs := replicas(t, "a", "b")
	a, b := rs["a"], rs["b"]
	staleEpoch := try(t, a) // a then pauses: its clock and belief stand still
	b.clock = a.clock.Add(time.Minute)
	newEpoch := try(t, b)
	signer := sign.Ed25519{ID: "k", Key: key(t)}
	build := func(seq uint64) (*snapshot.Snapshot, error) {
		return &snapshot.Snapshot{Schema: snapshot.Schema, PublishSeq: seq}, nil
	}
	pb := &publish.Publisher{State: b.State, Bucket: bkt, Signer: signer, Pool: "p"}
	if _, err := pb.Publish(t.Context(), newEpoch, 1, build); err != nil {
		t.Fatal(err)
	}
	pa := &publish.Publisher{State: a.State, Bucket: bkt, Signer: signer, Pool: "p"}
	if _, err := pa.Publish(t.Context(), staleEpoch, 1, build); !errors.Is(err, publish.ErrFenced) {
		t.Fatalf("the paused leader published at epoch %d after epoch %d: %v", staleEpoch, newEpoch, err)
	}
}

// broken is a bucket that writes nothing.
type broken struct{ bucket.Bucket }

func (broken) Put(context.Context, string, []byte, string) (string, error) {
	return "", errors.New("connection refused")
}

func key(t *testing.T) []byte {
	t.Helper()
	_, k, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return k
}
