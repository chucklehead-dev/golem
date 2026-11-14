package proxy

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/gateway/limit"
	"github.com/chucklehead-dev/golem/lib/key"
)

// accounts is an Admitter double: it refuses with refuse, if set, and
// records how each admitted call ended.
type accounts struct {
	refuse error
	mu     sync.Mutex
	calls  []*account
}

type account struct {
	call              limit.Call
	completed, cancel int
	cost              int64
	endAt             time.Time
}

func (a *accounts) Admit(_ context.Context, c limit.Call) (Ticket, error) {
	if a.refuse != nil {
		return nil, a.refuse
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	t := &account{call: c}
	a.calls = append(a.calls, t)
	return t, nil
}

func (t *account) Complete(_ context.Context, cost int64) { t.completed++; t.cost = cost }
func (t *account) Cancel(_ context.Context, seen int64)   { t.cancel++; t.cost = seen }
func (t *account) EndAt(at time.Time)                     { t.endAt = at }
func (t *account) Period() string                         { return "2026-11" }
func (t *account) Waived() bool                           { return false }

// TestAdmitterContract: the request path depends only on Admitter. A
// refusal is rendered by its kind, and every admitted call ends in exactly
// one of Complete (with its cost) or Cancel (with the cost seen, its slot
// held to the provider's tail).
func TestAdmitterContract(t *testing.T) {
	alice := key.New(key.Dev)
	cfg := `apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  scopes: [{name: team-a, models: [claude]}]
  keys: [{key: ` + alice + `, principal: alice, scope: team-a}]
`
	serve := func(rt http.RoundTripper, a Admitter) int {
		h, _ := standaloneHandler(t, cfg, rt)
		h.Limiter = a
		r := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(smallRequest))
		r.Header.Set("X-Api-Key", alice)
		w := httptest.NewRecorder()
		func() {
			defer func() { recover() }() // a cut response aborts the handler
			h.ServeHTTP(w, r)
		}()
		return w.Code
	}
	for err, want := range map[error]int{limit.ErrBudget: http.StatusPaymentRequired, limit.ErrConcurrency: http.StatusTooManyRequests} {
		if code := serve(&stub{events: anthropicStream(3)}, &accounts{refuse: err}); code != want {
			t.Errorf("%v: status %d, want %d", err, code, want)
		}
	}
	whole := &accounts{}
	serve(&stub{events: anthropicStream(3)}, whole)
	if len(whole.calls) != 1 || whole.calls[0].completed != 1 || whole.calls[0].cancel != 0 || whole.calls[0].cost <= 0 {
		t.Fatalf("a whole call: %+v, want one Complete with its cost", whole.calls)
	}
	if c := whole.calls[0].call; c.Principal != "alice" || c.Scope != "team-a" || c.ID == "" {
		t.Fatalf("admitted as %+v", c)
	}
	cut := &accounts{}
	serve(cutAfter{anthropicStream(5)[:4], 200}, cut)
	if len(cut.calls) != 1 || cut.calls[0].cancel != 1 || cut.calls[0].completed != 0 || cut.calls[0].endAt.IsZero() {
		t.Fatalf("a cut call: %+v, want one Cancel and a tail", cut.calls)
	}
}
