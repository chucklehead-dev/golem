package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/gateway/limit"
	"github.com/chucklehead-dev/golem/gateway/route"
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

// TestNoFirstEvent: a stream whose provider sent its headers and then
// failed (a reset, an end, the idle alarm) before its first event is not
// committed. The request went out, so the attempt is uncertain
// (model/attempts.md): the call ends without a retry, though the pool has
// another deployment, is charged its worst case, and its deployment's
// lease fails (with EjectAfter 1 it is ejected), never commits.
func TestNoFirstEvent(t *testing.T) {
	for name, body := range map[string]func(*http.Request) io.Reader{
		"reset before any event": func(*http.Request) io.Reader { return failing{} },
		"part of an event, then reset": func(*http.Request) io.Reader {
			return io.MultiReader(strings.NewReader("event: message_start\ndata: {\"type\""), failing{})
		},
		"ended before any event":       func(*http.Request) io.Reader { return strings.NewReader(": ping\n\n") },
		"silent past the idle timeout": func(r *http.Request) io.Reader { return silent{r} },
	} {
		t.Run(name, func(t *testing.T) {
			var n1 atomic.Int32
			p1 := roundTrip(func(r *http.Request) (*http.Response, error) {
				io.Copy(io.Discard, r.Body)
				r.Body.Close()
				n1.Add(1)
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(body(r))}, nil
			})
			d2 := &counting{fn: func(int32) (*http.Response, error) { return streamOf(anthropicStream(1)), nil }}
			h, rows := poolHandler(t, 2, p1, d2)
			h.Health = &route.Health{EjectAfter: 1}
			c, _ := h.Snapshots.Current()
			rt := c.Routes.Lookup([]byte("claude"))
			rt.Idle = 50 * time.Millisecond
			var w *httptest.ResponseRecorder
			func() {
				defer func() { recover() }() // a committed stream's abort, without the fix
				w = serveOne(h)
			}()
			if w == nil {
				t.Fatal("the stream was committed, then aborted: it never began")
			}
			all := rowsOf(t, rows)
			switch {
			case w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "upstream.unavailable"):
				t.Fatalf("status %d %s: want 503 upstream.unavailable, the stream never began", w.Code, w.Body)
			case n1.Load() != 1 || d2.n.Load() != 0:
				t.Fatalf("attempts %d and %d: an uncertain attempt ends the call", n1.Load(), d2.n.Load())
			case len(all) != 1 || all[0]["outcome"] != "uncertain" || all[0]["cost_micros"].(float64) == 0:
				t.Fatalf("rows %v: want one, uncertain and charged its worst case", all)
			}
			if dh := h.Health.Deployment(rt.Pool[0]); !dh.Ejected || dh.InFlight != 0 {
				t.Fatalf("deployment %+v: want its lease failed (ejected), not committed", dh)
			}
		})
	}
}

// silent is a stream body that sends nothing until its request ends.
type silent struct{ r *http.Request }

func (s silent) Read([]byte) (int, error) {
	<-s.r.Context().Done()
	return 0, s.r.Context().Err()
}
