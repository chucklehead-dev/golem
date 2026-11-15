package limit

import (
	"errors"
	"testing"

	"github.com/chucklehead-dev/golem/gateway/admission"
)

// TestBudgetModes: the budget in each mode (PRD §9.1, ADR 0050), on each
// store and in an outage's local check. With known spend at the budget,
// enforce refuses; warn and observe admit, the ticket over budget with the
// known spend checked; under the declared-worst-case check, a call whose
// worst case would not fit is over too. A call within the budget is never
// over, and the in-flight limits refuse in every mode.
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
				call := func(id, principal string) Call {
					c := scopeCall(id, principal)
					c.BudgetMode = mode
					return c
				}
				first, err := l.Admit(ctx, call("a", "p1"))
				if err != nil {
					t.Fatal(err)
				}
				if over, known := first.OverBudget(); over || known != 0 {
					t.Fatalf("mode %d: a call within the budget: over %v at %d", mode, over, known)
				}
				first.Complete(ctx, 10) // the budget, spent
				if tc.outage {
					// An admission learns the spend an outage's local check
					// falls back on; then the store goes.
					if probe, err := l.Admit(ctx, call("probe", "p9")); err == nil {
						probe.Complete(ctx, 0)
					}
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

// TestWorstCaseOverBudget: under the declared-worst-case check, a call in
// observe mode whose worst case would not fit is admitted over the budget,
// though known spend is below it; one that fits is not over.
func TestWorstCaseOverBudget(t *testing.T) {
	ctx := t.Context()
	l := New(NewMem())
	c := scopeCall("a", "p1")
	c.BudgetMode, c.DeclaredWorstCaseCheck, c.Worst = admission.Observe, true, 5
	tk, err := l.Admit(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	tk.Complete(ctx, 8)
	c.ID, c.Worst = "b", 2
	if tk, err := l.Admit(ctx, c); err != nil {
		t.Fatal(err)
	} else if over, _ := tk.OverBudget(); over {
		t.Fatal("8 + 2 fits 10: not over")
	} else {
		tk.Complete(ctx, 0)
	}
	c.ID, c.Worst = "c", 5
	if tk, err := l.Admit(ctx, c); err != nil {
		t.Fatal(err)
	} else if over, known := tk.OverBudget(); !over || known != 8 {
		t.Fatalf("8 + 5 past 10: over %v at %d", over, known)
	}
}
