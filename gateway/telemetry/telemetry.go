// Package telemetry is the gateway's OpenTelemetry (ADR 0015): a span per
// request with the GenAI semantic conventions and golem's attributes, the
// GenAI token and duration metrics, time to first token, counters of calls
// by outcome and error origin, of attempts by result and of refusals, the
// deployments' health (ObserveState), and the access-log
// record that is also the ledger's row (contracts.md), all over OTLP to
// the enterprise collector. Standalone, ledger rows go to stdout as JSON
// lines instead (ADR 0023).
package telemetry

import (
	"cmp"
	"context"
	"crypto/sha1"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"uuid"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/chucklehead-dev/golem/gateway/canon"
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

// golem's request-path instruments (operations.md). Their attributes
// are bounded by the snapshot's routes and deployments and by the fixed
// sets of outcomes, origins and attempt results: never a principal or a
// request.
const (
	MetricTTFT     = "golem.ttft"
	MetricCalls    = "golem.calls"
	MetricAttempts = "golem.attempts"
	MetricRefusals = "golem.refusals"
)

// Attribute names of golem's own.
const (
	attrDeployment    = "golem.deployment"
	attrOutcome       = "golem.outcome"
	attrErrorOrigin   = "golem.error.origin"
	attrAttemptResult = "golem.attempt.result"
)

// An attempt's result (model/attempts.md), Call.AttemptResult: how the
// attempt the row records ended.
const (
	// AttemptCommitted: the provider answered and its response is the
	// caller's (a stream's first event was not an error).
	AttemptCommitted = "committed"
	// AttemptRefused: refused before any output (429, 503, 529, or 200 with
	// an error as the first event): retried, or relayed if it was the last.
	AttemptRefused = "refused"
	// AttemptUnreached: the provider never had the request (DNS, dial,
	// TLS).
	AttemptUnreached = "unreached"
	// AttemptUncertain: the request went out and no answer came (a reset,
	// a timeout): the provider may have worked on it.
	AttemptUncertain = "uncertain"
)

// attemptResults are the results in attemptIndex's order.
var attemptResults = [...]string{AttemptCommitted, AttemptRefused, AttemptUnreached, AttemptUncertain} // read-only

// attemptIndex is result's place in attemptResults, or -1: no attempt.
func attemptIndex(result string) int {
	switch result {
	case AttemptCommitted:
		return 0
	case AttemptRefused:
		return 1
	case AttemptUnreached:
		return 2
	case AttemptUncertain:
		return 3
	}
	return -1
}

// Call is what the request path reports when a call ends.
type Call struct {
	RequestID                     string
	Principal, PrincipalKind, Key string
	Scope                         string
	Frontend, Backend             string // dialects
	Envelope                      string // the deployment's: "" or "bedrock"
	Alias, Model, Deployment      string
	Provider, ProviderRequestID   string // Provider: the backend's name
	// ProviderAccount is the backend's provider account (snapshot's
	// Backend.ProviderAccount), the ledger's provider_account; empty, the
	// row records Provider.
	ProviderAccount string
	// PriceVersion is the PriceList version Cost was priced by (the
	// snapshot's Price.Version), the ledger's price_version.
	PriceVersion   string
	Started, Ended time.Time
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
	// unreachable or in the grace after a recovery (ADR 0041).
	Waived bool
	// OverBudget: admitted over its scope's budget in observe or warn
	// mode, a call enforce would have refused (prd.md, ADR 0050).
	OverBudget bool
	// Attempt is the call's attempt the row records, from 1 (0 is 1): each
	// attempt is its own ledger event (AttemptID), so a retried call's
	// rows are never deduplicated into one.
	Attempt int
	// AttemptResult is how that attempt ended (AttemptCommitted, ...),
	// counted by golem.attempts; empty for a call that made none (one
	// withdrawn as it was admitted).
	AttemptResult string
	// Committed is the call's commit point: when a stream's first event was
	// complete, to be relayed, or a whole response's headers arrived; zero
	// if no attempt committed. golem.ttft records Committed − Started for a
	// call answered below 400.
	Committed time.Time
	// Funding is the scope's funding sources and their shares at request
	// time, and Units its other billed units by name (web_search, ...):
	// golem.spend's funding and units (contracts.md). The request path
	// fills neither yet; the row carries them as an empty list and map.
	Funding []Funding
	Units   map[string]int64
}

// Funding is one funding source of a scope and its share of the cost.
type Funding struct {
	Source string
	Share  float64
}

// AttemptID is the attempt's ID, its ledger event's: the request ID for a
// call's first attempt, then AttemptUUID's of the request ID and the
// attempt's number, a UUID like the request ID's (golem.spend's columns are
// UUIDs, contracts.md).
func (c *Call) AttemptID() string {
	if c.Attempt <= 1 {
		return c.RequestID
	}
	return AttemptUUID(c.RequestID, c.Attempt)
}

// AttemptUUID is attempt n's ID in the call requestID, for n > 1: the
// name-based UUID, version 5 (RFC 9562: SHA-1), whose namespace is the
// request ID and whose name is n in decimal ("2"), in the canonical
// lowercase form. Anyone with the request ID can derive it again, and
// distinct (request, attempt) pairs have distinct IDs. A request ID that is
// not a UUID (only tests have one) is hashed as the name's prefix
// instead, under the nil namespace: still a UUID, still deterministic.
func AttemptUUID(requestID string, n int) string {
	h := sha1.New()
	if ns, err := uuid.Parse(requestID); err == nil {
		h.Write(ns[:])
	} else {
		h.Write(make([]byte, 16))
		h.Write([]byte(requestID + "."))
	}
	h.Write(strconv.AppendInt(nil, int64(n), 10))
	var sum [sha1.Size]byte
	var id uuid.UUID
	copy(id[:], h.Sum(sum[:0]))
	id[6] = id[6]&0x0f | 0x50 // version 5
	id[8] = id[8]&0x3f | 0x80 // the RFC's variant
	return id.String()
}

// Telemetry records calls. Its methods are safe for concurrent use, and on
// a nil *Telemetry do nothing (a gateway without telemetry, tests).
type Telemetry struct {
	tracer   trace.Tracer
	tokens   metric.Int64Histogram
	duration metric.Float64Histogram
	ttft     metric.Float64Histogram
	calls    metric.Int64Counter
	attempts metric.Int64Counter
	refusals metric.Int64Counter
	Ledger   *Ledger
	refusalLog

	// Attribute sets are built once per route and per refusal code, not per
	// request: building and merging them was a quarter of a call's
	// allocations. The keys are bounded by the snapshot's routes and the
	// contract's codes.
	routes  sync.Map // routeKey -> *routeSets
	ends    sync.Map // endKey -> []metric.AddOption: golem.calls'
	refused sync.Map // code name -> []metric.AddOption
	starts  sync.Map // frontend -> []trace.SpanStartOption
}

// routeKey is a route's deployment: an alias and the deployment an attempt
// went to. The deployment's key names its backend and model, but they are
// in the key too, as the attributes are built from them.
type routeKey struct{ provider, alias, model, deployment string }

// routeSets are one route's deployment's precomputed options.
type routeSets struct {
	name          string // the span's: "chat <alias>"
	input, output []metric.RecordOption
	duration      []metric.RecordOption
	ttft          []metric.RecordOption
	attempts      [len(attemptResults)][]metric.AddOption // by attemptIndex
}

// routeSets are c's route's options, made on its first call.
func (t *Telemetry) routeSets(c *Call) *routeSets {
	return cached(&t.routes, routeKey{c.Provider, c.Alias, c.Model, c.Deployment}, func() *routeSets {
		attrs := []attribute.KeyValue{attribute.String(attrProvider, c.Provider), attribute.String(attrRequestModel, c.Alias),
			attribute.String(attrResponseModel, c.Model)}
		with := func(extra ...attribute.KeyValue) []metric.RecordOption {
			return []metric.RecordOption{metric.WithAttributeSet(attribute.NewSet(append(slices.Clone(attrs), extra...)...))}
		}
		rs := &routeSets{name: "chat " + c.Alias, input: with(attribute.String(attrTokenType, "input")),
			output: with(attribute.String(attrTokenType, "output")), duration: with(),
			ttft: []metric.RecordOption{metric.WithAttributeSet(attribute.NewSet(attribute.String(attrRequestModel, c.Alias),
				attribute.String(attrResponseModel, c.Model), attribute.String(attrDeployment, c.Deployment)))}}
		for i, r := range attemptResults {
			rs.attempts[i] = []metric.AddOption{metric.WithAttributeSet(attribute.NewSet(
				attribute.String(attrDeployment, c.Deployment), attribute.String(attrAttemptResult, r)))}
		}
		return rs
	})
}

// endKey is a call's end as golem.calls counts it.
type endKey struct{ alias, deployment, outcome, origin string }

// endSets are golem.calls' options for c's end, made on its first use. The
// outcomes and origins are the contract's few (contracts.md).
func (t *Telemetry) endSets(c *Call) []metric.AddOption {
	return cached(&t.ends, endKey{c.Alias, c.Deployment, c.Outcome, c.ErrorOrigin}, func() []metric.AddOption {
		return []metric.AddOption{metric.WithAttributeSet(attribute.NewSet(attribute.String(attrRequestModel, c.Alias),
			attribute.String(attrDeployment, c.Deployment), attribute.String(attrOutcome, c.Outcome),
			attribute.String(attrErrorOrigin, c.ErrorOrigin)))}
	})
}

// attempted counts c's attempt by its result, if it made one.
func (t *Telemetry) attempted(ctx context.Context, rs *routeSets, c *Call) {
	if i := attemptIndex(c.AttemptResult); i >= 0 {
		t.attempts.Add(ctx, 1, rs.attempts[i]...)
	}
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
	// Log is where refusals are logged (Refused); nil: slog.Default().
	Log *slog.Logger
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
	refusals, err3 := m.Int64Counter(MetricRefusals, metric.WithDescription("Requests refused, by contract code"))
	// 2 s is a boundary: exit 3's p95 is judged against it.
	ttft, err4 := m.Float64Histogram(MetricTTFT, metric.WithUnit("s"),
		metric.WithDescription("Time to first token: from the request to its commit point (a stream's first event, a whole response's headers)"),
		metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.25, 0.5, 0.75, 1, 1.5, 2, 3, 5, 10, 30, 60))
	calls, err5 := m.Int64Counter(MetricCalls, metric.WithUnit("{call}"),
		metric.WithDescription("Admitted calls ended, by route, last deployment, outcome and error origin"))
	attempts, err6 := m.Int64Counter(MetricAttempts, metric.WithUnit("{attempt}"),
		metric.WithDescription("Attempts made, by deployment and result: committed, refused, unreached or uncertain"))
	if err := errors.Join(err1, err2, err3, err4, err5, err6); err != nil {
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
		lp = otel.GetLoggerProvider()
	}
	return &Telemetry{tracer: p.Tracer.Tracer("golem/gateway"), tokens: tokens, duration: duration, ttft: ttft,
		calls: calls, attempts: attempts, refusals: refusals, Ledger: NewLedger(lp.Logger("golem/ledger"), site, pool),
		refusalLog: refusalLog{log: cmp.Or(p.Log, slog.Default())}}, nil
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
// metrics (its tokens, duration and time to first token, the call by outcome
// and its last attempt by result), and the ledger row. ctx is the request's,
// and may be done: nothing here stops for it (and a context.WithoutCancel would
// cost an allocation on every lookup through it).
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
	if !c.Committed.IsZero() && c.Status < 400 { // an error's headers are no first token
		t.ttft.Record(ctx, c.Committed.Sub(c.Started).Seconds(), rs.ttft...)
	}
	t.calls.Add(ctx, 1, t.endSets(&c)...)
	t.attempted(ctx, rs, &c)
	t.Ledger.Ended(ctx, c)
}

// Retried records an attempt that failed before it committed and was
// retried: its ledger row (charged nothing), golem.attempts by its result,
// and an event on the call's span. The call's last attempt is recorded by
// Ended.
func (t *Telemetry) Retried(ctx context.Context, c Call) {
	if t == nil {
		return
	}
	trace.SpanFromContext(ctx).AddEvent("golem.attempt.retried", trace.WithAttributes(
		attribute.String("golem.deployment", c.Deployment), attribute.Int("golem.attempt", c.Attempt),
		attribute.Int("http.response.status_code", c.Status)))
	t.attempted(ctx, t.routeSets(&c), &c)
	t.Ledger.Ended(ctx, c)
}
