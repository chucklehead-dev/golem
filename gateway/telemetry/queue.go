package telemetry

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// LedgerQueue is the ledger rows' log processor. The SDK's batch processor,
// when its queue is full, overwrites the oldest records and says so only in
// its own log: for billing rows that loss must be counted, alerted on and
// bounded by a queue sized for a collector stall. LedgerQueue exports in
// batches from a bounded queue and counts every row it could not deliver.
// (A durable spool, so no stall loses a row, is the next step.)
type LedgerQueue struct {
	exp     sdklog.Exporter
	q       chan sdklog.Record
	batch   int
	every   time.Duration
	stop    chan struct{}
	stopped sync.WaitGroup
	// Dropped counts rows refused because the queue was full; Failed, rows
	// in batches the exporter could not deliver.
	Dropped, Failed atomic.Int64
}

// NewLedgerQueue starts a queue of at most size rows, exported in batches of
// up to batch rows at least every interval.
func NewLedgerQueue(exp sdklog.Exporter, size, batch int, every time.Duration) *LedgerQueue {
	q := &LedgerQueue{exp: exp, q: make(chan sdklog.Record, size), batch: batch, every: every, stop: make(chan struct{})}
	q.stopped.Go(q.run)
	return q
}

// OnEmit queues a row; it never blocks the request path.
func (q *LedgerQueue) OnEmit(_ context.Context, r *sdklog.Record) error {
	select {
	case q.q <- r.Clone(): // the SDK reuses r after the call
	default:
		if q.Dropped.Add(1) == 1 {
			slog.Error("telemetry: the ledger queue is full; rows are being dropped (golem.ledger.dropped)", "size", cap(q.q))
		}
	}
	return nil
}

func (q *LedgerQueue) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }

func (q *LedgerQueue) run() {
	t := time.NewTicker(q.every)
	defer t.Stop()
	buf := make([]sdklog.Record, 0, q.batch)
	for {
		select {
		case r := <-q.q:
			if buf = append(buf, r); len(buf) < q.batch {
				continue
			}
		case <-t.C:
		case <-q.stop:
			for len(q.q) > 0 { // drain what is queued
				buf = append(buf, <-q.q)
				if len(buf) == q.batch {
					buf = q.export(buf)
				}
			}
			q.export(buf)
			return
		}
		buf = q.export(buf)
	}
}

func (q *LedgerQueue) export(buf []sdklog.Record) []sdklog.Record {
	if len(buf) == 0 {
		return buf
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := q.exp.Export(ctx, buf); err != nil {
		q.Failed.Add(int64(len(buf)))
		slog.Error("telemetry: ledger rows not delivered (golem.ledger.failed)", "rows", len(buf), "err", err)
	}
	clear(buf) // drop the records' references
	return buf[:0]
}

// Shutdown exports what is queued and stops. Rows emitted after it are
// dropped.
func (q *LedgerQueue) Shutdown(ctx context.Context) error {
	close(q.stop)
	q.stopped.Wait()
	return q.exp.Shutdown(ctx)
}

// ForceFlush is the exporter's: a batch in hand is exported at the next tick.
func (q *LedgerQueue) ForceFlush(ctx context.Context) error { return q.exp.ForceFlush(ctx) }
