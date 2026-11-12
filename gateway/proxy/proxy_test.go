package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/gateway/route"
	gsnap "github.com/chucklehead-dev/golem/gateway/snapshot"
)

var upstreamURL, _ = url.Parse("https://provider.example/v1/messages")

// routes is a snapshot with one alias, "claude", of dialect d.
func routes(d Dialect, u *url.URL, s http.RoundTripper) *gsnap.Holder {
	return gsnap.Static(&gsnap.Compiled{Routes: route.NewTable(&route.Route{Alias: "claude", Dialect: d.Name(),
		Pool: []*route.Target{{Model: "claude-sonnet", URL: u, Transport: s, Header: http.Header{"X-Api-Key": {"provider-key"}}}}})})
}

func handler(d Dialect, s http.RoundTripper, record func(*http.Request, Result)) *Handler {
	return &Handler{Dialect: d, MaxBody: 1 << 20, Record: record, Snapshots: routes(d, upstreamURL, s)}
}

func TestAnthropicStream(t *testing.T) {
	s := &stub{events: anthropicStream(3)}
	var res Result
	h := handler(AnthropicMessages, s, func(_ *http.Request, r Result) { res = r })
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(smallRequest))
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Golem-Session", "s1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), flat(anthropicStream(3))) {
		t.Fatalf("status %d; the caller must get the provider's stream byte for byte", w.Code)
	}
	if !w.Flushed {
		t.Fatal("the stream was not flushed")
	}
	var sent map[string]any
	json.Unmarshal(s.got, &sent)
	if sent["model"] != "claude-sonnet" {
		t.Fatalf("upstream model %v, want the route's", sent["model"])
	}
	if want := (canon.Usage{Input: 2048, Output: 12, CacheRead: 40000}); res.Usage != want || res.Err != nil {
		t.Fatalf("usage %+v, %v; want %+v", res.Usage, res.Err, want)
	}
}

func TestHeaders(t *testing.T) {
	var got http.Header
	h := handler(AnthropicMessages, roundTrip(func(r *http.Request) (*http.Response, error) {
		got = r.Header
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "Set-Cookie": {"x"}},
			Body: io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":1,"output_tokens":2}}`))}, nil
	}), nil)
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude"}`))
	req.Header.Set("Authorization", "Bearer glm_live_caller")
	req.Header.Set("Golem-Session", "s1")
	req.Header.Set("Anthropic-Beta", "b1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if got.Get("Authorization") != "" || got.Get("Golem-Session") != "" {
		t.Fatalf("the caller's credentials or golem's headers reached the provider: %v", got)
	}
	if got.Get("X-Api-Key") != "provider-key" || got.Get("Anthropic-Beta") != "b1" {
		t.Fatalf("upstream headers %v; want the route's key and the allowed caller headers", got)
	}
	if w.Header().Get("Set-Cookie") != "" {
		t.Fatal("a provider header outside the allowlist reached the caller")
	}
}

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestOpenAIStreamRequestsUsage(t *testing.T) {
	s := &stub{events: [][]byte{
		[]byte(`data: {"choices":[{"delta":{"content":"hi"}}],"usage":null}` + "\n\n"),
		[]byte(`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2}}` + "\n\ndata: [DONE]\n\n"),
	}}
	var res Result
	h := handler(OpenAIChat, s, func(_ *http.Request, r Result) { res = r })
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/v1/chat/completions",
		strings.NewReader(`{"model":"claude","stream":true,"messages":[]}`)))
	if !strings.Contains(string(s.got), `"include_usage":true`) {
		t.Fatalf("upstream body %s does not ask for usage", s.got)
	}
	if want := (canon.Usage{Input: 10, Output: 2}); res.Usage != want {
		t.Fatalf("usage %+v, want %+v", res.Usage, want)
	}
}

