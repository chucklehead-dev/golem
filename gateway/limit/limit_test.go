package limit

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

func scopeCall(id, principal string) Call {
	return Call{ID: id, Principal: principal, Scope: "s", Budget: 10, PrincipalLimit: 1, ScopeLimit: 2}
}

func TestSoftAndStrict(t *testing.T) {
	ctx := t.Context()
	l := New(NewMem())
	tk, err := l.Admit(ctx, scopeCall("a", "p1"))
	if err != nil {
		t.Fatal(err)
	}
	tk.Complete(ctx, 8)
	// Soft: 8 < 10 admits, though the call may cost more (the accepted risk).
	soft := scopeCall("b", "p1")
	soft.Worst = 5
	if _, err := l.Admit(ctx, soft); err != nil {
		t.Fatalf("soft: %v", err)
	}
	// Strict: 8 + 5 > 10 refuses (ADR 0031).
	strict := scopeCall("c", "p2")
	strict.Strict, strict.Worst = true, 5
	if _, err := l.Admit(ctx, strict); !errors.Is(err, ErrBudget) {
		t.Fatalf("strict: %v, want ErrBudget", err)
	}
	strict.Worst = 2
	if _, err := l.Admit(ctx, strict); err != nil {
		t.Fatalf("strict within the budget: %v", err)
	}
}

func TestLimits(t *testing.T) {
	ctx := t.Context()
	l := New(NewMem())
	a, _ := l.Admit(ctx, scopeCall("a", "p1"))
	if _, err := l.Admit(ctx, scopeCall("b", "p1")); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("a second call for p1: %v", err)
	}
	if _, err := l.Admit(ctx, scopeCall("c", "p2")); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Admit(ctx, scopeCall("d", "p3")); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("a third call in the scope: %v (the scope limit is 2)", err)
	}
	// A cancelled call keeps its slot until its upstream ends.
	a.Cancel(ctx, 1)
	if _, err := l.Admit(ctx, scopeCall("e", "p1")); !errors.Is(err, ErrConcurrency) {
		t.Fatalf("after cancel: %v; the provider may still be working", err)
	}
	a.Ended(ctx)
	if _, err := l.Admit(ctx, scopeCall("e", "p1")); err != nil {
		t.Fatalf("after the upstream ended: %v", err)
	}
}

func TestOutage(t *testing.T) {
	ctx := t.Context()
	st := NewMem()
	g1, g2 := New(st), New(st)
	var numbered []int64
	g1.OnNumbered = func(_ string, _ Event, seq int64) { numbered = append(numbered, seq) }
	st.SetDown(true)
	// Each gateway admits against its own counts: p1 has two calls running.
	a, err1 := g1.Admit(ctx, scopeCall("a", "p1"))
	_, err2 := g2.Admit(ctx, scopeCall("b", "p1"))
	if err1 != nil || err2 != nil || len(g1.Waived()) != 1 || len(g2.Waived()) != 1 {
		t.Fatalf("%v %v waived %v %v: both admitted and both recorded as waivers", err1, err2, g1.Waived(), g2.Waived())
	}
	a.Complete(ctx, 3)
	if p := g1.Pending("s"); len(p) != 1 || len(numbered) != 0 {
		t.Fatalf("pending %v numbered %v: the increment is held, and has no number yet", p, numbered)
	}
	st.SetDown(false)
	g1.Recover(ctx)
	g2.Recover(ctx)
	if g1.Known(ctx, "s") != 3 || len(st.Slots()) != 1 || !slices.Equal(numbered, []int64{1}) {
		t.Fatalf("known %d, slots %v, numbered %v: the increment replayed and numbered, b's slot re-acquired",
			g1.Known(ctx, "s"), st.Slots(), numbered)
	}
}

// TestWatermark: known spend is the snapshot's plus the counter after its
// watermark, on each gateway whichever snapshot it holds.
func TestWatermark(t *testing.T) {
	ctx := t.Context()
	st := NewMem()
	g1, g2 := New(st), New(st)
	for i, c := range []int64{3, 5, 2} {
		tk, _ := g1.Admit(ctx, scopeCall(fmt.Sprint(i), "p1"))
		tk.Complete(ctx, c)
	}
	// The ledger has events 1 and 2 (8); g1 applies that snapshot, g2 not.
	g1.ApplySnapshot(ctx, "s", Known{Spend: 8, Watermark: 2})
	if a, b := g1.Known(ctx, "s"), g2.Known(ctx, "s"); a != 10 || b != 10 {
		t.Fatalf("known %d on the new snapshot, %d on the old: both must be 10", a, b)
	}
}
