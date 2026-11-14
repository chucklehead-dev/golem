package state

import (
	"errors"
	"strings"
	"testing"

	"github.com/chucklehead-dev/golem/lib/bucket"
)

// TestMissingDocument: a store that finds no document where the bucket
// shows golem-control has run (any of Installed) refuses to start from an
// empty one, for reads and writes alike, and says how to recover; with no
// such object, it is a first start. Only Bootstrap creates the document
// then, create-only, its records at the floors given.
func TestMissingDocument(t *testing.T) {
	ctx := t.Context()
	b, err := bucket.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	installed := []string{"lead/lease.json", "control/internal/checkpoint", "snapshots/internal/current"}
	first := &Store{Bucket: b, Installed: installed}
	if err := first.Load(ctx); err != nil || first.Current().StateVersion != 0 {
		t.Fatalf("a first start: %v", err)
	}
	if _, err := first.Restore(ctx, &Document{Schema: Schema, StateVersion: 4}, "ops", false); err == nil || !strings.Contains(err.Error(), "no state/document.json to restore over") {
		t.Fatalf("a restore over no document: %v", err)
	}
	for _, evidence := range installed {
		t.Run(evidence, func(t *testing.T) {
			b, err := bucket.OpenDir(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer b.Close()
			b.Put(ctx, evidence, []byte("{}"), "")
			s := &Store{Bucket: b, Installed: installed}
			if err := s.Load(ctx); !errors.Is(err, ErrMissing) || !strings.Contains(err.Error(), evidence) || !strings.Contains(err.Error(), "golem-control bootstrap") {
				t.Fatalf("load: %v, want ErrMissing naming %s and the recovery", err, evidence)
			}
			if _, err := (&Store{Bucket: b, Installed: installed}).Update(ctx, func(*Document) error { return nil }); !errors.Is(err, ErrMissing) {
				t.Fatalf("update: %v, want ErrMissing", err)
			}
			if _, _, err := b.Get(ctx, DocumentKey, ""); !errors.Is(err, bucket.ErrNotFound) {
				t.Fatal("a document was written")
			}
			if _, err := s.Restore(ctx, &Document{Schema: Schema, StateVersion: 4}, "ops", false); err == nil || !strings.Contains(err.Error(), "bootstrap") {
				t.Fatalf("a restore over no document: %v", err)
			}
		})
	}

	b2, err := bucket.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	b2.Put(ctx, "lead/lease.json", []byte("{}"), "")
	s := &Store{Bucket: b2, Installed: installed}
	d, err := s.Bootstrap(ctx, map[string]uint64{"internal": 7, "edge": 0}, "ops", false)
	if err != nil {
		t.Fatal(err)
	}
	if d.Publications["internal"] != (Record{Seq: 7}) || len(d.Publications) != 1 || len(d.Audit) != 1 || d.Audit[0].Action != "state.bootstrap" {
		t.Fatalf("bootstrapped %+v", d)
	}
	again := &Store{Bucket: b2, Installed: installed}
	if err := again.Load(ctx); err != nil || again.Current().Publications["internal"].Seq != 7 {
		t.Fatalf("after the bootstrap: %v %+v", err, again.Current())
	}
	if _, err := again.Bootstrap(ctx, map[string]uint64{"internal": 9}, "ops", false); err == nil || !strings.Contains(err.Error(), "-adopt") {
		t.Fatalf("a second bootstrap: %v", err)
	}
	if _, err := again.Bootstrap(ctx, map[string]uint64{"internal": 7}, "ops", true); err == nil || !strings.Contains(err.Error(), "nothing to adopt") {
		t.Fatalf("an adopt with nothing to raise: %v", err)
	}
	d, err = again.Bootstrap(ctx, map[string]uint64{"internal": 9}, "ops", true)
	if err != nil || d.Publications["internal"].Seq != 9 || d.Audit[len(d.Audit)-1].Action != "state.adopt" {
		t.Fatalf("adopted %+v %v", d, err)
	}
}
