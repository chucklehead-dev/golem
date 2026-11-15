package route

import (
	"cmp"
	"sync"
	"time"
)

// Health is one gateway's view of its deployments and its routes' retries
// (model/attempts.md): each deployment's attempts in flight against its
// ceiling, its consecutive failures and its ejection, and each route's
// retry budget. It is kept across snapshots, by deployment identity
// (Target.Key) and alias, so a configuration change neither readmits an
// ejected deployment nor refills a budget. Safe for concurrent use; the
// zero value is ready, with the defaults below.
//
// Ejection is per gateway, as the model's §9 says: each gateway probes an
// ejected deployment on its own, at most one probe at a time.
type Health struct {
	// EjectAfter is how many consecutive failures eject a deployment
	// (default 3). A failed probe ejects it again at once.
	EjectAfter int
	// Ejection is a deployment's first ejection (default 10 s); each one
	// after doubles it, up to MaxEjection (default 5 min). A provider's
	// Retry-After, if longer, is taken instead.
	Ejection, MaxEjection time.Duration
	// RetryRatio is the share of a route's calls that may be retried
	// (default 0.2), and RetryBurst the retries a quiet route may make at
	// once (default 10): the pool's cap, so retries cannot multiply load on
	// a provider already failing.
	RetryRatio float64
	RetryBurst float64
	// Now is the clock (tests set it).
	Now func() time.Time

	mu      sync.Mutex
	deps    map[string]*deployment
	budgets map[string]*float64 // by alias: retry tokens
}

type deployment struct {
	inflight  int
	fails     int
	until     time.Time // ejected until; zero: not ejected
	probing   bool      // the one probe of an ejected deployment is in flight
	ejections int       // in a row, for the next ejection's length
}

func (h *Health) now() time.Time {
	if h.Now != nil {
		return h.Now()
	}
	return time.Now()
}

func (h *Health) dep(t *Target) *deployment {
	if h.deps == nil {
		h.deps = map[string]*deployment{}
	}
	d := h.deps[t.Key()]
	if d == nil {
		d = &deployment{}
		h.deps[t.Key()] = d
	}
	return d
}

// Lease is one attempt's hold on its deployment: its place under the
// ceiling, and the probe, if it is one. Exactly one of Commit and Fail
// reports how the attempt went; Release ends its hold when the attempt is
// over (a committed one's stream ended).
type Lease struct {
	h     *Health
	d     *deployment
	probe bool
	done  bool // Commit or Fail has reported
	freed bool
}

// Acquire takes a place on t for one attempt, unless t is at its ceiling,
// or ejected (an ejected deployment past its ejection takes one probe at a
// time). It reports, when it refuses, when t may take one again (zero:
// unknown, a ceiling frees when an attempt ends).
func (h *Health) Acquire(t *Target) (*Lease, time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.dep(t)
	probe := false
	if !d.until.IsZero() {
		switch now := h.now(); {
		case now.Before(d.until):
			return nil, d.until
		case d.probing:
			return nil, now.Add(cmp.Or(h.Ejection, 10*time.Second))
		}
		probe = true
	}
	if t.MaxInFlight > 0 && d.inflight >= t.MaxInFlight {
		return nil, time.Time{}
	}
	d.inflight++
	d.probing = d.probing || probe
	return &Lease{h: h, d: d, probe: probe}, time.Time{}
}

// Commit reports the attempt committed: the deployment answered, so its
// failures are forgotten and, if ejected, it is readmitted.
func (l *Lease) Commit() {
	l.h.mu.Lock()
	defer l.h.mu.Unlock()
	if l.done {
		return
	}
	l.done = true
	l.d.fails, l.d.until, l.d.ejections = 0, time.Time{}, 0
	if l.probe {
		l.d.probing = false
	}
}

// Fail reports the attempt failed before it committed, the provider's
// Retry-After with it if it gave one, and releases the lease. Enough
// failures in a row, or a failed probe, eject the deployment.
func (l *Lease) Fail(retryAfter time.Duration) {
	l.h.mu.Lock()
	defer l.h.mu.Unlock()
	if !l.done {
		l.done = true
		d := l.d
		d.fails++
		if l.probe {
			d.probing = false
		}
		if l.probe || d.fails >= cmp.Or(l.h.EjectAfter, 3) {
			e := cmp.Or(l.h.Ejection, 10*time.Second) << min(d.ejections, 10)
			e = min(e, cmp.Or(l.h.MaxEjection, 5*time.Minute))
			d.until = l.h.now().Add(max(e, retryAfter))
			d.ejections++
			d.fails = 0
		}
	}
	l.release()
}

// Release ends the lease's hold on its deployment's ceiling. A lease that
// reported neither Commit nor Fail (the caller left first) frees its probe
// too, leaving the deployment as it was.
func (l *Lease) Release() {
	l.h.mu.Lock()
	defer l.h.mu.Unlock()
	if !l.done && l.probe {
		l.d.probing = false
	}
	l.done = true
	l.release()
}

func (l *Lease) release() {
	if !l.freed {
		l.freed = true
		l.d.inflight--
	}
}

// Called credits a route's retry budget for one call: RetryRatio of a
// retry, up to RetryBurst.
func (h *Health) Called(alias string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.budget(alias)
	*b = min(*b+cmp.Or(h.RetryRatio, 0.2), cmp.Or(h.RetryBurst, 10))
}

// Retry takes one retry from a route's budget, reporting false if it has
// none left.
func (h *Health) Retry(alias string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.budget(alias)
	if *b < 1 {
		return false
	}
	*b--
	return true
}

func (h *Health) budget(alias string) *float64 {
	if h.budgets == nil {
		h.budgets = map[string]*float64{}
	}
	b := h.budgets[alias]
	if b == nil {
		v := cmp.Or(h.RetryBurst, 10)
		b = &v
		h.budgets[alias] = b
	}
	return b
}

// DeploymentHealth is what Health knows of a deployment, for its metrics
// and its tests.
type DeploymentHealth struct {
	InFlight int  // attempts holding a lease
	Ejected  bool // until a commit readmits it
	Probing  bool // its one probe is in flight
}

// Deployment reports t's health.
func (h *Health) Deployment(t *Target) DeploymentHealth {
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.dep(t)
	return DeploymentHealth{InFlight: d.inflight, Ejected: !d.until.IsZero(), Probing: d.probing}
}

// Retries reports the retries a route's budget holds.
func (h *Health) Retries(alias string) float64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return *h.budget(alias)
}
