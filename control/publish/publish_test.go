package publish

import (
	"context"
	"crypto/ed25519"
	"errors"
	"strings"
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

// leads gives holder the fence at epoch, as a claim would.
func leads(t *testing.T, st *state.Store, holder string, epoch uint64) state.Fence {
	t.Helper()
	if _, err := st.Update(t.Context(), func(d *state.Document) error {
		d.Fence = &state.Fence{Holder: holder, Epoch: epoch}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return state.Fence{Holder: holder, Epoch: epoch}
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

// TestFenced: a publisher that does not hold the fence at its epoch, or
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
		lease  state.Fence
		source uint64
	}{
		"an old epoch":          {state.Fence{Holder: "r1", Epoch: 1}, 9},
		"another's lease":       {state.Fence{Holder: "r2", Epoch: 2}, 9},
		"a lease not yet taken": {state.Fence{Holder: "r1", Epoch: 3}, 9},
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
		// r2 claims the fence and publishes twice while r1 is paused.
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
	if ptr, err := r2.Publish(ctx, state.Fence{Holder: "r2", Epoch: 2}, 1, build); err != nil || ptr.PublishSeq != 4 {
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

// TestRegressedOrTamperedRefused checks the counterexamples of the retired
// leadership model, each refused. A rolled-back record stops the pool's publication until the
// document is adopted, then numbers above the signed pointer. A race for
// the record is not mistaken for one. A forged or deleted checkpoint stops
// publication. A record with no checkpoint yet, the first publication's
// crash window, is not mistaken for tampering.
func TestRegressedOrTamperedRefused(t *testing.T) {
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

	// A rolled-back record, alone or with an older checkpoint copied back:
	// the publisher halts (ErrRegressed) rather than numbering past it,
	// which would keep the floor (the model's GUARD) but publish the older
	// document's entitlements. Nothing moves until the document is adopted
	// (golem-control bootstrap -adopt); then publication goes on above
	// what gateways trust (the signed pointer at 3).
	for name, oldCheckpoint := range map[string]bool{"a rolled-back record alone": false, "a rolled-back record and an older checkpoint copied back": true} {
		t.Run(name, func(t *testing.T) {
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
			d.Put(ctx, state.DocumentKey, oldDoc, "") // record back to 1
			cp := uint64(3)
			if oldCheckpoint {
				d.Put(ctx, snapshot.CheckpointKey("internal"), oldCp, "") // checkpoint back to 1
				cp = 1
			}
			ptrBefore, _, _ := d.Get(ctx, snapshot.PointerKey("internal"), "")
			q := fresh(p, d)
			if _, err := q.Publish(ctx, l, 1, build); !errors.Is(err, ErrRegressed) || !contains(err, "bootstrap -adopt") {
				t.Fatalf("%v, want ErrRegressed saying what to do", err)
			}
			if ptrAfter, _, _ := d.Get(ctx, snapshot.PointerKey("internal"), ""); cpSeq(d, keys) != cp || string(ptrAfter) != string(ptrBefore) {
				t.Fatal("the checkpoint or pointer moved past the rollback")
			}
			if rec, _ := q.Record(ctx); rec.Seq != 1 {
				t.Fatalf("record %d: a publication was won past the rollback", rec.Seq)
			}
			floor, err := q.Floor(ctx)
			if err != nil || floor != 3 {
				t.Fatalf("floor %d %v, want 3", floor, err)
			}
			if _, err := q.State.Bootstrap(ctx, map[string]uint64{"internal": floor}, "ops", true); err != nil {
				t.Fatal(err)
			}
			if ptr, err := q.Publish(ctx, l, 1, build); err != nil || ptr.PublishSeq != 4 || cpSeq(d, keys) != 4 {
				t.Fatalf("after adopting: published %d (%v), checkpoint %d; want 4", ptr.PublishSeq, err, cpSeq(d, keys))
			}
		})
	}
	t.Run("another pool's signed pointer copied over this one's", func(t *testing.T) {
		p, _, d := setup(t)
		l := leads(t, p.State, "r1", 1)
		p.Publish(ctx, l, 1, build)
		other, _ := snapshot.SignPointer(ctx, snapshot.Pointer{Pool: "edge", PublishSeq: 9}, p.Signer)
		d.Put(ctx, snapshot.PointerKey("internal"), other, "")
		if ptr, err := p.Publish(ctx, l, 1, build); err != nil || ptr.PublishSeq != 2 {
			t.Fatalf("%+v %v: want publication 2, the other pool's sequence not counted", ptr, err)
		}
	})
	t.Run("a record won by another publisher while this one read the bucket", func(t *testing.T) {
		p, _, d := setup(t)
		l := leads(t, p.State, "r1", 1)
		p.Publish(ctx, l, 1, build)
		other := fresh(p, d)
		racing := *p
		racing.Bucket = &beforeGet{Bucket: d, key: snapshot.PointerKey("internal"), run: func() {
			if _, err := other.Publish(ctx, l, 1, build); err != nil {
				t.Fatal(err)
			}
		}}
		if _, err := racing.Publish(ctx, l, 1, build); !errors.Is(err, ErrConflict) {
			t.Fatalf("%v, want ErrConflict: a race, not a rollback", err)
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

// TestKeyRotation: publications under key A, then the signer rotated to B
// with A trusted: publication goes on, and the checkpoint and pointer are
// re-signed under B, so the publication after (and a publisher trusting B
// alone) verifies them. Without A trusted, the rotation is ErrTampered.
func TestKeyRotation(t *testing.T) {
	ctx := t.Context()
	a, aKeys, d := setup(t)
	l := leads(t, a.State, "r1", 1)
	for range 2 {
		if _, err := a.Publish(ctx, l, 1, build); err != nil {
			t.Fatal(err)
		}
	}
	bPub, bPriv, _ := ed25519.GenerateKey(nil)
	rotated := func(trusted sign.Verifier) *Publisher {
		p := *a
		p.State = &state.Store{Bucket: d}
		p.Signer, p.Keys = sign.Ed25519{ID: "b", Key: bPriv}, trusted
		return &p
	}
	if _, err := rotated(nil).Publish(ctx, l, 1, build); !errors.Is(err, ErrTampered) || !contains(err, "-trusted-keys") {
		t.Fatalf("rotated without the old key trusted: %v, want ErrTampered naming -trusted-keys", err)
	}
	b := rotated(aKeys)
	for want := uint64(3); want <= 4; want++ {
		if ptr, err := b.Publish(ctx, l, 1, build); err != nil || ptr.PublishSeq != want {
			t.Fatalf("rotated with the old key trusted: %+v %v, want publication %d", ptr, err, want)
		}
	}
	bKeys := sign.Ed25519Keys{"b": bPub}
	raw, _, _ := d.Get(ctx, snapshot.CheckpointKey("internal"), "")
	if cp, err := snapshot.OpenCheckpoint(raw, bKeys); err != nil || cp.PublishSeq != 4 {
		t.Fatalf("checkpoint %+v %v: want publication 4 signed by the new key", cp, err)
	}
	raw, _, _ = d.Get(ctx, snapshot.PointerKey("internal"), "")
	if ptr, err := snapshot.OpenPointer(raw, bKeys); err != nil || ptr.PublishSeq != 4 {
		t.Fatalf("pointer %+v %v: want publication 4 signed by the new key", ptr, err)
	}
	// The old key retired: what is in the bucket needs only the new one.
	if ptr, err := rotated(nil).Publish(ctx, l, 1, build); err != nil || ptr.PublishSeq != 5 {
		t.Fatalf("the old key retired: %+v %v", ptr, err)
	}
}

func contains(err error, s string) bool { return err != nil && strings.Contains(err.Error(), s) }

// beforeGet runs run before the first get of key.
type beforeGet struct {
	bucket.Bucket
	key  string
	run  func()
	done bool
}

func (b *beforeGet) Get(ctx context.Context, key, inm string) ([]byte, string, error) {
	if key == b.key && !b.done {
		b.done = true
		b.run()
	}
	return b.Bucket.Get(ctx, key, inm)
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

// TestPublishAll: one cycle over several pools is one state write, every
// pool's record won in it, and each pool's pointer moves; a pool that
// cannot publish is left out without stopping the others.
func TestPublishAll(t *testing.T) {
	ctx := t.Context()
	p, keys, d := setup(t)
	l := leads(t, p.State, "r1", 1)
	var pubs []*Publisher
	for _, pool := range []string{"edge", "internal", "batch"} {
		q := *p
		q.Pool = pool
		pubs = append(pubs, &q)
	}
	before := p.State.Current().StateVersion
	ptrs, errs := PublishAll(ctx, p.State, l, 1, pubs, func(_ *Publisher, seq uint64) (*snapshot.Snapshot, error) { return build(seq) })
	if len(errs) != 0 || p.State.Current().StateVersion != before+1 {
		t.Fatalf("errors %v; %d state writes, want 1", errs, p.State.Current().StateVersion-before)
	}
	for _, q := range pubs {
		raw, _, _ := d.Get(ctx, snapshot.PointerKey(q.Pool), "")
		if got, err := snapshot.OpenPointer(raw, keys); err != nil || got != ptrs[q.Pool] || got.PublishSeq != 1 {
			t.Fatalf("pool %s: pointer %+v %v", q.Pool, got, err)
		}
	}
	// A tampered pool is left out; the others publish.
	d.Put(ctx, snapshot.CheckpointKey("edge"), []byte(`{"pool":"edge","publishSeq":9}`), "")
	ptrs, errs = PublishAll(ctx, p.State, l, 1, pubs, func(_ *Publisher, seq uint64) (*snapshot.Snapshot, error) { return build(seq) })
	if !errors.Is(errs["edge"], ErrTampered) || len(errs) != 1 || ptrs["internal"].PublishSeq != 2 || ptrs["batch"].PublishSeq != 2 {
		t.Fatalf("errors %v, pointers %v", errs, ptrs)
	}
	if rec := p.State.Current().Publications["edge"]; rec.Seq != 1 {
		t.Fatalf("the tampered pool's record moved: %+v", rec)
	}
}
