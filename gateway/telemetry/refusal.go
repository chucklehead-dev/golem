package telemetry

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/chucklehead-dev/golem/lib/contract"
)

// Refusal is a request golem refused before any provider saw it, as far
// as the request got: before authentication, Principal, Scope and Alias
// are empty, and Alias is the route's (never the caller's raw text).
type Refusal struct {
	Code      contract.Code
	Message   string // the message the caller was sent
	RequestID string
	Principal string
	Scope     string
	Key       string // the key's display prefix, if a key was used
	Alias     string
}

// Refused records a refusal: on the request's span (its code, message and
// who, an error status for a 5xx), as golem.refusals by code, and as a log
// line. No provider call, no ledger row.
//
// The log is the refusal's record for people (an operator asking why a
// team is refused, a drill's evidence), but a 429 storm must not become a
// log storm: each code and scope logs at most one line a second, and the
// next line it logs says how many it left out. The span and the metric
// keep every one.
func (t *Telemetry) Refused(ctx context.Context, r Refusal) {
	if t == nil {
		return
	}
	code := r.Code
	span := trace.SpanFromContext(ctx)
	attrs := [...]attribute.KeyValue{attribute.String("golem.error.origin", string(code.Origin)),
		attribute.String("golem.error.code", code.Name), attribute.String("golem.error.message", r.Message),
		attribute.Int("http.response.status_code", code.Status), attribute.String("golem.request_id", r.RequestID),
		attribute.String("golem.principal", r.Principal), attribute.String("golem.scope", r.Scope),
		attribute.String(attrRequestModel, r.Alias)}
	n := 5 // the rest only as far as the request got
	if r.Principal != "" {
		n = 7
	}
	if r.Alias != "" {
		attrs[n], n = attrs[7], n+1
	}
	span.SetAttributes(attrs[:n]...)
	if code.Status >= 500 {
		span.SetStatus(codes.Error, code.Name)
	}
	opts := cached(&t.refused, code.Name, func() []metric.AddOption {
		return []metric.AddOption{metric.WithAttributeSet(attribute.NewSet(
			attribute.String("golem.error.code", code.Name), attribute.String("golem.error.origin", string(code.Origin))))}
	})
	t.refusals.Add(ctx, 1, opts...) // a 429 storm costs no allocation here
	if left, ok := t.refusalLog.take(code.Name, r.Scope, time.Now()); ok {
		t.refusalLog.log.LogAttrs(ctx, slog.LevelInfo, "refused",
			slog.String("code", code.Name), slog.String("origin", string(code.Origin)), slog.Int("status", code.Status),
			slog.String("request_id", r.RequestID), slog.String("principal", r.Principal), slog.String("scope", r.Scope),
			slog.String("key", r.Key), slog.String("alias", r.Alias), slog.String("message", r.Message),
			slog.Int64("unlogged_since", left))
	}
}

// refusalLog limits refusal lines to one a second for each code and
// scope. Its keys are bounded by the contract's codes and the snapshot's
// scopes (and "" before authentication).
type refusalLog struct {
	log *slog.Logger
	mu  sync.Mutex
	by  map[refusalKey]*refusalEvery
}

type refusalKey struct{ code, scope string }

type refusalEvery struct {
	last time.Time
	left int64 // refusals not logged since last
}

// take reports whether a refusal of code in scope at now is logged, and
// how many of theirs were left out since the last that was.
func (l *refusalLog) take(code, scope string, now time.Time) (left int64, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	k := refusalKey{code, scope}
	e := l.by[k]
	if e == nil {
		if l.by == nil {
			l.by = map[refusalKey]*refusalEvery{}
		}
		e = &refusalEvery{}
		l.by[k] = e
	} else if now.Sub(e.last) < time.Second {
		e.left++
		return 0, false
	}
	left, e.left, e.last = e.left, 0, now
	return left, true
}
