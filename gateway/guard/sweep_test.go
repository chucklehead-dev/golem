package guard

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"
)

// lockProbe is a log handler that records whether the guard's lock was
// free while the record was written.
type lockProbe struct {
	g         *Guard
	logged    int
	underLock bool
	slog.Handler
}

func (p *lockProbe) Enabled(context.Context, slog.Level) bool { return true }
func (p *lockProbe) Handle(context.Context, slog.Record) error {
	p.logged++
	if p.g.mu.TryLock() {
		p.g.mu.Unlock()
	} else {
		p.underLock = true
	}
	return nil
}

// TestSweepBounded: a sweep of a full table looks at no more than its
// bound of sources (the request path waits on it, under the guard's lock),
// and the overflow's log line is written once the lock is released.
func TestSweepBounded(t *testing.T) {
	now := time.Unix(1000, 0)
	probe := &lockProbe{}
	g := &Guard{FailRate: 1, FailBurst: 1, MaxSources: 100, Now: func() time.Time { return now }, sweepMax: 10}
	probe.g, g.Log = g, slog.New(probe)
	for i := range 100 {
		g.Failed(fmt.Sprint("s", i))
	}
	g.Failed("over") // the table full and none refilled: unlimited, and logged
	if probe.logged != 1 || probe.underLock {
		t.Fatalf("logged %d, under the lock %v; want one line, outside the lock", probe.logged, probe.underLock)
	}
	now = now.Add(2 * time.Second) // every kept source refilled
	g.Failed("new")
	if n := len(g.sources); n != 100-10+1 {
		t.Fatalf("%d sources after one sweep: want 10 of 100 forgotten, the sweep bounded, and the new one kept", n)
	}
}
