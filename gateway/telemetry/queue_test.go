package telemetry

import (
	"context"
	"errors"
	"fmt"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"sync"
	"sync/atomic"
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
	q.SetBackoff(time.Millisecond)
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

// flaky fails its first n exports.
type flaky struct {
	recordingExp
	failures int
}

func (f *flaky) Export(ctx context.Context, rs []sdklog.Record) error {
	f.mu.Lock()
	if f.failures > 0 {
		f.failures--
		f.mu.Unlock()
		return errors.New("collector restarting")
	}
	f.mu.Unlock()
	return f.recordingExp.Export(ctx, rs)
}

// TestLedgerQueueRetries: a collector that restarts loses no rows: a failed
// batch is retried, with backoff, until it is taken.
func TestLedgerQueueRetries(t *testing.T) {
	exp := &flaky{failures: 3}
	q := NewLedgerQueue(exp, 100, 10, time.Hour)
	q.SetBackoff(time.Millisecond)
	var r sdklog.Record
	for range 25 {
		q.OnEmit(t.Context(), &r)
	}
	q.Shutdown(t.Context())
	if exp.got != 25 || q.Failed.Load() != 0 || q.Retried.Load() != 3 {
		t.Fatalf("delivered %d of 25, failed %d, retried %d", exp.got, q.Failed.Load(), q.Retried.Load())
	}
}

// partial takes every batch it is given but reports the first as failed:
// a collector that wrote it and lost its answer.
type partial struct {
	mu     sync.Mutex
	failed bool
	seen   map[string]int // event_id -> deliveries
}

func (p *partial) Export(_ context.Context, rs []sdklog.Record) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, r := range rs {
		r.WalkAttributes(func(kv attribute.KeyValue) bool {
			if kv.Key == "event_id" {
				p.seen[kv.Value.AsString()]++
			}
			return true
		})
	}
	if !p.failed {
		p.failed = true
		return errors.New("timeout reading the collector's answer")
	}
	return nil
}
func (p *partial) Shutdown(context.Context) error   { return nil }
func (p *partial) ForceFlush(context.Context) error { return nil }

// TestLedgerQueuePartialDelivery: a batch the collector took but did not
// acknowledge is sent again, whole, each row with its own event_id, so the
// ledger's deduplication by event (golem.spend_dedup; the watermark job's
// rows by event, TestAgainstBruteForce's duplicates) counts it once
// (golem#4).
func TestLedgerQueuePartialDelivery(t *testing.T) {
	exp := &partial{seen: map[string]int{}}
	q := NewLedgerQueue(exp, 100, 10, time.Hour)
	q.SetBackoff(time.Millisecond)
	// Rows come as production's do, through a provider (a bare record
	// keeps no attributes).
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(q), sdklog.WithAllowKeyDuplication())
	logger := lp.Logger("test")
	for i := range 10 {
		var r otellog.Record
		r.AddAttributes(attribute.String("event_id", fmt.Sprint("e", i)))
		logger.Emit(t.Context(), r)
	}
	lp.Shutdown(t.Context())
	if len(exp.seen) != 10 || q.Failed.Load() != 0 || q.Retried.Load() != 1 {
		t.Fatalf("%d distinct rows, failed %d, retried %d", len(exp.seen), q.Failed.Load(), q.Retried.Load())
	}
	for id, n := range exp.seen {
		if n != 2 {
			t.Fatalf("%s delivered %d times: once with the lost answer, once on retry, same event_id", id, n)
		}
	}
}

