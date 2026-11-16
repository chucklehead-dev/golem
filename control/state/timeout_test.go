package state

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/lib/bucket"
	"github.com/chucklehead-dev/golem/lib/bucket/buckettest"
)

// TestHungBucketTimesOut checks that a bucket that accepts a call and never
// answers ends a refresh and an update at the store's Timeout, though the
// caller's context has no deadline.
func TestHungBucketTimesOut(t *testing.T) {
	hang := &buckettest.Hang{}
	s := &Store{Bucket: hang, Timeout: 50 * time.Millisecond}
	for name, call := range map[string]func() error{
		"refresh": func() error { return s.Refresh(context.Background()) },
		"update":  func() error { _, err := s.Update(context.Background(), put("internal", 1)); return err },
	} {
		done := make(chan error, 1)
		go func() { done <- call() }()
		select {
		case err := <-done:
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s: %v, want its deadline exceeded", name, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("the %s waited on a bucket that never answers: unbounded", name)
		}
	}
}

// landsSilent is a bucket whose first document write lands but whose
// reply never comes: the put waits out its deadline.
type landsSilent struct {
	bucket.Bucket
	done bool
}

func (l *landsSilent) Put(ctx context.Context, key string, body []byte, ifMatch string) (string, error) {
	tag, err := l.Bucket.Put(ctx, key, body, ifMatch)
	if err != nil || l.done || key != DocumentKey {
		return tag, err
	}
	l.done = true
	<-ctx.Done()
	return "", ctx.Err()
}

// TestTimedOutPutLanded: an update whose put landed but timed out waiting
// for its reply is recognised by its write ID, and not applied twice.
func TestTimedOutPutLanded(t *testing.T) {
	b := open(t)
	if _, err := (&Store{Bucket: b}).Update(t.Context(), put("internal", 1)); err != nil {
		t.Fatal(err)
	}
	s := &Store{Bucket: &landsSilent{Bucket: b}, Timeout: 50 * time.Millisecond}
	applied := 0
	d, err := s.Update(t.Context(), func(d *Document) error {
		applied++
		d.Audit = append(d.Audit, AuditEvent{Actor: "root", Action: "scope.create", Target: "team-a"})
		return nil
	})
	if err != nil {
		t.Fatalf("a put that landed, its reply lost: %v; want success", err)
	}
	if applied != 1 || len(d.Audit) != 1 {
		t.Fatalf("applied %d times, %v, %d audit events; want once and success", applied, err, len(d.Audit))
	}
}
