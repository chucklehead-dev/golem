package limit

import (
	"errors"
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
	st.SetDown(true)
	// Each gateway admits against its own counts: p1 has two calls running.
	a, err1 := g1.Admit(ctx, scopeCall("a", "p1"))
	_, err2 := g2.Admit(ctx, scopeCall("b", "p1"))
	if err1 != nil || err2 != nil || len(g1.Waived()) != 1 || len(g2.Waived()) != 1 {
		t.Fatalf("%v %v waived %v %v: both admitted and both recorded as waivers", err1, err2, g1.Waived(), g2.Waived())
	}
	a.Complete(ctx, 3)
	if p := g1.Pending("s"); len(p) != 1 {
		t.Fatalf("pending %v: the increment is held", p)
	}
	st.SetDown(false)
	g1.Recover(ctx)
	g2.Recover(ctx)
	if g1.Known(ctx, "s") != 3 || len(st.Slots()) != 1 {
		t.Fatalf("known %d, slots %v: the increment replayed, b's slot re-acquired", g1.Known(ctx, "s"), st.Slots())
	}
}
