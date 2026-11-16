package telemetry

import (
	"context"
	"testing"
	"time"

	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// steadyLoad is a collector under a gateway's steady load: every export
// is followed by more rows emitted, so the queue is never empty.
type steadyLoad struct {
	recordingExp
	q *LedgerQueue
}

func (e *steadyLoad) Export(ctx context.Context, rs []sdklog.Record) error {
	var r sdklog.Record
	for range len(rs) {
		e.q.OnEmit(ctx, &r)
	}
	return e.recordingExp.Export(ctx, rs)
}

// TestLedgerQueueFlushUnderLoad: ForceFlush returns once what was queued
// when it was asked is exported, though rows keep arriving.
func TestLedgerQueueFlushUnderLoad(t *testing.T) {
	exp := &steadyLoad{}
	q := NewLedgerQueue(exp, 100, 5, time.Hour)
	exp.q = q
	defer q.Shutdown(t.Context())
	var r sdklog.Record
	for range 20 {
		q.OnEmit(t.Context(), &r)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := q.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush under steady load: %v; want it to end once the rows queued when asked are exported", err)
	}
	exp.mu.Lock()
	defer exp.mu.Unlock()
	if exp.got < 20 {
		t.Fatalf("flushed %d of the 20 queued", exp.got)
	}
}
