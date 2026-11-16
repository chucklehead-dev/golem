package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/gateway/admission"
	"github.com/chucklehead-dev/golem/gateway/route"
	gsnap "github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/gateway/telemetry"
	"github.com/chucklehead-dev/golem/lib/contract"
	libsnapshot "github.com/chucklehead-dev/golem/lib/snapshot"
)

// The attempt loop against model/attempts.md's properties, each with the
// provider behaviour that would break it.

// counting is a provider that answers each request by fn and counts them.
type counting struct {
	n  atomic.Int32
	fn func(n int32) (*http.Response, error)
}

func (c *counting) RoundTrip(r *http.Request) (*http.Response, error) {
	io.Copy(io.Discard, r.Body)
	r.Body.Close()
	return c.fn(c.n.Add(1))
}

func streamOf(events [][]byte) *http.Response {
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(&chunks{c: events})}
}

func statusOf(code int, retryAfter string) *http.Response {
	h := http.Header{"Content-Type": {"application/json"}}
	if retryAfter != "" {
		h.Set("Retry-After", retryAfter)
	}
	return &http.Response{StatusCode: code, Header: h, Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"overloaded_error"}}`))}
}

var dialFailed = &net.OpError{Op: "dial", Err: errors.New("connection refused")}

// poolHandler serves "claude" from a pool of providers, in order, with
// maxAttempts attempts a call, a price so a row's cost shows, and the
// ledger rows written to rows.
func poolHandler(t *testing.T, maxAttempts int, providers ...http.RoundTripper) (*Handler, *bytes.Buffer) {
	t.Helper()
	rt := &route.Route{Alias: "claude", Dialect: AnthropicMessages.Name(), MaxAttempts: maxAttempts}
	for i, p := range providers {
		rt.Pool = append(rt.Pool, &route.Target{Backend: "b", Deployment: string(rune('1' + i)), Model: "m", URL: upstreamURL, Transport: p})
	}
	var rows bytes.Buffer
	tel, err := telemetry.New(telemetry.Stdout(&rows), "local", "test")
	if err != nil {
		t.Fatal(err)
	}
	snap := &gsnap.Compiled{Routes: route.NewTable(rt), Prices: map[string]libsnapshot.Price{"m": {InputPerMTok: 1_000_000, OutputPerMTok: 1_000_000}}}
	return &Handler{Dialect: AnthropicMessages, MaxBody: 1 << 20, Snapshots: gsnap.Static(snap), Telemetry: tel}, &rows
}

func serveOne(h *Handler) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(smallRequest)))
	return w
}

// TestFailover: an attempt its provider refused before any output (a 529,
// a 200 then an overloaded event, a dial that failed) is free and fails
// over to the pool's next deployment, whose answer the caller gets byte
// for byte. Each attempt is its own ledger event; only the one that
// committed is charged (the model's failoverTest, ledgerMatches).
func TestFailover(t *testing.T) {
	for name, first := range map[string]func(int32) (*http.Response, error){
		"529":                    func(int32) (*http.Response, error) { return statusOf(529, ""), nil },
		"overloaded first event": func(int32) (*http.Response, error) { return streamOf([][]byte{overloaded}), nil },
		"dial failed":            func(int32) (*http.Response, error) { return nil, dialFailed },
	} {
		t.Run(name, func(t *testing.T) {
			d1 := &counting{fn: first}
			d2 := &counting{fn: func(int32) (*http.Response, error) { return streamOf(anthropicStream(2)), nil }}
			h, rows := poolHandler(t, 2, d1, d2)
			w := serveOne(h)
			if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), flat(anthropicStream(2))) || d1.n.Load() != 1 || d2.n.Load() != 1 {
				t.Fatalf("status %d, attempts %d and %d: want the second deployment's stream, one attempt each", w.Code, d1.n.Load(), d2.n.Load())
			}
			all := rowsOf(t, rows)
			if len(all) != 2 || all[0]["deployment"] != "b/1" || all[1]["deployment"] != "b/2" ||
				all[0]["cost_micros"].(float64) != 0 || all[1]["cost_micros"].(float64) == 0 ||
				all[0]["event_id"] == all[1]["event_id"] || all[1]["outcome"] != "completed" {
				t.Fatalf("rows %v: want the failed attempt free and the second charged, each its own event", all)
			}
		})
	}
}

// TestFinalAttempts: what ends a call without a retry, though the pool has
// another deployment. An uncertain attempt (the request went out, then
// the connection reset) is never retried: the provider may be working on
// it (the model's uncertainTest; workWithinLimit). A committed stream cut
// mid-way is not retried: its caller has part of an answer (oneCommit).
// A 4xx is the request's fault, not the deployment's; a 500 or a 502 may
// come after the provider worked (ADR 0051).
func TestFinalAttempts(t *testing.T) {
	for name, first := range map[string]func(int32) (*http.Response, error){
		"uncertain": func(int32) (*http.Response, error) { return nil, errors.New("read: connection reset by peer") },
		"cut after the first event": func(int32) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}},
				Body: io.NopCloser(io.MultiReader(bytes.NewReader(anthropicStream(1)[0]), &failing{}))}, nil
		},
		"400": func(int32) (*http.Response, error) { return statusOf(400, ""), nil },
		"500": func(int32) (*http.Response, error) { return statusOf(500, ""), nil },
		"502": func(int32) (*http.Response, error) { return statusOf(502, ""), nil },
	} {
		t.Run(name, func(t *testing.T) {
			d1 := &counting{fn: first}
			d2 := &counting{fn: func(int32) (*http.Response, error) { return streamOf(anthropicStream(1)), nil }}
			h, _ := poolHandler(t, 3, d1, d2)
			func() {
				defer func() { recover() }() // a cut response's abort
				serveOne(h)
			}()
			if d1.n.Load() != 1 || d2.n.Load() != 0 {
				t.Fatalf("attempts %d and %d: the call must end on its first attempt", d1.n.Load(), d2.n.Load())
			}
		})
	}
}

type failing struct{}

func (failing) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

// TestAttemptBudgets: a call makes at most its route's attempts, each on a
// deployment it has not tried (attemptsBounded; ADR 0051: a pool of one
// makes one attempt); and a route's retries are capped across calls
// (retriesCapped): with a budget of one retry, the second failing call
// gets none.
func TestAttemptBudgets(t *testing.T) {
	refuse := func(int32) (*http.Response, error) { return statusOf(503, ""), nil }
	d1, d2, d3 := &counting{fn: refuse}, &counting{fn: refuse}, &counting{fn: refuse}
	h, _ := poolHandler(t, 2, d1, d2, d3)
	h.Health = &route.Health{EjectAfter: 100}
	if w := serveOne(h); w.Code != 503 || d1.n.Load() != 1 || d2.n.Load() != 1 || d3.n.Load() != 0 {
		t.Fatalf("status %d, attempts %d, %d, %d: want two attempts (1, 1, 0), then the provider's 503", w.Code, d1.n.Load(), d2.n.Load(), d3.n.Load())
	}

	one := &counting{fn: refuse}
	h, _ = poolHandler(t, 5, one)
	if serveOne(h); one.n.Load() != 1 {
		t.Fatalf("%d attempts on a pool of one: a call is never retried on a deployment it tried", one.n.Load())
	}

	e1, e2 := &counting{fn: refuse}, &counting{fn: refuse}
	h, _ = poolHandler(t, 2, e1, e2)
	h.Health = &route.Health{EjectAfter: 100, RetryBurst: 1, RetryRatio: 0.001}
	serveOne(h)
	serveOne(h)
	if n := e1.n.Load() + e2.n.Load(); n != 3 {
		t.Fatalf("%d attempts over two calls: want the first call's two (its one retry) and the second's one", n)
	}
}

// TestCeilingFailsOver: a deployment at its ceiling takes no attempt, and
// the call goes to the next (quotaHonoured).
func TestCeilingFailsOver(t *testing.T) {
	release := make(chan struct{})
	d1 := &counting{fn: func(n int32) (*http.Response, error) {
		if n == 1 {
			<-release // the first call holds d1's one place
		}
		return streamOf(anthropicStream(1)), nil
	}}
	d2 := &counting{fn: func(int32) (*http.Response, error) { return streamOf(anthropicStream(1)), nil }}
	h, _ := poolHandler(t, 2, d1, d2)
	c, _ := h.Snapshots.Current()
	c.Routes.Lookup([]byte("claude")).Pool[0].MaxInFlight = 1
	done := make(chan struct{})
	go func() { serveOne(h); close(done) }()
	for d1.n.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	if w := serveOne(h); w.Code != 200 || d2.n.Load() != 1 || d1.n.Load() != 1 {
		t.Fatalf("status %d, attempts %d and %d: the second call must go to d2", w.Code, d1.n.Load(), d2.n.Load())
	}
	close(release)
	<-done
}

// TestKeptBound: a body past the handler's bound on kept bodies is not
// kept, and its call makes one attempt.
func TestKeptBound(t *testing.T) {
	d1 := &counting{fn: func(int32) (*http.Response, error) { return statusOf(503, ""), nil }}
	d2 := &counting{fn: func(int32) (*http.Response, error) { return streamOf(anthropicStream(1)), nil }}
	h, _ := poolHandler(t, 2, d1, d2)
	h.MaxKept = int64(len(smallRequest)) - 1
	if w := serveOne(h); w.Code != 503 || d2.n.Load() != 0 {
		t.Fatalf("status %d, d2 %d: a call whose body is not kept must not retry", w.Code, d2.n.Load())
	}
	if h.kept.Load() != 0 {
		t.Fatalf("%d bytes still counted as kept", h.kept.Load())
	}
}

// TestCallerGoneIsNoFailure: a call its caller cancelled, or a sweep
// withdrew, ends its attempt without a failure of its deployment's: a few
// Esc presses (or one revocation) must not eject a healthy deployment.
// Each case ends one attempt so with EjectAfter 1, which a failure would
// eject.
func TestCallerGoneIsNoFailure(t *testing.T) {
	// waitThen is a provider that answers, once its caller has left, as
	// then says.
	waitThen := func(arrived chan<- struct{}, then func(*http.Request) (*http.Response, error)) http.RoundTripper {
		return roundTrip(func(r *http.Request) (*http.Response, error) {
			io.Copy(io.Discard, r.Body)
			r.Body.Close()
			arrived <- struct{}{}
			<-r.Context().Done()
			return then(r)
		})
	}
	cancelled := func(t *testing.T, h *Handler, arrived <-chan struct{}) {
		t.Helper()
		ctx, cancel := context.WithCancel(t.Context())
		go func() { <-arrived; cancel() }()
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequestWithContext(ctx, "POST", "/v1/messages", bytes.NewReader(smallRequest)))
	}
	t.Run("unanswered", func(t *testing.T) {
		arrived := make(chan struct{}, 1)
		h, _ := poolHandler(t, 1, waitThen(arrived, func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() }))
		h.Health = &route.Health{EjectAfter: 1}
		cancelled(t, h, arrived)
		c, _ := h.Snapshots.Current()
		if dh := h.Health.Deployment(c.Routes.Lookup([]byte("claude")).Pool[0]); dh.Ejected || dh.InFlight != 0 {
			t.Fatalf("deployment %+v after its caller left: want it neither ejected nor held", dh)
		}
	})
	t.Run("refused as the caller left", func(t *testing.T) {
		arrived := make(chan struct{}, 1)
		d2 := &counting{fn: func(int32) (*http.Response, error) { return streamOf(anthropicStream(1)), nil }}
		h, _ := poolHandler(t, 2, waitThen(arrived, func(*http.Request) (*http.Response, error) { return statusOf(529, ""), nil }), d2)
		h.Health = &route.Health{EjectAfter: 1}
		cancelled(t, h, arrived)
		c, _ := h.Snapshots.Current()
		if dh := h.Health.Deployment(c.Routes.Lookup([]byte("claude")).Pool[0]); dh.Ejected || dh.InFlight != 0 || d2.n.Load() != 0 {
			t.Fatalf("deployment %+v, d2 attempts %d after the caller left: want it neither ejected nor held, and no retry", dh, d2.n.Load())
		}
	})
	t.Run("withdrawn", func(t *testing.T) {
		g := newTGW(t, AnthropicMessages, true)
		hl := &route.Health{EjectAfter: 1}
		g.srv.Config.Handler.(*Handler).Health = hl
		target := g.src.Holder.Applied().Routes.Lookup([]byte("claude")).Pool[0]
		alice := g.call(g.keyA, false).Body.(*lateBody)
		g.apply(true)
		resp, err := alice.done()
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if dh := hl.Deployment(target); dh.Ejected || resp.StatusCode != 401 {
			t.Fatalf("status %d, deployment %+v after a sweep withdrew its call: want 401 and it not ejected", resp.StatusCode, dh)
		}
	})
}

// TestRefusedCallsEarnNoRetries: the route's retry budget is credited by
// admitted calls only: a flood of refused ones (here by admission) must
// not refill the retries a failing pool draws on.
func TestRefusedCallsEarnNoRetries(t *testing.T) {
	ok := &counting{fn: func(int32) (*http.Response, error) { return streamOf(anthropicStream(1)), nil }}
	h, _ := poolHandler(t, 2, ok, ok)
	h.Health = &route.Health{RetryBurst: 2, RetryRatio: 1}
	for h.Health.Retry("claude") { // drained
	}
	h.Limiter = &admitter{t: t, refuse: &admission.Refusal{Code: contract.LimitConcurrency, Msg: "busy"}}
	for range 4 {
		if w := serveOne(h); w.Code != 429 {
			t.Fatalf("status %d: want the admitter's refusal", w.Code)
		}
	}
	if n := h.Health.Retries("claude"); n != 0 {
		t.Fatalf("%v retries after four refused calls: want none credited", n)
	}
	h.Limiter = nil
	serveOne(h)
	if n := h.Health.Retries("claude"); n != 1 {
		t.Fatalf("%v retries after one admitted call: want its credit, 1", n)
	}
}
