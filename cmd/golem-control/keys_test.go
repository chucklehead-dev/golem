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
	"github.com/chucklehead-dev/golem/lib/sign"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// TestTrustedKeys: golem-control's publishers trust -trusted-keys beside
// the signing key, so a signer rotated from A to B (under a new key ID)
// goes on publishing over A's checkpoint, and re-signs it; without A
// trusted it stops.
func TestTrustedKeys(t *testing.T) {
	ctx := t.Context()
	keys := t.TempDir()
	if err := keygen(keys); err != nil {
		t.Fatal(err)
	}
	a, _ := openSigner(ctx, "file:"+filepath.Join(keys, "publishing.key"))
	next := t.TempDir()
	if err := keygen(next); err != nil {
		t.Fatal(err)
	}
	os.Rename(filepath.Join(next, "publishing.key"), filepath.Join(next, "publishing-2.key")) // a new key ID
	b, _ := openSigner(ctx, "file:"+filepath.Join(next, "publishing-2.key"))
	bk, err := bucket.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st := &state.Store{Bucket: bk}
	if _, err := st.Update(ctx, func(d *state.Document) error {
		d.Fence = &state.Fence{Holder: "r1", Epoch: 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	publishWith := func(signer sign.Signer, trusted string) error {
		c, closeAudit, err := newController(ctx, options{config: t.TempDir(), pools: "internal", trustedKeys: trusted}, st, bk, signer, "r1")
		if err != nil {
			t.Fatal(err)
		}
		defer closeAudit()
		_, err = c.Publishers["internal"].Publish(ctx, state.Fence{Holder: "r1", Epoch: 1}, st.Current().StateVersion,
			func(seq uint64) (*snapshot.Snapshot, error) {
				return &snapshot.Snapshot{Schema: snapshot.Schema, PublishSeq: seq, ValidUntil: time.Now().Add(time.Hour)}, nil
			})
		return err
	}
	if err := publishWith(a, ""); err != nil {
		t.Fatal(err)
	}
	if err := publishWith(b, ""); !errors.Is(err, publish.ErrTampered) {
		t.Fatalf("rotated, A not trusted: %v, want ErrTampered", err)
	}
	for range 2 {
		if err := publishWith(b, keys); err != nil {
			t.Fatalf("rotated with -trusted-keys holding A: %v", err)
		}
	}
	if err := publishWith(b, ""); err != nil {
		t.Fatalf("A retired after B re-signed: %v", err)
	}
	if _, _, err := newController(ctx, options{config: t.TempDir(), pools: "internal", trustedKeys: t.TempDir()}, st, bk, b, "r1"); err == nil || !strings.Contains(err.Error(), "-trusted-keys") {
		t.Fatalf("an empty -trusted-keys directory: %v", err)
	}
}
