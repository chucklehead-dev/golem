package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/lib/key"
)

// TestPolicy: each row of contracts.md §8 the gateway enforces, through a
// real LocalIdentity's scopes.
func TestPolicy(t *testing.T) {
	strict, open, capped := key.New(key.Dev), key.New(key.Dev), key.New(key.Dev)
	s := &stub{events: anthropicStream(1)}
	h, _ := standaloneHandler(t, `apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  scopes:
  - {name: strict, models: [claude], strict: true, budgetMicros: 1000000000000}
  - {name: open, models: [claude], providerStorage: allowed}
  - {name: capped, models: [claude], maxOutputTokens: 1000, maxInputBytes: 300}
  keys:
  - {key: `+strict+`, principal: alice, scope: strict}
  - {key: `+open+`, principal: bob, scope: open}
  - {key: `+capped+`, principal: carol, scope: capped}
`, s)
	webSearch := `"tools":[{"type":"web_search_20250305","name":"web_search"}],`
	for _, tc := range []struct {
		name, key, body string
		want            int
		code            string
	}{
		{"strict: a server tool", strict, `{"model":"claude","max_tokens":10,` + webSearch + `"messages":[]}`, 403, "authz.feature_forbidden"},
		{"strict: an MCP server", strict, `{"model":"claude","max_tokens":10,"mcp_servers":[{"type":"url","url":"https://x"}],"messages":[]}`, 403, "authz.feature_forbidden"},
		{"strict: client tools", strict, `{"model":"claude","max_tokens":10,"tools":[{"name":"f","input_schema":{}}],"messages":[]}`, 200, ""},
		{"not strict: a server tool", open, `{"model":"claude","max_tokens":10,` + webSearch + `"messages":[]}`, 200, ""},
		{"storage forbidden: a container", strict, `{"model":"claude","max_tokens":10,"container":"c1","messages":[]}`, 403, "authz.provider_storage_forbidden"},
		{"storage allowed: a container", open, `{"model":"claude","max_tokens":10,"container":"c1","messages":[]}`, 200, ""},
		{"ceiling: within", capped, `{"model":"claude","max_tokens":1000,"messages":[]}`, 200, ""},
		{"ceiling: max_tokens over", capped, `{"model":"claude","max_tokens":1001,"messages":[]}`, 400, "request.over_ceiling"},
		{"ceiling: input over", capped, `{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":"` + strings.Repeat("x", 300) + `"}]}`, 400, "request.over_ceiling"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s.events = anthropicStream(1)
			w := send(h, tc.key, tc.body)
			if w.Code != tc.want || (tc.code != "" && errorCode(t, w) != tc.code) {
				t.Fatalf("%d %s; want %d %s", w.Code, w.Body, tc.want, tc.code)
			}
		})
	}
}

// TestStoreOff: where provider-side storage is forbidden (the default),
// OpenAI's store: true reaches the provider as false (ADR 0027); n counts in
// the ceiling.
func TestStoreOff(t *testing.T) {
	var got string
	rt := roundTrip(func(r *http.Request) (*http.Response, error) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))}, nil
	})
	h := handler(OpenAIChat, rt, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"claude","store":true,"messages":[]}`)))
	if w.Code != 200 || !strings.Contains(got, `"store":false`) || strings.Contains(got, `"store":true`) {
		t.Fatalf("%d; the provider got %s: store must be off", w.Code, got)
	}
}

func TestPolicyUnits(t *testing.T) {
	h := &Handler{Dialect: OpenAIChat}
	capped := &auth.Principal{Limits: auth.Limits{MaxOutputTokens: 1000}}
	strictP := &auth.Principal{Limits: auth.Limits{Strict: true}}
	for _, tc := range []struct {
		name string
		p    *auth.Principal
		req  scanned
		code string
	}{
		{"no max_tokens under a ceiling", capped, scanned{n: 1}, "request.over_ceiling"},
		{"n completions count", capped, scanned{n: 3, maxTokens: 400, hasMax: true}, "request.over_ceiling"},
		{"n within", capped, scanned{n: 2, maxTokens: 500, hasMax: true}, ""},
		{"OpenAI web search, strict", strictP, scanned{n: 1, serverTools: true}, "authz.feature_forbidden"},
	} {
		got := ""
		if no := h.policy(getExchange(), tc.p, tc.req, 10); no != nil {
			got = no.code.Name
		}
		if got != tc.code {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.code)
		}
	}
}