// TestInjectedUsageWithheld: the caller did not ask for usage, so the
// usage-only chunk golem asked for is not forwarded; every other event is,
// as sent. A caller that asked gets it.
func TestInjectedUsageWithheld(t *testing.T) {
	events := [][]byte{
		[]byte(`data: {"choices":[{"delta":{"content":"hi"}}],"usage":null}` + "\n\n"),
		[]byte(`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2}}` + "\n\ndata: [DONE]\n\n"),
	}
	for _, tc := range []struct {
		body      string
		wantUsage bool
	}{
		{`{"model":"claude","stream":true,"messages":[]}`, false},
		{`{"model":"claude","stream":true,"stream_options":{"include_usage":true},"messages":[]}`, true},
	} {
		var res Result
		h := handler(OpenAIChat, &stub{events: slices.Clone(events)}, func(_ *http.Request, r Result) { res = r })
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(tc.body)))
		got := w.Body.String()
		if strings.Contains(got, `"choices":[]`) != tc.wantUsage || !strings.Contains(got, `"content":"hi"`) || !strings.HasSuffix(got, "data: [DONE]\n\n") {
			t.Errorf("%s: the caller got %q", tc.body, got)
		}
		if res.Usage.Input != 10 {
			t.Errorf("%s: usage %+v: golem must still read it", tc.body, res.Usage)
		}
	}
}

func TestRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		max        int64
		status     int
		code       string
	}{
		{"unknown alias", `{"model":"gpt-9"}`, 1 << 20, 404, "route.unknown_model"},
		{"duplicate model", `{"model":"claude","model":"other"}`, 1 << 20, 400, "request.invalid"},
		{"escaped duplicate", `{"model":"claude","model":"other"}`, 1 << 20, 400, "request.invalid"},
		{"trailing value", `{"model":"claude"}{"model":"other"}`, 1 << 20, 400, "request.invalid"},
		{"too large", `{"model":"claude","pad":"` + strings.Repeat("x", 100) + `"}`, 64, 413, "request.too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := handler(AnthropicMessages, &stub{}, nil)
			h.MaxBody = tc.max
			w := httptest.NewRecorder()
			h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", strings.NewReader(tc.body)))
			var e struct {
				Type  string
				Error struct{ Type, Message string }
			}
			json.Unmarshal(w.Body.Bytes(), &e)
			if w.Code != tc.status || e.Type != "error" || e.Error.Type != tc.code || e.Error.Message == "" {
				t.Fatalf("status %d body %s; want %d with an Anthropic error %q", w.Code, w.Body, tc.status, tc.code)
			}
		})
	}
}

// TestCallerCancels: when the caller goes away mid-stream, the upstream
// request is cancelled with it, and the result says the stream was cut.
func TestCallerCancels(t *testing.T) {
	upstreamDone := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write(anthropicStream(0)[0])
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(upstreamDone)
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	results := make(chan Result, 1)
	h := &Handler{Dialect: AnthropicMessages, MaxBody: 1 << 20, Record: func(_ *http.Request, r Result) { results <- r },
		Snapshots: routes(AnthropicMessages, u, up.Client().Transport)}
	gw := httptest.NewServer(h)
	defer gw.Close()

	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, "POST", gw.URL, bytes.NewReader(smallRequest))
	resp, err := gw.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	resp.Body.Read(buf) // the first event arrived
	cancel()
	select {
	case <-upstreamDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream request was not cancelled with the caller")
	}
	if r := <-results; r.Err == nil || r.Usage.Input != 2048 {
		t.Fatalf("result %+v; want the cut reported and the usage seen so far", r)
	}
}

func TestWrongFrontend(t *testing.T) {
	h := handler(OpenAIChat, &stub{}, nil)
	h.Snapshots = routes(AnthropicMessages, upstreamURL, &stub{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"claude"}`)))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "route.wrong_frontend") {
		t.Fatalf("%d %s; want a refusal naming the dialect, not an implied conversion (ADR 0033)", w.Code, w.Body)
	}
}

type expired struct{}

func (expired) Current() (*gsnap.Compiled, error) { return nil, gsnap.ErrExpired }

func TestLeaseExpired(t *testing.T) {
	s := &stub{events: anthropicStream(1)}
	h := handler(AnthropicMessages, s, nil)
	h.Snapshots = expired{}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(smallRequest)))
	if w.Code != 503 || !strings.Contains(w.Body.String(), "gateway.authorization_expired") || s.got != nil {
		t.Fatalf("%d %s; want a 503 and nothing sent upstream", w.Code, w.Body)
	}
}
