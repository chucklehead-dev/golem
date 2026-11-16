package proxy

import (
	"encoding/json"
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
	plain, open, capped := key.New(key.Dev), key.New(key.Dev), key.New(key.Dev)
	s := &stub{events: anthropicStream(1)}
	h, _ := standaloneHandler(t, `apiVersion: golem.dev/v1alpha1
kind: ScopePolicy
metadata: {name: plain}
spec: {models: [claude], budgetMicros: 1000000000000}
---
apiVersion: golem.dev/v1alpha1
kind: ScopePolicy
metadata: {name: open}
spec: {models: [claude], providerStorage: allowed}
---
apiVersion: golem.dev/v1alpha1
kind: ScopePolicy
metadata: {name: capped}
spec: {models: [claude], maxOutputTokens: 1000, maxInputBytes: 300}
---
apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  keys:
  - {key: `+plain+`, principal: alice, scope: plain}
  - {key: `+open+`, principal: bob, scope: open}
  - {key: `+capped+`, principal: carol, scope: capped}
`, s)
	webSearch := `"tools":[{"type":"web_search_20250305","name":"web_search"}],`
	for _, tc := range []struct {
		name, key, body string
		want            int
		code            string
	}{
		{"a server tool", open, `{"model":"claude","max_tokens":10,` + webSearch + `"messages":[]}`, 200, ""},
		{"storage forbidden: a container", plain, `{"model":"claude","max_tokens":10,"container":"c1","messages":[]}`, 403, "authz.provider_storage_forbidden"},
		{"a provider-held file", plain, `{"model":"claude","max_tokens":10,"messages":[{"role":"user","content":[{"type":"document","source":{"type":"file","file_id":"file_1"}}]}]}`, 403, "authz.provider_storage_forbidden"},
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

// TestStoreNotBoolean: a store that is not a boolean (a string, a number)
// is refused as request.invalid naming the field, never forwarded as if it
// asked for no storage.
func TestStoreNotBoolean(t *testing.T) {
	rt := roundTrip(func(r *http.Request) (*http.Response, error) {
		t.Error("a request with a non-boolean store reached the provider")
		return nil, io.EOF
	})
	h := handler(OpenAIChat, rt, nil)
	for _, v := range []string{`"true"`, `1`} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"claude","store":`+v+`,"messages":[]}`)))
		var e struct {
			Error struct{ Code, Message string }
		}
		json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != 400 || e.Error.Code != "request.invalid" || !strings.Contains(e.Error.Message, `"store"`) {
			t.Errorf("store %s: %d %s; want 400 request.invalid naming \"store\"", v, w.Code, w.Body)
		}
	}
}

func TestPolicyUnits(t *testing.T) {
	h := &Handler{Dialect: OpenAIChat}
	capped := &auth.Principal{Limits: auth.Limits{MaxOutputTokens: 1000}}
	for _, tc := range []struct {
		name string
		p    *auth.Principal
		req  scanned
		code string
	}{
		{"no max_tokens under a ceiling", capped, scanned{n: 1}, "request.over_ceiling"},
		{"n completions count", capped, scanned{n: 3, maxTokens: 400, hasMax: true}, "request.over_ceiling"},
		{"n within", capped, scanned{n: 2, maxTokens: 500, hasMax: true}, ""},
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
