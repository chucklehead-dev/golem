package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/chucklehead-dev/golem/gateway/admission"
	"github.com/chucklehead-dev/golem/gateway/limit"
	"github.com/chucklehead-dev/golem/gateway/route"
	gsnap "github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/lib/key"
)

// quotaRoute is a route over two deployments of one stub each, with quotas.
func quotaRoute(a, b *stub, qa, qb admission.Rate) *gsnap.Holder {
	u, _ := url.Parse("https://provider.example/v1/messages")
	return gsnap.Static(&gsnap.Compiled{Routes: route.NewTable(&route.Route{Alias: "claude", Dialect: AnthropicMessages.Name(), MaxAttempts: 2,
		Pool: []*route.Target{
			{Backend: "anthropic", Deployment: "a", Model: "claude-sonnet", URL: u, Transport: a, Quota: qa},
			{Backend: "anthropic", Deployment: "b", Model: "claude-sonnet", URL: u, Transport: b, Quota: qb},
		}})})
}

func quotaCall(h *Handler) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(smallRequest))
	r.Header.Set("Anthropic-Version", "2023-06-01")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// TestQuotaFailover: a deployment at its share of the provider's requests
// a minute is passed over for the pool's next, and a call finding every
// deployment at its share is refused limit.provider_quota with a
// Retry-After, no provider called.
func TestQuotaFailover(t *testing.T) {
	a, b := &stub{events: anthropicStream(1)}, &stub{events: anthropicStream(1)}
	one := admission.Rate{RequestsPerMinute: 1}
	h := &Handler{Dialect: AnthropicMessages, MaxBody: 1 << 20, Snapshots: quotaRoute(a, b, one, one), Meters: limit.New(limit.NewMem())}
	if w := quotaCall(h); w.Code != 200 || a.got == nil || b.got != nil {
		t.Fatalf("the first call: %d, a %v b %v; want a's", w.Code, a.got != nil, b.got != nil)
	}
	a.got = nil
	if w := quotaCall(h); w.Code != 200 || a.got != nil || b.got == nil {
		t.Fatalf("the second: %d, a %v b %v; want b's, a at its quota", w.Code, a.got != nil, b.got != nil)
	}
	b.got = nil
	w := quotaCall(h)
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "limit.provider_quota") || w.Header().Get("Retry-After") == "" {
		t.Fatalf("the third: %d %s (Retry-After %q); want 429 limit.provider_quota", w.Code, w.Body, w.Header().Get("Retry-After"))
	}
	if a.got != nil || b.got != nil {
		t.Fatal("a provider called past its quota")
	}
}

// TestQuotaTokens: an attempt is charged its estimate (bytes over four
// and max_tokens) and corrected to its usage when it ends: here 2,052
// tokens (uncached input and output, not cache reads) of a 3,000 share,
// so the second call is admitted into debt and the third refused. Without
// the correction three estimates of a few dozen tokens would all fit.
func TestQuotaTokens(t *testing.T) {
	a := &stub{events: anthropicStream(1)}
	q := admission.Rate{TokensPerMinute: 3000}
	h := &Handler{Dialect: AnthropicMessages, MaxBody: 1 << 20, Snapshots: quotaRoute(a, &stub{}, q, q), Meters: limit.New(limit.NewMem())}
	h.Snapshots = gsnap.Static(&gsnap.Compiled{Routes: route.NewTable(&route.Route{Alias: "claude", Dialect: AnthropicMessages.Name(),
		Pool: []*route.Target{{Backend: "anthropic", Deployment: "a", Model: "claude-sonnet", URL: upstreamURL, Transport: a, Quota: q}}})})
	for i, want := range []int{200, 200, http.StatusTooManyRequests} {
		a.events = anthropicStream(1)
		if w := quotaCall(h); w.Code != want {
			t.Fatalf("call %d: %d %s, want %d", i, w.Code, w.Body, want)
		}
	}
}

// TestRateLimits: a scope's rate limits, each member's and the whole
// scope's, refuse a call past either with limit.rate and a Retry-After,
// in observe mode too (a limit, not a budget), naming which; a member
// past its own leaves the others theirs.
func TestRateLimits(t *testing.T) {
	alice, bob, carol := key.New(key.Dev), key.New(key.Dev), key.New(key.Dev)
	s := &stub{events: anthropicStream(1)}
	h, _ := standaloneHandler(t, `apiVersion: golem.dev/v1alpha1
kind: ScopePolicy
metadata: {name: team-a}
spec: {models: [claude], rateLimits: {member: {requestsPerMinute: 1}, scope: {requestsPerMinute: 2}}}
---
apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  keys: [{key: `+alice+`, principal: alice, scope: team-a}, {key: `+bob+`, principal: bob, scope: team-a}, {key: `+carol+`, principal: carol, scope: team-a}]
`, s)
	body := `{"model":"claude","max_tokens":10,"stream":true,"messages":[]}`
	for _, tc := range []struct {
		key, who string
		code     int
		says     string
	}{
		{alice, "alice", 200, ""},
		{alice, "alice again", 429, "your rate limit"},
		{bob, "bob", 200, ""},
		{carol, "carol, the scope's third", 429, "this scope's rate limit"},
	} {
		s.events = anthropicStream(1)
		w := send(h, tc.key, body)
		if w.Code != tc.code || tc.code == 429 && (errorCode(t, w) != "limit.rate" || w.Header().Get("Retry-After") == "" || !strings.Contains(w.Body.String(), tc.says)) {
			t.Fatalf("%s: %d %s (Retry-After %q), want %d %s", tc.who, w.Code, w.Body, w.Header().Get("Retry-After"), tc.code, tc.says)
		}
	}
}
