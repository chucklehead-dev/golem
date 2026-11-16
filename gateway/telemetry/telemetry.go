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
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
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
	RequestID                     string
	Principal, PrincipalKind, Key string
	Scope                         string
	Frontend, Backend             string // dialects
	Envelope                      string // the deployment's: "" or "bedrock"
	Alias, Model, Deployment      string
	Provider, ProviderRequestID   string
	Started, Ended                time.Time
	// Period is the budget period the call is counted in (the limiter's,
	// lib/period); empty: the ledger names it from Started.
	Period                          string
	Usage                           canon.Usage
	Cost                            int64 // micro-units
	Currency                        string
	Status                          int
	Outcome, ErrorOrigin, ErrorCode string
	// EstimatedFraction is the share of Cost estimated from what was seen
	// (a cut stream, a missing usage report), not reported: 0 for actual.
	EstimatedFraction float64
	// Waived: admitted outside the budget's bound, while the site store was
	// unreachable or in the grace after a recovery (ADR 0041 §7.3).
	Waived bool
	// OverBudget: admitted over one of its accounts' limits (its key's cap,
	// its member's, its scope's budget) in observe or warn mode, a call
	// enforce would have refused (PRD §9.1, ADR 0050).
	OverBudget bool
	// Attempt is the call's attempt the row records, from 1 (0 is 1): each
	// attempt is its own ledger event (AttemptID), so a retried call's
	// rows are never deduplicated into one.
	Attempt int
}

// AttemptID is the attempt's ID, its ledger event's: the request ID for a
// call's first attempt, then the request ID and the attempt's number.
func (c *Call) AttemptID() string {
	if c.Attempt <= 1 {
		return c.RequestID
	}
	return c.RequestID + "." + strconv.Itoa(c.Attempt)
}

// Telemetry records calls. Its methods are safe for concurrent use, and on
// a nil *Telemetry do nothing (a gateway without telemetry, tests).
type Telemetry struct {
	tracer   trace.Tracer
	tokens   metric.Int64Histogram
	duration metric.Float64Histogram
	refusals metric.Int64Counter
	Ledger   *Ledger

	// Attribute sets are built once per route and per refusal code, not per
	// request: building and merging them was a quarter of a call's
	// allocations. The keys are bounded by the snapshot's routes and the
	// contract's codes.
	routes  sync.Map // routeKey -> *routeSets
	refused sync.Map // code name -> []metric.AddOption
	starts  sync.Map // frontend -> []trace.SpanStartOption
}

type routeKey struct{ provider, alias, model string }

// routeSets are one route's precomputed options.
type routeSets struct {
	name          string // the span's: "chat <alias>"
	input, output []metric.RecordOption
	duration      []metric.RecordOption
}

// routeSets are c's route's options, made on its first call.
func (t *Telemetry) routeSets(c *Call) *routeSets {
	return cached(&t.routes, routeKey{c.Provider, c.Alias, c.Model}, func() *routeSets {
		attrs := []attribute.KeyValue{attribute.String(attrProvider, c.Provider), attribute.String(attrRequestModel, c.Alias),
			attribute.String(attrResponseModel, c.Model)}
		with := func(extra ...attribute.KeyValue) []metric.RecordOption {
			return []metric.RecordOption{metric.WithAttributeSet(attribute.NewSet(append(slices.Clone(attrs), extra...)...))}
		}
		return &routeSets{name: "chat " + c.Alias, input: with(attribute.String(attrTokenType, "input")),
			output: with(attribute.String(attrTokenType, "output")), duration: with()}
	})
}

// cached is m's value for k, made by mk and stored on first use; racing
// first uses all get the one stored. k is typed, not any, so that each
// call boxes it on its own: the lookup's box stays on the stack, and only
// a first use's escapes into the map.
func cached[K comparable, V any](m *sync.Map, k K, mk func() V) V {
	if v, ok := m.Load(k); ok {
		return v.(V)
	}
	v, _ := m.LoadOrStore(k, mk())
	return v.(V)
}

