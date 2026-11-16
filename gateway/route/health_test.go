package route

import (
	"testing"
	"time"
)

// TestEjection: EjectAfter failures in a row eject a deployment for its
// ejection; past it, one probe at a time (probeOnly); a probe that
// commits readmits it, one that fails ejects it again for twice as long;
// a provider's longer Retry-After wins; a commit forgets earlier failures.
func TestEjection(t *testing.T) {
	now := time.Unix(1000, 0)
	h := &Health{EjectAfter: 2, Ejection: 10 * time.Second, Now: func() time.Time { return now }}
	d := &Target{Backend: "b", Deployment: "d"}
	fail := func(after time.Duration) {
		t.Helper()
		l, _ := h.Acquire(d, "")
		if l == nil {
			t.Fatal("refused while healthy")
		}
		l.Fail(after)
	}
	fail(0)
	l, _ := h.Acquire(d, "")
	l.Commit() // forgets the first failure
	l.Release()
	fail(0)
	if l, _ := h.Acquire(d, ""); l == nil {
		t.Fatal("ejected after one failure since a commit")
	} else {
		l.Fail(0)
	}
	if l, until := h.Acquire(d, ""); l != nil || !until.Equal(now.Add(10*time.Second)) {
		t.Fatalf("hold %v until %v: two failures in a row eject for 10 s", l, until)
	}
	now = now.Add(10 * time.Second)
	probe, _ := h.Acquire(d, "")
	if probe == nil {
		t.Fatal("no probe after the ejection")
	}
	if l, _ := h.Acquire(d, ""); l != nil {
		t.Fatal("a second attempt while the probe is in flight")
	}
	probe.Fail(0)
	if _, until := h.Acquire(d, ""); !until.Equal(now.Add(20 * time.Second)) {
		t.Fatalf("until %v: a failed probe ejects again, twice as long", until)
	}
	now = now.Add(20 * time.Second)
	probe, _ = h.Acquire(d, "")
	probe.Commit()
	probe.Release()
	if l, _ := h.Acquire(d, ""); l == nil {
		t.Fatal("a committed probe did not readmit the deployment")
	} else {
		l.Release()
	}
	// A provider's Retry-After longer than the ejection is taken.
	fail(time.Minute)
	fail(time.Minute)
	if _, until := h.Acquire(d, ""); !until.Equal(now.Add(time.Minute)) {
		t.Fatalf("until %v: the provider's Retry-After of a minute", until)
	}
}

// TestProbeAbandoned: a probe whose attempt ended without a verdict (its
// caller left) frees the probe, leaving the deployment ejected but
// probeable at once.
func TestProbeAbandoned(t *testing.T) {
	now := time.Unix(1000, 0)
	h := &Health{EjectAfter: 1, Ejection: time.Second, Now: func() time.Time { return now }}
	d := &Target{Backend: "b", Deployment: "d"}
	l, _ := h.Acquire(d, "")
	l.Fail(0)
	now = now.Add(time.Second)
	probe, _ := h.Acquire(d, "")
	probe.Release()
	if l, _ := h.Acquire(d, ""); l == nil {
		t.Fatal("an abandoned probe kept the deployment unprobeable")
	}
}

// TestCeiling: a deployment takes at most MaxInFlight attempts at once, a
// lease's release making room; Release after Fail frees once.
func TestCeiling(t *testing.T) {
	h := &Health{}
	d := &Target{Backend: "b", Deployment: "d", MaxInFlight: 1}
	l, _ := h.Acquire(d, "")
	if x, _ := h.Acquire(d, ""); x != nil {
		t.Fatal("past the ceiling")
	}
	l.Commit()
	l.Release()
	l.Release()
	a, _ := h.Acquire(d, "")
	if a == nil {
		t.Fatal("the release made no room")
	}
	if x, _ := h.Acquire(d, ""); x != nil {
		t.Fatal("a double release freed two places")
	}
}

