package snapshot

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/lib/bucket/buckettest"
	"github.com/chucklehead-dev/golem/lib/sign"
)

// TestPollTimeout: a bucket that accepts a call and never answers ends a
// poll, and a start, at their Timeout, though the caller's context has no
// deadline: the next poll can try again before the lease runs out.
func TestPollTimeout(t *testing.T) {
	hang := &buckettest.Hang{}
	b := &Bucket{Bucket: hang, Keys: sign.Ed25519Keys{}, Pool: "internal", Holder: &Holder{}, Timeout: 50 * time.Millisecond}
	for name, call := range map[string]func(context.Context) error{"poll": b.Poll, "start": b.Start} {
		done := make(chan error, 1)
		go func() { done <- call(context.Background()) }()
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s: %v, want its deadline exceeded", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the %s waited on a bucket that never answers: unbounded", name)
		}
	}
	if hang.Calls.Load() < 2 {
		t.Fatalf("%d bucket calls: the poll and the start did not reach the bucket", hang.Calls.Load())
	}
}