// Providers are the OTel providers telemetry records through.
type Providers struct {
	Tracer trace.TracerProvider
	Meter  metric.MeterProvider
	Logger otellog.LoggerProvider // the ledger's rows; nil: the global one
	// Ledger, if set, is the rows' queue: its losses are reported as the
	// golem.ledger.dropped and golem.ledger.failed counters.
	Ledger *LedgerQueue
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
	if q := p.Ledger; q != nil {
		var errs []error
		for _, c := range []struct {
			name, desc string
			n          *atomic.Int64
		}{
			{"golem.ledger.dropped", "Ledger rows dropped: the queue was full", &q.Dropped},
			{"golem.ledger.failed", "Ledger rows the collector did not take", &q.Failed},
			{"golem.ledger.retried", "Ledger exports retried", &q.Retried},
		} {
			_, err := m.Int64ObservableCounter(c.name, metric.WithDescription(c.desc),
				metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error { o.Observe(c.n.Load()); return nil }))
			errs = append(errs, err)
		}
		if err := errors.Join(errs...); err != nil {
			return nil, err
		}
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
	if t == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	opts := cached(&t.starts, frontend, func() []trace.SpanStartOption {
		return []trace.SpanStartOption{trace.WithSpanKind(trace.SpanKindServer),
			trace.WithAttributes(attribute.String(attrOperation, "chat"), attribute.String("golem.dialect", frontend))}
	})
	return t.tracer.Start(ctx, "chat", opts...)
}

// Ended records a call that reached a provider: the span's attributes, the
// metrics, and the ledger row. ctx is the request's, and may be done:
// nothing here stops for it (and a context.WithoutCancel would cost an
// allocation on every lookup through it).
func (t *Telemetry) Ended(ctx context.Context, c Call) {
	if t == nil {
		return
	}
	rs := t.routeSets(&c)
	input := c.Usage.Input + c.Usage.CacheRead + c.Usage.CacheWrite // GenAI's input tokens include the cache's
	span := trace.SpanFromContext(ctx)
	span.SetName(rs.name)
	span.SetAttributes(
		attribute.String(attrProvider, c.Provider),
		attribute.String(attrRequestModel, c.Alias),
		attribute.String(attrResponseModel, c.Model),
		attribute.Int64(attrInputTokens, input),
		attribute.Int64(attrOutputTokens, c.Usage.Output),
		attribute.String("golem.request_id", c.RequestID),
		attribute.String("golem.principal", c.Principal),
		attribute.String("golem.scope", c.Scope),
		attribute.String("golem.deployment", c.Deployment),
		attribute.Int64("golem.cost_micros", c.Cost),
		attribute.Bool("golem.usage_estimated", c.EstimatedFraction > 0),
		attribute.Bool("golem.waived", c.Waived),
		attribute.Bool("golem.over_budget", c.OverBudget),
		attribute.Int("http.response.status_code", c.Status),
	)
	if c.ErrorOrigin != "" {
		span.SetAttributes(attribute.String("golem.error.origin", c.ErrorOrigin), attribute.String("golem.error.code", c.ErrorCode))
	}
	t.tokens.Record(ctx, input, rs.input...)
	t.tokens.Record(ctx, c.Usage.Output, rs.output...)
	t.duration.Record(ctx, c.Ended.Sub(c.Started).Seconds(), rs.duration...)
	t.Ledger.Ended(ctx, c)
}

// Retried records an attempt that failed before it committed and was
// retried: its ledger row (charged nothing) and an event on the call's
// span. The call's last attempt is recorded by Ended.
func (t *Telemetry) Retried(ctx context.Context, c Call) {
	if t == nil {
		return
	}
	trace.SpanFromContext(ctx).AddEvent("golem.attempt.retried", trace.WithAttributes(
		attribute.String("golem.deployment", c.Deployment), attribute.Int("golem.attempt", c.Attempt),
		attribute.Int("http.response.status_code", c.Status)))
	t.Ledger.Ended(ctx, c)
}

// Refused records a request golem refused (no provider call, no ledger row).
func (t *Telemetry) Refused(ctx context.Context, code contract.Code) {
	if t == nil {
		return
	}
	span := trace.SpanFromContext(ctx)
	span.SetAttributes(attribute.String("golem.error.origin", string(code.Origin)), attribute.String("golem.error.code", code.Name),
		attribute.Int("http.response.status_code", code.Status))
	opts := cached(&t.refused, code.Name, func() []metric.AddOption {
		return []metric.AddOption{metric.WithAttributeSet(attribute.NewSet(
			attribute.String("golem.error.code", code.Name), attribute.String("golem.error.origin", string(code.Origin))))}
	})
	t.refusals.Add(ctx, 1, opts...) // a 429 storm costs no allocation here
}
