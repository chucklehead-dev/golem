package compile

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/chucklehead-dev/golem/lib/api/v1alpha1"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// TestScopePolicyCheck: each value a gateway would misread (a negative limit
// or ceiling is none, a budget past 2^50 is not summed exactly, an unknown
// mode is the default) is refused at its own path, every one reported; a
// valid policy compiles as given, with an in-flight limit of 0 the default.
func TestScopePolicyCheck(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Policy
		want []string // path: message prefix, in order
		sc   snapshot.Scope
	}{
		{"unset", Policy{}, nil, snapshot.Scope{BudgetMode: BudgetObserve, PrincipalInFlight: DefaultPrincipalInFlight, ScopeInFlight: DefaultScopeInFlight}},
		{"a default member cap, enforced, no budget", Policy{DefaultMemberCapMicros: 7, BudgetMode: BudgetEnforce}, nil,
			snapshot.Scope{DefaultMemberCapMicros: 7, BudgetMode: BudgetEnforce, PrincipalInFlight: DefaultPrincipalInFlight, ScopeInFlight: DefaultScopeInFlight}},
		{"rate limits", Policy{RateLimits: &v1.RateLimits{Member: &v1.Rate{RequestsPerMinute: 10}, Scope: &v1.Rate{TokensPerMinute: 900}}}, nil,
			snapshot.Scope{BudgetMode: BudgetObserve, MemberRPM: 10, ScopeTPM: 900, PrincipalInFlight: DefaultPrincipalInFlight, ScopeInFlight: DefaultScopeInFlight}},
		{"a negative rate limit", Policy{RateLimits: &v1.RateLimits{Scope: &v1.Rate{RequestsPerMinute: -1}}}, []string{"s.rateLimits.scope.requestsPerMinute: must not be negative"}, snapshot.Scope{}},
		{"negative default member cap", Policy{DefaultMemberCapMicros: -1}, []string{"s.defaultMemberCapMicros: must not be negative (is -1); 0 is no cap"}, snapshot.Scope{}},
		{"default member cap past exact sums", Policy{DefaultMemberCapMicros: snapshot.MaxMicros + 1}, []string{"s.defaultMemberCapMicros: at most 1125899906842624"}, snapshot.Scope{}},
		{"every field valid", Policy{BudgetMicros: snapshot.MaxMicros, DeclaredWorstCaseCheck: true, InFlight: v1.InFlight{Principal: 1, Scope: 2},
			MaxOutputTokens: 3, MaxInputBytes: 4, Features: "compatible", ProviderStorage: "allowed"}, nil,
			snapshot.Scope{BudgetMicros: snapshot.MaxMicros, BudgetMode: BudgetObserve, DeclaredWorstCaseCheck: true, PrincipalInFlight: 1, ScopeInFlight: 2,
				StorageAllowed: true, MaxOutputTokens: 3, MaxInputBytes: 4, Features: "compatible"}},
		{"warn", Policy{BudgetMicros: 1, BudgetMode: BudgetWarn}, nil, snapshot.Scope{BudgetMicros: 1, BudgetMode: BudgetWarn,
			PrincipalInFlight: DefaultPrincipalInFlight, ScopeInFlight: DefaultScopeInFlight}},
		{"enforce", Policy{BudgetMicros: 1, BudgetMode: BudgetEnforce}, nil, snapshot.Scope{BudgetMicros: 1, BudgetMode: BudgetEnforce,
			PrincipalInFlight: DefaultPrincipalInFlight, ScopeInFlight: DefaultScopeInFlight}},
		{"unknown budget mode", Policy{BudgetMicros: 1, BudgetMode: "strict"}, []string{`s.budgetMode: observe (the default), warn or enforce, not "strict"`}, snapshot.Scope{}},
		{"budget mode without a budget", Policy{BudgetMode: BudgetEnforce}, []string{"s.budgetMode: enforce, but there is no budget: set budgetMicros (or defaultMemberCapMicros), or remove budgetMode"}, snapshot.Scope{}},
		{"negative budget", Policy{BudgetMicros: -1}, []string{"s.budgetMicros: must not be negative (is -1); 0 is no budget"}, snapshot.Scope{}},
		{"budget past exact sums", Policy{BudgetMicros: snapshot.MaxMicros + 1}, []string{"s.budgetMicros: at most 1125899906842624"}, snapshot.Scope{}},
		{"negative principal limit", Policy{InFlight: v1.InFlight{Principal: -1}}, []string{"s.inFlight.principal: must not be negative (is -1); 0 or unset is the default, 4"}, snapshot.Scope{}},
		{"negative scope limit", Policy{InFlight: v1.InFlight{Scope: -1}}, []string{"s.inFlight.scope: must not be negative (is -1); 0 or unset is the default, 16"}, snapshot.Scope{}},
		{"negative output ceiling", Policy{MaxOutputTokens: -1}, []string{"s.maxOutputTokens: must not be negative"}, snapshot.Scope{}},
		{"negative input ceiling", Policy{MaxInputBytes: -1}, []string{"s.maxInputBytes: must not be negative"}, snapshot.Scope{}},
		{"unknown features", Policy{Features: "loose"}, []string{`s.features: restricted or compatible, not "loose"`}, snapshot.Scope{}},
		{"unknown storage", Policy{ProviderStorage: "sometimes"}, []string{`s.providerStorage: forbidden (the default) or allowed, not "sometimes"`}, snapshot.Scope{}},
		{"every problem", Policy{BudgetMicros: -1, InFlight: v1.InFlight{Principal: -1, Scope: -1}, MaxInputBytes: -1, Features: "x"},
			[]string{"s.budgetMicros", "s.inFlight.principal", "s.inFlight.scope", "s.maxInputBytes", "s.features"}, snapshot.Scope{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			add := func(obj, path, msg string, args ...any) {
				got = append(got, path+": "+fmt.Sprintf(msg, args...))
			}
			sc := scopePolicy("o", "s", tc.p, add)
			if len(got) != len(tc.want) {
				t.Fatalf("problems %q, want %q", got, tc.want)
			}
			for i := range got {
				if !strings.HasPrefix(got[i], tc.want[i]) {
					t.Errorf("problem %q, want %q", got[i], tc.want[i])
				}
			}
			if len(tc.want) > 0 {
				return
			}
			if fmt.Sprint(sc) != fmt.Sprint(tc.sc) {
				t.Errorf("compiled %+v, want %+v", sc, tc.sc)
			}
		})
	}
}

