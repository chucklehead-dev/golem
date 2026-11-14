package snapshot

import (
	"context"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/control/publish"
	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/bucket"
	"github.com/chucklehead-dev/golem/lib/sign"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// published is a publisher and a gateway's source over one bucket, with
// the attacker's view: anyone who can write the bucket but not sign.
type published struct {
	pub    *publish.Publisher
	bucket *bucket.Dir
	keys   sign.Ed25519Keys
	signer sign.Ed25519
	dir    string // the gateway's disk
}

func newPublished(t *testing.T) *published {
	t.Helper()
	pk, sk, _ := ed25519.GenerateKey(nil)
	d, err := bucket.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	s := sign.Ed25519{ID: "k1", Key: sk}
	st := &state.Store{Bucket: d}
	if _, err := st.Update(t.Context(), func(doc *state.Document) error {
		doc.Leader = &state.Lease{Holder: "r1", Epoch: 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return &published{pub: &publish.Publisher{State: st, Bucket: d, Signer: s, Pool: "internal"}, bucket: d,
		keys: sign.Ed25519Keys{"k1": pk}, signer: s, dir: t.TempDir()}
}

// gateway starts a gateway on the pool, its checkpoint on its disk.
func (p *published) gateway(t *testing.T) *Bucket {
	t.Helper()
	b := &Bucket{Bucket: p.bucket, Keys: p.keys, Pool: "internal", Holder: &Holder{},
		Checkpoint: filepath.Join(p.dir, "checkpoint"), GatewayID: "gw-1"}
	if err := b.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return b
}

func (p *published) publish(t *testing.T) snapshot.Pointer {
	t.Helper()
	ptr, err := p.pub.Publish(t.Context(), publish.Lease{Holder: "r1", Epoch: 1}, 1, func(seq uint64) (*snapshot.Snapshot, error) {
		return &snapshot.Snapshot{Schema: snapshot.Schema, PublishSeq: seq, ValidUntil: time.Now().Add(time.Hour).UTC()}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return ptr
}

// pointerBytes is the stored pointer, as an attacker would copy it.
func (p *published) pointerBytes(t *testing.T) []byte {
	b, _, err := p.bucket.Get(t.Context(), snapshot.PointerKey("internal"), "")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (p *published) put(t *testing.T, key string, b []byte) {
	if _, err := p.bucket.Put(t.Context(), key, b, ""); err != nil {
		t.Fatal(err)
	}
}

func applied(b *Bucket) uint64 {
	if c := b.Holder.cur.Load(); c != nil {
		return c.Snapshot.PublishSeq
	}
	return 0
}

func mustRefuse(t *testing.T, b *Bucket, want string) {
	t.Helper()
	before := applied(b)
	if err := b.Poll(t.Context()); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("poll: %v, want a refusal naming %q", err, want)
	}
	if applied(b) != before {
		t.Fatalf("applied %d after a refusal, held %d", applied(b), before)
	}
}

// TestBucketSource: a gateway applies each signed publication in turn, and
// keeps what it holds against everything else the bucket may hold.
func TestBucketSource(t *testing.T) {
	p := newPublished(t)
	gw := p.gateway(t)
	if err := gw.Poll(t.Context()); err == nil {
		t.Fatal("a pool with no pointer yet: want an error")
	}
	p.publish(t)
	old := p.pointerBytes(t)
	p.publish(t)
	if err := gw.Poll(t.Context()); err != nil || applied(gw) != 2 {
		t.Fatalf("poll: %v, applied %d, want 2", err, applied(gw))
	}
	if err := gw.Poll(t.Context()); err != nil {
		t.Fatalf("an unchanged pointer: %v", err)
	}
	if st, _, err := p.bucket.Get(t.Context(), "status/internal/gw-1", ""); err != nil || !strings.Contains(string(st), `"publishSeq":2`) {
		t.Fatalf("status %s %v", st, err)
	}

	t.Run("an old signed pointer copied back", func(t *testing.T) {
		p.put(t, snapshot.PointerKey("internal"), old)
		mustRefuse(t, gw, "below the floor")
	})
	t.Run("the same, after a restart with its disk", func(t *testing.T) {
		restarted := p.gateway(t) // the floor from the checkpoint
		mustRefuse(t, restarted, "below the floor")
	})
	t.Run("the same, after a restart with an empty disk", func(t *testing.T) {
		empty := &Bucket{Bucket: p.bucket, Keys: p.keys, Pool: "internal", Holder: &Holder{}}
		if err := empty.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		if f := empty.Holder.Floor(); f != 2 {
			t.Fatalf("floor %d, want 2: the trusted checkpoint golem-control wrote", f)
		}
		mustRefuse(t, empty, "below the floor")
	})
	t.Run("an empty disk and an unreadable bucket", func(t *testing.T) {
		gone := &Bucket{Bucket: unreadable{p.bucket}, Keys: p.keys, Pool: "internal", Holder: &Holder{}}
		if err := gone.Start(t.Context()); err == nil {
			t.Fatal("started with no floor at all: it could not tell a replay from the current pointer")
		}
		withDisk := &Bucket{Bucket: unreadable{p.bucket}, Keys: p.keys, Pool: "internal", Holder: &Holder{},
			Checkpoint: filepath.Join(p.dir, "checkpoint")}
		if err := withDisk.Start(t.Context()); err != nil || withDisk.Holder.Floor() != 2 {
			t.Fatalf("with its disk: %v, floor %d; want a start from the local checkpoint", err, withDisk.Holder.Floor())
		}
	})
	t.Run("the trusted checkpoint replaced", func(t *testing.T) {
		cp, _, _ := p.bucket.Get(t.Context(), snapshot.CheckpointKey("internal"), "")
		defer p.put(t, snapshot.CheckpointKey("internal"), cp)
		_, sk, _ := ed25519.GenerateKey(nil)
		forged, _ := snapshot.SignCheckpoint(t.Context(), snapshot.Checkpoint{Pool: "internal"}, sign.Ed25519{ID: "k1", Key: sk})
		for name, b := range map[string][]byte{
			"unsigned":                   []byte(`{"pool":"internal","publishSeq":0}`),
			"signed by an untrusted key": forged,
			"a pointer in its place":     p.pointerBytes(t), // signed, but in a pointer's context
		} {
			p.put(t, snapshot.CheckpointKey("internal"), b)
			empty := &Bucket{Bucket: p.bucket, Keys: p.keys, Pool: "internal", Holder: &Holder{}}
			if err := empty.Start(t.Context()); err == nil {
				t.Errorf("%s: started with floor %d from a checkpoint golem-control did not sign", name, empty.Holder.Floor())
			}
		}
	})
	t.Run("the trusted checkpoint deleted", func(t *testing.T) {
		cp, _, _ := p.bucket.Get(t.Context(), snapshot.CheckpointKey("internal"), "")
		defer p.put(t, snapshot.CheckpointKey("internal"), cp)
		gone := missing{p.bucket, snapshot.CheckpointKey("internal")}
		empty := &Bucket{Bucket: gone, Keys: p.keys, Pool: "internal", Holder: &Holder{}}
		if err := empty.Start(t.Context()); err == nil || !strings.Contains(err.Error(), "has a pointer") {
			t.Fatalf("%v (floor %d): a published pool's missing checkpoint is not floor zero", err, empty.Holder.Floor())
		}
	})
	t.Run("a pointer signed by an untrusted key", func(t *testing.T) {
		_, sk, _ := ed25519.GenerateKey(nil)
		b, _ := snapshot.SignPointer(t.Context(), snapshot.Pointer{Pool: "internal", PublishSeq: 9, Hash: "x"}, sign.Ed25519{ID: "k1", Key: sk})
		p.put(t, snapshot.PointerKey("internal"), b)
		mustRefuse(t, gw, "does not verify")
	})
	t.Run("another pool's pointer", func(t *testing.T) {
		b, _ := snapshot.SignPointer(t.Context(), snapshot.Pointer{Pool: "edge", PublishSeq: 9, Hash: "x"}, p.signer)
		p.put(t, snapshot.PointerKey("internal"), b)
		mustRefuse(t, gw, `pool "edge"`)
	})
	t.Run("a snapshot object replaced", func(t *testing.T) {
		ptr := p.publish(t)
		p.put(t, snapshot.ObjectKey("internal", ptr.Hash), []byte(`{"schema":1,"publishSeq":3}`))
		mustRefuse(t, gw, "not the object the pointer signed")
	})
	t.Run("a pointer naming another publication's snapshot", func(t *testing.T) {
		older := p.publish(t) // seq 4
		b, _ := snapshot.SignPointer(t.Context(), snapshot.Pointer{Pool: "internal", PublishSeq: 9, Hash: older.Hash}, p.signer)
		p.put(t, snapshot.PointerKey("internal"), b)
		mustRefuse(t, gw, "its pointer says 9")
	})
	t.Run("a snapshot not yet readable is retried", func(t *testing.T) {
		ptr := p.publish(t) // above the signed pointer at 9 planted earlier
		gw.Bucket = missing{p.bucket, snapshot.ObjectKey("internal", ptr.Hash)}
		if err := gw.Poll(t.Context()); err == nil {
			t.Fatal("poll with the object missing: want an error")
		}
		gw.Bucket = p.bucket
		if err := gw.Poll(t.Context()); err != nil || applied(gw) != ptr.PublishSeq {
			t.Fatalf("poll once the object is back: %v, applied %d, want %d", err, applied(gw), ptr.PublishSeq)
		}
	})
}

// missing is a bucket where one object cannot be read.
type missing struct {
	*bucket.Dir
	key string
}

func (m missing) Get(ctx context.Context, key, inm string) ([]byte, string, error) {
	if key == m.key {
		return nil, "", bucket.ErrNotFound
	}
	return m.Dir.Get(ctx, key, inm)
}

// unreadable is a bucket that answers nothing.
type unreadable struct{ *bucket.Dir }

func (unreadable) Get(context.Context, string, string) ([]byte, string, error) {
	return nil, "", errors.New("connection refused")
}

// TestOverdue: a gateway that sees no pointer newer than one past its
// Expires says so, and keeps serving; a new publication clears it.
func TestOverdue(t *testing.T) {
	p := newPublished(t)
	now := time.Now()
	p.pub.Now = func() time.Time { return now }
	p.pub.PointerTTL = 10 * time.Minute
	gw := p.gateway(t)
	gw.Holder.Now = func() time.Time { return now }
	p.publish(t)
	if err := gw.Poll(t.Context()); err != nil || gw.Overdue() {
		t.Fatalf("poll %v, overdue %v: a fresh pointer", err, gw.Overdue())
	}
	now = now.Add(11 * time.Minute) // the publisher stopped, or the bucket froze
	if err := gw.Poll(t.Context()); err != nil || !gw.Overdue() {
		t.Fatalf("poll %v, overdue %v: want overdue on an unchanged pointer", err, gw.Overdue())
	}
	if _, err := gw.Holder.Current(); err != nil {
		t.Fatalf("an overdue pointer stopped serving: %v", err)
	}
	p.publish(t)
	if err := gw.Poll(t.Context()); err != nil || gw.Overdue() {
		t.Fatalf("poll %v, overdue %v: a new publication clears it", err, gw.Overdue())
	}
}

// mounting is secrets that are not there until mounted.
type mounting struct{ mounted *atomic.Bool }

func (m mounting) Secret(ref, env string) (string, error) {
	if !m.mounted.Load() {
		return "", errors.New("secret " + ref + " not mounted yet")
	}
	return "sk", nil
}

// TestApplyRetried: a verified publication that fails to apply (a secret
// not mounted yet) is tried again while it is current, every ApplyRetry,
// not left until the next publication; a refused one is not.
func TestApplyRetried(t *testing.T) {
	p := newPublished(t)
	now := time.Now()
	var mounted atomic.Bool
	gw := p.gateway(t)
	gw.Holder.Now = func() time.Time { return now }
	gw.Secrets = mounting{&mounted}
	gw.ApplyRetry = 30 * time.Second
	if _, err := p.pub.Publish(t.Context(), publish.Lease{Holder: "r1", Epoch: 1}, 1, func(seq uint64) (*snapshot.Snapshot, error) {
		s := snap(seq, now.Add(time.Hour))
		return s, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := gw.Poll(t.Context()); err == nil || applied(gw) != 0 {
		t.Fatalf("poll with the secret missing: %v, applied %d", err, applied(gw))
	}
	mounted.Store(true)
	if err := gw.Poll(t.Context()); err != nil || applied(gw) != 0 {
		t.Fatalf("within the retry interval: %v, applied %d; want no retry yet", err, applied(gw))
	}
	now = now.Add(31 * time.Second)
	if err := gw.Poll(t.Context()); err != nil || applied(gw) != 1 {
		t.Fatalf("after the retry interval: %v, applied %d; want 1", err, applied(gw))
	}
}
