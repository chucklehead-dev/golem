package route

import (
	"cmp"
	"sync"
	"time"
)

// Health is one gateway's view of its deployments and its routes' retries.
// It tracks each deployment's attempts in flight, its consecutive failures
// and its ejection, and each route's retry budget. model/attempts.md is its
// model.
//
// Health is kept across snapshots, by Target.Key and alias, so a config
// change neither readmits an ejected deployment nor refills a budget. It is
// safe for concurrent use, and its zero value is ready with the defaults.
//
// Ejection is per gateway. Each gateway probes an ejected deployment on its
// own, at most one probe at a time.
type Health struct {
	// EjectAfter is how many consecutive failures eject a deployment
	// (default 3). A failed probe ejects it again at once.
	EjectAfter int
	// Ejection is the length of a deployment's first ejection (default
	// 10 s). Each ejection in a row doubles it, up to MaxEjection (default
	// 5 min). A longer Retry-After from the provider is taken instead.
	Ejection, MaxEjection time.Duration
	// RetryRatio is the share of a route's calls that may be retried
	// (default 0.2). Retries cannot then multiply the load on a failing
	// provider.
	RetryRatio float64
	// RetryBurst is how many retries a quiet route may make at once
	// (default 10).
	RetryBurst float64
	// Now is the clock. Tests set it.
	Now func() time.Time

	mu      sync.Mutex
	deps    map[string]*deployment
	budgets map[string]*float64 // retry tokens by alias
}

// deployment is Health's state for one deployment.

type deployment struct {
	inflight int
	// byPrincipal counts each principal's holds, for the per-principal
	// ceiling (Target.MaxPerPrincipal). It is nil until a ceiling is set.
	byPrincipal map[string]int
	fails       int
	until       time.Time // the end of the ejection; zero when not ejected
	probing     bool      // the one probe of an ejected deployment is in flight
	ejections   int       // ejections in a row, for the next one's length
	// readmissions counts the deployment's readmissions. A probe taken
	// before the latest readmission is an ordinary attempt now, so its
	// result is not a probe's.
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

// Hold is one attempt's place under its deployment's ceiling, and its
// probe if it is one. Commit or Fail, never both, reports how the attempt
// went. Release ends the hold when the attempt is over, as when a
// committed stream ends.
type Hold struct {
	h           *Health
	d           *deployment
	probe       bool
	readmission int    // d.readmissions when the hold was taken
	principal   string // counted in d.byPrincipal, if the deployment has a per-principal ceiling
	done        bool   // Commit or Fail has reported
	freed       bool
}

// Refused is why Acquire refused an attempt. It is the deployment's state,
// or the principal's own share of it.
type Refused int

const (
	// NotRefused means a hold was taken.
	NotRefused Refused = iota
	// Ejected means the deployment rests after failures, or its one probe
	// is in flight.
	Ejected
	// AtCeiling means the deployment holds as many attempts as its ceiling.
	AtCeiling
	// AtPrincipalCeiling means the principal holds its own share of the
	// deployment (Target.MaxPerPrincipal). Other principals are still taken.
	AtPrincipalCeiling
)

// Acquire takes a hold on a deployment for one of a principal's attempts.
// It refuses when the deployment is at its ceiling, the principal at its
// share, or the deployment ejected. Past its ejection, a deployment takes
// one probe at a time.
//
// When it refuses, it reports when the deployment may take an attempt
// again. Zero means unknown, as for a ceiling, which frees when an attempt
// ends. AcquireWhy also says why. ADR 0017.
func (h *Health) Acquire(t *Target, principal string) (*Hold, time.Time) {
	l, at, _ := h.AcquireWhy(t, principal)
	return l, at
}

// AcquireWhy is Acquire, and also says why it refused. A principal at its
// own share can then be told so, rather than that the deployment is down.
func (h *Health) AcquireWhy(t *Target, principal string) (*Hold, time.Time, Refused) {
	h.mu.Lock()
	defer h.mu.Unlock()
	d := h.dep(t)
	probe := false
	if !d.until.IsZero() {
		switch now := h.now(); {
		case now.Before(d.until):
			return nil, d.until, Ejected
		//quint:enforces attempts.probeOnly
		case d.probing:
			return nil, now.Add(cmp.Or(h.Ejection, 10*time.Second)), Ejected
		}
		probe = true
	}
	//quint:enforces attempts.quotaHonoured
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

// isProbe reports whether the hold is still its deployment's probe. It is
// if it was taken as a probe and the deployment has not been readmitted
// since.
func (l *Hold) isProbe() bool { return l.probe && l.readmission == l.d.readmissions }

// Commit reports that the attempt committed. The deployment answered, so
// its failures are forgotten and an ejection ends. Readmission also ends a
// probe in flight, since an attempt sent before the ejection may answer
// first.
func (l *Hold) Commit() {
	l.h.mu.Lock()
	defer l.h.mu.Unlock()
	if l.done {
		return
	}
	l.done = true
	l.d.readmit()
}

// readmit forgets the deployment's failures and ends its ejection and any
// probe.
func (d *deployment) readmit() {
	if !d.until.IsZero() {
		d.readmissions++
	}
	d.fails, d.until, d.ejections, d.probing = 0, time.Time{}, 0, false
}

// eject ejects the deployment from now, for its next ejection's length or
// for wait if that is longer. Each ejection in a row doubles the length,
// up to MaxEjection.
func (h *Health) eject(d *deployment, now time.Time, wait time.Duration) {
	e := cmp.Or(h.Ejection, 10*time.Second) << min(d.ejections, 10)
	e = min(e, cmp.Or(h.MaxEjection, 5*time.Minute))
	d.until = now.Add(max(e, wait))
	d.ejections++
	d.fails = 0
}

// Fail reports that the attempt failed before it committed, with the
// provider's Retry-After if it gave one, and releases the hold. Enough
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

// Release ends the hold. A hold that reported neither Commit nor Fail,
// because its caller left first, also frees its probe and leaves the
// deployment as it was.
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

// Checked reports a Checker's active check of a deployment. A failed check
// ejects it as a failed probe does, unless it is still ejected. A passing
// check readmits it once its ejection is over, as a probe's commit does.
//
// A pass never cuts an ejection short. A box that lists its models may
// still refuse calls with a Retry-After. A liveness check cannot speak for
// calls' failures.
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

// Called credits a route's retry budget for one call, with RetryRatio of a
// retry, up to RetryBurst.
func (h *Health) Called(alias string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	b := h.budget(alias)
	*b = min(*b+cmp.Or(h.RetryRatio, 0.2), cmp.Or(h.RetryBurst, 10))
}

// Retry takes one retry from a route's budget, reporting false if it has
// none left.
//
//quint:enforces attempts.retriesCapped
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

// DeploymentHealth is what Health knows of a deployment, for metrics and
// tests.
type DeploymentHealth struct {
	InFlight int  // attempts holding the deployment
	Ejected  bool // until a commit or a check readmits it
	Probing  bool // its one probe is in flight
}

// Deployment reports a deployment's health.
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
