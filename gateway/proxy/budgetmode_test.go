package proxy

import (
	"bytes"
	"testing"

	metricnoop "go.opentelemetry.io/otel/metric/noop"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/chucklehead-dev/golem/gateway/telemetry"
	"github.com/chucklehead-dev/golem/lib/contract"
	"github.com/chucklehead-dev/golem/lib/key"
)

// TestBudgetModes: a scope's budget in each mode (PRD §9.1, ADR 0050),
// spent past by calls of 6,204,000 micro-units against a budget of
// 10,000,000: the third is over it. observe admits it and marks its
// ledger row, adding nothing to the response; warn admits it, marks it and
// says so in golem-warning; enforce refuses it as budget.pool_exhausted,
// as the default did before modes. A call within the budget is never
// marked or warned. The in-flight limit holds in every mode (TestAuthAndLimits,
// TestConcurrencyAndCancel).
func TestBudgetModes(t *testing.T) {
	for _, tc := range []struct {
		mode    string // spec.budgetMode; "" is the default, observe
		code    int    // the third call's status
		warning string // its golem-warning
	}{
		{"", 200, ""},
		{"observe", 200, ""},
		{"warn", 200, "budget.pool_exhausted 124%"},
		{"enforce", 402, ""},
	} {
		t.Run("mode "+tc.mode, func(t *testing.T) {
			alice := key.New(key.Dev)
			spec := "{models: [claude], budgetMicros: 10000000}"
			if tc.mode != "" {
				spec = "{models: [claude], budgetMicros: 10000000, budgetMode: " + tc.mode + "}"
			}
			s := &stub{events: anthropicStream(1)}
			h, _ := standaloneHandler(t, `apiVersion: golem.dev/v1alpha1
kind: ScopePolicy
metadata: {name: team-a}
spec: `+spec+`
---
apiVersion: golem.dev/v1alpha1
kind: LocalIdentity
metadata: {name: local}
spec:
  keys: [{key: `+alice+`, principal: alice, scope: team-a}]
`, s)
			var rows bytes.Buffer
			tel, err := telemetry.New(telemetry.Providers{Tracer: tracenoop.NewTracerProvider(), Meter: metricnoop.NewMeterProvider(),
				Logger: telemetry.Stdout(&rows).Logger}, "local", "standalone")
			if err != nil {
				t.Fatal(err)
			}
			h.Telemetry = tel
			stream := `{"model":"claude","max_tokens":10,"stream":true,"messages":[]}`
			// Within the budget (0, then 6,204,000 spent), then over it
			// (12,408,000 of 10,000,000: 124%).
			for i := range 3 {
				s.events = anthropicStream(1)
				w := send(h, alice, stream)
				want, warning := 200, ""
				if i == 2 {
					want, warning = tc.code, tc.warning
				}
				if w.Code != want || w.Header().Get(contract.HeaderWarning) != warning {
					t.Fatalf("call %d: %d, golem-warning %q; want %d, %q: %s", i, w.Code, w.Header().Get(contract.HeaderWarning), want, warning, w.Body)
				}
				if want == 402 && errorCode(t, w) != "budget.pool_exhausted" {
					t.Fatalf("call %d refused %s, want budget.pool_exhausted", i, w.Body)
				}
			}
			got := splitRows(t, &rows)
			wantRows := 3
			if tc.code == 402 {
				wantRows = 2 // a refusal writes no row
			}
			if len(got) != wantRows {
				t.Fatalf("%d rows, want %d", len(got), wantRows)
			}
			for i, r := range got {
				if over := i == 2; r["over_budget"] != over {
					t.Errorf("row %d: over_budget %v, want %v", i, r["over_budget"], over)
				}
			}
		})
	}
}

// TestBudgetWarning: warn mode's header is the refusal's code and the share
// of the budget spent, rounded down, without overflow.
func TestBudgetWarning(t *testing.T) {
	for _, tc := range []struct {
		known, budget int64
		want          string
	}{
		{10, 10, "budget.pool_exhausted 100%"},
		{9, 10, "budget.pool_exhausted 90%"}, // over by its declared worst case
		{12_408_000, 10_000_000, "budget.pool_exhausted 124%"},
		{1 << 62, 1 << 50, "budget.pool_exhausted 409600%"},
	} {
		if got := budgetWarning(tc.known, tc.budget); got != tc.want {
			t.Errorf("budgetWarning(%d, %d) = %q, want %q", tc.known, tc.budget, got, tc.want)
		}
	}
}
