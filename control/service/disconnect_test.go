package service

import (
	"context"
	"testing"

	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/bucket"
)

// hangsUp is a bucket whose caller disconnects as each write goes out: the
// request's context is cancelled during the put, which then reports the
// context's error as an SDK would, though the write landed.
type hangsUp struct {
	bucket.Bucket
	cancel context.CancelFunc
}

func (h *hangsUp) Put(ctx context.Context, key string, body []byte, ifMatch string) (string, error) {
	h.cancel()
	tag, err := h.Bucket.Put(ctx, key, body, ifMatch)
	if err == nil {
		err = ctx.Err()
	}
	return tag, err
}

// TestWriteOutlivesTheCaller: a key minted for a caller that disconnects
// while its write is under way is answered as minted, with its plaintext,
// and is in the document: the write is not bound to the request.
func TestWriteOutlivesTheCaller(t *testing.T) {
	s := setup(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s.State = &state.Store{Bucket: &hangsUp{Bucket: s.State.Bucket, cancel: cancel}}
	m, err := s.MintKey(ctx, alice, Options{}, KeyRequest{Scope: "team-a"})
	if err != nil || m.Key == "" {
		t.Fatalf("a mint whose caller disconnected mid-write: %+v, %v; want the key", m, err)
	}
	if err := s.State.Refresh(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.State.Current().Keys[m.Hash]; !ok {
		t.Fatal("the minted key is not in the document")
	}
}
