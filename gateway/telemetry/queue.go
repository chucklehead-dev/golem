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
// batches from a bounded queue and counts every row it could not deliver,
// shutdown included. Durability through outages is the collector's
// (ADR 0043): the gateway's queue only bridges a node-local agent's restart.
type LedgerQueue struct {
	exp     sdklog.Exporter
	q       chan sdklog.Record
	batch   int
	every   time.Duration
	flush   chan chan error
	stop    chan context.Context
	stopped sync.WaitGroup
	// mu orders rows against the shutdown: OnEmit holds it shared from
	// its check of closed to its enqueue, Shutdown exclusively to set
	// closed. Every row OnEmit accepted is then queued before the worker
	// drains, and every later one is refused and counted: none is queued
	// with no worker left to deliver or count it (golem#4).
	mu     sync.RWMutex
	closed bool
	// emitting, if set, runs inside OnEmit's critical section (a test's
	// pause between the check and the enqueue).
	emitting func()
	// base is the running exports' context: a shutdown's deadline cancels
	// it, so an export already blocked on the collector ends with it.
	base       context.Context
	cancelBase context.CancelFunc
	// Dropped counts rows refused (the queue was full, or shut down);
	// Failed, rows the exporter did not take after its retries, or the
	// shutdown's deadline left undelivered; Retried, the exports retried.
	Dropped, Failed, Retried atomic.Int64
	backoff                  atomic.Int64 // the first retry's wait (SetBackoff)
}

// exportTimeout bounds one export while running; a shutdown's deadline
// bounds its own.
const exportTimeout = 30 * time.Second

// NewLedgerQueue starts a queue of at most size rows, exported in batches of
// up to batch rows at least every interval.
func NewLedgerQueue(exp sdklog.Exporter, size, batch int, every time.Duration) *LedgerQueue {
	q := &LedgerQueue{exp: exp, q: make(chan sdklog.Record, size), batch: batch, every: every,
		flush: make(chan chan error), stop: make(chan context.Context, 1)}
	q.base, q.cancelBase = context.WithCancel(context.Background())
	q.backoff.Store(int64(time.Second))
	q.stopped.Go(q.run)
	return q
}

// OnEmit queues a row; it never blocks the request path.
func (q *LedgerQueue) OnEmit(_ context.Context, r *sdklog.Record) error {
	q.mu.RLock()
	defer q.mu.RUnlock()
	if q.closed {
		q.drop("the ledger queue is shut down")
		return nil
	}
	if q.emitting != nil {
		q.emitting()
	}
	select {
	case q.q <- r.Clone(): // the SDK reuses r after the call
	default:
		q.drop("the ledger queue is full")
	}
	return nil
}

// drop counts a refused row, and says so at the 1st, 2nd, 4th, 8th...
func (q *LedgerQueue) drop(why string) {
	if n := q.Dropped.Add(1); n&(n-1) == 0 {
		slog.Error("telemetry: "+why+"; ledger rows are being dropped (golem.ledger.dropped)", "dropped", n, "size", cap(q.q))
	}
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
		case reply := <-q.flush:
			buf = q.drain(q.base, buf)
			reply <- nil
			continue
		case ctx := <-q.stop:
			buf = q.drain(ctx, buf)
			if n := len(buf) + len(q.q); n > 0 { // the deadline passed
				q.Failed.Add(int64(n))
				slog.Error("telemetry: ledger rows undelivered at shutdown (golem.ledger.failed)", "rows", n)
			}
			return
		}
		buf = q.export(q.base, buf)
	}
}

// drain exports everything queued, while ctx lasts; it returns what it
// could not send.
func (q *LedgerQueue) drain(ctx context.Context, buf []sdklog.Record) []sdklog.Record {
	for ctx.Err() == nil {
		for len(buf) < q.batch && len(q.q) > 0 {
			buf = append(buf, <-q.q)
		}
		if len(buf) == 0 {
			return buf
		}
		buf = q.export(ctx, buf)
	}
	return buf
}

// export delivers a batch, retrying a failure with backoff: a collector
// restarting is a few seconds, and its rows must not be dropped for it
// (golem#4). A batch the collector may have partly taken is sent whole
// again; the ledger deduplicates by event (golem.spend_dedup,
// golem.spend_seq's ReplacingMergeTree). After the last attempt, or at the
// shutdown's deadline, its rows are counted as failed.
func (q *LedgerQueue) export(ctx context.Context, buf []sdklog.Record) []sdklog.Record {
	if len(buf) == 0 {
		return buf
	}
	backoff := time.Duration(q.backoff.Load())
	for attempt := 1; ; attempt++ {
		ectx, cancel := context.WithTimeout(ctx, exportTimeout)
		err := q.exp.Export(ectx, buf)
		cancel()
		if err == nil {
			break
		}
		if attempt == exportAttempts || ctx.Err() != nil {
			q.Failed.Add(int64(len(buf)))
			slog.Error("telemetry: ledger rows not delivered (golem.ledger.failed)", "rows", len(buf), "attempts", attempt, "err", err)
			break
		}
		q.Retried.Add(1)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
		}
		backoff = min(2*backoff, 30*time.Second)
	}
	clear(buf) // drop the records' references
	return buf[:0]
}

// exportAttempts bounds a batch's retries: with the backoff, about four
// minutes of a collector down before its rows are given up.
const exportAttempts = 8

// SetBackoff sets the first retry's wait (default 1 s); it doubles, to at
// most 30 s.
func (q *LedgerQueue) SetBackoff(d time.Duration) { q.backoff.Store(int64(d)) }

// Shutdown refuses further rows (counting them), exports what is queued
// while ctx lasts, counts the rest as failed, and stops.
func (q *LedgerQueue) Shutdown(ctx context.Context) error {
	q.mu.Lock()
	was := q.closed
	q.closed = true
	q.mu.Unlock()
	if was {
		return nil
	}
	stopCancel := context.AfterFunc(ctx, q.cancelBase)
	defer stopCancel()
	defer q.cancelBase()
	q.stop <- ctx
	q.stopped.Wait()
	return q.exp.Shutdown(ctx)
}

// ForceFlush exports every row queued now.
func (q *LedgerQueue) ForceFlush(ctx context.Context) error {
	q.mu.RLock()
	closed := q.closed
	q.mu.RUnlock()
	if closed {
		return nil
	}
	reply := make(chan error, 1)
	select {
	case q.flush <- reply:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-reply:
		if err != nil {
			return err
		}
		return q.exp.ForceFlush(ctx)
	case <-ctx.Done():
		return ctx.Err()
	}
}
