package proxy

import (
	"cmp"
	"context"
	"io"
	"net/http"
	"time"

	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/lib/contract"
)

// CountTokensPath is Anthropic's token count, beside its Messages
// endpoint: a deployment's URL with this suffix.
const CountTokensPath = "/count_tokens"

// maxCountResponse bounds a token count's answer: a small JSON object.
const maxCountResponse = 64 << 10

// ServeCountTokens serves POST /v1/messages/count_tokens: the request is
// authenticated, routed and checked as a Messages call is (its alias the
// scope's, its features the route's), then sent to the route's first
// deployment that takes it, at the deployment's count endpoint (a bedrock
// deployment's envelope sends it to Bedrock's CountTokens). It is free:
// nothing is admitted, charged or written to the ledger, and it takes no
// slot; the provider bills no count. (A rate limit for it is step 4's.)
func (h *Handler) ServeCountTokens(w http.ResponseWriter, r *http.Request) {
	_, reqIDs := newRequestID()
	w.Header()[contract.HeaderRequestID] = []string{reqIDs}
	ctx := r.Context()
	if err := contract.CheckSingleHeaders(r.Header); err != nil {
		h.fail(ctx, w, &refusal{code: contract.RequestInvalid, msg: err.Error()})
		return
	}
	var grant auth.Grant
	snap, principal, no := h.authenticate(r, &grant)
	if no != nil {
		h.fail(ctx, w, no)
		return
	}
	ex := getExchange()
	defer ex.release()
	in := getBody()
	defer putBody(in)
	body, no := h.readBody(w, r, in)
	if no != nil {
		h.fail(ctx, w, no)
		return
	}
	req, err := ex.scan(h.Dialect, body)
	if err != nil {
		h.fail(ctx, w, &refusal{code: contract.RequestInvalid, msg: err.Error()})
		return
	}
	rt, no := h.route(snap, principal, req.alias)
	if no != nil {
		h.fail(ctx, w, no)
		return
	}
	if no := h.features(r.Header, principal, rt, req); no != nil {
		h.fail(ctx, w, no)
		return
	}
	target, lease, retryAt := pick(h.health(), rt, make([]bool, len(rt.Pool)), principal.ID)
	if target == nil {
		h.fail(ctx, w, &refusal{code: contract.UpstreamUnavailable, retryAfter: time.Until(retryAt),
			msg: "every deployment of this route is resting after failures or at its ceiling; retry shortly"})
		return
	}
	defer lease.Release() // a count says nothing of the deployment's health
	out := getBody()
	*out = ex.rewrite(h.Dialect, (*out)[:0], body, target.Model)
	upCtx, cancel := context.WithTimeout(ctx, cmp.Or(rt.Headers, time.Minute))
	defer cancel()
	up, ub := upstreamRequest(upCtx, r, target, out, h.Dialect.forwardRequest())
	u := *target.URL
	u.Path += CountTokensPath
	if u.RawPath != "" {
		u.RawPath += CountTokensPath
	}
	up.URL = &u
	resp, err := target.Transport.RoundTrip(up)
	if err != nil {
		ub.Close()
		Refuse(w, h.Dialect, contract.UpstreamUnavailable, "the provider could not count the request's tokens; retry shortly")
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header, forwardResponse)
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, io.LimitReader(resp.Body, maxCountResponse))
}
