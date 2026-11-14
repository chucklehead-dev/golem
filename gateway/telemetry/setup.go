package telemetry

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/embedded"
	"go.opentelemetry.io/otel/log/global"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/chucklehead-dev/golem/lib/otlp"
)

// OTLP sets up OTLP/HTTP exporters for traces, metrics and logs to the
// collector at endpoint (host:port; lib/otlp's transport rule), installs
// them globally, and returns the providers and their shutdown.
func OTLP(ctx context.Context, endpoint string, res *resource.Resource) (Providers, func(context.Context) error, error) {
	ex, err := otlp.New(ctx, endpoint)
	if err != nil {
		return Providers{}, nil, err
	}
	p, shutdown := SDK(ex.Spans, sdkmetric.NewPeriodicReader(ex.Metrics), ex.Logs, res)
	otel.SetTracerProvider(p.Tracer)
	otel.SetMeterProvider(p.Meter)
	global.SetLoggerProvider(p.Logger)
	return p, shutdown, nil
}

// SDK builds the providers over any exporters: OTLP's, or a benchmark's.
func SDK(spans sdktrace.SpanExporter, metrics sdkmetric.Reader, logs sdklog.Exporter, res *resource.Resource) (Providers, func(context.Context) error) {
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(spans), sdktrace.WithResource(res))
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(metrics), sdkmetric.WithResource(res))
	// The ledger's rows are billing data: their own queue, which counts
	// what it cannot deliver. Their 39 keys are unique by construction (a
	// test checks them against the DDL), so the SDK's per-record key
	// deduplication, a quarter of a call's CPU, is turned off.
	ledger := NewLedgerQueue(logs, ledgerQueueSize, 512, time.Second)
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(ledger), sdklog.WithResource(res), sdklog.WithAllowKeyDuplication())
	// The ledger first, the metrics last: the final drop and failure
	// counts are exported after the rows they count.
	shutdown := func(ctx context.Context) error {
		return errors.Join(lp.Shutdown(ctx), tp.Shutdown(ctx), mp.Shutdown(ctx))
	}
	return Providers{Tracer: tp, Meter: mp, Logger: lp, Ledger: ledger}, shutdown
}

// ledgerQueueSize holds about 30 seconds of rows at 2,000 calls a second: a
// collector restart, not an outage (a durable spool is for that).
const ledgerQueueSize = 1 << 16

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

// jsonLogger is jsonLogs' one logger, whatever the name asked for: rows are
// written whole under mu, one buffer and encoder reused for each.
type jsonLogger struct {
	embedded.Logger
	mu  sync.Mutex
	w   io.Writer
	buf bytes.Buffer
	enc jsontext.Encoder
	// failed counts rows not written (an encoding or a write error):
	// standalone has no meter, so each power of two is logged instead.
	failed int64
}

func (j *jsonLogs) Logger(string, ...otellog.LoggerOption) otellog.Logger { return &j.l }

func (j *jsonLogger) Enabled(context.Context, otellog.EnabledParameters) bool { return true }

// Emit writes the record's attributes as one JSON object, in their order
// (the DDL's, for ledger rows).
func (j *jsonLogger) Emit(_ context.Context, r otellog.Record) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.buf.Reset()
	j.enc.Reset(&j.buf)
	err := j.enc.WriteToken(jsontext.BeginObject)
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		err = errors.Join(err, j.enc.WriteToken(jsontext.String(string(kv.Key))), writeValue(&j.enc, kv.Value))
		return err == nil
	})
	if err == nil {
		err = j.enc.WriteToken(jsontext.EndObject) // the encoder ends a top-level value with "\n"
	}
	if err == nil {
		_, err = j.w.Write(j.buf.Bytes())
	}
	if err != nil {
		if j.failed++; j.failed&(j.failed-1) == 0 {
			slog.Error("telemetry: a ledger row was not written to stdout; rows are being lost", "failed", j.failed, "err", err)
		}
	}
}

// writeValue writes an attribute's value as its JSON type; the ledger's
// other types (none today) as their string form.
func writeValue(enc *jsontext.Encoder, v attribute.Value) error {
	switch v.Type() {
	case attribute.BOOL:
		return enc.WriteToken(jsontext.Bool(v.AsBool()))
	case attribute.INT64:
		return enc.WriteToken(jsontext.Int(v.AsInt64()))
	case attribute.FLOAT64:
		return enc.WriteToken(jsontext.Float(v.AsFloat64()))
	case attribute.STRINGSLICE:
		err := enc.WriteToken(jsontext.BeginArray)
		for _, s := range v.AsStringSlice() {
			err = errors.Join(err, enc.WriteToken(jsontext.String(s)))
		}
		return errors.Join(err, enc.WriteToken(jsontext.EndArray))
	default:
		return enc.WriteToken(jsontext.String(v.Emit()))
	}
}
