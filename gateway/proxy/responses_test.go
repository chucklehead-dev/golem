package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chucklehead-dev/golem/gateway/canon"
)

func responsesStream() [][]byte {
	return [][]byte{
		[]byte("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\",\"usage\":null}}\n\n"),
		[]byte("event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hi\"}\n\n"),
		[]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":50,\"input_tokens_details\":{\"cached_tokens\":10},\"output_tokens\":7,\"output_tokens_details\":{\"reasoning_tokens\":2},\"total_tokens\":57}}}\n\n"),
	}
}

// TestResponses: a Responses stream passes byte for byte, its usage read
// from response.completed; the provider sees the deployment's model and
// store false, which the caller left out (the provider's default would
// store it); a call needing provider-side state, or asking store true, is
// refused with a stable policy error, before any provider call.
func TestResponses(t *testing.T) {
	s := &stub{events: responsesStream()}
	var res Result
	h := handler(OpenAIResponses, s, func(_ *http.Request, r Result) { res = r })
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"claude","stream":true,"input":"hi"}`)))
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), flat(responsesStream())) {
		t.Fatalf("status %d %s", w.Code, w.Body)
	}
	var sent map[string]any
	json.Unmarshal(s.got, &sent)
	if sent["model"] != "claude-sonnet" || sent["store"] != false {
		t.Fatalf("upstream body %s: the deployment's model, store false", s.got)
	}
	if want := (canon.Usage{Input: 40, CacheRead: 10, Output: 7, Reasoning: 2}); res.Usage != want {
		t.Fatalf("usage %+v, want %+v", res.Usage, want)
	}

	for name, body := range map[string]string{
		"previous_response_id": `{"model":"claude","previous_response_id":"resp_0","input":"and then?"}`,
		"background":           `{"model":"claude","background":true,"input":"hi"}`,
		"a provider file":      `{"model":"claude","input":[{"role":"user","content":[{"type":"input_file","file_id":"file-1"}]}]}`,
		"store true":           `{"model":"claude","store":true,"input":"hi"}`,
	} {
		s := &stub{events: responsesStream()}
		w := httptest.NewRecorder()
		handler(OpenAIResponses, s, nil).ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body)))
		var e struct{ Error struct{ Code string } }
		json.Unmarshal(w.Body.Bytes(), &e)
		if w.Code != http.StatusForbidden || e.Error.Code != "authz.provider_storage_forbidden" || s.got != nil || w.Header().Get("Retry-After") != "" {
			t.Errorf("%s: status %d %s, provider called %v: want a stable 403 before any call", name, w.Code, w.Body, s.got != nil)
		}
	}

	// An error as the stream's first event: refused before the commit.
	s = &stub{events: [][]byte{[]byte("event: error\ndata: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"overloaded\"}\n\n")}}
	w = httptest.NewRecorder()
	handler(OpenAIResponses, s, nil).ServeHTTP(w, httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"claude","stream":true,"input":"hi"}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d %s: an error first is a refusal before the commit", w.Code, w.Body)
	}
}
