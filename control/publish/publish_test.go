package publish

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/bucket"
	"github.com/chucklehead-dev/golem/lib/sign"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

func setup(t *testing.T) (*Publisher, sign.Ed25519Keys, *bucket.Dir) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(nil)
	d, err := bucket.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return &Publisher{State: &state.Store{Bucket: d}, Bucket: d, Signer: sign.Ed25519{ID: "k", Key: priv}, Pool: "internal"}, sign.Ed25519Keys{"k": pub}, d
}

// leads gives holder the lease at epoch, as control/lead would.
func leads(t *testing.T, st *state.Store, holder string, epoch uint64) Lease {
	t.Helper()
	if _, err := st.Update(t.Context(), func(d *state.Document) error {
		d.Leader = &state.Lease{Holder: holder, Epoch: epoch, RenewedAt: time.Now().UTC(), DurationMillis: 15000}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return Lease{Holder: holder, Epoch: epoch}
}

func build(seq uint64) (*snapshot.Snapshot, error) {
	return &snapshot.Snapshot{Schema: snapshot.Schema, PublishSeq: seq, ValidUntil: time.Now().Add(time.Hour).UTC()}, nil
}

// TestPublish: each publication is numbered from the record, its pointer
// is signed and names the snapshot written, and the record follows.
func TestPublish(t *testing.T) {
	p, keys, d := setup(t)
	ctx := t.Context()
	l := leads(t, p.State, "r1", 1)
	for want := uint64(1); want <= 3; want++ {
		ptr, err := p.Publish(ctx, l, 5, build)
		if err != nil {
			t.Fatal(err)
		}
		raw, _, _ := d.Get(ctx, snapshot.PointerKey("internal"), "")
		got, err := snapshot.OpenPointer(raw, keys)
		if err != nil || got != ptr || got.PublishSeq != want {
			t.Fatalf("pointer %+v %v, want seq %d (%+v)", got, err, want, ptr)
		}
		body, _, err := d.Get(ctx, snapshot.ObjectKey("internal", ptr.Hash), "")
		if err != nil || snapshot.Hash(body) != ptr.Hash {
			t.Fatalf("snapshot object: %v", err)
		}
	}
	if rec, _ := p.Record(ctx); rec != (state.Record{Epoch: 1, Seq: 3, Source: 5}) {
		t.Fatalf("record %+v", rec)
	}
}

// TestFenced: a publisher that does not hold the lease at its epoch, or
// has older state than the record's, publishes nothing, and the record and
// pointer stay (the model's FENCE).
func TestFenced(t *testing.T) {
	p, _, d := setup(t)
	ctx := t.Context()
	l := leads(t, p.State, "r1", 2)
	if _, err := p.Publish(ctx, l, 5, build); err != nil {
		t.Fatal(err)
	}
	before, _, _ := d.Get(ctx, snapshot.PointerKey("internal"), "")
	for name, c := range map[string]struct {
		lease  Lease
		source uint64
	}{
		"an old epoch":          {Lease{"r1", 1}, 9},
		"another's lease":       {Lease{"r2", 2}, 9},
		"a lease not yet taken": {Lease{"r1", 3}, 9},
		"old state":             {l, 4},
	} {
		if _, err := p.Publish(ctx, c.lease, c.source, build); !errors.Is(err, ErrFenced) {
			t.Errorf("%s: %v, want ErrFenced", name, err)
		}
	}
	after, _, _ := d.Get(ctx, snapshot.PointerKey("internal"), "")
	if rec, _ := p.Record(ctx); rec.Seq != 1 || string(before) != string(after) {
		t.Fatalf("record %+v, pointer changed %v: a fenced publisher changed something", rec, string(before) != string(after))
	}
}

// TestConcurrentPublishers: publishers racing from one record take
// distinct sequence numbers; the losers are told, and none overwrites a
// winner's number.
func TestConcurrentPublishers(t *testing.T) {
	p, _, _ := setup(t)
	ctx := t.Context()
	l := leads(t, p.State, "r1", 1)
	var wg sync.WaitGroup
	var mu sync.Mutex
	seqs := map[uint64]int{}
	for range 8 {
		wg.Go(func() {
			// A replica of golem-control: its own view of the state.
			replica := *p
			replica.State = &state.Store{Bucket: p.Bucket}
			ptr, err := replica.Publish(ctx, l, 1, func(seq uint64) (*snapshot.Snapshot, error) {
				time.Sleep(time.Millisecond) // all read the record before any swaps it
				return build(seq)
			})
			if errors.Is(err, ErrConflict) {
				return
			}
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			seqs[ptr.PublishSeq]++
			mu.Unlock()
		})
	}
	wg.Wait()
	for seq, n := range seqs {
		if n > 1 {
			t.Fatalf("publication %d published %d times", seq, n)
		}
	}
	if rec, _ := p.Record(ctx); len(seqs) == 0 || rec.Seq != uint64(len(seqs)) {
		t.Fatalf("record %+v after %d publications", rec, len(seqs))
	}
}

// TestLeaseLostBeforeRecord: a leader that read the record, then lost the
// lease before winning it, is refused by the record's own write, though no
// successor has published yet.
func TestLeaseLostBeforeRecord(t *testing.T) {
	p, _, _ := setup(t)
	ctx := t.Context()
	l := leads(t, p.State, "r1", 1)
	successor := &state.Store{Bucket: p.Bucket}
	_, err := p.Publish(ctx, l, 1, func(seq uint64) (*snapshot.Snapshot, error) {
		leads(t, successor, "r2", 2) // while r1 builds
		return build(seq)
	})
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("%v, want ErrFenced", err)
	}
	if rec, _ := p.Record(ctx); rec.Seq != 0 {
		t.Fatalf("record %+v: the deposed leader won it", rec)
	}
}

// TestPausedAfterRecord: a leader paused between winning the record and
// writing the checkpoint and pointer, whose successor has published since,
// leaves the successor's in place; a forged pointer above the record is
// replaced.
func TestPausedAfterRecord(t *testing.T) {
	p, keys, d := setup(t)
	ctx := t.Context()
	l := leads(t, p.State, "r1", 1)
	paused := *p
	paused.Bucket = &pauseAfterRecord{Bucket: d, at: snapshot.CheckpointKey("internal"), pause: func() {
		// r2 takes the lease and publishes twice while r1 is paused.
		r2 := *p
		r2.State = &state.Store{Bucket: d}
		l2 := leads(t, r2.State, "r2", 2)
		for range 2 {
			if _, err := r2.Publish(ctx, l2, 1, build); err != nil {
				t.Fatal(err)
			}
		}
	}}
	if _, err := paused.Publish(ctx, l, 1, build); !errors.Is(err, ErrFenced) {
		t.Fatalf("%v, want ErrFenced", err)
	}
	raw, _, _ := d.Get(ctx, snapshot.PointerKey("internal"), "")
	ptr, err := snapshot.OpenPointer(raw, keys)
	if err != nil || ptr.PublishSeq != 3 || ptr.Epoch != 2 {
		t.Fatalf("pointer %+v %v: want the successor's publication 3", ptr, err)
	}
	cp, _, _ := d.Get(ctx, snapshot.CheckpointKey("internal"), "")
	if c, err := snapshot.OpenCheckpoint(cp, keys); err != nil || c.PublishSeq != 3 {
		t.Fatalf("checkpoint %+v %v: want 3, the successor's", c, err)
	}

	// A pointer forged above the record does not block the next publication.
	forged, _ := snapshot.SignPointer(ctx, snapshot.Pointer{Pool: "internal", PublishSeq: 99}, sign.Ed25519{ID: "x", Key: ed25519.NewKeyFromSeed(make([]byte, 32))})
	d.Put(ctx, snapshot.PointerKey("internal"), forged, "")
	r2 := *p
	r2.State = &state.Store{Bucket: d}
	if ptr, err := r2.Publish(ctx, Lease{"r2", 2}, 1, build); err != nil || ptr.PublishSeq != 4 {
		t.Fatalf("after a forged pointer: %+v %v", ptr, err)
	}
}

// pauseAfterRecord runs pause before the first put of key at.
type pauseAfterRecord struct {
	bucket.Bucket
	at    string
	pause func()
	done  bool
}

func (b *pauseAfterRecord) Put(ctx context.Context, key string, body []byte, ifMatch string) (string, error) {
	if key == b.at && !b.done {
		b.done = true
		b.pause()
	}
	return b.Bucket.Put(ctx, key, body, ifMatch)
}
