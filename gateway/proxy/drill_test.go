package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/gateway/route"
	gsnap "github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/gateway/telemetry"
	libsnapshot "github.com/chucklehead-dev/golem/lib/snapshot"
)

// TestFailoverDrill is m1-plan.md's failover drill in small: two providers
// over real HTTP, the gateway in front of them (its own HTTP server), callers
// streaming steadily, and the first provider killed mid-run (its listener
// closed and its connections cut). It shows:
//
//   - every call that starts after the kill succeeds whole, failing over
//     to the second provider with no change at the caller (M1's exit: an
//     outage fails over without caller changes);
//   - a call cut by the kill ends with the contract's error event, never a
//     stream that reads as whole;
//   - one ledger row per attempt, each its own event: the free attempts at
//     the dead provider charged nothing, every answer charged once;
//   - the dead provider ejected, so later calls stop trying it first.
//
// The real drill (a Bedrock region, a local box) runs on the install
// (m1-plan.md); this one runs in CI.
//
//quint:checks attempts.ledgerMatches
func TestFailoverDrill(t *testing.T) {
	if testing.Short() {
		t.Skip("a two-second load")
	}
	// A provider streams an answer over about 50 ms.
	provider := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "text/event-stream")
			for _, ev := range anthropicStream(10) {
				if _, err := w.Write(ev); err != nil {
					return
				}
				w.(http.Flusher).Flush()
				time.Sleep(4 * time.Millisecond)
			}
		}))
	}
	p1, p2 := provider(), provider()
	defer p2.Close()
	target := func(name string, s *httptest.Server) *route.Target {
		u, _ := url.Parse(s.URL + "/v1/messages")
		return &route.Target{Backend: "b", Deployment: name, ID: "b/" + name, Model: "m", URL: u, Transport: &http.Transport{}}
	}
	rt := &route.Route{Alias: "claude", Dialect: AnthropicMessages.Name(), MaxAttempts: 2, Pool: []*route.Target{target("p1", p1), target("p2", p2)}}
	var rows lockedBuffer
	tel, err := telemetry.New(telemetry.Stdout(&rows), "local", "drill")
	if err != nil {
		t.Fatal(err)
	}
	health := &route.Health{EjectAfter: 3, Ejection: time.Minute}
	h := &Handler{Dialect: AnthropicMessages, MaxBody: 1 << 20, Telemetry: tel, Health: health,
		Snapshots: gsnap.Static(&gsnap.Compiled{Routes: route.NewTable(rt),
			Prices: map[string]libsnapshot.Price{"m": {InputPerMTok: 1_000_000, OutputPerMTok: 1_000_000}}})}
	gw := httptest.NewServer(h)
	defer gw.Close()

	// Callers: eight, each calling in a loop for two seconds; the first
	// provider dies at 0.7 s.
	type result struct {
		started time.Time
		status  int
		whole   bool // ended with message_stop
		errored bool // ended with the contract's error event
	}
	var mu sync.Mutex
	var results []result
	var killed atomic.Int64 // UnixNano of the kill
	stop := time.Now().Add(2 * time.Second)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			client := &http.Client{}
			for time.Now().Before(stop) {
				r := result{started: time.Now()}
				resp, err := client.Post(gw.URL+"/v1/messages", "application/json", bytes.NewReader(smallRequest))
				if err == nil {
					body, _ := io.ReadAll(resp.Body)
					resp.Body.Close()
					r.status = resp.StatusCode
					r.whole = bytes.HasSuffix(body, []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
					r.errored = bytes.Contains(body, []byte("upstream.stream_failed"))
				}
				mu.Lock()
				results = append(results, r)
				mu.Unlock()
			}
		})
	}
	time.Sleep(700 * time.Millisecond)
	killed.Store(time.Now().UnixNano())
	p1.CloseClientConnections()
	p1.Close()
	wg.Wait()

	kill := time.Unix(0, killed.Load())
	var before, after, cut int
	for _, r := range results {
		switch {
		case r.started.After(kill.Add(10 * time.Millisecond)):
			after++
			if r.status != 200 || !r.whole {
				t.Errorf("a call started %v after the kill: status %d, whole %v: want the second provider's answer", r.started.Sub(kill), r.status, r.whole)
			}
		case !r.whole:
			cut++
			if r.status == 200 && !r.errored {
				t.Errorf("a stream cut by the kill read as whole: no error event")
			}
		default:
			before++
		}
	}
	if after < 50 || before < 50 {
		t.Fatalf("%d calls before the kill, %d after: the load is too light to show anything", before, after)
	}

	// The ledger: one row per attempt, each its own event; a call's rows
	// share its request ID.
	type row struct {
		RequestID  string  `json:"request_id"`
		EventID    string  `json:"event_id"`
		Deployment string  `json:"deployment"`
		Outcome    string  `json:"outcome"`
		Cost       float64 `json:"cost_micros"`
	}
	events := map[string]bool{}
	byCall := map[string][]row{}
	for _, line := range bytes.Split(bytes.TrimSpace(rows.snapshot().Bytes()), []byte("\n")) {
		var r row
		if err := json.Unmarshal(line, &r); err != nil {
			t.Fatal(err)
		}
		if events[r.EventID] {
			t.Fatalf("event %s twice: each attempt is its own event", r.EventID)
		}
		events[r.EventID] = true
		byCall[r.RequestID] = append(byCall[r.RequestID], r)
	}
	if len(byCall) != len(results) {
		t.Fatalf("%d calls in the ledger, %d made", len(byCall), len(results))
	}
	failovers := 0
	for id, rs := range byCall {
		last := rs[len(rs)-1]
		for _, r := range rs[:len(rs)-1] {
			if r.Deployment != "b/p1" || r.Cost != 0 || r.Outcome != "failed" {
				t.Errorf("call %s: a retried attempt %+v: want a free failure at the dead provider", id, r)
			}
		}
		if len(rs) > 1 {
			failovers++
			if last.Deployment != "b/p2" || last.Outcome != "completed" || last.Cost == 0 {
				t.Errorf("call %s: its failover %+v: want the second provider's answer, charged", id, last)
			}
		}
	}
	if !health.Deployment(rt.Pool[0]).Ejected {
		t.Error("the dead provider was not ejected")
	}
	t.Logf("%d calls before the kill, %d cut by it, %d after; %d failed over (the rest after the ejection went to p2 at once); %d ledger rows",
		before, cut, after, failovers, len(events))
	if failovers == 0 {
		t.Error("no call failed over: the kill was not seen")
	}
}
