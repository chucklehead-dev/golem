package telemetry

import (
	"log/slog"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/chucklehead-dev/golem/lib/contract"
)

// BenchmarkCall is the request path's telemetry for a call that failed
// over once (an attempt retried, the next committed) against the SDK's
// meter, with the ledger's rows written to a discarding logger: its
// allocations are the metrics' and the row's, none per label set.
func BenchmarkCall(b *testing.B) {
	p := Providers{Log: slog.New(slog.DiscardHandler), Tracer: tracenoop.NewTracerProvider(), Meter: sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewManualReader())),
		Logger: Stdout(discardWriter{}).Logger}
	tel, err := New(p, "us-east-2", "internal")
	if err != nil {
		b.Fatal(err)
	}
	ctx := b.Context()
	retried, ended := call, call
	retried.Attempt, retried.Outcome, retried.Status, retried.AttemptResult = 1, "failed", 503, AttemptRefused
	ended.Attempt, ended.AttemptResult, ended.Committed = 2, AttemptCommitted, call.Started.Add(time.Second)
	b.ReportAllocs()
	for b.Loop() {
		tel.Retried(ctx, retried)
		tel.Ended(ctx, ended)
		tel.Refused(ctx, Refusal{Code: contract.BudgetPoolExhausted, RequestID: "r", Principal: "alice", Scope: "team-a"})
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
