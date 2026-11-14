package publish

import (
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

func build(seq uint64) (*snapshot.Snapshot, error) {
	return &snapshot.Snapshot{Schema: snapshot.Schema, PublishSeq: seq, ValidUntil: time.Now().Add(time.Hour).UTC()}, nil
}

// TestPublish: each publication is numbered from the record, its pointer
// is signed and names the snapshot written, and the record follows.
func TestPublish(t *testing.T) {
	p, keys, d := setup(t)
	ctx := t.Context()
	for want := uint64(1); want <= 3; want++ {
		ptr, err := p.Publish(ctx, 1, 5, build)
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

// TestFenced: a publisher with an older epoch or older state publishes
// nothing, and the record and pointer stay (the model's FENCE).
func TestFenced(t *testing.T) {
	p, _, d := setup(t)
	ctx := t.Context()
	if _, err := p.Publish(ctx, 2, 5, build); err != nil {
		t.Fatal(err)
	}
	before, _, _ := d.Get(ctx, snapshot.PointerKey("internal"), "")
	for name, c := range map[string]struct{ epoch, source uint64 }{"an old epoch": {1, 9}, "old state": {3, 4}} {
		if _, err := p.Publish(ctx, c.epoch, c.source, build); !errors.Is(err, ErrFenced) {
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
	var wg sync.WaitGroup
	var mu sync.Mutex
	seqs := map[uint64]int{}
	for range 8 {
		wg.Go(func() {
			// A replica of golem-control: its own view of the state.
			replica := *p
			replica.State = &state.Store{Bucket: p.Bucket}
			ptr, err := replica.Publish(ctx, 1, 1, func(seq uint64) (*snapshot.Snapshot, error) {
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
