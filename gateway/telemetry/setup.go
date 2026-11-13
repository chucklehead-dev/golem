package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/embedded"
	"go.opentelemetry.io/otel/log/global"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

// OTLP sets up OTLP/HTTP exporters for traces, metrics and logs to the
// collector at endpoint (host:port), installs them globally, and returns
// the providers and their shutdown.
func OTLP(ctx context.Context, endpoint string, res *resource.Resource) (Providers, func(context.Context) error, error) {
	te, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpoint(endpoint), otlptracehttp.WithInsecure())
	if err != nil {
		return Providers{}, nil, err
	}
	me, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpoint(endpoint), otlpmetrichttp.WithInsecure())
	if err != nil {
		return Providers{}, nil, err
	}
	le, err := otlploghttp.New(ctx, otlploghttp.WithEndpoint(endpoint), otlploghttp.WithInsecure())
	if err != nil {
		return Providers{}, nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(te), sdktrace.WithResource(res))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(me)), sdkmetric.WithResource(res))
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(le)), sdklog.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	global.SetLoggerProvider(lp)
	shutdown := func(ctx context.Context) error {
		return errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx), lp.Shutdown(ctx))
	}
	return Providers{Tracer: tp, Meter: mp, Logger: lp}, shutdown, nil
}

// Stdout is standalone telemetry: no spans or metrics, and the ledger's
// rows written to w as JSON lines, one per row.
func Stdout(w io.Writer) Providers {
	return Providers{Tracer: tracenoop.NewTracerProvider(), Meter: metricnoop.NewMeterProvider(), Logger: &jsonLogs{l: jsonLogger{w: w}}}
}

// jsonLogs is a minimal LoggerProvider writing each record's attributes as
// a JSON object, one per line.
type jsonLogs struct {
	embedded.LoggerProvider
	l jsonLogger
}

type jsonLogger struct {
	embedded.Logger
	mu sync.Mutex
	w  io.Writer
}

func (j *jsonLogs) Logger(string, ...otellog.LoggerOption) otellog.Logger { return &j.l }

func (j *jsonLogger) Enabled(context.Context, otellog.EnabledParameters) bool { return true }

func (j *jsonLogger) Emit(_ context.Context, r otellog.Record) {
	m := map[string]any{}
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		m[string(kv.Key)] = kv.Value.AsInterface()
		return true
	})
	b, _ := json.Marshal(m)
	j.mu.Lock()
	defer j.mu.Unlock()
	j.w.Write(append(b, '\n'))
}
