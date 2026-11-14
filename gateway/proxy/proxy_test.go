package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/chucklehead-dev/golem/gateway/telemetry"
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
	c, _ := h.Snapshots.Current()
	c.Routes.Lookup([]byte("claude")).Betas = map[string]bool{"b1": true} // restricted mode forwards approved betas
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

// TestOpenAIHeaders: an anthropic-* header on an OpenAI call is neither
// checked against the route's approvals (restricted mode would refuse an
// unapproved beta) nor passed to the provider; OpenAI's own is passed.
func TestOpenAIHeaders(t *testing.T) {
	var got http.Header
	h := handler(OpenAIChat, roundTrip(func(r *http.Request) (*http.Response, error) {
		got = r.Header
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"usage":{"prompt_tokens":1,"completion_tokens":2}}`))}, nil
	}), nil)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"claude","messages":[]}`))
	req.Header.Set("Anthropic-Beta", "unapproved-2026")
	req.Header.Set("Anthropic-Version", "2099-01-01")
	req.Header.Set("Openai-Organization", "org-1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d %s: an Anthropic header refused an OpenAI call", w.Code, w.Body)
	}
	if got.Get("Anthropic-Beta") != "" || got.Get("Anthropic-Version") != "" || got.Get("Openai-Organization") != "org-1" {
		t.Fatalf("upstream headers %v; want OpenAI's own only", got)
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
		[]byte(": keep-alive\n\n"),
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
		if strings.Contains(got, `"choices":[]`) != tc.wantUsage || !strings.HasPrefix(got, ": keep-alive\n\n") ||
			!strings.Contains(got, `"content":"hi"`) || !strings.HasSuffix(got, "data: [DONE]\n\n") {
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
		{"not an object", `["claude"]`, 1 << 20, 400, "request.invalid"},
		{"duplicate model", `{"model":"claude","model":"other"}`, 1 << 20, 400, "request.invalid"},
		{"escaped duplicate", `{"model":"claude","model":"other"}`, 1 << 20, 400, "request.invalid"},
		{"trailing value", `{"model":"claude"}{"model":"other"}`, 1 << 20, 400, "request.invalid"},
		{"too large", `{"model":"claude","pad":"` + strings.Repeat("x", 100) + `"}`, 64, 413, "request.too_large"},
		{"zero is the default cap, not none", `{"model":"gpt-9"}`, 0, 404, "route.unknown_model"},
		{"over the default cap", `{"model":"claude","pad":"` + strings.Repeat("x", 32<<20) + `"}`, 0, 413, "request.too_large"},
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
			if id := w.Header().Get("Golem-Request-Id"); len(id) != 36 || !strings.Contains(w.Body.String(), `"request_id":"`+id+`"`) {
				t.Fatalf("request id %q, body %s: every response carries one, and errors name it", id, w.Body)
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

// TestUpstreamCut: a provider response that ends early must not reach the
// caller as a clean, shorter one. A stream gets upstream.stream_failed as an
// event, and the response is aborted, streamed or not; the call is still
// accounted for.
func TestUpstreamCut(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		d          Dialect
		event      bool
	}{
		{"an Anthropic stream", string(smallRequest), AnthropicMessages, true},
		{"an OpenAI stream", `{"model":"claude","stream":true,"messages":[]}`, OpenAIChat, true},
		{"a whole response", `{"model":"claude","messages":[]}`, OpenAIChat, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.event {
					w.Header().Set("Content-Type", "text/event-stream")
					w.Write(anthropicStream(0)[0])
				} else {
					w.Write([]byte(`{"choices":[{"message":`))
				}
				w.(http.Flusher).Flush()
				panic(http.ErrAbortHandler) // the provider's connection drops
			}))
			defer up.Close()
			u, _ := url.Parse(up.URL)
			results := make(chan Result, 1)
			h := &Handler{Dialect: tc.d, MaxBody: 1 << 20, Record: func(_ *http.Request, r Result) { results <- r },
				Snapshots: routes(tc.d, u, up.Client().Transport)}
			gw := httptest.NewServer(h)
			defer gw.Close()
			var got []byte
			resp, err := gw.Client().Post(gw.URL, "application/json", strings.NewReader(tc.body))
			if err == nil { // else aborted before the headers: nothing flushed yet
				got, err = io.ReadAll(resp.Body)
			}
			if err == nil {
				t.Fatalf("the caller read %q to a clean end: a cut response must not look whole", got)
			}
			if strings.Contains(string(got), "upstream.stream_failed") != tc.event {
				t.Fatalf("the caller got %q; an error event: %v", got, tc.event)
			}
			if r := <-results; r.Err == nil || r.CallerGone {
				t.Fatalf("result %+v: the cut is the provider's", r)
			}
		})
	}
}

