package telemetry

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

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
// the providers and their shutdown. Telemetry carries the ledger: it goes
// over TLS unless the collector is on this machine (a node-local agent at
// a literal loopback address, 127.0.0.1 or [::1]; ADR 0043, golem#5).
func OTLP(ctx context.Context, endpoint string, res *resource.Resource) (Providers, func(context.Context) error, error) {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return Providers{}, nil, fmt.Errorf("telemetry endpoint %q: want host:port", endpoint)
	}
	traceOpts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(endpoint)}
	metricOpts := []otlpmetrichttp.Option{otlpmetrichttp.WithEndpoint(endpoint)}
	logOpts := []otlploghttp.Option{otlploghttp.WithEndpoint(endpoint)}
	if plaintextOK(host) {
		traceOpts = append(traceOpts, otlptracehttp.WithInsecure())
		metricOpts = append(metricOpts, otlpmetrichttp.WithInsecure())
		logOpts = append(logOpts, otlploghttp.WithInsecure())
	}
	te, err := otlptracehttp.New(ctx, traceOpts...)
	if err != nil {
		return Providers{}, nil, err
	}
	me, err := otlpmetrichttp.New(ctx, metricOpts...)
	if err != nil {
		return Providers{}, nil, err
	}
	le, err := otlploghttp.New(ctx, logOpts...)
	if err != nil {
		return Providers{}, nil, err
	}
	p, shutdown := SDK(te, sdkmetric.NewPeriodicReader(me), le, res)
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
	// what it cannot deliver. Their 40 keys are unique by construction (a
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

type jsonLogger struct {
	embedded.Logger
	mu  sync.Mutex
	w   io.Writer
	buf bytes.Buffer
	enc jsontext.Encoder
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
	j.enc.WriteToken(jsontext.BeginObject)
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		j.enc.WriteToken(jsontext.String(string(kv.Key)))
		writeValue(&j.enc, kv.Value)
		return true
	})
	j.enc.WriteToken(jsontext.EndObject) // the encoder ends a top-level value with "\n"
	j.w.Write(j.buf.Bytes())
}

func writeValue(enc *jsontext.Encoder, v attribute.Value) {
	switch v.Type() {
	case attribute.BOOL:
		enc.WriteToken(jsontext.Bool(v.AsBool()))
	case attribute.INT64:
		enc.WriteToken(jsontext.Int(v.AsInt64()))
	case attribute.FLOAT64:
		enc.WriteToken(jsontext.Float(v.AsFloat64()))
	case attribute.STRINGSLICE:
		enc.WriteToken(jsontext.BeginArray)
		for _, s := range v.AsStringSlice() {
			enc.WriteToken(jsontext.String(s))
		}
		enc.WriteToken(jsontext.EndArray)
	default:
		enc.WriteToken(jsontext.String(v.Emit()))
	}
}

// plaintextOK reports whether telemetry may go to host unencrypted: only
// to this machine, named by a literal loopback address. A name, localhost
// included, is resolved when dialed, by whatever resolver and hosts file
// the node has, so it would not hold telemetry to this machine: a name
// gets TLS.
func plaintextOK(host string) bool {
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}
