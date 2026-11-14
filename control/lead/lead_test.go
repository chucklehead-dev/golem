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

func replicas(t *testing.T, ids ...string) (*bucket.Dir, map[string]*replica) {
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
		r.Elector = &Elector{State: &state.Store{Bucket: b}, ID: id, Duration: 30 * time.Second, Now: func() time.Time { return r.clock }}
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

func fence(t *testing.T, r *replica) state.Fence {
	t.Helper()
	if err := r.State.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	f := r.State.Current().Fence
	if f == nil {
		return state.Fence{}
	}
	return *f
}

// TestLease: the first replica takes epoch 1, claims the fence and keeps it
// by renewing; another cannot take a lease it has not seen unchanged for a
// whole duration by its own clock, takes it then at the next epoch, and the
// first then knows it does not lead.
func TestLease(t *testing.T) {
	_, rs := replicas(t, "a", "b")
	a, b := rs["a"], rs["b"]
	if try(t, a) != 1 || try(t, a) != 1 || fence(t, a) != (state.Fence{Holder: "a", Epoch: 1}) {
		t.Fatal("a takes epoch 1, claims the fence and renews")
	}
	if try(t, b) != 0 {
		t.Fatal("b took a held lease")
	}
	b.clock = b.clock.Add(29 * time.Second)
	if try(t, b) != 0 {
		t.Fatal("b took a lease before seeing it unchanged for its duration")
	}
	b.clock = b.clock.Add(2 * time.Second)
	if try(t, b) != 2 || fence(t, b) != (state.Fence{Holder: "b", Epoch: 2}) {
		t.Fatal("b takes the lapsed lease at epoch 2 and claims the fence")
	}
	if try(t, a) != 0 || a.Epoch() != 0 {
		t.Fatal("a still believes it leads after seeing b's lease")
	}
}

// TestRenewalKeepsIt: a holder that renews is never judged lapsed, however
// long it holds: each renewal changes the object a taker watches.
func TestRenewalKeepsIt(t *testing.T) {
	_, rs := replicas(t, "a", "b")
	a, b := rs["a"], rs["b"]
	try(t, a)
	for range 10 {
		try(t, b)
		a.clock, b.clock = a.clock.Add(10*time.Second), b.clock.Add(10*time.Second)
		if try(t, a) != 1 {
			t.Fatal("a lost the lease it renews")
		}
	}
	if try(t, b) != 0 {
		t.Fatal("b took a renewed lease")
	}
}

// TestGivesUpByItsClock: a leader that cannot renew stops believing when
// its own clock passes its last renewal plus the duration; a store error
// shorter than that keeps it.
func TestGivesUpByItsClock(t *testing.T) {
	b, rs := replicas(t, "a")
	a := rs["a"]
	try(t, a)
	a.Bucket = broken{b}
	a.clock = a.clock.Add(20 * time.Second)
	if e, err := a.Try(t.Context()); err == nil || e != 1 {
		t.Fatalf("a store error within the lease: epoch %d, %v; want 1 and the error", e, err)
	}
	a.clock = a.clock.Add(11 * time.Second)
	if a.Epoch() != 0 {
		t.Fatal("a believes it leads past its lease with no renewal")
	}
}

// TestStaleLeaderFenced: a leader paused past its lease, which publishes
// after the new leader has, is refused by the fence.
func TestStaleLeaderFenced(t *testing.T) {
	bkt, rs := replicas(t, "a", "b")
	a, b := rs["a"], rs["b"]
	staleEpoch := try(t, a) // a then pauses: its clock and belief stand still
	try(t, b)
	b.clock = b.clock.Add(31 * time.Second)
	newEpoch := try(t, b)
	signer := sign.Ed25519{ID: "k", Key: key(t)}
	build := func(seq uint64) (*snapshot.Snapshot, error) {
		return &snapshot.Snapshot{Schema: snapshot.Schema, PublishSeq: seq}, nil
	}
	pb := &publish.Publisher{State: b.State, Bucket: bkt, Signer: signer, Pool: "p"}
	if _, err := pb.Publish(t.Context(), publish.Lease{Holder: "b", Epoch: newEpoch}, 1, build); err != nil {
		t.Fatal(err)
	}
	pa := &publish.Publisher{State: a.State, Bucket: bkt, Signer: signer, Pool: "p"}
	if _, err := pa.Publish(t.Context(), publish.Lease{Holder: "a", Epoch: staleEpoch}, 1, build); !errors.Is(err, publish.ErrFenced) {
		t.Fatalf("the paused leader published at epoch %d after epoch %d: %v", staleEpoch, newEpoch, err)
	}
}

// TestLeaseRolledBack: a lease object put back to an older version neither
// reuses an epoch nor lets the old holder lead again: the next epoch is
// above the fence's, and the old holder renewing finds the fence another's
// (model/leadership.md: the lease object holds no authority).
func TestLeaseRolledBack(t *testing.T) {
	bkt, rs := replicas(t, "a", "b", "c")
	a, b, c := rs["a"], rs["b"], rs["c"]
	try(t, a)
	old, _, _ := bkt.Get(t.Context(), LeaseKey, "")
	try(t, b)
	b.clock = b.clock.Add(31 * time.Second)
	if try(t, b) != 2 {
		t.Fatal("b takes epoch 2")
	}
	bkt.Put(t.Context(), LeaseKey, old, "") // back to a's, epoch 1
	a.State.Refresh(t.Context())
	if try(t, a) != 0 {
		t.Fatal("a leads again on a rolled-back lease")
	}
	try(t, c)
	c.clock = c.clock.Add(31 * time.Second)
	if e := try(t, c); e != 3 {
		t.Fatalf("c took epoch %d after the rollback, want 3: above the fence's", e)
	}
}

// TestRelease: a replica that gives up the lease stops believing at once,
// and another takes it without waiting for it to lapse, at the next epoch.
func TestRelease(t *testing.T) {
	_, rs := replicas(t, "a", "b")
	a, b := rs["a"], rs["b"]
	try(t, a)
	if try(t, b) != 0 {
		t.Fatal("b took a held lease")
	}
	if err := a.Release(t.Context()); err != nil {
		t.Fatal(err)
	}
	if a.Epoch() != 0 {
		t.Fatal("a still believes it leads after giving up")
	}
	if try(t, b) != 2 {
		t.Fatal("b did not take the given-up lease at the next epoch")
	}
	if err := a.Release(t.Context()); err != nil {
		t.Fatalf("releasing a lease another holds: %v, want no change", err)
	}
	if fence(t, b).Holder != "b" || try(t, b) != 2 {
		t.Fatal("a's second release took b's lease")
	}
}

// TestLostClaim: a claim that lands but whose reply is lost is adopted at
// the next try, not left with no leader until the lease lapses; a write
// that lands and then fails its condition on a retry is the writer's own.
func TestLostClaim(t *testing.T) {
	bkt, rs := replicas(t, "a")
	a := rs["a"]
	lossy := &loseReply{Bucket: bkt}
	a.State = &state.Store{Bucket: lossy}
	a.Bucket = bkt
	lossy.lose = errors.New("connection reset")
	if e, err := a.Try(t.Context()); err == nil || e != 0 {
		t.Fatalf("a claim whose reply was lost: epoch %d, %v", e, err)
	}
	if fence(t, a) != (state.Fence{Holder: "a", Epoch: 1}) {
		t.Fatal("the claim did not land")
	}
	if try(t, a) != 1 {
		t.Fatal("the landed claim was not adopted")
	}
	// A retry of a landed put fails its own condition: the store must see
	// its own write, not take it for another's and apply the change again
	// (a publication would then report a false conflict).
	lossy.lose = bucket.ErrPrecondition
	before := a.State.Current().StateVersion
	applied := 0
	if _, err := a.State.Update(t.Context(), func(*state.Document) error { applied++; return nil }); err != nil {
		t.Fatalf("a write that landed, reported as a conflict: %v", err)
	}
	if v := fence(t, a); applied != 1 || a.State.Current().StateVersion != before+1 {
		t.Fatalf("the change was applied %d times, the version moved %d (fence %+v): the store took its own write for another's", applied, a.State.Current().StateVersion-before, v)
	}
}

// loseReply applies the document's next put, then reports lose instead of
// its result.
type loseReply struct {
	bucket.Bucket
	lose error
}

func (l *loseReply) Put(ctx context.Context, key string, body []byte, ifMatch string) (string, error) {
	if l.lose != nil && key == state.DocumentKey {
		if _, err := l.Bucket.Put(ctx, key, body, ifMatch); err != nil {
			return "", err
		}
		err := l.lose
		l.lose = nil
		return "", err
	}
	return l.Bucket.Put(ctx, key, body, ifMatch)
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
