package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/control/publish"
	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/bucket"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// TestBootstrapCommand: after the state document is deleted, golem-control
// refuses to start from an empty one (the pool's checkpoint shows it ran);
// bootstrap creates the document, its record at what the bucket proves,
// and publication goes on above it.
func TestBootstrapCommand(t *testing.T) {
	ctx := t.Context()
	keys := t.TempDir()
	if err := keygen(keys); err != nil {
		t.Fatal(err)
	}
	signer, _ := openSigner(ctx, "file:"+filepath.Join(keys, "publishing.key"))
	dir := t.TempDir()
	b, err := bucket.OpenDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	build := func(seq uint64) (*snapshot.Snapshot, error) {
		return &snapshot.Snapshot{Schema: snapshot.Schema, PublishSeq: seq, ValidUntil: time.Now().Add(time.Hour)}, nil
	}
	publishOnce := func(st *state.Store) (snapshot.Pointer, error) {
		f := state.Fence{Holder: "r1", Epoch: st.Current().StateVersion + 1}
		if _, err := st.Update(ctx, func(d *state.Document) error { d.Fence = &f; return nil }); err != nil {
			t.Fatal(err)
		}
		p := &publish.Publisher{State: st, Bucket: b, Signer: signer, Pool: "internal"}
		return p.Publish(ctx, f, st.Current().StateVersion, build)
	}
	st := &state.Store{Bucket: b, Installed: installed("internal")}
	for range 2 {
		if _, err := publishOnce(st); err != nil {
			t.Fatal(err)
		}
	}
	os.Remove(filepath.Join(dir, filepath.FromSlash(state.DocumentKey)))

	if err := (&state.Store{Bucket: b, Installed: installed("internal")}).Load(ctx); !errors.Is(err, state.ErrMissing) {
		t.Fatalf("a start after the deletion: %v, want ErrMissing", err)
	}
	var out strings.Builder
	if err := bootstrap(ctx, &out, []string{"-bucket", dir, "-keys", keys, "-pools", "internal"}); err == nil {
		t.Fatal("a bootstrap with no actor")
	}
	if err := bootstrap(ctx, &out, []string{"-bucket", dir, "-keys", keys, "-pools", "internal", "-actor", "ops"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "pool internal: publication record 2") {
		t.Fatalf("bootstrap said:\n%s", out.String())
	}
	st = &state.Store{Bucket: b, Installed: installed("internal")}
	if err := st.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if ptr, err := publishOnce(st); err != nil || ptr.PublishSeq != 3 {
		t.Fatalf("after the bootstrap: %+v %v, want publication 3", ptr, err)
	}
	if err := bootstrap(ctx, &out, []string{"-bucket", dir, "-keys", keys, "-pools", "internal", "-actor", "ops"}); err == nil || !strings.Contains(err.Error(), "-adopt") {
		t.Fatalf("a bootstrap over a document: %v", err)
	}
}
