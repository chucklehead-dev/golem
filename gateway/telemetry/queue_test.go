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

func (e *recordingExp) Export(_ context.Context, rs []sdklog.Record) error {
	if e.gate != nil {
		<-e.gate
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
