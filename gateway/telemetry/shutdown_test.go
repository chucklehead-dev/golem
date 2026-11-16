package telemetry

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	otellog "go.opentelemetry.io/otel/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// metricExp counts the exports made with time left on their context.
type metricExp struct{ exported atomic.Int64 }

func (*metricExp) Temporality(k sdkmetric.InstrumentKind) metricdata.Temporality {
	return sdkmetric.DefaultTemporalitySelector(k)
}
func (*metricExp) Aggregation(k sdkmetric.InstrumentKind) sdkmetric.Aggregation {
	return sdkmetric.DefaultAggregationSelector(k)
}
func (e *metricExp) Export(ctx context.Context, _ *metricdata.ResourceMetrics) error {
	if ctx.Err() == nil {
		e.exported.Add(1)
	}
	return ctx.Err()
}
func (*metricExp) ForceFlush(context.Context) error { return nil }
func (*metricExp) Shutdown(context.Context) error   { return nil }

// TestShutdownBudgets: a ledger drain that uses all of its shutdown budget
// (the collector not answering) leaves the meter provider its own: the
// last metrics, golem.ledger.failed among them, still export.
func TestShutdownBudgets(t *testing.T) {
	logs := &recordingExp{gate: make(chan struct{})} // never answers
	metrics := &metricExp{}
	p, shutdown := SDK(tracetest.NewInMemoryExporter(), sdkmetric.NewPeriodicReader(metrics, sdkmetric.WithInterval(time.Hour)), logs, resource.Empty())
	var r otellog.Record
	p.Logger.Logger("golem.ledger").Emit(t.Context(), r)
	c, _ := p.Meter.Meter("test").Int64Counter("golem.ledger.failed")
	c.Add(t.Context(), 1)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	shutdown(ctx)
	if p.Ledger.Failed.Load() != 1 {
		t.Fatalf("failed %d: the ledger's row was not given up at its deadline", p.Ledger.Failed.Load())
	}
	if metrics.exported.Load() == 0 {
		t.Fatal("the last metrics were not exported: the ledger's drain used the meter provider's budget")
	}
}
