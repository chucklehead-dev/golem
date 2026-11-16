package proxy

import (
	"cmp"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/chucklehead-dev/golem/gateway/route"
	"github.com/chucklehead-dev/golem/lib/contract"
)

// The attempt loop's parts. The loop itself is callState.attempts, and
// model/attempts.md is its model.

// health is the handler's Health, or its own if none was given. Tests use
// their own, so each handler judges its deployments alone.
func (h *Handler) health() *route.Health {
	if h.Health != nil {
		return h.Health
	}
	h.healthOnce.Do(func() { h.ownHealth = &route.Health{} })
	return h.ownHealth
}

// reading is the handler's Reading, or its own if none was given.
func (h *Handler) reading() *Reading {
	if h.Reading != nil {
		return h.Reading
	}
	h.readingOnce.Do(func() { h.ownReading = &Reading{} })
	return h.ownReading
}

// pick takes the pool's next deployment for an attempt, in pool order. It
// skips deployments the call has tried, those its betas rule out, and
// those whose health refuses. It marks the one it takes in tried.
//
// A call never retries a deployment it tried. The client's SDK already
// retries the same endpoint with backoff, so the gateway's retry is a
// failover. A pool of one makes one attempt. ADR 0051.
//
// When it takes none, it returns the earliest time one may take an
// attempt again, zero if unknown. It also reports whether every deployment
// passed over was at the principal's own share (route.AtPrincipalCeiling).
// The principal then waits on its own calls, not on the deployments.
func pick(hl *route.Health, rt *route.Route, tried []bool, principal string, betas []string) (*route.Target, *route.Hold, time.Time, bool) {
	var earliest time.Time
	later := func(at time.Time) {
		if !at.IsZero() && (earliest.IsZero() || at.Before(earliest)) {
			earliest = at
		}
	}
	own, passed := true, false
	for i, t := range rt.Pool {
		if tried[i] || !t.Accepts(betas) {
			continue // a deployment that would refuse the betas is no failover
		}
		l, at, why := hl.AcquireWhy(t, principal)
		if l == nil {
			later(at)
			passed, own = true, own && why == route.AtPrincipalCeiling
			continue
		}
		tried[i] = true
		return t, l, time.Time{}, false
	}
	return nil, nil, earliest, passed && own
}

// noDeployment refuses a call that no deployment of its route takes now.
// If every deployment is at the principal's own share, the refusal is the
// principal's in-flight limit. Otherwise the deployments are resting or
// full, and retryAt is when one may take a call again.
func noDeployment(retryAt time.Time, own bool) *refusal {
	if own {
		return &refusal{code: contract.LimitConcurrency,
			msg: "too many calls in flight for you on this route's deployments; retry when one of yours finishes"}
	}
	return &refusal{code: contract.UpstreamUnavailable, retryAfter: time.Until(retryAt),
		msg: "every deployment of this route is resting after failures or at its ceiling; retry shortly"}
}

// requestBetas lists an Anthropic request's anthropic-beta values, with
// each comma-separated list split. Other dialects have none.
func requestBetas(d Dialect, hdr http.Header) []string {
	if d != AnthropicMessages {
		return nil
	}
	var out []string
	for _, v := range hdr.Values("Anthropic-Beta") {
		for b := range strings.SplitSeq(v, ",") {
			if b = strings.TrimSpace(b); b != "" {
				out = append(out, b)
			}
		}
	}
	return out
}

// invalidBetas refuses betas that are not valid UTF-8. Bedrock takes betas
// as JSON strings, which would carry such a value as U+FFFD, a beta the
// caller never named.
func invalidBetas(betas []string) *refusal {
	for _, b := range betas {
		if !utf8.ValidString(b) {
			return &refusal{code: contract.RequestInvalid,
				msg: "the anthropic-beta header has a value that is not valid UTF-8: send each beta's name as its provider documents it"}
		}
	}
	return nil
}

