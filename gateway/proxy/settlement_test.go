package proxy

import (
	"context"
	"testing"
	"time"
)

// untilTicket records the hold a Cancel asks for.
type untilTicket struct{ until time.Time }

func (*untilTicket) Complete(context.Context, int64)                  {}
func (t *untilTicket) Cancel(_ context.Context, _ int64, u time.Time) { t.until = u }
func (*untilTicket) Period() string                                   { return "" }
func (*untilTicket) Waived() bool                                     { return false }
func (*untilTicket) OverBudget() (bool, int64)                        { return false, 0 }

// TestTailClampedToDeadline: a cancelled call's slot is held for the
// provider's tail, but never past the call's own deadline (the provider
// cannot still be working on it after that); with no deadline, the whole
// tail.
//
//quint:checks attempts.workWithinLimit
func TestTailClampedToDeadline(t *testing.T) {
	for _, tc := range []struct {
		name     string
		deadline time.Duration // from now; 0: none
		clamped  bool
	}{
		{"a deadline before the tail ends", time.Second, true},
		{"a deadline after the tail", time.Hour, false},
		{"no deadline", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tk := &untilTicket{}
			a := &settlement{ticket: tk, ctx: t.Context(), tail: time.Minute}
			if tc.deadline != 0 {
				a.deadline = time.Now().Add(tc.deadline)
			}
			before := time.Now()
			a.cancel(0)
			switch {
			case tc.clamped && !tk.until.Equal(a.deadline):
				t.Fatalf("held until %v, want the deadline %v", tk.until, a.deadline)
			case !tc.clamped && (tk.until.Before(before.Add(time.Minute)) || tk.until.After(time.Now().Add(time.Minute))):
				t.Fatalf("held until %v, want the tail from now", tk.until)
			}
		})
	}
}
