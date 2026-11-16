// Package buckettest is lib/bucket's conformance suite: S3's conditional
// semantics, which the publisher's compare-and-swap depends on, checked
// for any Bucket (Dir, and s3bucket against an S3 endpoint).
package buckettest

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/chucklehead-dev/golem/lib/bucket"
)

// Run checks b, which must be empty under the keys "a/b" and "r".
func Run(t *testing.T, b bucket.Bucket) {
	t.Helper()
	ctx := t.Context()
	t.Run("conditions", func(t *testing.T) { conditions(t, ctx, b) })
	t.Run("swap race", func(t *testing.T) { swapRace(t, ctx, b) })
}

func conditions(t *testing.T, ctx context.Context, b bucket.Bucket) {
	if _, _, err := b.Get(ctx, "a/b", ""); !errors.Is(err, bucket.ErrNotFound) {
		t.Fatalf("get of nothing: %v", err)
	}
	if _, err := b.Put(ctx, "a/b", []byte("1"), `"nope"`); !errors.Is(err, bucket.ErrPrecondition) && !errors.Is(err, bucket.ErrNotFound) {
		t.Fatalf("if-match on nothing: %v, want a refusal", err)
	}
	t1, err := b.Put(ctx, "a/b", []byte("1"), bucket.Absent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Put(ctx, "a/b", []byte("2"), bucket.Absent); !errors.Is(err, bucket.ErrPrecondition) {
		t.Fatalf("create-only over an object: %v", err)
	}
	if _, tag, err := b.Get(ctx, "a/b", t1); !errors.Is(err, bucket.ErrNotModified) {
		t.Fatalf("conditional get, unchanged: %v (tag %s)", err, tag)
	}
	t2, err := b.Put(ctx, "a/b", []byte("2"), t1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Put(ctx, "a/b", []byte("3"), t1); !errors.Is(err, bucket.ErrPrecondition) {
		t.Fatalf("if-match on a stale tag: %v", err)
	}
	if body, tag, err := b.Get(ctx, "a/b", t1); err != nil || string(body) != "2" || tag != t2 {
		t.Fatalf("conditional get, changed: %q %s %v (want tag %s)", body, tag, err, t2)
	}
	if _, err := b.Put(ctx, "a/b", []byte("4"), ""); err != nil {
		t.Fatalf("unconditional put: %v", err)
	}
}

// swapRace: of concurrent swaps from one tag, exactly one wins.
func swapRace(t *testing.T, ctx context.Context, b bucket.Bucket) {
	tag, err := b.Put(ctx, "r", []byte("0"), bucket.Absent)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := range 16 {
		wg.Go(func() {
			_, err := b.Put(ctx, "r", []byte{byte('a' + i)}, tag)
			switch {
			case err == nil:
				mu.Lock()
				won++
				mu.Unlock()
			case !errors.Is(err, bucket.ErrPrecondition):
				t.Errorf("a losing swap: %v, want ErrPrecondition", err)
			}
		})
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("%d swaps won from one tag, want 1", won)
	}
}

// Hang is a bucket that never answers: every call waits until its context
// ends, then returns the context's error. For tests that a caller bounds
// its bucket calls (a poll, a state write, a publication).
type Hang struct {
	// Calls counts the calls begun.
	Calls atomic.Int64
}

var _ bucket.Bucket = (*Hang)(nil)

// Get waits for ctx to end.
func (h *Hang) Get(ctx context.Context, _, _ string) ([]byte, string, error) {
	h.Calls.Add(1)
	<-ctx.Done()
	return nil, "", ctx.Err()
}

// Put waits for ctx to end.
func (h *Hang) Put(ctx context.Context, _ string, _ []byte, _ string) (string, error) {
	h.Calls.Add(1)
	<-ctx.Done()
	return "", ctx.Err()
}
