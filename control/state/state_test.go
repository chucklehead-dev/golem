package state

import (
	"context"
	"errors"
	"slices"
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

// TestUpdateIncrementsVersion checks that each write increments
// state_version from the document written, and a second store over the
// bucket reads it.
func TestUpdateIncrementsVersion(t *testing.T) {
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

// TestRecordsNeverGoBack checks that a publication record removed or going
// back is refused, and nothing is written.
//
//quint:checks publicationsOrdered
func TestRecordsNeverGoBack(t *testing.T) {
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

// TestFenceInvariant: the fence's epoch going back, or the fence dropped,
// is refused, so a deposed epoch stays deposed; a later epoch is written,
// and FencedUpdate writes only under the fence it names.
//
//quint:checks publicationsOrdered
func TestFenceInvariant(t *testing.T) {
	ctx := t.Context()
	s := &Store{Bucket: open(t)}
	claim := func(holder string, epoch uint64) func(*Document) error {
		return func(d *Document) error { d.Fence = &Fence{Holder: holder, Epoch: epoch}; return nil }
	}
	if _, err := s.Update(ctx, claim("a", 2)); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*Document) error{
		"back in epoch": claim("b", 1),
		"dropped":       func(d *Document) error { d.Fence = nil; return nil },
	} {
		if _, err := s.Update(ctx, change); !errors.Is(err, ErrInvariant) {
			t.Errorf("%s: %v, want ErrInvariant", name, err)
		}
	}
	if _, err := s.FencedUpdate(ctx, Fence{Holder: "b", Epoch: 2}, put("internal", 1)); !errors.Is(err, ErrFenced) {
		t.Errorf("a write under another's fence: %v, want ErrFenced", err)
	}
	if _, err := s.Update(ctx, claim("b", 3)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.FencedUpdate(ctx, Fence{Holder: "b", Epoch: 3}, put("internal", 1)); err != nil {
		t.Fatalf("a write under its own fence: %v", err)
	}
	if f := s.Current().Fence; *f != (Fence{Holder: "b", Epoch: 3}) {
		t.Fatalf("fence %+v", f)
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

// overtaken lands the document's next put, then lets another writer write
// over it before reporting the put's reply lost, as a retry of it would:
// failing its own condition.
type overtaken struct {
	bucket.Bucket
	then func() // another writer's write; nil once used
}

func (o *overtaken) Put(ctx context.Context, key string, body []byte, ifMatch string) (string, error) {
	if o.then == nil || key != DocumentKey {
		return o.Bucket.Put(ctx, key, body, ifMatch)
	}
	if _, err := o.Bucket.Put(ctx, key, body, ifMatch); err != nil {
		return "", err
	}
	then := o.then
	o.then = nil
	then()
	return "", bucket.ErrPrecondition
}

// TestLostReplyOvertaken: a write whose reply is lost and which another
// writer then writes over is still recognised as landed (its ID among the
// document's recent writes), not applied again: one audit event, and a
// removal that landed is not answered "not found".
func TestLostReplyOvertaken(t *testing.T) {
	b := open(t)
	other := &Store{Bucket: b}
	if _, err := other.Update(t.Context(), func(d *Document) error {
		d.Scopes = map[string]Scope{"team-a": {}, "team-b": {}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	o := &overtaken{Bucket: b, then: func() {
		if _, err := other.Update(t.Context(), put("internal", 1)); err != nil {
			t.Fatal(err)
		}
	}}
	s := &Store{Bucket: o}
	applied := 0
	errGone := errors.New("not found: team-a")
	d, err := s.Update(t.Context(), func(d *Document) error {
		applied++
		if _, ok := d.Scopes["team-a"]; !ok {
			return errGone // a RemoveMember's NotFound
		}
		delete(d.Scopes, "team-a")
		d.Audit = append(d.Audit, AuditEvent{Actor: "root", Action: "membership.remove", Target: "team-a"})
		return nil
	})
	if err != nil || applied != 1 {
		t.Fatalf("a landed write overtaken by another: applied %d times, %v; want once and success", applied, err)
	}
	if d.StateVersion != 3 || len(d.Audit) != 1 || d.Publications["internal"].Seq != 1 {
		t.Fatalf("the document returned: version %d, %d audit events, publications %v; want the current one, the change once", d.StateVersion, len(d.Audit), d.Publications)
	}

	// The ring keeps the last RecentWrites write IDs, newest last.
	var last *Document
	for i := range RecentWrites + 8 {
		if last, err = s.Update(t.Context(), put("internal", uint64(i+2))); err != nil {
			t.Fatal(err)
		}
	}
	if len(last.Writes) != RecentWrites || len(slices.Compact(slices.Sorted(slices.Values(last.Writes)))) != RecentWrites {
		t.Fatalf("%d write IDs kept, want the last %d, distinct", len(last.Writes), RecentWrites)
	}
}

// TestClaim: each claim takes the fence at one above the document's epoch;
// the replica claimed over is refused at its next fenced write and, once
// it has read the document, no longer leads; a claim whose reply was lost
// is made once.
func TestClaim(t *testing.T) {
	ctx := t.Context()
	b := open(t)
	old, next := &Store{Bucket: b}, &Store{Bucket: b}
	fo, err := old.Claim(ctx, "r1")
	if err != nil || fo != (Fence{Holder: "r1", Epoch: 1}) || !old.Leads(fo) {
		t.Fatalf("the first claim: %+v %v", fo, err)
	}
	fn, err := next.Claim(ctx, "r2")
	if err != nil || fn != (Fence{Holder: "r2", Epoch: 2}) || !next.Leads(fn) {
		t.Fatalf("the second claim: %+v %v", fn, err)
	}
	if _, err := old.FencedUpdate(ctx, fo, put("internal", 1)); !errors.Is(err, ErrFenced) || !strings.Contains(err.Error(), "r2 leads at epoch 2") {
		t.Fatalf("the predecessor's write: %v, want ErrFenced naming r2", err)
	}
	if old.Leads(fo) {
		t.Fatal("the predecessor still leads after reading the successor's claim")
	}
	if _, err := old.Claim(ctx, ""); err == nil {
		t.Fatal("a claim with no holder")
	}

	o := &overtaken{Bucket: b, then: func() {
		if _, err := next.Update(ctx, put("internal", 2)); err != nil {
			t.Fatal(err)
		}
	}}
	lost := &Store{Bucket: o}
	f, err := lost.Claim(ctx, "r3")
	if err != nil || f != (Fence{Holder: "r3", Epoch: 3}) || *lost.Current().Fence != f {
		t.Fatalf("a claim whose reply was lost: %+v %v, the document's %+v; want epoch 3, once", f, err, lost.Current().Fence)
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

// TestRegressed: a document put back to an older version, or deleted,
// behind the store's back is not adopted, nor written over: reads and
// writes say so, instead of spinning on conflicts for ever.
func TestRegressed(t *testing.T) {
	for name, regress := range map[string]func(t *testing.T, b bucket.Bucket, old []byte){
		"an older version put back": func(t *testing.T, b bucket.Bucket, old []byte) {
			if _, err := b.Put(t.Context(), DocumentKey, old, ""); err != nil {
				t.Fatal(err)
			}
		},
		"deleted": func(t *testing.T, b bucket.Bucket, _ []byte) {
			b.(*deletable).gone = true
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir, err := bucket.OpenDir(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer dir.Close()
			b := &deletable{Dir: dir}
			s := &Store{Bucket: b}
			if _, err := s.Update(t.Context(), func(d *Document) error { return nil }); err != nil {
				t.Fatal(err)
			}
			old, _, _ := b.Get(t.Context(), DocumentKey, "")
			for range 2 {
				if _, err := s.Update(t.Context(), func(d *Document) error { return nil }); err != nil {
					t.Fatal(err)
				}
			}
			regress(t, b, old)
			if err := s.Refresh(t.Context()); !errors.Is(err, ErrRegressed) {
				t.Fatalf("refresh: %v, want ErrRegressed", err)
			}
			if _, err := s.Update(t.Context(), func(d *Document) error { return nil }); !errors.Is(err, ErrRegressed) {
				t.Fatalf("update: %v, want ErrRegressed", err)
			}
			if s.Current().StateVersion != 3 {
				t.Fatalf("version %d: the regressed document was adopted", s.Current().StateVersion)
			}
		})
	}
}

// deletable is a bucket whose document can vanish.
type deletable struct {
	*bucket.Dir
	gone bool
}

func (d *deletable) Get(ctx context.Context, key, inm string) ([]byte, string, error) {
	if d.gone && key == DocumentKey {
		return nil, "", bucket.ErrNotFound
	}
	return d.Dir.Get(ctx, key, inm)
}

func (d *deletable) Put(ctx context.Context, key string, body []byte, ifMatch string) (string, error) {
	if d.gone && key == DocumentKey && ifMatch != "" && ifMatch != bucket.Absent {
		return "", bucket.ErrPrecondition
	}
	return d.Dir.Put(ctx, key, body, ifMatch)
}
