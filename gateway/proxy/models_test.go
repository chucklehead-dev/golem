package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/chucklehead-dev/golem/lib/key"
)

// TestModels: GET /v1/models lists the aliases the caller's scope may use,
// of the dialect it calls with, in that dialect's shape; one by ID; a model
// the scope may not use is not listed and, asked for, is unknown (a list
// must not tell a caller what other scopes have); no credential, no list.
func TestModels(t *testing.T) {
	alice := key.New(key.Dev)
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
`, nil)
	get := func(path string, anthropic bool, apiKey string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if anthropic {
			r.Header.Set("Anthropic-Version", "2023-06-01")
		}
		if apiKey != "" {
			r.Header.Set("X-Api-Key", apiKey)
		}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /v1/models", h.ServeModels)
		mux.HandleFunc("GET /v1/models/{id}", h.ServeModels)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}

	w := get("/v1/models", true, alice)
	var list struct {
		Data []struct {
			Type, ID string
		}
		HasMore bool   `json:"has_more"`
		FirstID string `json:"first_id"`
	}
	json.Unmarshal(w.Body.Bytes(), &list)
	if w.Code != 200 || len(list.Data) != 1 || list.Data[0].ID != "claude" || list.Data[0].Type != "model" || list.FirstID != "claude" {
		t.Fatalf("status %d %s: want claude alone, haiku not the scope's", w.Code, w.Body)
	}

	w = get("/v1/models", false, alice)
	var oai struct {
		Object string
		Data   []any
	}
	json.Unmarshal(w.Body.Bytes(), &oai)
	if w.Code != 200 || oai.Object != "list" || len(oai.Data) != 0 {
		t.Fatalf("status %d %s: OpenAI's shape, and no Messages route (its endpoint would refuse it)", w.Code, w.Body)
	}

	if w := get("/v1/models/claude", true, alice); w.Code != 200 || !json.Valid(w.Body.Bytes()) {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	if w := get("/v1/models/haiku", true, alice); w.Code != http.StatusNotFound || errorCode(t, w) != "route.unknown_model" {
		t.Fatalf("status %d %s: another scope's model is unknown, not forbidden", w.Code, w.Body)
	}
	if w := get("/v1/models", true, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("status %d: no credential, no list", w.Code)
	}
}
