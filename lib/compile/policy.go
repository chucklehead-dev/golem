package compile

import (
	"cmp"

	v1 "github.com/chucklehead-dev/golem/lib/api/v1alpha1"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// Policy is a scope's policy as a config CRD gives it: a LocalScope's
// today, the coming ScopePolicy CRD's (the PRD, ADR 0005) next. Zero is
// "unset" in every field: no budget, the default in-flight limit, no
// ceiling, the route's feature mode, provider storage forbidden.
type Policy struct {
	BudgetMicros           int64
	DeclaredWorstCaseCheck bool
	InFlight               v1.InFlight
	ProviderStorage        string
	MaxOutputTokens        int64
	MaxInputBytes          int64
	Features               string
}

// localPolicy is a LocalScope's policy.
func localPolicy(sc v1.LocalScope) Policy {
	return Policy{BudgetMicros: sc.BudgetMicros, DeclaredWorstCaseCheck: sc.DeclaredWorstCaseCheck, InFlight: sc.InFlight,
		ProviderStorage: sc.ProviderStorage, MaxOutputTokens: sc.MaxOutputTokens, MaxInputBytes: sc.MaxInputBytes, Features: sc.Features}
}

// scopePolicy checks p, the policy at path at of object obj, reporting
// every field that fails to add, and returns the snapshot's scope with
// the policy filled in and its defaults made explicit: an in-flight limit
// of 0 is DefaultPrincipalInFlight or DefaultScopeInFlight, never "none".
// A negative limit or ceiling is refused, since the gateway applies one
// only above zero (a negative one would be none); a budget above
// snapshot.MaxMicros would not be summed exactly in the site store; an
// unknown features or providerStorage value would be read as the default.
// The one check of a scope's policy: every CRD that gives one calls it.
func scopePolicy(obj, at string, p Policy, add adder) snapshot.Scope {
	switch {
	case p.BudgetMicros < 0:
		add(obj, at+".budgetMicros", "must not be negative (is %d); 0 is no budget", p.BudgetMicros)
	case p.BudgetMicros > snapshot.MaxMicros:
		add(obj, at+".budgetMicros", "at most %d (2^50), not %d: larger sums are not exact in the site store", int64(snapshot.MaxMicros), p.BudgetMicros)
	}
	if p.InFlight.Principal < 0 {
		add(obj, at+".inFlight.principal", "must not be negative (is %d); 0 or unset is the default, %d", p.InFlight.Principal, DefaultPrincipalInFlight)
	}
	if p.InFlight.Scope < 0 {
		add(obj, at+".inFlight.scope", "must not be negative (is %d); 0 or unset is the default, %d", p.InFlight.Scope, DefaultScopeInFlight)
	}
	if p.MaxOutputTokens < 0 {
		add(obj, at+".maxOutputTokens", "must not be negative (is %d); 0 or unset is no ceiling", p.MaxOutputTokens)
	}
	if p.MaxInputBytes < 0 {
		add(obj, at+".maxInputBytes", "must not be negative (is %d); 0 or unset is no ceiling", p.MaxInputBytes)
	}
	if p.Features != "" && p.Features != "restricted" && p.Features != "compatible" {
		add(obj, at+".features", "restricted or compatible, not %q (unset: restricted, compatible on agent routes)", p.Features)
	}
	if p.ProviderStorage != "" && p.ProviderStorage != "forbidden" && p.ProviderStorage != "allowed" {
		add(obj, at+".providerStorage", "forbidden (the default) or allowed, not %q", p.ProviderStorage)
	}
	return snapshot.Scope{BudgetMicros: p.BudgetMicros, DeclaredWorstCaseCheck: p.DeclaredWorstCaseCheck,
		PrincipalInFlight: cmp.Or(p.InFlight.Principal, DefaultPrincipalInFlight), ScopeInFlight: cmp.Or(p.InFlight.Scope, DefaultScopeInFlight),
		StorageAllowed: p.ProviderStorage == "allowed", MaxOutputTokens: p.MaxOutputTokens, MaxInputBytes: p.MaxInputBytes, Features: p.Features}
}
