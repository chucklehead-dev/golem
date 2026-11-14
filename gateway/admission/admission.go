// Package admission is the request path's contract with whatever admits
// calls against budgets and limits (ADR 0031, 0044; gateway.md step 6).
// It is a leaf: the proxy depends on it, gateway/limit implements it, and
// another backend or a test double needs nothing else.
//
// Admission is a fixed stage after routing, not a hook: ADR 0014's hooks
// have no phase after routing, are stateless per phase, and may drop
// Complete work, while an admitted call holds a slot across its whole life
// (renewed, held through the provider's tail) and its cost must be
// counted exactly once.
package admission

import (
	"context"
	"time"

	"github.com/chucklehead-dev/golem/lib/contract"
)

// Call is one request as admission sees it: who, under which limits, to
// which deployment, at what declared worst case.
type Call struct {
	ID               string // the request ID: the call's event in the ledger
	Principal, Scope string
	// The route chosen: what route-level limits (rates, provider quota
	// shares) key on.
	Alias, Deployment, Model string
	Budget                   int64 // the scope's budget in micro-units; 0: none
	PrincipalLimit           int   // in-flight calls per principal; 0: none
	ScopeLimit               int   // in-flight calls per scope (ADR 0041 §7.6); 0: none
	// DeclaredWorstCaseCheck (a scope option): admit only if known spend
	// plus Worst, the call's declared worst case, fits (ADR 0044); else
	// known spend below the budget.
	DeclaredWorstCaseCheck bool
	Worst                  int64
	// Started is when the call began (the request's arrival): it fixes the
	// budget period. Deadline is when it must have ended (the route's
	// maximum duration); zero: none.
	Started, Deadline time.Time
}

// Admitter admits calls. Admit returns a Ticket, or a *Refusal (rendered
// with its code), or another error (the admitter failed: a 500, or, if
// ctx ended, the caller having gone). A nil Ticket with a nil error admits
// the call uncounted.
//
// An implementation that counts spend owes the ledger one numbering record
// per counted event, a sequence of 0 if the event never reached the
// counter (ADR 0042, 0043).
type Admitter interface {
	Admit(ctx context.Context, c Call) (Ticket, error)
}

// Ticket is an admitted call's account. It ends exactly once, by Complete
// or Cancel; any later end is ignored.
type Ticket interface {
	// Complete records the call's cost and frees its slot.
	Complete(ctx context.Context, cost int64)
	// Cancel records the cost seen of a call abandoned or cut, and holds
	// its slot until until (the provider's tail: it may go on working,
	// and billing, after the connection closes; ADR 0041 §7.6).
	Cancel(ctx context.Context, seen int64, until time.Time)
	// Period is the budget period the call is counted in, for its row.
	Period() string
	// Waived reports an admission outside the bound (the store was down).
	Waived() bool
}

// Refusal is a call admission turned away: rendered with its code, and a
// Retry-After when RetryAfter is set.
type Refusal struct {
	Code       contract.Code
	Msg        string
	RetryAfter time.Duration
}

// Error is the refusal's code and message.
func (r *Refusal) Error() string { return r.Code.Name + ": " + r.Msg }
