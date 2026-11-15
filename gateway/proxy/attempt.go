package proxy

import (
	"cmp"
	"context"
	"net/http"
	"time"

	"github.com/chucklehead-dev/golem/gateway/admission"
	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/gateway/route"
)

// The attempt loop's parts (model/attempts.md); the loop is ServeHTTP's.

// health is the handler's Health, or its own if none was given (each
// handler then judges its deployments alone: a test's).
func (h *Handler) health() *route.Health {
	if h.Health != nil {
		return h.Health
	}
	h.healthOnce.Do(func() { h.ownHealth = &route.Health{} })
	return h.ownHealth
}

// pick is the pool's next deployment for an attempt, in pool order: one
// the call has not tried, and whose health takes the attempt. A call is
// never retried on a deployment it tried (ADR 0051): the client's SDK
// already retries the same endpoint with backoff, so the gateway's retry
// is a failover or nothing, and a pool of one makes one attempt. It marks
// the one it takes in tried. With none, it returns the earliest time one
// may take an attempt again (zero: unknown). A deployment its health takes
// must also have quota for the attempt (q, nil for none): one at its quota
// is passed over, as one at its ceiling is, and left untried.
func pick(hl *route.Health, rt *route.Route, tried []bool, principal string, q *meterHold) (*route.Target, *route.Lease, time.Time) {
	var earliest time.Time
	later := func(at time.Time) {
		if !at.IsZero() && (earliest.IsZero() || at.Before(earliest)) {
			earliest = at
		}
	}
	for i, t := range rt.Pool {
		if tried[i] {
			continue
		}
		l, at := hl.Acquire(t, principal)
		if l == nil {
			later(at)
			continue
		}
		if ok, at := q.take(t); !ok {
			l.Release() // not attempted: the deployment as it was
			later(at)
			continue
		}
		tried[i] = true
		return t, l, time.Time{}
	}
	return nil, nil, earliest
}

// meterHold is what a request holds of meters (admission.Meters): a
// call's rate limits, or an attempt's deployment's quota share (PRD §5.5,
// §9.2). Each took a request and the tokens estimated (the input's bytes
// over four, and max_tokens), corrected when the request ends: to its
// usage, or given back whole for an attempt refused before any output.
// Its zero value, or one with no Meters, holds nothing.
type meterHold struct {
	m      admission.Meters
	ctx    context.Context
	tokens int64
	held   []admission.Meter // nil: none
	one    [1]admission.Meter
	full   bool         // a deployment was passed over at its quota
	usage  *canon.Usage // the call's usage, once there is a call
}

// takeAll takes ms (none limited: nothing), reporting the first without
// room and when it will have.
func (h *meterHold) takeAll(ms []admission.Meter) (bool, int, time.Time) {
	if h == nil || h.m == nil || len(ms) == 0 {
		return true, 0, time.Time{}
	}
	ok, over, wait := h.m.Take(h.ctx, ms, h.tokens)
	if !ok {
		return false, over, time.Now().Add(wait)
	}
	h.held = ms
	return true, 0, time.Time{}
}

// take takes t's quota share, if it has one, for an attempt.
func (h *meterHold) take(t *route.Target) (bool, time.Time) {
	if h == nil || t.Quota == (admission.Rate{}) {
		return true, time.Time{}
	}
	h.one[0] = admission.Meter{Key: admission.QuotaMeter(t.Key()), Rate: t.Quota}
	ok, _, at := h.takeAll(h.one[:])
	if !ok {
		h.full = true
	}
	return ok, at
}

// refund gives the held attempt's tokens back: it was refused, or never
// reached its provider. Its request stays counted, as the provider's own
// meter counts it.
func (h *meterHold) refund() { h.correct(0) }

// done settles the hold when the request is over: to the usage of its
// last attempt, or, before any attempt began, nothing used.
func (h *meterHold) done() {
	var used int64
	if h != nil && h.usage != nil {
		u := h.usage
		used = u.Input + u.CacheWrite + u.Output // what providers meter: cache reads are not
	}
	h.correct(used)
}

func (h *meterHold) correct(used int64) {
	if h == nil || h.held == nil {
		return
	}
	h.m.Adjust(h.ctx, h.held, h.tokens-used)
	h.held = nil
}

// retryable reports a provider's status for a request it refused before
// any output, which another deployment may serve: a rate limit,
// unavailability, Anthropic's overload (ADR 0051). Not 500, which a
// provider may send after the model has worked; nor 502 or 504, which a
// proxy in front of the provider sends after it may have passed the
// request on: uncertain, not refused.
func retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, 529:
		return true
	}
	return false
}

// defaultMaxKept is MaxKept's default: about a thousand agent requests.
const defaultMaxKept = 256 << 20

// keep counts n bytes of a request body kept for retries against MaxKept,
// reporting false (nothing counted) if they would not fit.
func (h *Handler) keep(n int64) bool {
	limit := int64(cmp.Or(h.MaxKept, defaultMaxKept))
	if h.kept.Add(n) > limit {
		h.kept.Add(-n)
		return false
	}
	return true
}

func (h *Handler) unkeep(n int64) { h.kept.Add(-n) }