// priced prices good's one model.
const priced = `---
apiVersion: golem.dev/v1alpha1
kind: PriceList
metadata: {name: default}
spec:
  currency: USD
  prices: [{model: claude-sonnet-4-5, inputPerMTok: 3000000, outputPerMTok: 15000000}]
`

// policy is a ScopePolicy document: name, and its spec in flow style.
func policy(name, spec string) string {
	return "---\napiVersion: golem.dev/v1alpha1\nkind: ScopePolicy\nmetadata: {name: " + name + "}\nspec: " + spec + "\n"
}

// TestJoin: a published snapshot's scopes are the state's, each with its
// ScopePolicy's policy and the state's known spend; a scope with none fails
// closed (DefaultScope: no models), and a policy whose scope the state has
// not is left out; both are reported, and the rest is published.
func TestJoin(t *testing.T) {
	objs := objects(t, map[string]string{"golem.yaml": good + priced +
		policy("team-a", "{models: [claude, claude], budgetMicros: 9, budgetMode: warn, inFlight: {scope: 3}}") +
		policy("team-later", "{models: [claude]}")})
	since := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	var unmatched []string
	o := published
	o.Entitlements = func(aliases []string) snapshot.Entitlements {
		return snapshot.Entitlements{Scopes: []snapshot.Scope{{ID: "team-b"}, {ID: "team-a", Period: "2026-11", KnownSpend: 4, Since: since}}}
	}
	o.Unmatched = func(p Problem) { unmatched = append(unmatched, p.String()) }
	s, err := Compile(objs, o)
	if err != nil {
		t.Fatal(err)
	}
	want := []snapshot.Scope{
		{ID: "team-a", Models: []string{"claude"}, BudgetMicros: 9, BudgetMode: BudgetWarn, PrincipalInFlight: DefaultPrincipalInFlight, ScopeInFlight: 3,
			Period: "2026-11", KnownSpend: 4, Since: since},
		DefaultScope("team-b"),
	}
	if !reflect.DeepEqual(s.Entitlements.Scopes, want) {
		t.Fatalf("scopes %+v\nwant %+v", s.Entitlements.Scopes, want)
	}
	wantReports := []string{
		`scope team-b: ScopePolicy: no ScopePolicy named "team-b": the scope is published with no models (its members can call nothing) until one is added to the config`,
		`ScopePolicy/team-later (golem.yaml#6): metadata.name: the state has no scope "team-later": the policy is not published until an admin creates the scope (golemctl scopes create team-later), or the name is corrected`,
	}
	if !slices.Equal(unmatched, wantReports) {
		t.Fatalf("reported %q\nwant %q", unmatched, wantReports)
	}
	if d := DefaultScope("x"); len(d.Models) != 0 || d.BudgetMicros != 0 {
		t.Fatalf("the default scope %+v can call something", d)
	}

	// Without the state (golemctl config validate), the policies are only
	// checked: nothing is joined, or reported.
	unmatched = nil
	o.Entitlements = nil
	if s, err := Compile(objs, o); err != nil || len(s.Entitlements.Scopes) != 0 || unmatched != nil {
		t.Fatalf("without the state: %v, %+v, reported %q", err, s, unmatched)
	}
}

// TestPolicyProblems: a ScopePolicy's problems are the config's, so they
// fail the compile with or without the state: an alias no route has, an
// unpriced model a budget reaches, a name missing.
func TestPolicyProblems(t *testing.T) {
	for _, tc := range []struct{ name, yaml, want string }{
		{"unknown alias", good + priced + policy("team-a", "{models: [claude, other]}"), `ScopePolicy/team-a (golem.yaml#5): spec.models[1]: no ModelRoute has alias "other"`},
		{"unpriced budget", good + policy("team-a", "{models: [claude], budgetMicros: 1}"),
			`ScopePolicy/team-a (golem.yaml#4): spec.models: alias "claude" reaches model "claude-sonnet-4-5", which no PriceList prices: the scope's budget could not be charged; price the model in a PriceList, or remove the alias`},
		{"no name", good + strings.Replace(policy("x", "{}"), "{name: x}", "{}", 1), "metadata.name: required: the scope's ID"},
		{"policy problems", good + policy("team-a", "{budgetMicros: -1}"), "ScopePolicy/team-a (golem.yaml#4): spec.budgetMicros: must not be negative"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(objects(t, map[string]string{"golem.yaml": tc.yaml}), published)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%v; want a problem containing %q", err, tc.want)
			}
		})
	}
	// A budget over priced models, and one without a budget over unpriced.
	for _, y := range []string{good + priced + policy("team-a", "{models: [claude], budgetMicros: 1}"), good + policy("team-a", "{models: [claude]}")} {
		if _, err := Compile(objects(t, map[string]string{"golem.yaml": y}), published); err != nil {
			t.Fatal(err)
		}
	}
}
