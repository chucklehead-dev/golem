package limit

import (
	"errors"
	"testing"
	"time"

	"github.com/chucklehead-dev/golem/gateway/admission"
)

// TestBudgetModes: the budget in each mode (PRD §9.1, ADR 0050), on each
// store and in an outage's local check. With known spend at the budget,
// enforce refuses; warn and observe admit, the ticket over budget with the
// known spend checked. A call within the budget is never over, and the
// in-flight limits refuse in every mode.
func TestBudgetModes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		newStore func(t *testing.T) Store
		outage   bool
	}{
		{"mem", func(*testing.T) Store { return NewMem() }, false},
		{"valkey", func(t *testing.T) Store { return newValkey(t, valkeyAddr(t)) }, false},
		{"outage", func(*testing.T) Store { return NewMem() }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			for _, mode := range []admission.BudgetMode{admission.Enforce, admission.Warn, admission.Observe} {
				st := tc.newStore(t)
				l := New(st)
				now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
				l.Now = func() time.Time { return now }
				call := func(id, principal string) Call {
					c := scopeCall(id, principal)
					c.BudgetMode = mode
					return c
				}
				first, err := l.Admit(ctx, call("a", "p1"))
				if err != nil {
					t.Fatal(err)
				}
				if over, _ := first.OverBudget(); over {
					t.Fatalf("mode %d: a call within the budget is over", mode)
				}
				first.Complete(ctx, 10)
				l.ApplySnapshot(ctx, "s", Known{Period: "2026-11", Spend: 10}) // the budget, spent
				if tc.outage {
					st.(*Mem).SetDown(true)
				}
				tk, err := l.Admit(ctx, call("b", "p1"))
				switch {
				case mode == admission.Enforce && !errors.Is(err, ErrBudget):
					t.Fatalf("enforce: %v, want ErrBudget", err)
				case mode == admission.Enforce:
				case err != nil:
					t.Fatalf("mode %d: %v, want admitted over the budget", mode, err)
				default:
					if over, known := tk.OverBudget(); !over || known != 10 {
						t.Fatalf("mode %d: over %v at %d, want over at 10", mode, over, known)
					}
					// The in-flight limit holds: p1 has one running.
					if _, err := l.Admit(ctx, call("c", "p1")); !errors.Is(err, ErrConcurrency) {
						t.Fatalf("mode %d: a second call for p1: %v, want ErrConcurrency", mode, err)
					}
				}
			}
		})
	}
}
