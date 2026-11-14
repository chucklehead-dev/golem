package compile

import (
	"fmt"
	"strings"
	"testing"

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
		{"unset", Policy{}, nil, snapshot.Scope{PrincipalInFlight: DefaultPrincipalInFlight, ScopeInFlight: DefaultScopeInFlight}},
		{"every field valid", Policy{BudgetMicros: snapshot.MaxMicros, DeclaredWorstCaseCheck: true, InFlight: v1.InFlight{Principal: 1, Scope: 2},
			MaxOutputTokens: 3, MaxInputBytes: 4, Features: "compatible", ProviderStorage: "allowed"}, nil,
			snapshot.Scope{BudgetMicros: snapshot.MaxMicros, DeclaredWorstCaseCheck: true, PrincipalInFlight: 1, ScopeInFlight: 2,
				StorageAllowed: true, MaxOutputTokens: 3, MaxInputBytes: 4, Features: "compatible"}},
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
