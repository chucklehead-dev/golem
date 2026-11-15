package proxy

import (
	"cmp"
	"net/http"
	"time"

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
// not yet tried first, else one tried before (a pool of one is retried on
// itself), each only if its health takes the attempt. It marks the one it
// takes in tried. With none, it returns the earliest time one may take an
// attempt again (zero: unknown).
func pick(hl *route.Health, rt *route.Route, tried []bool) (*route.Target, *route.Lease, time.Time) {
	var earliest time.Time
	for _, again := range []bool{false, true} {
		for i, t := range rt.Pool {
			if tried[i] != again {
				continue
			}
			l, at := hl.Acquire(t)
			if l != nil {
				tried[i] = true
				return t, l, time.Time{}
			}
			if !at.IsZero() && (earliest.IsZero() || at.Before(earliest)) {
				earliest = at
			}
		}
	}
	return nil, nil, earliest
}

// retryable reports a provider's status for a request it refused before
// any output, which another attempt may serve: a rate limit, an internal
// error, unavailability, Anthropic's overload. 502 and 504 are not: they
// come from a proxy in front of the provider, which may have passed the
// request on (uncertain, not refused).
func retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable, 529:
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
