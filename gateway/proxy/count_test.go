package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chucklehead-dev/golem/gateway/limit"
	"github.com/chucklehead-dev/golem/gateway/route"
	"github.com/chucklehead-dev/golem/lib/key"
)

// TestCountTokens: a token count is checked as a Messages call is (its
// alias the scope's) and sent to the deployment's count endpoint, its
// model the deployment's, the answer relayed; nothing is admitted (no slot
// taken, though the scope allows none in flight) and no row written.
func TestCountTokens(t *testing.T) {
	alice := key.New(key.Dev)
	var gotPath string
	var gotBody map[string]any
	h, _ := standaloneHandler(t, `apiVersion: golem.dev/v1alpha1
kind: ScopePolicy
metadata: {name: team-a}
spec: {models: [claude]}
---
apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  keys: [{key: `+alice+`, principal: alice, scope: team-a}]
`, roundTrip(func(r *http.Request) (*http.Response, error) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"input_tokens":42}`))}, nil
	}))
	st := limit.NewMem()
	h.Limiter = limit.New(st).Admitter()
	count := func(model string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		r.Header.Set("X-Api-Key", alice)
		w := httptest.NewRecorder()
		h.ServeCountTokens(w, r)
		return w
	}
	w := count("claude")
	if w.Code != 200 || w.Body.String() != `{"input_tokens":42}` || gotPath != "/v1/messages/count_tokens" || gotBody["model"] != "claude-sonnet" {
		t.Fatalf("status %d %s, provider path %s body %v", w.Code, w.Body, gotPath, gotBody)
	}
	if len(st.Slots()) != 0 {
		t.Fatal("a count took a slot")
	}
	if w := count("haiku"); w.Code != http.StatusForbidden {
		t.Fatalf("status %d: another scope's model is refused for a count too", w.Code)
	}
}

// TestCountTokensChecks: a count is held to what a call is, short of
// admission: the scope's input ceiling, a deployment that takes its betas
// (route.no_capable_deployment), the scope's rate limits (limit.rate), and
// the shared health (an ejected deployment takes no count).
func TestCountTokensChecks(t *testing.T) {
	alice, bob := key.New(key.Dev), key.New(key.Dev)
	counted := 0
	h, _ := standaloneHandler(t, `apiVersion: golem.dev/v1alpha1
kind: ScopePolicy
metadata: {name: team-a}
spec: {models: [claude], features: compatible, maxInputBytes: 200, rateLimits: {member: {requestsPerMinute: 2}}}
---
apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  keys: [{key: `+alice+`, principal: alice, scope: team-a}, {key: `+bob+`, principal: bob, scope: team-a}]
`, roundTrip(func(r *http.Request) (*http.Response, error) {
		counted++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"input_tokens":42}`))}, nil
	}))
	hl := &route.Health{EjectAfter: 1}
	h.Health = hl
	c, _ := h.Snapshots.Current()
	target := c.Routes.Lookup([]byte("claude")).Pool[0]
	target.Betas = map[string]bool{"known-beta": true}
	count := func(k, beta, text string) string {
		r := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(`{"model":"claude","messages":[{"role":"user","content":"`+text+`"}]}`))
		r.Header.Set("X-Api-Key", k)
		if beta != "" {
			r.Header.Set("Anthropic-Beta", beta)
		}
		w := httptest.NewRecorder()
		h.ServeCountTokens(w, r)
		if w.Code == 200 {
			return "ok"
		}
		return errorCode(t, w)
	}
	for _, tc := range []struct {
		who, key, beta, text, want string
	}{
		{"over the input ceiling", alice, "", strings.Repeat("x", 300), "request.over_ceiling"},
		{"a beta no deployment takes", alice, "other-beta", "hi", "route.no_capable_deployment"},
		{"alice's first", alice, "known-beta", "hi", "ok"},
		{"alice's second", alice, "", "hi", "ok"},
		{"alice's third, past her rate", alice, "", "hi", "limit.rate"},
	} {
		if got := count(tc.key, tc.beta, tc.text); got != tc.want {
			t.Fatalf("%s: %s, want %s", tc.who, got, tc.want)
		}
	}
	l, _ := hl.Acquire(target, "x")
	l.Fail(0) // ejected
	if got := count(bob, "", "hi"); got != "upstream.unavailable" {
		t.Fatalf("a count on an ejected deployment: %s, want upstream.unavailable", got)
	}
	if counted != 2 {
		t.Fatalf("%d counts reached the provider: want alice's two", counted)
	}
}
