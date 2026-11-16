package route

import (
	"context"
	"io"
	"maps"
	"net/http"
	"sync"
	"time"
)

// Checker checks the deployments that have a health check (Target's
// HealthPath; a local box, ADR 0017) and reports each result to Health,
// so a box that is down is ejected before a call finds it, and its route
// fails over at once, and one that answers again is readmitted without a
// caller's call as its probe. Each gateway checks on its own, as each
// ejects on its own (model/attempts.md).
type Checker struct {
	Health *Health
	// Timeout bounds one check (default 5 s, at most its interval).
	Timeout time.Duration
	// Now is the clock (tests set it).
	Now func() time.Time

	mu       sync.Mutex
	due      map[string]time.Time // by Target.Key: the next check
	checking map[string]bool
}

func (c *Checker) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Run checks targets' deployments every second for those due, until ctx
// ends. targets is read each time: a new snapshot's deployments are
// checked from its next tick.
func (c *Checker) Run(ctx context.Context, targets func() []*Target) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		c.Check(ctx, targets())
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Check checks, in parallel, each of ts that has a health check, is due,
// and has no check in flight, and returns when those checks have ended.
func (c *Checker) Check(ctx context.Context, ts []*Target) {
	var wg sync.WaitGroup
	now := c.now()
	c.mu.Lock()
	if c.due == nil {
		c.due, c.checking = map[string]time.Time{}, map[string]bool{}
	}
	for _, t := range ts {
		k := t.Key()
		if t.HealthPath == "" || c.checking[k] || now.Before(c.due[k]) {
			continue
		}
		c.checking[k] = true
		c.due[k] = now.Add(t.HealthInterval)
		wg.Go(func() {
			ok := c.check(ctx, t)
			if ctx.Err() == nil { // a check cut short by shutdown says nothing
				c.Health.Checked(t, ok)
			}
			c.mu.Lock()
			delete(c.checking, k)
			c.mu.Unlock()
		})
	}
	c.mu.Unlock()
	wg.Wait()
}

// check GETs t's health path with its credential; a 2xx passes.
func (c *Checker) check(ctx context.Context, t *Target) bool {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	if t.HealthInterval > 0 {
		timeout = min(timeout, t.HealthInterval)
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u := *t.URL
	u.Path, u.RawPath, u.RawQuery, u.Fragment = t.HealthPath, "", "", ""
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false
	}
	maps.Copy(req.Header, t.Header)
	rt := t.Transport
	if rt == nil {
		rt = http.DefaultTransport
	}
	resp, err := rt.RoundTrip(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode/100 == 2
}