// incapable refuses a request whose betas no deployment of the route
// accepts, or returns nil. The refusal names the first beta no deployment
// takes, or else the combination none takes whole. ADR 0028.
func incapable(rt *route.Route, betas []string) *refusal {
	if len(betas) == 0 {
		return nil
	}
	for _, t := range rt.Pool {
		if t.Accepts(betas) {
			return nil
		}
	}
	for _, b := range betas {
		taken := false
		for _, t := range rt.Pool {
			taken = taken || t.Accepts([]string{b})
		}
		if !taken {
			return &refusal{code: contract.RouteNoCapableDeployment, msg: "no deployment of route " + rt.Alias + " accepts anthropic-beta " + b + ": drop it, or call a route that serves it"}
		}
	}
	return &refusal{code: contract.RouteNoCapableDeployment, msg: "no deployment of route " + rt.Alias + " accepts these anthropic-beta values together (" + strings.Join(betas, ", ") + "): send fewer"}
}

// next picks the call's next deployment.
func (c *callState) next() (*route.Target, *route.Hold, time.Time, bool) {
	return pick(c.hl, c.rt, c.tried, c.principal.ID, c.betas)
}

// retry ends an attempt that failed before it committed, and reports
// whether the call goes on to the pool's next deployment. It goes on only
// if its body was kept, it has attempts left, its caller and its upstream
// context are live, a deployment takes it, and the route's budget has a
// retry.
//
// A retry records the failed attempt's row with its result, unreached or
// refused. The row charges nothing, since the provider saw nothing or
// refused before any output.
func (c *callState) retry(result string, status int, after time.Duration) bool {
	gone := c.r.Context().Err() != nil || terminatedBy(c.upCtx) != nil
	c.endHold(after, gone)
	c.h.runBeforePick(c.r)
	if !c.kept || c.attempt >= c.rt.MaxAttempts || c.r.Context().Err() != nil || c.upCtx.Err() != nil {
		return false
	}
	next, nl, _, _ := c.next()
	if next == nil {
		return false
	}
	if !c.hl.Retry(c.rt.Alias) {
		nl.Release()
		return false
	}
	code := contract.UpstreamUnavailable
	c.row.Ended, c.row.Status = time.Now(), cmp.Or(status, code.Status)
	c.row.Outcome, c.row.ErrorOrigin, c.row.ErrorCode = coded(outcomeFailed, code)
	c.row.AttemptResult = result
	c.h.Telemetry.Retried(c.ctx, c.row)
	c.row.ProviderRequestID, c.row.Outcome, c.row.ErrorOrigin, c.row.ErrorCode = "", "", "", ""
	c.target, c.hold = next, nl
	return true
}

// endHold ends a failed attempt's hold. Normally the deployment is charged
// a failure, with the provider's Retry-After. If the call itself ended,
// because its caller left or a sweep withdrew it, the hold is only
// released. Callers cancelling then cannot eject a healthy deployment.
func (c *callState) endHold(after time.Duration, gone bool) {
	if gone {
		c.hold.Release()
		return
	}
	c.hold.Fail(after)
}

// runBeforePick runs the beforePick test hook, if set.
func (h *Handler) runBeforePick(r *http.Request) {
	if h.beforePick != nil {
		h.beforePick(r)
	}
}

// retryable reports whether a status refuses a request before any output,
// so another deployment may serve it. Those are 429, 503 and Anthropic's
// 529 overload. A 500 may come after the model worked, and a 502 or 504
// after a proxy passed the request on, so those are uncertain. ADR 0051.
func retryable(status int) bool {
	switch status {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, 529:
		return true
	}
	return false
}

// defaultMaxKept is MaxKept's default, about a thousand agent requests.
const defaultMaxKept = 256 << 20

// keep counts n bytes of a request body kept for retries against MaxKept.
// It reports false, counting nothing, if they would not fit.
func (h *Handler) keep(n int64) bool {
	limit := int64(cmp.Or(h.MaxKept, defaultMaxKept))
	if h.kept.Add(n) > limit {
		h.kept.Add(-n)
		return false
	}
	return true
}

// unkeep stops counting n bytes kept for retries.
func (h *Handler) unkeep(n int64) { h.kept.Add(-n) }
