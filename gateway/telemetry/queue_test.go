package telemetry

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	sdklog "go.opentelemetry.io/otel/sdk/log"
)

type recordingExp struct {
	mu   sync.Mutex
	got  int
	fail bool
	gate chan struct{} // if set, Export waits on it
}

func (e *recordingExp) Export(ctx context.Context, rs []sdklog.Record) error {
	if e.gate != nil {
		select {
		case <-e.gate:
		case <-ctx.Done():
			return ctx.Err() // as an OTLP exporter does
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.fail {
		return errors.New("collector down")
	}
	e.got += len(rs)
	return nil
}
func (e *recordingExp) Shutdown(context.Context) error   { return nil }
func (e *recordingExp) ForceFlush(context.Context) error { return nil }

// TestLedgerQueue: every row is delivered on shutdown, and a row the queue
// or the collector could not take is counted, never lost silently.
func TestLedgerQueue(t *testing.T) {
	exp := &recordingExp{}
	q := NewLedgerQueue(exp, 100, 7, time.Hour)
	var r sdklog.Record
	for range 50 {
		q.OnEmit(t.Context(), &r)
	}
	q.Shutdown(t.Context())
	if exp.got != 50 || q.Dropped.Load() != 0 {
		t.Fatalf("delivered %d of 50, dropped %d", exp.got, q.Dropped.Load())
	}

	// A stalled collector: the queue fills, and the rest are counted.
	stalled := &recordingExp{gate: make(chan struct{})}
	q = NewLedgerQueue(stalled, 10, 1, time.Hour)
	for range 30 {
		q.OnEmit(t.Context(), &r)
	}
	if d := q.Dropped.Load(); d < 19 { // 10 queued, at most one in hand
		t.Fatalf("dropped %d of 30 with a queue of 10", d)
	}
	close(stalled.gate)
	q.Shutdown(t.Context())
	if stalled.got+int(q.Dropped.Load()) != 30 {
		t.Fatalf("delivered %d + dropped %d != 30", stalled.got, q.Dropped.Load())
	}

	failing := &recordingExp{fail: true}
	q = NewLedgerQueue(failing, 10, 5, time.Hour)
	for range 5 {
		q.OnEmit(t.Context(), &r)
	}
	q.Shutdown(t.Context())
	if q.Failed.Load() != 5 {
		t.Fatalf("failed %d, want 5", q.Failed.Load())
	}
}

// TestLedgerQueueShutdown: shutdown keeps to its deadline even when the
// collector does not answer, and every row it could not send, or that came
// after it, is counted.
func TestLedgerQueueShutdown(t *testing.T) {
	var r sdklog.Record
	stuck := &recordingExp{gate: make(chan struct{})}
	q := NewLedgerQueue(stuck, 100, 10, time.Hour)
	for range 25 {
		q.OnEmit(t.Context(), &r)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	q.Shutdown(ctx)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("shutdown took %v past its 200 ms deadline", took)
	}
	if q.Failed.Load() != 25 {
		t.Fatalf("failed %d: all 25 undelivered rows must be counted", q.Failed.Load())
	}
	q.OnEmit(t.Context(), &r)
	if q.Dropped.Load() != 1 {
		t.Fatal("a row after shutdown was neither delivered nor counted")
	}
}

// TestLedgerQueueForceFlush: ForceFlush delivers what is queued now.
func TestLedgerQueueForceFlush(t *testing.T) {
	var r sdklog.Record
	exp := &recordingExp{}
	q := NewLedgerQueue(exp, 100, 50, time.Hour)
	defer q.Shutdown(t.Context())
	for range 7 {
		q.OnEmit(t.Context(), &r)
	}
	if err := q.ForceFlush(t.Context()); err != nil {
		t.Fatal(err)
	}
	exp.mu.Lock()
	defer exp.mu.Unlock()
	if exp.got != 7 {
		t.Fatalf("flushed %d of 7", exp.got)
	}
}