// TestLedgerQueueShutdownRaces: rows emitted from many goroutines while
// shutdown runs, with exports failing and retrying, or blocked in flight:
// shutdown keeps to its deadline, and every row is delivered, failed or
// dropped, none unaccounted (run with -race).
func TestLedgerQueueShutdownRaces(t *testing.T) {
	for _, tc := range []struct {
		name string
		exp  interface {
			sdklog.Exporter
			delivered() int
		}
	}{
		{"exports failing, retrying", &counting{fail: true}},
		{"an export blocked in flight", &counting{block: make(chan struct{})}},
		{"exports succeeding", &counting{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := NewLedgerQueue(tc.exp, 64, 8, time.Millisecond)
			q.SetBackoff(5 * time.Millisecond)
			var emitted atomic.Int64
			var wg sync.WaitGroup
			for range 8 {
				wg.Go(func() {
					var r sdklog.Record
					for range 200 {
						q.OnEmit(context.Background(), &r)
						emitted.Add(1)
					}
				})
			}
			time.Sleep(2 * time.Millisecond)
			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()
			start := time.Now()
			q.Shutdown(ctx)
			took := time.Since(start)
			wg.Wait()
			if took > 2*time.Second {
				t.Fatalf("shutdown took %v past its 200 ms deadline", took)
			}
			got := int64(tc.exp.delivered()) + q.Failed.Load() + q.Dropped.Load()
			if got != emitted.Load() {
				t.Fatalf("emitted %d; delivered %d + failed %d + dropped %d = %d", emitted.Load(), tc.exp.delivered(),
					q.Failed.Load(), q.Dropped.Load(), got)
			}
		})
	}
}

// TestLedgerQueueEmitDuringShutdown: a row whose emission had passed the
// closed check when Shutdown began is delivered or counted, not queued
// after the worker's last drain (golem#4). The emission is held between
// its check and its enqueue while Shutdown runs.
func TestLedgerQueueEmitDuringShutdown(t *testing.T) {
	exp := &counting{}
	q := NewLedgerQueue(exp, 64, 8, time.Millisecond)
	checked, release := make(chan struct{}), make(chan struct{})
	q.emitting = func() { close(checked); <-release }
	emitted := make(chan struct{})
	go func() {
		var r sdklog.Record
		q.OnEmit(context.Background(), &r)
		close(emitted)
	}()
	<-checked
	q.emitting = nil // the shutdown's own rows, if any, are not held
	done := make(chan struct{})
	go func() { q.Shutdown(t.Context()); close(done) }()
	select {
	case <-done: // the shutdown did not wait: the row comes too late
	case <-time.After(100 * time.Millisecond): // waiting for the emission
	}
	close(release)
	<-emitted
	<-done
	if got := exp.delivered() + int(q.Failed.Load()+q.Dropped.Load()); got != 1 {
		t.Fatalf("delivered %d + failed %d + dropped %d: the row emitted during shutdown is lost",
			exp.delivered(), q.Failed.Load(), q.Dropped.Load())
	}
}

// TestLedgerQueueFlushDuringShutdown: a ForceFlush that began before a
// Shutdown which stopped the worker returns, rather than waiting forever
// on a worker that is gone. The flush is held before its request until the
// shutdown has finished.
func TestLedgerQueueFlushDuringShutdown(t *testing.T) {
	q := NewLedgerQueue(&counting{}, 64, 8, time.Millisecond)
	started, release := make(chan struct{}), make(chan struct{})
	q.flushing = func() { close(started); <-release }
	flushed := make(chan error, 1)
	go func() { flushed <- q.ForceFlush(context.Background()) }()
	<-started
	if err := q.Shutdown(t.Context()); err != nil {
		t.Fatal(err)
	}
	close(release)
	select {
	case err := <-flushed:
		if err != nil {
			t.Fatalf("ForceFlush after shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ForceFlush waits on a worker that shut down")
	}
}

// counting counts the rows it took; it may fail every export, or block
// each until its context ends.
type counting struct {
	mu    sync.Mutex
	got   int
	fail  bool
	block chan struct{}
}

func (c *counting) Export(ctx context.Context, rs []sdklog.Record) error {
	if c.block != nil {
		select {
		case <-c.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if c.fail {
		return errors.New("collector down")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.got += len(rs)
	return nil
}
func (c *counting) delivered() int                   { c.mu.Lock(); defer c.mu.Unlock(); return c.got }
func (c *counting) Shutdown(context.Context) error   { return nil }
func (c *counting) ForceFlush(context.Context) error { return nil }
