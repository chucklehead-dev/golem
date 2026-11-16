// Package otlp is golem's OTLP/HTTP transport to a collector, shared by the
// gateway and golem-control. Telemetry carries billing and audit data, so
// it goes over TLS unless the collector is a node-local agent at a literal
// loopback address, 127.0.0.1 or [::1] (ADR 0043).
//
// Each exporter is given its whole URL, scheme included: the SDK applies
// the OTEL_EXPORTER_OTLP_* variables (INSECURE, ENDPOINT, the per-signal
// ones) before options, and an explicit URL overrides every one of them,
// so none can turn TLS off or send the data elsewhere. The CERTIFICATE
// variables still apply: they add trust, they cannot remove verification.
package otlp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// Exporters are the three signals' exporters to one collector.
type Exporters struct {
	Spans   sdktrace.SpanExporter
	Metrics sdkmetric.Exporter
	Logs    sdklog.Exporter
}

// New is the exporters to the collector at endpoint (host:port).
func New(ctx context.Context, endpoint string) (Exporters, error) {
	base, err := Base(endpoint)
	if err != nil {
		return Exporters{}, err
	}
	te, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(base+"/v1/traces"))
	if err != nil {
		return Exporters{}, err
	}
	me, err := Metrics(ctx, endpoint)
	if err != nil {
		return Exporters{}, err
	}
	le, err := Logs(ctx, endpoint)
	if err != nil {
		return Exporters{}, err
	}
	return Exporters{Spans: te, Metrics: me, Logs: le}, nil
}

// Logs is the log exporter alone, to the collector at endpoint.
func Logs(ctx context.Context, endpoint string) (sdklog.Exporter, error) {
	base, err := Base(endpoint)
	if err != nil {
		return nil, err
	}
	return otlploghttp.New(ctx, otlploghttp.WithEndpointURL(base+"/v1/logs"))
}

// Metrics is the metric exporter alone, to the collector at endpoint.
func Metrics(ctx context.Context, endpoint string) (sdkmetric.Exporter, error) {
	base, err := Base(endpoint)
	if err != nil {
		return nil, err
	}
	return otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpointURL(base+"/v1/metrics"))
}

// MeterProvider is a meter provider over Metrics, read periodically (the
// SDK's default, every minute), for a component that emits no spans
// (golem-control), and its shutdown, which exports a last collection.
func MeterProvider(ctx context.Context, endpoint string, res *resource.Resource) (*sdkmetric.MeterProvider, func(context.Context) error, error) {
	me, err := Metrics(ctx, endpoint)
	if err != nil {
		return nil, nil, err
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(me)), sdkmetric.WithResource(res))
	return mp, mp.Shutdown, nil
}

// LoggerProvider is a batching log provider over Logs, for a component
// that emits only log records (golem-control's audit), and its shutdown,
// which flushes.
func LoggerProvider(ctx context.Context, endpoint string, res *resource.Resource) (otellog.LoggerProvider, func(context.Context) error, error) {
	le, err := Logs(ctx, endpoint)
	if err != nil {
		return nil, nil, err
	}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(le)), sdklog.WithResource(res))
	return lp, func(ctx context.Context) error { return errors.Join(lp.ForceFlush(ctx), lp.Shutdown(ctx)) }, nil
}

// Base is the collector's base URL: https unless plaintext is allowed.
func Base(endpoint string) (string, error) {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", fmt.Errorf("telemetry endpoint %q: want host:port", endpoint)
	}
	if PlaintextOK(host) {
		return "http://" + endpoint, nil
	}
	return "https://" + endpoint, nil
}

// PlaintextOK reports whether host is a literal loopback address: a name
// is resolved when dialed, so it is not held to this machine.
func PlaintextOK(host string) bool {
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}