// serve starts h as a test server speaking HTTP/1.1, or h2c as
// golem-gateway does behind the mesh, and returns its URL and a client
// speaking the same.
func serve(t *testing.T, h http.Handler, h2c bool) (string, *http.Client) {
	srv := httptest.NewUnstartedServer(h)
	var p http.Protocols
	if h2c {
		p.SetUnencryptedHTTP2(true)
	} else {
		p.SetHTTP1(true)
	}
	srv.Config.Protocols = &p
	srv.Start()
	t.Cleanup(srv.Close)
	return srv.URL, &http.Client{Transport: &http.Transport{Protocols: &p}}
}

func protocols(t *testing.T, f func(t *testing.T, h2c bool)) {
	t.Run("http1", func(t *testing.T) { f(t, false) })
	t.Run("h2c", func(t *testing.T) { f(t, true) })
}

// TestStalledClient: a client that stops reading a stream is a caller gone
// once a write has blocked for WriteStall, so it cannot hold its slot and
// its upstream connection for as long as the provider streams.
func TestStalledClient(t *testing.T) {
	protocols(t, func(t *testing.T, h2c bool) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			chunk := []byte("data: " + strings.Repeat("x", 64<<10) + "\n\n")
			for r.Context().Err() == nil {
				if _, err := w.Write(chunk); err != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
		}))
		defer up.Close()
		u, _ := url.Parse(up.URL)
		results := make(chan Result, 1)
		h := &Handler{Dialect: OpenAIChat, MaxBody: 1 << 20, WriteStall: 200 * time.Millisecond,
			Record: func(_ *http.Request, r Result) { results <- r }, Snapshots: routes(OpenAIChat, u, up.Client().Transport)}
		gw, client := serve(t, h, h2c)
		resp, err := client.Post(gw, "application/json",
			strings.NewReader(`{"model":"claude","stream":true,"stream_options":{"include_usage":true},"messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close() // and never read: the buffers fill, and the gateway's write blocks
		select {
		case r := <-results:
			if !r.CallerGone {
				t.Fatalf("result %+v: a stalled client is a caller gone", r)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("the gateway kept writing to a client that stopped reading")
		}
	})
}

// TestSilentProviderKeepsClient: a provider silent for longer than the
// stall (a reasoning pause, a slow tool) must not cut a client that reads.
// Under HTTP/2 a write deadline left set between writes resets the stream
// when it passes, so the stall is armed only while a write is in flight.
func TestSilentProviderKeepsClient(t *testing.T) {
	protocols(t, func(t *testing.T, h2c bool) {
		const stall = 150 * time.Millisecond
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for i := range 3 {
				fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"%d\"}}]}\n\n", i)
				w.(http.Flusher).Flush()
				time.Sleep(3 * stall) // the provider thinks
			}
			io.WriteString(w, "data: [DONE]\n\n")
		}))
		defer up.Close()
		u, _ := url.Parse(up.URL)
		results := make(chan Result, 1)
		h := &Handler{Dialect: OpenAIChat, MaxBody: 1 << 20, WriteStall: stall,
			Record: func(_ *http.Request, r Result) { results <- r }, Snapshots: routes(OpenAIChat, u, up.Client().Transport)}
		gw, client := serve(t, h, h2c)
		resp, err := client.Post(gw, "application/json",
			strings.NewReader(`{"model":"claude","stream":true,"stream_options":{"include_usage":true},"messages":[]}`))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil || !strings.HasSuffix(string(body), "data: [DONE]\n\n") {
			t.Fatalf("the client got %q, %v: the whole stream, however long the provider paused", body, err)
		}
		if r := <-results; r.CallerGone || r.Err != nil {
			t.Fatalf("result %+v: a reading client is not gone", r)
		}
	})
}

// bounded is a handler over one OpenAI route with the given bounds.
func bounded(t *testing.T, up *httptest.Server, maxD, headers, idle time.Duration, results chan Result) *Handler {
	u, _ := url.Parse(up.URL)
	h := routes(OpenAIChat, u, up.Client().Transport)
	c, _ := h.Current()
	rt := c.Routes.Lookup([]byte("claude"))
	rt.MaxDuration, rt.Headers, rt.Idle = maxD, headers, idle
	return &Handler{Dialect: OpenAIChat, MaxBody: 1 << 20, Snapshots: h, Record: func(_ *http.Request, r Result) { results <- r }}
}

const streamReq = `{"model":"claude","stream":true,"stream_options":{"include_usage":true},"messages":[]}`

// TestRouteBounds: a provider that never answers, falls silent mid-stream,
// or streams for ever is cut at its route's bound, the call accounted as
// the provider's failure, not held (with its slot) for ever.
func TestRouteBounds(t *testing.T) {
	t.Run("headers", func(t *testing.T) {
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.ReadAll(r.Body) // the server notices the client leave only once the body is read
			<-r.Context().Done()
		}))
		defer up.Close()
		h := bounded(t, up, time.Minute, 100*time.Millisecond, time.Minute, make(chan Result, 1))
		w := httptest.NewRecorder()
		start := time.Now()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(streamReq)))
		if w.Code != 503 || !strings.Contains(w.Body.String(), "headers timeout") || time.Since(start) > 5*time.Second {
			t.Fatalf("%d %s after %v", w.Code, w.Body, time.Since(start))
		}
	})
	for _, tc := range []struct {
		name            string
		maxD, idle      time.Duration
		silentAfterOnce bool
	}{
		{"idle", time.Minute, 150 * time.Millisecond, true},
		{"max duration", 300 * time.Millisecond, time.Minute, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				for r.Context().Err() == nil {
					w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
					w.(http.Flusher).Flush()
					if tc.silentAfterOnce {
						<-r.Context().Done()
					}
					time.Sleep(20 * time.Millisecond)
				}
			}))
			defer up.Close()
			results := make(chan Result, 1)
			gw, client := serve(t, bounded(t, up, tc.maxD, time.Minute, tc.idle, results), false)
			resp, err := client.Post(gw, "application/json", strings.NewReader(streamReq))
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err == nil || !strings.Contains(string(body), "upstream.stream_failed") {
				t.Fatalf("the caller read %q, %v: a cut stream ends with the error event and an abort", body, err)
			}
			select {
			case r := <-results:
				if r.CallerGone || r.Err == nil {
					t.Fatalf("result %+v: the provider's failure, not the caller's", r)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("the call was not cut")
			}
		})
	}
}

// TestCallerGoneBeforeHeaders: a caller that leaves while the provider is
// still to answer is the caller's cancellation, not the provider's failure.
func TestCallerGoneBeforeHeaders(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.ReadAll(r.Body)
		<-r.Context().Done()
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL)
	var rows bytes.Buffer
	tel, _ := telemetry.New(telemetry.Stdout(&rows), "local", "standalone")
	h := &Handler{Dialect: OpenAIChat, MaxBody: 1 << 20, Snapshots: routes(OpenAIChat, u, up.Client().Transport), Telemetry: tel}
	ctx, cancel := context.WithCancel(t.Context())
	time.AfterFunc(100*time.Millisecond, cancel)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, "POST", "/v1/chat/completions", strings.NewReader(streamReq)))
	if !strings.Contains(rows.String(), `"outcome":"cancelled"`) || !strings.Contains(rows.String(), `"error_origin":"client"`) {
		t.Fatalf("row %s: the caller's cancellation", rows.String())
	}
}
