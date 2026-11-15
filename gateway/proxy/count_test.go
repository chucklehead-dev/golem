package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chucklehead-dev/golem/gateway/limit"
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
