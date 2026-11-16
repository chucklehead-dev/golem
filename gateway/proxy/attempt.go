package proxy

import (
	"cmp"
	"context"
	"net/http"
	"time"

	"github.com/chucklehead-dev/golem/gateway/admission"
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
// must also have quota for the attempt (quota; nil: none checked): one at
// its quota is passed over, as one at its ceiling is, and left untried.
func pick(hl *route.Health, rt *route.Route, tried []bool, principal string, quota func(*route.Target) (bool, time.Time)) (*route.Target, *route.Lease, time.Time) {
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
		if quota == nil {
			tried[i] = true
			return t, l, time.Time{}
		}
		if ok, at := quota(t); !ok {
			l.Release() // not attempted: the deployment as it was
			later(at)
			continue
		}
		tried[i] = true
		return t, l, time.Time{}
	}
	return nil, nil, earliest
}

// takeQuota takes an attempt's request from t's quota share, if it has
// one (PRD §9.2), reporting when the share has room again if not; it
// records in full that a deployment was passed over at its quota.
func takeQuota(ctx context.Context, m admission.Meters, t *route.Target, full *bool) (bool, time.Time) {
	if m == nil || t.QuotaRPM == 0 {
		return true, time.Time{}
	}
	ok, _, wait := m.Take(ctx, []admission.Meter{{Key: admission.QuotaMeter(t.Key()), PerMinute: t.QuotaRPM}})
	if !ok {
		*full = true
		return false, time.Now().Add(wait)
	}
	return true, time.Time{}
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
