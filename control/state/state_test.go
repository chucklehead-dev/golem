package state

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/chucklehead-dev/golem/lib/bucket"
)

func open(t *testing.T) bucket.Bucket {
	t.Helper()
	d, err := bucket.OpenDir(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func put(pool string, seq uint64) func(*Document) error {
	return func(d *Document) error {
		if d.Publications == nil {
			d.Publications = map[string]Record{}
		}
		r := d.Publications[pool]
		r.Seq = seq
		d.Publications[pool] = r
		return nil
	}
}

// TestUpdate: each write increments state_version from the document
// written, and a second store over the bucket reads it.
func TestUpdate(t *testing.T) {
	ctx := t.Context()
	b := open(t)
	s := &Store{Bucket: b}
	for i := uint64(1); i <= 3; i++ {
		d, err := s.Update(ctx, put("internal", i))
		if err != nil || d.StateVersion != i {
			t.Fatalf("update %d: version %d, %v", i, d.StateVersion, err)
		}
	}
	other := &Store{Bucket: b}
	if err := other.Load(ctx); err != nil {
		t.Fatal(err)
	}
	if d := other.Current(); d.StateVersion != 3 || d.Publications["internal"].Seq != 3 {
		t.Fatalf("read by another store: %+v", d)
	}
	if err := other.Refresh(ctx); err != nil { // unchanged: a conditional get
		t.Fatal(err)
	}
}

// TestInvariants: a record removed or going back is refused, and nothing
// is written.
func TestInvariants(t *testing.T) {
	ctx := t.Context()
	s := &Store{Bucket: open(t)}
	if _, err := s.Update(ctx, put("internal", 5)); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Document) error{
		"back in sequence": put("internal", 4),
		"removed":          func(d *Document) error { delete(d.Publications, "internal"); return nil },
		"back in epoch": func(d *Document) error {
			d.Publications["internal"] = Record{Seq: 6}
			return nil
		},
	} {
		if name == "back in epoch" {
			if _, err := s.Update(ctx, func(d *Document) error { d.Publications["internal"] = Record{Epoch: 2, Seq: 5}; return nil }); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Update(ctx, change); !errors.Is(err, ErrInvariant) {
			t.Errorf("%s: %v, want ErrInvariant", name, err)
		}
	}
	if r := s.Current().Publications["internal"]; r != (Record{Epoch: 2, Seq: 5}) {
		t.Fatalf("record %+v after refused changes", r)
	}
}

// TestConcurrentStores: replicas updating one bucket lose no update: each
// retries from the document the winner wrote.
func TestConcurrentStores(t *testing.T) {
	ctx := t.Context()
	b := open(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			s := &Store{Bucket: b, Attempts: 64}
			if _, err := s.Update(ctx, func(d *Document) error {
				if d.Publications == nil {
					d.Publications = map[string]Record{}
				}
				r := d.Publications["internal"]
				r.Seq++
				d.Publications["internal"] = r
				return nil
			}); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	s := &Store{Bucket: b}
	s.Load(ctx)
	if d := s.Current(); d.StateVersion != 8 || d.Publications["internal"].Seq != 8 {
		t.Fatalf("after 8 concurrent increments: %+v", d)
	}
}

// TestChangeError: a change's error ends the update, returned as it is.
func TestChangeError(t *testing.T) {
	stop := errors.New("no")
	s := &Store{Bucket: open(t)}
	if _, err := s.Update(t.Context(), func(*Document) error { return stop }); err != stop {
		t.Fatalf("%v", err)
	}
	if s.Current().StateVersion != 0 {
		t.Fatal("written anyway")
	}
}

// TestDecode: another schema, or a member this reader does not know, is
// refused.
func TestDecode(t *testing.T) {
	for in, want := range map[string]string{
		`{"schema":2,"stateVersion":1}`:              "schema 2",
		`{"schema":1,"stateVersion":1,"extra":true}`: `"extra"`,
	} {
		if _, err := Decode([]byte(in)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %s", in, err, want)
		}
	}
}
