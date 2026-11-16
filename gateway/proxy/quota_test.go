package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/chucklehead-dev/golem/gateway/limit"
	"github.com/chucklehead-dev/golem/gateway/route"
	gsnap "github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/lib/key"
)

// quotaRoute is a route over two deployments of one stub each, with quotas.
func quotaRoute(a, b *stub, qa, qb int64) *gsnap.Holder {
	u, _ := url.Parse("https://provider.example/v1/messages")
	return gsnap.Static(&gsnap.Compiled{Routes: route.NewTable(&route.Route{Alias: "claude", Dialect: AnthropicMessages.Name(), MaxAttempts: 2,
		Pool: []*route.Target{
			{Backend: "anthropic", Deployment: "a", Model: "claude-sonnet", URL: u, Transport: a, QuotaRPM: qa},
			{Backend: "anthropic", Deployment: "b", Model: "claude-sonnet", URL: u, Transport: b, QuotaRPM: qb},
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
	h := &Handler{Dialect: AnthropicMessages, MaxBody: 1 << 20, Snapshots: quotaRoute(a, b, 1, 1), Meters: limit.New(limit.NewMem())}
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

// TestBetaRouting: a deployment that does not accept a request's beta is
// passed over for one that does, even ahead of it in the pool; a beta no
// deployment accepts is refused route.no_capable_deployment, naming it,
// before any provider is called; a request without betas takes the pool
// in order.
func TestBetaRouting(t *testing.T) {
	bedrock, direct := &stub{events: anthropicStream(1)}, &stub{events: anthropicStream(1)}
	u, _ := url.Parse("https://provider.example/v1/messages")
	h := &Handler{Dialect: AnthropicMessages, MaxBody: 1 << 20, Snapshots: gsnap.Static(&gsnap.Compiled{Routes: route.NewTable(&route.Route{
		Alias: "claude", Dialect: AnthropicMessages.Name(), MaxAttempts: 2, Agent: true,
		Pool: []*route.Target{
			{Backend: "bedrock", Deployment: "east", Model: "claude-sonnet", URL: u, Transport: bedrock, Betas: map[string]bool{"context-1m-2025-08-07": true}},
			{Backend: "anthropic", Deployment: "direct", Model: "claude-sonnet", URL: u, Transport: direct},
		}})})}
	call := func(beta string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(smallRequest))
		r.Header.Set("Anthropic-Version", "2023-06-01")
		if beta != "" {
			r.Header.Set("Anthropic-Beta", beta)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := call("fine-grained-tool-streaming-2025-05-14"); w.Code != 200 || bedrock.got != nil || direct.got == nil {
		t.Fatalf("a beta bedrock lacks: %d, bedrock %v direct %v; want direct's", w.Code, bedrock.got != nil, direct.got != nil)
	}
	direct.got = nil
	if w := call(""); w.Code != 200 || bedrock.got == nil || direct.got != nil {
		t.Fatalf("no beta: %d, want the pool's first, bedrock", w.Code)
	}
	bedrock.got = nil
	h.Snapshots.(*gsnap.Holder).Applied().Routes.Lookup([]byte("claude")).Pool[1].Betas = map[string]bool{}
	w := call("fine-grained-tool-streaming-2025-05-14")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "route.no_capable_deployment") || !strings.Contains(w.Body.String(), "fine-grained-tool-streaming-2025-05-14") {
		t.Fatalf("a beta no deployment accepts: %d %s", w.Code, w.Body)
	}
	if bedrock.got != nil || direct.got != nil {
		t.Fatal("a provider was called for a request none accepts")
	}
}
