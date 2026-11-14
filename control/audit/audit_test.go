package audit

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"

	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/bucket"
)

// recorder is a log exporter keeping what it is given.
type recorder struct {
	mu sync.Mutex
	rs []sdklog.Record
}

func (r *recorder) Export(_ context.Context, rs []sdklog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, x := range rs {
		r.rs = append(r.rs, x.Clone())
	}
	return nil
}
func (*recorder) Shutdown(context.Context) error   { return nil }
func (*recorder) ForceFlush(context.Context) error { return nil }

type world struct {
	state   *state.Store
	archive *bucket.Dir
	logs    *recorder
	ship    *Shipper
}

func newWorld(t *testing.T, events int) *world {
	t.Helper()
	sb, _ := bucket.OpenDir(t.TempDir())
	ab, _ := bucket.OpenDir(t.TempDir())
	t.Cleanup(func() { sb.Close(); ab.Close() })
	w := &world{state: &state.Store{Bucket: sb}, archive: ab, logs: &recorder{}}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(w.logs)))
	w.ship = &Shipper{State: w.state, Archive: ab, Logger: lp.Logger("golem-control")}
	w.add(t, events)
	return w
}

// add writes n audited changes, as the services do.
func (w *world) add(t *testing.T, n int) {
	t.Helper()
	for i := range n {
		if _, err := w.state.Update(t.Context(), func(d *state.Document) error {
			d.Audit = append(d.Audit, state.AuditEvent{At: time.Unix(int64(i), 0).UTC(), Actor: "root", Action: "scope.create",
				Target: fmt.Sprint("s", i), Detail: map[string]string{"n": fmt.Sprint(i)}})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestShip: each event is archived as its own object, emitted with its
// attributes, then trimmed from the outbox.
func TestShip(t *testing.T) {
	w := newWorld(t, 3)
	if n, err := w.ship.Step(t.Context()); err != nil || n != 3 {
		t.Fatalf("step: %d %v", n, err)
	}
	if out := w.state.Current().Audit; len(out) != 0 {
		t.Fatalf("outbox %v after shipping", out)
	}
	for seq := uint64(1); seq <= 3; seq++ {
		if _, _, err := w.archive.Get(t.Context(), Key(seq), ""); err != nil {
			t.Fatalf("event %d not archived: %v", seq, err)
		}
	}
	if len(w.logs.rs) != 3 || w.logs.rs[0].EventName() != "golem.audit" {
		t.Fatalf("%d records emitted", len(w.logs.rs))
	}
	seen := map[string]string{}
	w.logs.rs[2].WalkAttributes(func(kv attribute.KeyValue) bool { seen[string(kv.Key)] = kv.Value.Emit(); return true })
	if seen["golem.audit.seq"] != "3" || seen["golem.audit.target"] != "s2" || seen["golem.audit.detail.n"] != "2" {
		t.Fatalf("attributes %v", seen)
	}
	if n, err := w.ship.Step(t.Context()); err != nil || n != 0 {
		t.Fatalf("an empty outbox: %d %v", n, err)
	}
}

// TestRetryAfterCrash: events archived by a step that died before its
// trim are found archived, as they are, and trimmed.
func TestRetryAfterCrash(t *testing.T) {
	w := newWorld(t, 2)
	broken := &Shipper{State: &state.Store{Bucket: noWrites{w.state.Bucket}}, Archive: w.archive}
	if _, err := broken.Step(t.Context()); err == nil {
		t.Fatal("the trim cannot have succeeded")
	}
	if len(w.state.Current().Audit) != 2 {
		t.Fatal("the outbox lost events with its trim failing")
	}
	if n, err := w.ship.Step(t.Context()); err != nil || n != 2 || len(w.state.Current().Audit) != 0 {
		t.Fatalf("the retry: %d %v, outbox %v", n, err, w.state.Current().Audit)
	}
}

// TestMismatch: an archived object that is not the event stops shipping;
// nothing is emitted or trimmed.
func TestMismatch(t *testing.T) {
	w := newWorld(t, 2)
	w.archive.Put(t.Context(), Key(2), []byte(`{"seq":2,"action":"something else"}`), "")
	if _, err := w.ship.Step(t.Context()); !errors.Is(err, ErrMismatch) {
		t.Fatalf("%v, want ErrMismatch", err)
	}
	if len(w.state.Current().Audit) != 2 || len(w.logs.rs) != 0 {
		t.Fatalf("outbox %d, emitted %d: nothing may go past a mismatch", len(w.state.Current().Audit), len(w.logs.rs))
	}
}

// TestBatchesAndNewEvents: a step ships at most a batch, and events
// written during a step are not trimmed by it.
func TestBatchesAndNewEvents(t *testing.T) {
	w := newWorld(t, 5)
	w.ship.Batch = 3
	if n, _ := w.ship.Step(t.Context()); n != 3 {
		t.Fatalf("first step %d", n)
	}
	w.add(t, 2)
	if n, _ := w.ship.Step(t.Context()); n != 3 {
		t.Fatalf("second step %d", n)
	}
	if out := w.state.Current().Audit; len(out) != 1 || out[0].Seq != 7 {
		t.Fatalf("outbox %v, want seq 7 alone", out)
	}
}

// noWrites is a state bucket that refuses writes.
type noWrites struct{ bucket.Bucket }

func (noWrites) Put(context.Context, string, []byte, string) (string, error) {
	return "", errors.New("connection refused")
}
