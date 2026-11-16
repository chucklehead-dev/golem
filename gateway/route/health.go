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
// Ejection is per gateway, as the attempts model says: each gateway probes an
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
	inflight int
	// byPrincipal counts each principal's holds, for the deployment's
	// per-principal ceiling (Target.MaxPerPrincipal); nil until one is set.
	byPrincipal map[string]int
	fails       int
	until       time.Time // ejected until; zero: not ejected
	probing     bool      // the one probe of an ejected deployment is in flight
	ejections   int       // in a row, for the next ejection's length
	// readmissions counts the deployment's readmissions: a probe taken before the latest is an
	// ordinary attempt now (its deployment no longer ejected), so its
	// verdict is not a probe's.
	readmissions int
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

// Hold is one attempt's hold on its deployment: its place under the
// ceiling, and the probe, if it is one. Exactly one of Commit and Fail
// reports how the attempt went; Release ends its hold when the attempt is
// over (a committed one's stream ended).
type Hold struct {
	h           *Health
	d           *deployment
	probe       bool
	readmission int    // d.readmissions when the hold was taken
	principal   string // counted in d.byPrincipal, if the deployment has a per-principal ceiling
	done        bool   // Commit or Fail has reported
	freed       bool
}

// Refused is why Acquire refused an attempt: the deployment's state, or
// the principal's own share of it.
type Refused int

const (
	// NotRefused: a hold was taken.
	NotRefused Refused = iota
	// Ejected: the deployment rests after failures, or its one probe is
	// in flight.
	Ejected
	// AtCeiling: the deployment holds its ceiling's attempts.
	AtCeiling
	// AtPrincipalCeiling: the principal holds its own share of the
	// deployment (Target.MaxPerPrincipal); others' attempts are taken.
	AtPrincipalCeiling
)

// Acquire takes a place on t for one attempt of principal's, unless t is
// at its ceiling or principal at its per-principal one (a shared box, ADR
// 0017: one principal must not fill it), or t is ejected (an ejected
// deployment past its ejection takes one probe at a time). It reports,
// when it refuses, when t may take one again (zero: unknown, a ceiling
// frees when an attempt ends). AcquireWhy says why, too.
func (h *Health) Acquire(t *Target, principal string) (*Hold, time.Time) {
	l, at, _ := h.AcquireWhy(t, principal)
	return l, at
}

// AcquireWhy is Acquire, with why it refused: a principal at its own
// per-principal ceiling is told so, not that the deployment is down.
func (h *Health) AcquireWhy(t *Target, principal string) (*Hold, time.Time, Refused) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.dep(t)
	probe := false
	if !d.until.IsZero() {
		switch now := h.now(); {
		case now.Before(d.until):
			return nil, d.until, Ejected
		case d.probing:
			return nil, now.Add(cmp.Or(h.Ejection, 10*time.Second)), Ejected
		}
		probe = true
	}
	if t.MaxInFlight > 0 && d.inflight >= t.MaxInFlight {
		return nil, time.Time{}, AtCeiling
	}
	l := &Hold{h: h, d: d, probe: probe, readmission: d.readmissions}
	if t.MaxPerPrincipal > 0 {
		if d.byPrincipal[principal] >= t.MaxPerPrincipal {
			return nil, time.Time{}, AtPrincipalCeiling
		}
		if d.byPrincipal == nil {
			d.byPrincipal = map[string]int{}
		}
		d.byPrincipal[principal]++
		l.principal = principal
	}
	d.inflight++
	d.probing = d.probing || probe
	return l, time.Time{}, NotRefused
}

// isProbe reports whether the hold is its deployment's probe still: one
// taken as a probe, before no readmission since.
func (l *Hold) isProbe() bool { return l.probe && l.readmission == l.d.readmissions }

// Commit reports the attempt committed: the deployment answered, so its
// failures are forgotten and, if ejected, it is readmitted, which ends any
// probe in flight (an attempt sent before the ejection may answer first).
func (l *Hold) Commit() {
	l.h.mu.Lock()
	defer l.h.mu.Unlock()
	if l.done {
		return
	}
	l.done = true
	l.d.readmit()
}

// readmit forgets d's failures and ends its ejection and any probe.
func (d *deployment) readmit() {
	if !d.until.IsZero() {
		d.readmissions++
	}
	d.fails, d.until, d.ejections, d.probing = 0, time.Time{}, 0, false
}

// eject ejects d from now: for its next ejection's length (doubling each
// time in a row, up to MaxEjection), or for wait if that is longer.
func (h *Health) eject(d *deployment, now time.Time, wait time.Duration) {
	e := cmp.Or(h.Ejection, 10*time.Second) << min(d.ejections, 10)
	e = min(e, cmp.Or(h.MaxEjection, 5*time.Minute))
	d.until = now.Add(max(e, wait))
	d.ejections++
	d.fails = 0
}

// Fail reports the attempt failed before it committed, the provider's
// Retry-After with it if it gave one, and releases the hold. Enough
// failures in a row, or a failed probe, eject the deployment.
func (l *Hold) Fail(retryAfter time.Duration) {
	l.h.mu.Lock()
	defer l.h.mu.Unlock()
	if !l.done {
		l.done = true
		d := l.d
		d.fails++
		probe := l.isProbe()
		if probe {
			d.probing = false
		}
		if probe || d.fails >= cmp.Or(l.h.EjectAfter, 3) {
			l.h.eject(d, l.h.now(), retryAfter)
		}
	}
	l.release()
}

// Release ends the hold on its deployment's ceiling. A hold that
// reported neither Commit nor Fail (the caller left first) frees its probe
// too, leaving the deployment as it was.
func (l *Hold) Release() {
	l.h.mu.Lock()
	defer l.h.mu.Unlock()
	if !l.done && l.isProbe() {
		l.d.probing = false
	}
	l.done = true
	l.release()
}

func (l *Hold) release() {
	if !l.freed {
		l.freed = true
		l.d.inflight--
		if l.d.byPrincipal != nil {
			if n := l.d.byPrincipal[l.principal] - 1; n > 0 {
				l.d.byPrincipal[l.principal] = n
			} else {
				delete(l.d.byPrincipal, l.principal)
			}
		}
	}
}

// Checked reports an active check of t (a Checker's): a failed one ejects
// t, as a failed probe does, unless it is ejected still; a passing one
// readmits t if its ejection is over, as a probe's commit does. A pass
// does not cut an ejection short: a box that lists its models may still
// be refusing calls (a Retry-After), and a pass forgets no failures of
// calls, which a liveness check cannot speak for.
func (h *Health) Checked(t *Target, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d, now := h.dep(t), h.now()
	ejected := !d.until.IsZero()
	switch {
	case ok && ejected && !now.Before(d.until):
		d.readmit()
	case !ok && (!ejected || !now.Before(d.until)):
		h.eject(d, now, 0)
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
	InFlight int  // attempts holding the deployment
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
