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
	lim.OnNumbered = func(_ string, e limit.Event, seq int64) { tel.Numbered(e.ID, seq) }

	lc := &lifecycle{drain: 200 * time.Millisecond, cut: 5 * time.Second}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/chat/completions", lc.track(&proxy.Handler{Dialect: proxy.OpenAIChat, Snapshots: holder,
		Limiter: lim, MaxBody: 1 << 20, Telemetry: tel}))
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
	finish(lim, tel)

	var row map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(rows.Bytes()), &row); err != nil {
		t.Fatalf("one ledger row expected, got %q: %v", rows.String(), err)
	}
	if row["outcome"] != "cancelled" || row["counter_seq"] != 1.0 {
		t.Fatalf("row %v: the cut call is a cancellation, numbered", row)
	}
	month := time.Now().UTC().Format("2006-01")
	if len(st.Counter("any", month)) != 1 || len(st.Slots()) != 0 {
		t.Fatalf("counter %v, slots %v: the call is counted and its slot freed", st.Counter("any", month), st.Slots())
	}
}
