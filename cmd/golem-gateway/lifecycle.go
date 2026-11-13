package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// lifecycle is the gateway's shutdown, in the order that loses no
// accounting: readiness fails first (the mesh stops sending), calls in
// flight drain, then those still running are cut and their accounting
// (under context.WithoutCancel) is waited for. Only then may the limiter's
// and the telemetry's last work run: a call ended by the process exiting
// would have no ledger row and no counter add.
type lifecycle struct {
	srv      *http.Server
	inflight sync.WaitGroup
	draining atomic.Bool
	// unready is how long /readyz fails before the server stops taking
	// connections, for the endpoints to follow; drain, how long calls in
	// flight may finish; cut, how long cut calls have to account.
	unready, drain, cut time.Duration
}

// track counts h's calls in flight.
func (lc *lifecycle) track(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lc.inflight.Add(1)
		defer lc.inflight.Done()
		h.ServeHTTP(w, r)
	})
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
	done := make(chan struct{})
	go func() { // ends when the last call does; the process exits after
		lc.inflight.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(lc.cut):
		slog.Error("golem-gateway: calls had not accounted when the cut's time ran out; their rows are lost", "after", lc.cut)
	}
}
