package compile

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"time"

	v1 "github.com/chucklehead-dev/golem/lib/api/v1alpha1"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// The budget modes (the PRD): what a scope's budget does at admission.
// A scope with a budget and no mode observes: rollouts start in observe,
// so a mis-set budget cannot stop a team's work on day one.
const (
	BudgetObserve = "observe" // admitted; the ledger row marks a call over the budget
	BudgetWarn    = "warn"    // admitted and marked, the response carrying golem-warning
	BudgetEnforce = "enforce" // refused, budget.pool_exhausted
)

// Policy is a scope's policy as a ScopePolicy's spec gives it. Zero is
// "unset" in every field: no models, no budget, the default in-flight
// limit, no ceiling, the route's feature mode, provider storage forbidden.
type Policy = v1.ScopePolicySpec

// scopePolicy checks p, the policy at path at of object obj, reporting
// every field that fails to add, and returns the snapshot's scope with
// the policy filled in and its defaults made explicit: an in-flight limit
// of 0 is DefaultPrincipalInFlight or DefaultScopeInFlight, never "none",
// and a budget's mode is observe unless set. A negative limit or ceiling
// is refused, since the gateway applies one only above zero (a negative
// one would be none); a budget above snapshot.MaxMicros would not be
// summed exactly in the site store; an unknown mode, features or
// providerStorage value would be read as the default; a budget mode with
// no budget is a budget forgotten. The one check of a scope's policy.
// Its models are the caller's to check against the routes.
func scopePolicy(obj, at string, p Policy, add adder) snapshot.Scope {
	switch {
	case p.BudgetMicros < 0:
		add(obj, at+".budgetMicros", "must not be negative (is %d); 0 is no budget", p.BudgetMicros)
	case p.BudgetMicros > snapshot.MaxMicros:
		add(obj, at+".budgetMicros", "at most %d (2^50), not %d: larger sums are not exact in the site store", int64(snapshot.MaxMicros), p.BudgetMicros)
	}
	switch {
	case p.BudgetMode != "" && p.BudgetMode != BudgetObserve && p.BudgetMode != BudgetWarn && p.BudgetMode != BudgetEnforce:
		add(obj, at+".budgetMode", "observe (the default), warn or enforce, not %q", p.BudgetMode)
	case p.BudgetMode != "" && p.BudgetMicros == 0:
		add(obj, at+".budgetMode", "%s, but there is no budget: set budgetMicros, or remove budgetMode", p.BudgetMode)
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
	var limits v1.RateLimits
	if p.RateLimits != nil {
		limits = *p.RateLimits
	}
	memberRPM := requestsPerMinute(obj, at+".rateLimits.member", limits.Member, add)
	scopeRPM := requestsPerMinute(obj, at+".rateLimits.scope", limits.Scope, add)
	// Every scope has a mode, whether or not it has a budget.
	mode := cmp.Or(p.BudgetMode, BudgetObserve)
	return snapshot.Scope{BudgetMicros: p.BudgetMicros, BudgetMode: mode,
		MemberRPM: memberRPM, ScopeRPM: scopeRPM,
		PrincipalInFlight: cmp.Or(p.InFlight.Principal, DefaultPrincipalInFlight), ScopeInFlight: cmp.Or(p.InFlight.Scope, DefaultScopeInFlight),
		StorageAllowed: p.ProviderStorage == "allowed", MaxOutputTokens: p.MaxOutputTokens, MaxInputBytes: p.MaxInputBytes, Features: p.Features}
}

// policyName names a ScopePolicy in a problem.
func policyName(src v1.Source[v1.ScopePolicy]) string {
	return objectName("ScopePolicy", src.Object.Metadata.Name, src.Where())
}

// compiledPolicy is a ScopePolicy compiled: its scope, and the object it
// came from, for a problem about it.
type compiledPolicy struct {
	scope snapshot.Scope
	obj   string
}

// compilePolicies validates the ScopePolicies, by name: each needs one,
// once; its policy is checked (scopePolicy, every problem), and each of its
// models must be a route's alias. Each scope's models are sorted.
func compilePolicies(srcs []v1.Source[v1.ScopePolicy], aliases map[string]string, add adder) map[string]compiledPolicy {
	out := map[string]compiledPolicy{}
	for _, src := range srcs {
		sp := src.Object
		obj := policyName(src)
		switch name := sp.Metadata.Name; {
		case name == "":
			add(obj, "metadata.name", "required: the scope's ID")
			continue
		case out[name].obj != "":
			add(obj, "metadata.name", "%s is also scope %q's policy: one ScopePolicy per scope", out[name].obj, name)
			continue
		}
		for j, m := range sp.Spec.Models {
			if _, ok := aliases[m]; !ok {
				add(obj, fmt.Sprintf("spec.models[%d]", j), "no ModelRoute has alias %q", m)
			}
		}
		sc := scopePolicy(obj, "spec", sp.Spec, add)
		sc.ID, sc.Models = sp.Metadata.Name, slices.Compact(slices.Sorted(slices.Values(sp.Spec.Models)))
		out[sc.ID] = compiledPolicy{sc, obj}
	}
	return out
}

// DefaultScope is the scope a state scope with no ScopePolicy is published
// as: it fails closed, its members able to call nothing (no models, no
// budget to spend) until its policy is added in the config.
func DefaultScope(id string) snapshot.Scope {
	return snapshot.Scope{ID: id, PrincipalInFlight: DefaultPrincipalInFlight, ScopeInFlight: DefaultScopeInFlight}
}

// join gives each of the state's scopes its policy, by ID, keeping what
// the state's scope carries of its own (its known spend). A scope with no
// policy is DefaultScope's, and a policy whose scope the state has not is
// left out; each is told to report, for the publication goes on: the
// state and the config change apart (an admin creates a scope, a pull
// request adds its policy), and neither order may stop every other scope's
// publication.
func join(state []snapshot.Scope, policies map[string]compiledPolicy, report func(Problem)) []snapshot.Scope {
	out := make([]snapshot.Scope, 0, len(state))
	seen := map[string]bool{}
	for _, st := range state {
		seen[st.ID] = true
		p, ok := policies[st.ID]
		sc := p.scope
		if !ok {
			sc = DefaultScope(st.ID)
			report(Problem{Object: "scope " + st.ID, Path: "ScopePolicy",
				Msg: fmt.Sprintf("no ScopePolicy named %q: the scope is published with no models (its members can call nothing) until one is added to the config", st.ID)})
		}
		sc.ID, sc.Period, sc.KnownSpend, sc.Since = st.ID, st.Period, st.KnownSpend, st.Since
		out = append(out, sc)
	}
	for _, id := range slices.Sorted(maps.Keys(policies)) {
		if !seen[id] {
			report(Problem{Object: policies[id].obj, Path: "metadata.name",
				Msg: fmt.Sprintf("the state has no scope %q: the policy is not published until an admin creates the scope (golemctl scopes create %s), or the name is corrected", id, id)})
		}
	}
	slices.SortFunc(out, func(a, b snapshot.Scope) int { return cmp.Compare(a.ID, b.ID) })
	return out
}

// policiesOf is a published snapshot's scopes as compiled policies, for a
// renewal: what Compile joined them from, as far as the snapshot keeps it.
func policiesOf(scopes []snapshot.Scope) map[string]compiledPolicy {
	out := make(map[string]compiledPolicy, len(scopes))
	for _, sc := range scopes {
		sc.Period, sc.KnownSpend, sc.Since = "", 0, time.Time{}
		out[sc.ID] = compiledPolicy{sc, "ScopePolicy/" + sc.ID + " (as published)"}
	}
	return out
}
