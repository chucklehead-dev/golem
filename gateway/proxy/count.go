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
// slot and no rate limit; the provider bills no count.
func (h *Handler) ServeCountTokens(w http.ResponseWriter, r *http.Request) {
	_, reqIDs := newRequestID()
	w.Header()[contract.HeaderRequestID] = []string{reqIDs}
	ctx := r.Context()
	ex := getExchange()
	defer ex.release()
	in := getBody()
	defer putBody(in)
	var grant auth.Grant
	p, no := h.prepare(w, r, ex, in, &grant)
	if no != nil {
		h.fail(ctx, w, no)
		return
	}
	rt := p.rt
	target, lease, retryAt := pick(h.health(), rt, make([]bool, len(rt.Pool)), p.principal.ID, p.betas, nil)
	if target == nil {
		h.fail(ctx, w, noDeployment(false, retryAt))
		return
	}
	defer lease.Release() // a count says nothing of the deployment's health
	out := getBody()
	*out = ex.rewrite(h.Dialect, (*out)[:0], p.body, target.Model)
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
