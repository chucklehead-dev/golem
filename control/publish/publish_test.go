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
		d.Fence = &state.Fence{Holder: holder, Epoch: epoch}
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
		"an old epoch":          {Lease{Holder: "r1", Epoch: 1}, 9},
		"another's lease":       {Lease{Holder: "r2", Epoch: 2}, 9},
		"a lease not yet taken": {Lease{Holder: "r1", Epoch: 3}, 9},
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
	if ptr, err := r2.Publish(ctx, Lease{Holder: "r2", Epoch: 2}, 1, build); err != nil || ptr.PublishSeq != 4 {
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

// TestGuard: model/leadership.md's counterexamples, each refused. A
// rolled-back record, met by an older checkpoint copied back, still
// numbers above what gateways trust (the signed pointer); a forged or
// deleted checkpoint stops publication; the first publication's crash
// window (a record with no checkpoint yet) is not mistaken for tampering.
func TestGuard(t *testing.T) {
	ctx := t.Context()
	// fresh is another replica, started after the bucket changed under
	// it: its store has no memory of newer versions.
	fresh := func(p *Publisher, d *bucket.Dir) *Publisher {
		q := *p
		q.State = &state.Store{Bucket: d}
		return &q
	}
	cpSeq := func(d *bucket.Dir, keys sign.Ed25519Keys) uint64 {
		raw, _, _ := d.Get(ctx, snapshot.CheckpointKey("internal"), "")
		cp, err := snapshot.OpenCheckpoint(raw, keys)
		if err != nil {
			t.Fatal(err)
		}
		return cp.PublishSeq
	}

	t.Run("a rolled-back record and an older checkpoint copied back", func(t *testing.T) {
		p, keys, d := setup(t)
		l := leads(t, p.State, "r1", 1)
		p.Publish(ctx, l, 1, build)
		oldDoc, _, _ := d.Get(ctx, state.DocumentKey, "")
		oldCp, _, _ := d.Get(ctx, snapshot.CheckpointKey("internal"), "")
		for range 2 {
			if _, err := p.Publish(ctx, l, 1, build); err != nil {
				t.Fatal(err)
			}
		}
		d.Put(ctx, state.DocumentKey, oldDoc, "")                 // record back to 1
		d.Put(ctx, snapshot.CheckpointKey("internal"), oldCp, "") // checkpoint back to 1
		ptr, err := fresh(p, d).Publish(ctx, l, 1, build)
		if err != nil || ptr.PublishSeq != 4 || cpSeq(d, keys) != 4 {
			t.Fatalf("published %d (%v), checkpoint %d; want 4: above the signed pointer at 3", ptr.PublishSeq, err, cpSeq(d, keys))
		}
	})
	t.Run("a rolled-back record alone", func(t *testing.T) {
		p, keys, d := setup(t)
		l := leads(t, p.State, "r1", 1)
		p.Publish(ctx, l, 1, build)
		oldDoc, _, _ := d.Get(ctx, state.DocumentKey, "")
		p.Publish(ctx, l, 1, build)
		p.Publish(ctx, l, 1, build)
		d.Put(ctx, state.DocumentKey, oldDoc, "")
		if ptr, err := fresh(p, d).Publish(ctx, l, 1, build); err != nil || ptr.PublishSeq != 4 || cpSeq(d, keys) != 4 {
			t.Fatalf("published %d (%v); want 4, not 2 (today's advance lowered the checkpoint)", ptr.PublishSeq, err)
		}
	})
	// Each tampering returns the bucket as the publisher then sees it.
	for name, tamper := range map[string]func(d *bucket.Dir) bucket.Bucket{
		"a forged checkpoint": func(d *bucket.Dir) bucket.Bucket {
			_, sk, _ := ed25519.GenerateKey(nil)
			forged, _ := snapshot.SignCheckpoint(ctx, snapshot.Checkpoint{Pool: "internal", PublishSeq: 1 << 40}, sign.Ed25519{ID: "k", Key: sk})
			d.Put(ctx, snapshot.CheckpointKey("internal"), forged, "")
			return d
		},
		"an unsigned checkpoint": func(d *bucket.Dir) bucket.Bucket {
			d.Put(ctx, snapshot.CheckpointKey("internal"), []byte(`{"pool":"internal","publishSeq":99}`), "")
			return d
		},
		"a deleted checkpoint": func(d *bucket.Dir) bucket.Bucket {
			return &hidden{Bucket: d, key: snapshot.CheckpointKey("internal")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			p, _, d := setup(t)
			l := leads(t, p.State, "r1", 1)
			p.Publish(ctx, l, 1, build)
			p.Publish(ctx, l, 1, build)
			before, _, _ := d.Get(ctx, snapshot.PointerKey("internal"), "")
			p.Bucket = tamper(d)
			if _, err := p.Publish(ctx, l, 1, build); !errors.Is(err, ErrTampered) {
				t.Fatalf("%v, want ErrTampered", err)
			}
			if rec, _ := p.Record(ctx); rec.Seq != 2 {
				t.Fatalf("record %d: a publication was won past the tampering", rec.Seq)
			}
			if after, _, _ := d.Get(ctx, snapshot.PointerKey("internal"), ""); string(after) != string(before) {
				t.Fatal("the pointer moved past the tampering")
			}
		})
	}
	t.Run("the first publication's crash window", func(t *testing.T) {
		p, _, d := setup(t)
		l := leads(t, p.State, "r1", 1)
		dying := *p
		dying.Bucket = &failAt{Bucket: d, key: snapshot.CheckpointKey("internal")}
		if _, err := dying.Publish(ctx, l, 1, build); err == nil {
			t.Fatal("the dying publisher wrote its checkpoint")
		}
		if ptr, err := p.Publish(ctx, l, 1, build); err != nil || ptr.PublishSeq != 2 {
			t.Fatalf("after a death between record 1 and its checkpoint: %+v %v", ptr, err)
		}
	})
}

// failAt is a bucket whose puts of key fail.
type failAt struct {
	bucket.Bucket
	key string
}

func (f *failAt) Put(ctx context.Context, key string, body []byte, ifMatch string) (string, error) {
	if key == f.key {
		return "", errors.New("the publisher died")
	}
	return f.Bucket.Put(ctx, key, body, ifMatch)
}

// hidden is a bucket where one object has been deleted.
type hidden struct {
	bucket.Bucket
	key string
}

func (h *hidden) Get(ctx context.Context, key, inm string) ([]byte, string, error) {
	if key == h.key {
		return nil, "", bucket.ErrNotFound
	}
	return h.Bucket.Get(ctx, key, inm)
}
