package limit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valkey-io/valkey-go"
)

// TestStartWithValkeyDown: a store whose Valkey is unreachable as the
// gateway starts is still a store; the limiter starts in its outage mode
// (waivers), stays down while Valkey is unreachable even with nothing
// running, and once Connect has made a client, Recover re-registers the
// running calls' slots in Valkey.
func TestStartWithValkeyDown(t *testing.T) {
	addr := valkeyAddr(t)
	ctx := t.Context()
	var reachable atomic.Bool
	var dials atomic.Int32
	siteN++
	site := fmt.Sprintf("test-%d-%d", os.Getpid(), siteN)
	v, err := DialValkey(site, func() (valkey.Client, error) {
		dials.Add(1)
		if !reachable.Load() {
			return nil, errors.New("dial tcp: connection refused")
		}
		return valkey.NewClient(valkey.ClientOption{InitAddress: []string{addr}, DisableCache: true})
	})
	if err == nil || v == nil || v.Connected() {
		t.Fatalf("DialValkey: %v, %v: an unreachable Valkey is an error and a store without a client", v, err)
	}
	defer v.Close()
	// Connect's waits, without waiting: Valkey answers from the second retry.
	var waits []time.Duration
	v.afterFn = func(d time.Duration) <-chan time.Time {
		if waits = append(waits, d); len(waits) == 2 {
			reachable.Store(true)
		}
		c := make(chan time.Time, 1)
		c <- time.Time{}
		return c
	}
	l := New(v)
	l.MarkDown(err)
	if !l.Down() {
		t.Fatal("MarkDown: the limiter is not down")
	}
	// Nothing running: Recover must still find the store down.
	if err := l.Recover(ctx); !errors.Is(err, ErrUnavailable) || !l.Down() {
		t.Fatalf("Recover with Valkey unreachable: %v, down %v; want ErrUnavailable, still down", err, l.Down())
	}
	tk, err := l.Admit(ctx, scopeCall("a", "p1"))
	if err != nil || !tk.Waived() {
		t.Fatalf("Admit while down: %v; want a waiver", err)
	}
	if err := l.Recover(ctx); !errors.Is(err, ErrUnavailable) || !l.Down() {
		t.Fatalf("Recover before a client: %v, down %v", err, l.Down())
	}

	// Connect retries, backing off: the first retry fails, the next makes
	// the client.
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	v.Connect(cctx)
	if !v.Connected() {
		t.Fatal("Connect returned without a client")
	}
	if n := dials.Load(); n != 3 || len(waits) != 2 || waits[0] != time.Second || waits[1] != 2*time.Second {
		t.Fatalf("%d dials after waits %v; want the first and two retries, after 1s and 2s", n, waits)
	}
	if err := l.Recover(ctx); err != nil || l.Down() {
		t.Fatalf("Recover with Valkey up: %v, down %v", err, l.Down())
	}
	if got, err := v.ScopeSlots(ctx, "s"); err != nil || len(got) != 1 || got[0] != "a" {
		t.Fatalf("Valkey's slots: %v, %v; want the running call's, re-registered", got, err)
	}
}
