package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/gateway/limit"
	"github.com/chucklehead-dev/golem/gateway/proxy"
	"github.com/chucklehead-dev/golem/gateway/route"
	"github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/gateway/telemetry"
)

// TestShutdownAccounts: a stream still running at the drain deadline is
// cut, and still leaves its ledger row and its counter add; readiness
// fails from the start of the shutdown.
func TestShutdownAccounts(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for r.Context().Err() == nil { // a stream longer than any drain
			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"))
			w.(http.Flusher).Flush()
			time.Sleep(20 * time.Millisecond)
		}
	}))
	defer up.Close()
	u, _ := url.Parse(up.URL + "/v1/chat/completions")
	holder := snapshot.Static(&snapshot.Compiled{Routes: route.NewTable(&route.Route{Alias: "gpt", Dialect: "openai.chat",
		Pool: []*route.Target{{Backend: "openai", Deployment: "gpt", Model: "gpt-5", URL: u, Transport: up.Client().Transport}}})})

	var rows bytes.Buffer
	tel, err := telemetry.New(telemetry.Stdout(&rows), "local", "standalone")
	if err != nil {
		t.Fatal(err)
	}
	st := limit.NewMem()
	lim := limit.New(st)

	lc := &lifecycle{drain: 200 * time.Millisecond, cut: 5 * time.Second}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", lc.track(proxy.OpenAIChat, &proxy.Handler{Dialect: proxy.OpenAIChat, Snapshots: holder, ProviderTail: 50 * time.Millisecond,
		Limiter: lim.Admitter(), MaxBody: 1 << 20, Telemetry: tel}))
	mux.HandleFunc("GET /readyz", lc.readyz(func() error { return nil }))
	ts := httptest.NewUnstartedServer(mux)
	lc.srv = ts.Config
	ts.Start()
	defer ts.Close()

	resp, err := ts.Client().Post(ts.URL+"/v1/chat/completions", "application/json",
		strings.NewReader(`{"model":"gpt","stream":true,"messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	resp.Body.Read(make([]byte, 64)) // the stream is under way

	done := make(chan struct{})
	go func() { lc.shutdown(); close(done) }()
	time.Sleep(50 * time.Millisecond)
	w := httptest.NewRecorder()
	lc.readyz(func() error { return nil })(w, nil)
	if w.Code != 503 {
		t.Errorf("readyz %d while draining, want 503", w.Code)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	time.Sleep(100 * time.Millisecond) // the cut call's slot is held for the provider's tail (golem#4)
	if len(st.Slots()) != 0 {
		t.Fatalf("slots %v: the call's slot freed after its tail", st.Slots())
	}
	lim.Finish(t.Context())

	lines := strings.Split(strings.TrimSpace(rows.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("one row expected, got %q", rows.String())
	}
	var row map[string]any
	json.Unmarshal([]byte(lines[0]), &row)
	if row["outcome"] != "cancelled" {
		t.Fatalf("row %v: the cut call is a cancellation", row)
	}
}

// TestTrackAfterWait: a call that starts once the wait has begun is
// refused, not counted after the count was taken, in its dialect's error
// shape with Retry-After; the wait ends with the last call that started
// before it.
func TestTrackAfterWait(t *testing.T) {
	lc := &lifecycle{}
	release := make(chan struct{})
	started := make(chan struct{})
	h := lc.track(proxy.AnthropicMessages, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { close(started); <-release }))
	openai := lc.track(proxy.OpenAIChat, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("a call ran after the wait began") }))
	go h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/", nil))
	<-started
	idle := lc.wait()
	for name, tc := range map[string]struct {
		h    http.Handler
		code func(body []byte) (string, error)
	}{
		"anthropic": {h, func(b []byte) (string, error) {
			var e struct {
				Type  string
				Error struct{ Type, Message string }
			}
			err := json.Unmarshal(b, &e)
			if e.Type != "error" || e.Error.Message == "" {
				return "", fmt.Errorf("not Anthropic's error shape")
			}
			return e.Error.Type, err
		}},
		"openai": {openai, func(b []byte) (string, error) {
			var e struct {
				Error struct{ Code, Type, Message string }
			}
			err := json.Unmarshal(b, &e)
			if e.Error.Type != "gateway" || e.Error.Message == "" {
				return "", fmt.Errorf("not OpenAI's error shape")
			}
			return e.Error.Code, err
		}},
	} {
		w := httptest.NewRecorder()
		tc.h.ServeHTTP(w, httptest.NewRequest("POST", "/", nil))
		code, err := tc.code(w.Body.Bytes())
		if w.Code != 503 || w.Header().Get("Retry-After") == "" || w.Header().Get("Content-Type") != "application/json" || err != nil || code != "gateway.not_ready" {
			t.Errorf("%s: a call after the wait began: %d, Retry-After %q, %s (%v); want 503, Retry-After, gateway.not_ready in the dialect's shape",
				name, w.Code, w.Header().Get("Retry-After"), w.Body, err)
		}
	}
	select {
	case <-idle:
		t.Fatal("the wait ended with a call in flight")
	default:
	}
	close(release)
	select {
	case <-idle:
	case <-time.After(5 * time.Second):
		t.Fatal("the wait did not end with the last call")
	}
}
