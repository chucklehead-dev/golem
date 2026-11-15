package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chucklehead-dev/golem/gateway/proxy"
	"github.com/chucklehead-dev/golem/lib/contract"
)

// lifecycle is the gateway's shutdown, in the order that loses no
// accounting: readiness fails first (the mesh stops sending), calls in
// flight drain, then those still running are cut and their accounting
// (under context.WithoutCancel) is waited for. Only then may the limiter's
// and the telemetry's last work run: a call ended by the process exiting
// would have no ledger row and no counter add.
type lifecycle struct {
	srv      *http.Server
	draining atomic.Bool
	// The calls in flight. Not a WaitGroup: after Close a handler whose
	// connection was accepted can still start while the wait runs, an Add
	// at zero concurrent with Wait. Once the wait has begun (closed), a
	// call that starts is refused rather than counted after it.
	mu       sync.Mutex
	inflight int
	closed   bool
	idle     chan struct{} // closed when closed and none is in flight
	// unready is how long /readyz fails before the server stops taking
	// connections, for the endpoints to follow; drain, how long calls in
	// flight may finish; cut, how long cut calls have to account.
	unready, drain, cut time.Duration
}

// track counts h's calls in flight. A call refused once the wait has
// begun is answered in h's dialect, d, as every refusal is (contracts.md
// §1): an SDK parses it and retries after its Retry-After, reaching
// another of the pool's gateways.
func (lc *lifecycle) track(d proxy.Dialect, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !lc.begin() {
			proxy.Refuse(w, d, contract.GatewayNotReady, "this gateway is shutting down: retry the call, and another of the pool's gateways will take it")
			return
		}
		defer lc.end()
		h.ServeHTTP(w, r)
	})
}

// begin counts a call in, unless the wait has begun.
func (lc *lifecycle) begin() bool {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if lc.closed {
		return false
	}
	lc.inflight++
	return true
}

// end counts a call out, ending the wait if it was the last.
func (lc *lifecycle) end() {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if lc.inflight--; lc.inflight == 0 && lc.closed {
		close(lc.idle)
	}
}

// wait stops counting new calls and returns a channel closed when the
// last call in flight has ended.
func (lc *lifecycle) wait() <-chan struct{} {
	lc.mu.Lock()
	defer lc.mu.Unlock()
	if !lc.closed {
		lc.closed, lc.idle = true, make(chan struct{})
		if lc.inflight == 0 {
			close(lc.idle)
		}
	}
	return lc.idle
}

// readyz fails while draining, else as check says.
func (lc *lifecycle) readyz(check func() error) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if lc.draining.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		if err := check(); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	}
}

// shutdown runs the sequence; it returns when every call has accounted, or
// the cut's time is up.
func (lc *lifecycle) shutdown() {
	lc.draining.Store(true)
	time.Sleep(lc.unready)
	drain, cancel := context.WithTimeout(context.Background(), lc.drain)
	defer cancel()
	if err := lc.srv.Shutdown(drain); err != nil {
		// Closing the connections fails the calls' writes and ends their
		// contexts: each accounts as a cancellation (ADR 0041 §7.6).
		slog.Warn("golem-gateway: cutting the calls still in flight at the drain deadline", "err", err)
		lc.srv.Close()
	}
	select {
	case <-lc.wait():
	case <-time.After(lc.cut):
		slog.Error("golem-gateway: calls had not accounted when the cut's time ran out; their rows are lost", "after", lc.cut)
	}
}
