package bucket

import (
	"errors"
	"sync"
	"testing"
)

// TestDir: Dir keeps S3's conditional semantics, which the publisher's
// compare-and-swap depends on.
func TestDir(t *testing.T) {
	d, err := OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := t.Context()
	if _, _, err := d.Get(ctx, "a/b", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get of nothing: %v", err)
	}
	if _, err := d.Put(ctx, "a/b", []byte("1"), `"nope"`); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("if-match on nothing: %v", err)
	}
	t1, err := d.Put(ctx, "a/b", []byte("1"), Absent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Put(ctx, "a/b", []byte("2"), Absent); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("create-only over an object: %v", err)
	}
	if _, _, err := d.Get(ctx, "a/b", t1); !errors.Is(err, ErrNotModified) {
		t.Fatalf("conditional get, unchanged: %v", err)
	}
	t2, err := d.Put(ctx, "a/b", []byte("2"), t1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Put(ctx, "a/b", []byte("3"), t1); !errors.Is(err, ErrPrecondition) {
		t.Fatalf("if-match on a stale tag: %v", err)
	}
	if b, tag, err := d.Get(ctx, "a/b", t1); err != nil || string(b) != "2" || tag != t2 {
		t.Fatalf("conditional get, changed: %q %s %v", b, tag, err)
	}
	for _, k := range []string{"../x", "/abs", "a/../../x", "a//b", ""} {
		if _, err := d.Put(ctx, k, nil, ""); err == nil {
			t.Errorf("key %q accepted", k)
		}
	}
}

// TestDirSwapRace: of concurrent swaps from one tag, exactly one wins.
func TestDirSwapRace(t *testing.T) {
	d, _ := OpenDir(t.TempDir())
	defer d.Close()
	tag, _ := d.Put(t.Context(), "r", []byte("0"), Absent)
	var wins sync.WaitGroup
	won := make(chan int, 16)
	for i := range 16 {
		wins.Go(func() {
			if _, err := d.Put(t.Context(), "r", []byte{byte('a' + i)}, tag); err == nil {
				won <- i
			}
		})
	}
	wins.Wait()
	close(won)
	if n := len(won); n != 1 {
		t.Fatalf("%d swaps won from one tag, want 1", n)
	}
}
