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
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/gateway/admission"
	"github.com/chucklehead-dev/golem/gateway/limit"
	"github.com/chucklehead-dev/golem/lib/contract"
	"github.com/chucklehead-dev/golem/lib/key"
)

// admitter is an admission.Admitter double: it refuses with refuse, if
// set, and fails the test on any end after a ticket's first.
type admitter struct {
	t      *testing.T
	refuse error
	calls  []*account
}

type account struct {
	t     *testing.T
	call  admission.Call
	ends  []string // "complete" or "cancel", in order
	cost  int64
	until time.Time
}

func (a *admitter) Admit(ctx context.Context, c admission.Call) (admission.Ticket, error) {
	if a.refuse != nil {
		return nil, a.refuse
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	t := &account{t: a.t, call: c}
	a.calls = append(a.calls, t)
	return t, nil
}

func (t *account) end(how string, cost int64) {
	if len(t.ends) > 0 {
		t.t.Errorf("ticket %s: %s after %v: a ticket ends once", t.call.ID, how, t.ends)
	}
	t.ends, t.cost = append(t.ends, how), cost
}

func (t *account) Complete(_ context.Context, cost int64) { t.end("complete", cost) }
func (t *account) Cancel(_ context.Context, seen int64, until time.Time) {
	t.end("cancel", seen)
	t.until = until
}
func (t *account) Period() string { return "2026-11" }
func (t *account) Waived() bool   { return false }

// TestAdmitterContract: the request path depends only on admission. A
// refusal is rendered by its code (with its Retry-After), a caller gone
// during admission is told nothing, and every admitted call, however it
// ends, ends its ticket exactly once: Complete with the cost charged, or
// Cancel with the cost seen and a tail to hold its slot to.
func TestAdmitterContract(t *testing.T) {
	alice := key.New(key.Dev)
	cfg := `apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  scopes: [{name: team-a, models: [claude]}]
  keys: [{key: ` + alice + `, principal: alice, scope: team-a}]
`
	type served struct {
		w      *httptest.ResponseRecorder
		panicV any
	}
	serve := func(ctx context.Context, rt http.RoundTripper, a admission.Admitter) served {
		h, _ := standaloneHandler(t, cfg, rt)
		h.Limiter = a
		h.ProviderTail = time.Minute
		r := httptest.NewRequestWithContext(ctx, "POST", "/v1/messages", bytes.NewReader(smallRequest))
		r.Header.Set("X-Api-Key", alice)
		s := served{w: httptest.NewRecorder()}
		func() {
			defer func() {
				if v := recover(); v != nil && v != http.ErrAbortHandler {
					s.panicV = v
				}
			}()
			h.ServeHTTP(s.w, r)
		}()
		return s
	}
	ok := func() http.RoundTripper { return &stub{events: anthropicStream(3)} }

	t.Run("refusals", func(t *testing.T) {
		for _, tc := range []struct {
			err   error
			code  int
			after string
		}{
			{limit.ErrBudget, 402, ""},
			{limit.ErrConcurrency, 429, "5"},
			{&admission.Refusal{Code: contract.LimitRate, Msg: "slow down", RetryAfter: 1500 * time.Millisecond}, 429, "2"},
			{errors.New("store on fire"), 500, ""},
		} {
			s := serve(t.Context(), ok(), &admitter{t: t, refuse: tc.err})
			if s.w.Code != tc.code || s.w.Header().Get("Retry-After") != tc.after {
				t.Errorf("%v: %d, Retry-After %q; want %d, %q", tc.err, s.w.Code, s.w.Header().Get("Retry-After"), tc.code, tc.after)
			}
		}
	})
	t.Run("the caller gone during admission", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		s := serve(ctx, ok(), &admitter{t: t})
		if s.w.Body.Len() != 0 {
			t.Fatalf("%d %s: a caller who left is told nothing, and it is not the gateway's failure", s.w.Code, s.w.Body)
		}
	})

	dial := roundTrip(func(r *http.Request) (*http.Response, error) {
		r.Body.Close()
		return nil, &net.OpError{Op: "dial", Err: errors.New("connection refused")}
	})
	reset := roundTrip(func(r *http.Request) (*http.Response, error) {
		r.Body.Close()
		return nil, errors.New("read: connection reset by peer")
	})
	providerError := roundTrip(func(r *http.Request) (*http.Response, error) {
		r.Body.Close()
		return &http.Response{StatusCode: 529, Header: http.Header{"Content-Type": {"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"overloaded_error"}}`))}, nil
	})
	panics := roundTrip(func(r *http.Request) (*http.Response, error) { panic("a bug in the transport") })
	gone, leave := context.WithCancel(t.Context())
	stalls := roundTrip(func(r *http.Request) (*http.Response, error) {
		r.Body.Close()
		pr, pw := io.Pipe()
		go func() { //qenv:ok a test's provider
			pw.Write(anthropicStream(3)[0])
			leave() // the caller goes after the first event
			<-r.Context().Done()
			pw.CloseWithError(r.Context().Err())
		}()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: pr}, nil
	})
	for _, tc := range []struct {
		name    string
		ctx     context.Context
		rt      http.RoundTripper
		end     string
		charged bool // a cost above zero
		panics  bool
	}{
		{"a whole call", t.Context(), ok(), "complete", true, false},
		{"a cut stream", t.Context(), cutAfter{anthropicStream(5)[:4], 200}, "cancel", true, false},
		{"a dial failure", t.Context(), dial, "complete", false, false},
		{"an uncertain transport error", t.Context(), reset, "cancel", true, false},
		{"a provider error", t.Context(), providerError, "complete", false, false},
		{"the caller gone mid-stream", gone, stalls, "cancel", true, false},
		{"a panic after admission", t.Context(), panics, "cancel", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &admitter{t: t}
			s := serve(tc.ctx, tc.rt, a)
			if (s.panicV != nil) != tc.panics {
				t.Fatalf("panic %v, want one: %v", s.panicV, tc.panics)
			}
			if len(a.calls) != 1 || len(a.calls[0].ends) != 1 || a.calls[0].ends[0] != tc.end {
				t.Fatalf("ends %+v, want one %s", a.calls, tc.end)
			}
			acc := a.calls[0]
			if (acc.cost > 0) != tc.charged {
				t.Fatalf("cost %d, charged %v", acc.cost, tc.charged)
			}
			if tc.end == "cancel" && !acc.until.After(time.Now()) {
				t.Fatalf("cancelled with no tail to hold its slot to (%v)", acc.until)
			}
			if c := acc.call; c.Principal != "alice" || c.Scope != "team-a" || c.Alias != "claude" || c.Deployment == "" || c.Model == "" {
				t.Fatalf("admitted as %+v: the route chosen is the admitter's to see", c)
			}
		})
	}
}