// TestRetryBudget: a route starts with RetryBurst retries, each call
// credits RetryRatio of one, and the budget never passes the burst.
func TestRetryBudget(t *testing.T) {
	h := &Health{RetryBurst: 2, RetryRatio: 0.5}
	for range 10 {
		h.Called("a") // capped at the burst
	}
	if !h.Retry("a") || !h.Retry("a") || h.Retry("a") {
		t.Fatal("want two retries, then none")
	}
	h.Called("a")
	h.Called("a")
	if !h.Retry("a") || h.Retry("b") == false {
		t.Fatal("two calls credit one retry; another route has its own budget")
	}
}

// TestReadmissionEndsProbes: an attempt sent before the ejection commits
// while the probe is in flight: the deployment is readmitted, the probe is
// an ordinary attempt from then on, and its failure is an ordinary one, not
// a failed probe's ejection (model/attempts.md: a commit readmits and ends
// the probes in flight).
func TestReadmissionEndsProbes(t *testing.T) {
	now := time.Unix(1000, 0)
	h := &Health{EjectAfter: 2, Ejection: time.Second, Now: func() time.Time { return now }}
	d := &Target{Backend: "b", Deployment: "d"}
	early, _ := h.Acquire(d, "") // sent before the ejection
	f1, _ := h.Acquire(d, "")
	f1.Fail(0)
	f2, _ := h.Acquire(d, "")
	f2.Fail(0) // two in a row: ejected
	now = now.Add(time.Second)
	probe, _ := h.Acquire(d, "")
	if probe == nil || !h.Deployment(d).Probing {
		t.Fatal("no probe after the ejection")
	}
	early.Commit()
	if dh := h.Deployment(d); dh.Ejected || dh.Probing {
		t.Fatalf("%+v: a commit readmits and ends the probe", dh)
	}
	probe.Fail(0)
	if dh := h.Deployment(d); dh.Ejected {
		t.Fatalf("%+v: the old probe's failure is one failure, not a failed probe's ejection", dh)
	}
}

// TestPerPrincipalCeiling: a deployment's per-principal ceiling holds one
// principal to its share of a shared box while others still get in, and
// a release makes that principal's room again.
func TestPerPrincipalCeiling(t *testing.T) {
	h := &Health{}
	d := &Target{Backend: "b", Deployment: "box", MaxInFlight: 4, MaxPerPrincipal: 2}
	a1, _ := h.Acquire(d, "alice")
	a2, _ := h.Acquire(d, "alice")
	if x, _, why := h.AcquireWhy(d, "alice"); x != nil || why != AtPrincipalCeiling {
		t.Fatalf("alice past her ceiling (refused %v): want AtPrincipalCeiling", why)
	}
	if b, _ := h.Acquire(d, "bob"); b == nil {
		t.Fatal("bob kept out by alice's calls")
	}
	a1.Commit()
	a1.Release()
	if a3, _ := h.Acquire(d, "alice"); a3 == nil {
		t.Fatal("a release made alice no room")
	}
	a2.Fail(0)
	if h.Deployment(d).InFlight != 2 {
		t.Fatalf("in flight %d: bob's and alice's third", h.Deployment(d).InFlight)
	}
}

// TestAcquireWhy: a refusal says why: the deployment's ejection, its
// ceiling, or the principal's own share (TestPerPrincipalCeiling).
func TestAcquireWhy(t *testing.T) {
	now := time.Unix(1000, 0)
	h := &Health{EjectAfter: 1, Now: func() time.Time { return now }}
	d := &Target{Backend: "b", Deployment: "box", MaxInFlight: 1}
	l, _, why := h.AcquireWhy(d, "alice")
	if l == nil || why != NotRefused {
		t.Fatalf("refused %v on an idle deployment", why)
	}
	if _, _, why := h.AcquireWhy(d, "bob"); why != AtCeiling {
		t.Fatalf("refused %v at the deployment's ceiling: want AtCeiling", why)
	}
	l.Fail(0)
	if _, _, why := h.AcquireWhy(d, "bob"); why != Ejected {
		t.Fatalf("refused %v while ejected: want Ejected", why)
	}
}
