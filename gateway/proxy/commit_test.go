package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/chucklehead-dev/golem/gateway/limit"
	"github.com/chucklehead-dev/golem/gateway/telemetry"
	"github.com/chucklehead-dev/golem/lib/key"
)

// overloaded is Anthropic's error as a stream's first event, after a 200.
var overloaded = []byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"Overloaded\"}}\n\n")

// TestFailedBeforeCommit: a provider that answers 200 and then an error as
// its first event has produced nothing and told the caller nothing. The
// attempt is not committed: with no other deployment to fail over to, the
// caller gets a retryable 503 in its own dialect (with the provider's
// Retry-After), never a 200; the call is charged nothing and its slot
// freed at once; its row says failed, of the provider.
func TestFailedBeforeCommit(t *testing.T) {
	alice := key.New(key.Dev)
	cfg := `apiVersion: golem.dev/v1alpha1
kind: ScopePolicy
metadata: {name: team-a}
spec: {models: [claude], inFlight: {principal: 4}}
---
apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  keys: [{key: ` + alice + `, principal: alice, scope: team-a}]
`
	h, _ := standaloneHandler(t, cfg, roundTrip(func(r *http.Request) (*http.Response, error) {
		r.Body.Close()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}, "Retry-After": {"7"}},
			Body: io.NopCloser(&chunks{c: [][]byte{overloaded}})}, nil
	}))
	st := limit.NewMem()
	h.Limiter = limit.New(st).Admitter()
	var rows bytes.Buffer
	h.Telemetry, _ = telemetry.New(telemetry.Stdout(&rows), "local", "standalone")
	r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude","max_tokens":100,"stream":true,"messages":[]}`))
	r.Header.Set("X-Api-Key", alice)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	var e struct {
		Type  string
		Error struct{ Type string }
	}
	json.Unmarshal(w.Body.Bytes(), &e)
	switch {
	case w.Code != http.StatusServiceUnavailable || e.Type != "error" || e.Error.Type != "upstream.unavailable":
		t.Fatalf("status %d %s: want 503 upstream.unavailable in Anthropic's shape", w.Code, w.Body)
	case w.Header().Get("Retry-After") != "7":
		t.Fatalf("Retry-After %q: want the provider's", w.Header().Get("Retry-After"))
	case w.Header().Get("Content-Type") != "application/json":
		t.Fatalf("Content-Type %q: the provider's stream headers were sent", w.Header().Get("Content-Type"))
	}
	row := rowOf(t, &rows)
	if row["outcome"] != "failed" || row["cost_micros"].(float64) != 0 || row["error_origin"] != "provider" || row["error_code"] != "upstream.unavailable" {
		t.Fatalf("row %v: want failed, uncharged, the provider's", row)
	}
	if len(st.Slots()) != 0 {
		t.Fatalf("slots %v: a failed attempt's slot is freed at once", st.Slots())
	}
}

// TestFailedBeforeCommitOpenAI: an OpenAI stream whose first chunk is an
// error object is refused alike, in OpenAI's shape.
func TestFailedBeforeCommitOpenAI(t *testing.T) {
	s := &stub{events: [][]byte{[]byte(`data: {"error":{"message":"The server is overloaded","type":"server_error"}}` + "\n\n")}}
	h := handler(OpenAIChat, s, nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"claude","stream":true,"messages":[]}`)))
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"code":"upstream.unavailable"`) || w.Header().Get("Retry-After") == "" {
		t.Fatalf("status %d %s %v: want a retryable 503 in OpenAI's shape", w.Code, w.Body, w.Header())
	}
}

// TestCommitAtFirstEvent: a stream whose first event is not an error is
// committed and relayed byte for byte, however its bytes are split across
// reads (here one byte a read) and with comments ahead of it; an error
// after the first event is the provider's, relayed in the committed
// stream as before.
func TestCommitAtFirstEvent(t *testing.T) {
	whole := flat(anthropicStream(2))
	var bytewise [][]byte
	for _, b := range append([]byte(": ping\n\n"), whole...) {
		bytewise = append(bytewise, []byte{b})
	}
	late := append(flat(anthropicStream(0)[:1]), overloaded...)
	for name, events := range map[string][][]byte{"one byte a read": bytewise, "an error after the first event": {late}} {
		h := handler(AnthropicMessages, &stub{events: events}, nil)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(smallRequest)))
		if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), flat(events)) {
			t.Errorf("%s: status %d, body %q: want the provider's stream as sent", name, w.Code, w.Body)
		}
	}
}

func TestIsErrorEvent(t *testing.T) {
	for _, tc := range []struct {
		d           Dialect
		event, data string
		want        bool
	}{
		{AnthropicMessages, "error", `{"type":"error"}`, true},
		{AnthropicMessages, "message_start", `{"type":"message_start"}`, false},
		{OpenAIChat, "", ` { "error" : {}}`, true},
		{OpenAIChat, "", `{"choices":[],"error":{}}`, false}, // a chunk, not an error object
		{OpenAIChat, "", `[DONE]`, false},
	} {
		if got := isErrorEvent(tc.d, []byte(tc.event), []byte(tc.data)); got != tc.want {
			t.Errorf("%v %q %q: %v", tc.d, tc.event, tc.data, got)
		}
	}
}
