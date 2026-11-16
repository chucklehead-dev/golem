package controller

import (
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/control/spend"
	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/bucket"
	"github.com/chucklehead-dev/golem/lib/key"
)

// silentLedger answers its first query, then never again: a ClickHouse
// that accepts queries and does not answer.
type silentLedger struct{ calls atomic.Int64 }

func (l *silentLedger) Spend(ctx context.Context, _ string, _ time.Time) (spend.Totals, error) {
	if l.calls.Add(1) == 1 {
		return spend.Totals{Scopes: map[string]int64{"team-a": 1}}, nil
	}
	<-ctx.Done()
	return spend.Totals{}, ctx.Err()
}

// TestRevocationNotBehindSpend: while known spend's ledger hangs, a
// revocation is still published within seconds: publication does not wait
// on the spend job (nor on the audit or the sync).
//
//quint:checks revocationBounded
func TestRevocationNotBehindSpend(t *testing.T) {
	w := newWorld(t)
	r1 := w.replicas["r1"]
	ledger := &silentLedger{}
	r1.Spend = &spend.Job{Ledger: ledger, Now: r1.Now}
	r1.SpendEvery = 10 * time.Millisecond
	alice := key.New(key.Live)
	h := key.Hash(alice)
	hash := hex.EncodeToString(h[:])
	w.policy("team-a", "{models: [claude]}")
	if _, err := r1.State.Update(t.Context(), func(d *state.Document) error {
		d.Scopes = map[string]state.Scope{"team-a": {}}
		d.Principals = map[string]state.Principal{"alice": {Kind: "user", Scopes: []string{"team-a"}}}
		d.Keys = map[string]state.Key{hash: {Display: key.Display(alice), Owner: "alice", Scope: "team-a"}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- r1.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Run did not stop its jobs and return")
		}
	}()
	refused := func() string {
		if err := w.gateway.Poll(t.Context()); err != nil && !errors.Is(err, bucket.ErrNotFound) {
			t.Fatalf("gateway poll: %v", err)
		}
		c, err := w.gateway.Holder.Current()
		if err != nil {
			return "none"
		}
		if _, no := c.Auth.Authenticate(http.Header{"X-Api-Key": {alice}}); no != nil {
			return no.Code.Name
		}
		return ""
	}
	waitFor := func(what string, ok func() bool) {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); !ok(); time.Sleep(20 * time.Millisecond) { // Run's loop: another goroutine's
			if time.Now().After(deadline) {
				t.Fatal(what)
			}
		}
	}
	waitFor("the first publication did not reach the gateway", func() bool { return refused() == "" })
	waitFor("the spend job never reached its hanging query", func() bool { return ledger.calls.Load() >= 2 })
	if _, err := r1.State.Update(t.Context(), func(d *state.Document) error { return d.Revoke(hash, r1.clock) }); err != nil {
		t.Fatal(err)
	}
	waitFor("the revocation was not published while the spend ledger hung", func() bool { return refused() == "auth.key_revoked" })
}
