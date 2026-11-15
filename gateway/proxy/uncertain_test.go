package proxy

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/gateway/limit"
	"github.com/chucklehead-dev/golem/gateway/telemetry"
	"github.com/chucklehead-dev/golem/lib/key"
)

// TestUncertainUpstream: absent response bytes are not absent work
// (golem#4). Only a failure the provider cannot have seen is free, frees
// its slot at once, and is retried (the route's two attempts: two rows,
// the second the call's end); otherwise the call is charged an estimate,
// its slot held for the provider's tail, and it is not retried
// (model/attempts.md: an uncertain attempt ends the call).
func TestUncertainUpstream(t *testing.T) {
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
	body := `{"model":"claude","max_tokens":100,"stream":true,"messages":[]}`
	for _, tc := range []struct {
		name     string
		err      error
		cancel   bool
		outcome  string
		charged  bool
		heldSlot bool
		rows     int           // its attempts: a free failure is retried once
		headers  time.Duration // the route's first-byte timeout, if the provider stays silent
	}{
		{"the dial failed", &net.OpError{Op: "dial", Err: errors.New("connection refused")}, false, "failed", false, false, 2, 0},
		{"the name did not resolve", &net.DNSError{Err: "no such host", Name: "provider.example"}, false, "failed", false, false, 2, 0},
		{"reset after the request went out", errors.New("read: connection reset by peer"), false, "uncertain", true, true, 1, 0},
		{"the caller left before the headers", context.Canceled, true, "cancelled", true, true, 1, 0},
		{"the headers timed out", nil, false, "uncertain", true, true, 1, 50 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			h, _ := standaloneHandler(t, cfg, roundTrip(func(r *http.Request) (*http.Response, error) {
				r.Body.Close()
				if tc.cancel {
					cancel()
				}
				if tc.headers > 0 { // the request went out; the provider says nothing
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				return nil, tc.err
			}))
			if tc.headers > 0 {
				c, _ := h.Snapshots.Current()
				c.Routes.Lookup([]byte("claude")).Headers = tc.headers
			}
			h.ProviderTail = 100 * time.Millisecond
			st := limit.NewMem()
			h.Limiter = limit.New(st).Admitter()
			var rows bytes.Buffer
			tel, _ := telemetry.New(telemetry.Stdout(&rows), "local", "standalone")
			h.Telemetry = tel
			r := httptest.NewRequestWithContext(ctx, "POST", "/v1/messages", strings.NewReader(body))
			r.Header.Set("X-Api-Key", alice)
			h.ServeHTTP(httptest.NewRecorder(), r)

			all := rowsOf(t, &rows)
			if len(all) != tc.rows {
				t.Fatalf("%d rows, want %d: %v", len(all), tc.rows, all)
			}
			row := all[len(all)-1]
			if row["outcome"] != tc.outcome || (row["cost_micros"].(float64) > 0) != tc.charged ||
				(row["estimated_fraction"] == 1.0) != tc.charged {
				t.Fatalf("row %v: want outcome %s, charged %v", row, tc.outcome, tc.charged)
			}
			if held := len(st.Slots()) == 1; held != tc.heldSlot {
				t.Fatalf("slots %v: held %v, want %v", st.Slots(), held, tc.heldSlot)
			}
			if tc.heldSlot {
				deadline := time.Now().Add(5 * time.Second)
				for len(st.Slots()) > 0 && time.Now().Before(deadline) {
					time.Sleep(10 * time.Millisecond) // the tail's timer: real time
				}
				if len(st.Slots()) != 0 {
					t.Fatal("the slot was not freed after the provider's tail")
				}
			}
		})
	}
}
