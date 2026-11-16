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

// reading is the handler's Reading, or its own if none was given.
func (h *Handler) reading() *Reading {
	if h.Reading != nil {
		return h.Reading
	}
	h.readingOnce.Do(func() { h.ownReading = &Reading{} })
	return h.ownReading
}

// pick is the pool's next deployment for an attempt, in pool order: one
// the call has not tried, that accepts the request's betas, and whose
// health takes the attempt. A call is never retried on a deployment it
// tried (ADR 0051): the client's SDK already retries the same endpoint
// with backoff, so the gateway's retry is a failover or nothing, and a pool
// of one makes one attempt. It marks the one it takes in tried. With none,
// it returns the earliest time one may take an attempt again (zero:
// unknown), and whether every one it passed over was for the principal's
// own share of it (route.AtPrincipalCeiling): its own calls, not the
// deployments, are what it waits on.
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
			continue // a deployment the request's betas would be refused by is no failover (ADR 0028)
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

// noDeployment is the refusal of a call no deployment of its route takes
// now, with the earliest time one may (retryAt): every one resting or at
// its ceiling; or, if every one is at the principal's own share of it
// (own), the principal's limit.
func noDeployment(retryAt time.Time, own bool) *refusal {
	if own {
		return &refusal{code: contract.LimitConcurrency,
			msg: "too many calls in flight for you on this route's deployments; retry when one of yours finishes"}
	}
	return &refusal{code: contract.UpstreamUnavailable, retryAfter: time.Until(retryAt),
		msg: "every deployment of this route is resting after failures or at its ceiling; retry shortly"}
}

// requestBetas are an Anthropic request's anthropic-beta values, each
// comma list split; another dialect's, none.
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

// incapable is the refusal of a request no deployment of rt accepts, for
// its betas (ADR 0028), or nil: the first beta no deployment takes, or
// the combination none takes whole.
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

// next picks the call's next deployment (pick).
func (c *callState) next() (*route.Target, *route.Hold, time.Time, bool) {
	return pick(c.hl, c.rt, c.tried, c.principal.ID, c.betas)
}

// retry, given an attempt's failure before it committed, takes the pool's
// next deployment if the call may retry: it was kept, has attempts left,
// its caller is there, nothing withdrew it, its upstream context is not
// done (a route's alarm that fired would fail the retry at once, after it
// took the next deployment's hold and a retry from the budget), and the
// pool's budget has a retry. It records the failed attempt's row (charged nothing: the
// provider saw nothing, or refused before any output), its result one of
// telemetry's (unreached or refused), and reports whether the call goes on.
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

// endHold ends a failed attempt's hold: a failure of its deployment's
// (after, the provider's Retry-After), unless the call itself was ended,
// its caller gone or a sweep withdrawing it (gone). Then the attempt's end
// says nothing of the deployment: it is released, so callers cancelling
// cannot eject a healthy deployment.
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
