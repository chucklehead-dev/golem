// Package telemetry is the gateway's OpenTelemetry (ADR 0015): a span per
// request with the GenAI semantic conventions and golem's attributes, the
// GenAI token and duration metrics, a refusal counter, and the access-log
// record that is also the ledger's row (contracts.md §3), all over OTLP to
// the enterprise collector. Standalone, ledger rows go to stdout as JSON
// lines instead (ADR 0023).
package telemetry

import (
	"context"
	"errors"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/lib/contract"
)

// GenAI semantic-convention names (the OTel Go release golem uses does not
// yet generate them).
const (
	attrOperation     = "gen_ai.operation.name"
	attrProvider      = "gen_ai.provider.name"
	attrRequestModel  = "gen_ai.request.model"
	attrResponseModel = "gen_ai.response.model"
	attrInputTokens   = "gen_ai.usage.input_tokens"
	attrOutputTokens  = "gen_ai.usage.output_tokens"
	attrTokenType     = "gen_ai.token.type"
	metricTokens      = "gen_ai.client.token.usage"
	metricDuration    = "gen_ai.client.operation.duration"
)

// Call is what the request path reports when a call ends.
type Call struct {
	RequestID                       string
	Principal, PrincipalKind, Key   string
	Scope                           string
	Frontend, Backend               string // dialects
	Alias, Model, Deployment        string
	Provider, ProviderRequestID     string
	Started, Ended                  time.Time
	Usage                           canon.Usage
	Cost                            int64 // micro-units
	Currency                        string
	Status                          int
	Outcome, ErrorOrigin, ErrorCode string
	Estimated                       bool // usage counted from a cut stream's deltas
	// Counted: the limiter numbers this call's event, and its ledger row
	// waits for the number (Numbered). Else the row is written at once.
	Counted bool
}

// Telemetry records calls. Its methods are safe for concurrent use.
type Telemetry struct {
	tracer   trace.Tracer
	tokens   metric.Int64Histogram
	duration metric.Float64Histogram
	refusals metric.Int64Counter
	Ledger   *Ledger
}

// Providers are the OTel providers telemetry records through.
type Providers struct {
	Tracer trace.TracerProvider
	Meter  metric.MeterProvider
	Logger otellog.LoggerProvider // the ledger's rows; nil: the global one
}

// New makes telemetry for a site and gateway pool.
func New(p Providers, site, pool string) (*Telemetry, error) {
	m := p.Meter.Meter("golem/gateway")
	tokens, err1 := m.Int64Histogram(metricTokens, metric.WithUnit("{token}"),
		metric.WithDescription("Tokens used per call, by type"),
		metric.WithExplicitBucketBoundaries(1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304))
	duration, err2 := m.Float64Histogram(metricDuration, metric.WithUnit("s"),
		metric.WithDescription("Call duration, from the request to the response's end"))
	refusals, err3 := m.Int64Counter("golem.refusals", metric.WithDescription("Requests refused, by contract code"))
	if err := errors.Join(err1, err2, err3); err != nil {
		return nil, err
	}
	lp := p.Logger
	if lp == nil {
		lp = global.GetLoggerProvider()
	}
	return &Telemetry{tracer: p.Tracer.Tracer("golem/gateway"), tokens: tokens, duration: duration, refusals: refusals,
		Ledger: NewLedger(lp.Logger("golem/ledger"), site, pool)}, nil
}

// Start begins a request's span; the request path ends it with Ended or
// Refused.
func (t *Telemetry) Start(ctx context.Context, frontend string) (context.Context, trace.Span) {
	return t.tracer.Start(ctx, "chat", trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attribute.String(attrOperation, "chat"), attribute.String("golem.dialect", frontend)))
}

// Ended records a call that reached a provider: the span's attributes, the
// metrics, and the ledger row.
func (t *Telemetry) Ended(ctx context.Context, c Call) {
	span := trace.SpanFromContext(ctx)
	span.SetName("chat " + c.Alias)
	span.SetAttributes(
		attribute.String(attrProvider, c.Provider),
		attribute.String(attrRequestModel, c.Alias),
		attribute.String(attrResponseModel, c.Model),
		attribute.Int64(attrInputTokens, c.Usage.Input+c.Usage.CacheRead+c.Usage.CacheWrite),
		attribute.Int64(attrOutputTokens, c.Usage.Output),
		attribute.String("golem.request_id", c.RequestID),
		attribute.String("golem.principal", c.Principal),
		attribute.String("golem.scope", c.Scope),
		attribute.String("golem.deployment", c.Deployment),
		attribute.Int64("golem.cost_micros", c.Cost),
		attribute.Bool("golem.usage_estimated", c.Estimated),
		attribute.Int("http.response.status_code", c.Status),
	)
	if c.ErrorOrigin != "" {
		span.SetAttributes(attribute.String("golem.error.origin", c.ErrorOrigin), attribute.String("golem.error.code", c.ErrorCode))
	}
	set := metric.WithAttributes(attribute.String(attrProvider, c.Provider), attribute.String(attrRequestModel, c.Alias),
		attribute.String(attrResponseModel, c.Model))
	in := c.Usage.Input + c.Usage.CacheRead + c.Usage.CacheWrite
	t.tokens.Record(ctx, in, set, metric.WithAttributes(attribute.String(attrTokenType, "input")))
	t.tokens.Record(ctx, c.Usage.Output, set, metric.WithAttributes(attribute.String(attrTokenType, "output")))
	t.duration.Record(ctx, c.Ended.Sub(c.Started).Seconds(), set)
	t.Ledger.Ended(ctx, c)
}

// Refused records a request golem refused (no provider call, no ledger row).
func (t *Telemetry) Refused(ctx context.Context, code contract.Code) {
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("golem.error.origin", string(code.Origin)), attribute.String("golem.error.code", code.Name),
		attribute.Int("http.response.status_code", code.Status))
	t.refusals.Add(ctx, 1, metric.WithAttributes(attribute.String("golem.error.code", code.Name),
		attribute.String("golem.error.origin", string(code.Origin))))
}
