package main

import (
	"bytes"
	"encoding/json"
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
	lim.OnNumbered = func(n limit.Numbered) { tel.Numbered(n.Scope, n.Period, n.ID, n.Seq) }

	lc := &lifecycle{drain: 200 * time.Millisecond, cut: 5 * time.Second}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", lc.track(&proxy.Handler{Dialect: proxy.OpenAIChat, Snapshots: holder, ProviderTail: 50 * time.Millisecond,
		Limiter: proxy.Limits(lim), MaxBody: 1 << 20, Telemetry: tel}))
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
	lim.Finish(t.Context())

	lines := strings.Split(strings.TrimSpace(rows.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("a row and its numbering record expected, got %q", rows.String())
	}
	var row, num map[string]any
	json.Unmarshal([]byte(lines[0]), &row)
	json.Unmarshal([]byte(lines[1]), &num)
	if row["outcome"] != "cancelled" || num["counter_seq"] != 1.0 || num["event_id"] != row["event_id"] {
		t.Fatalf("row %v, numbering %v: the cut call is a cancellation, numbered", row, num)
	}
	month := time.Now().UTC().Format("2006-01")
	time.Sleep(100 * time.Millisecond) // the cut call's slot is held for the provider's tail (golem#4)
	if len(st.Counter("any", month)) != 1 || len(st.Slots()) != 0 {
		t.Fatalf("counter %v, slots %v: the call is counted and its slot freed", st.Counter("any", month), st.Slots())
	}
}
