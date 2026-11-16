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
	// BudgetMode is what the budget does (PRD §9.1): Enforce refuses a call
	// over it; Warn and Observe admit it, its ticket saying so (OverBudget).
	// In-flight limits apply in every mode.
	BudgetMode     BudgetMode
	PrincipalLimit int // in-flight calls per principal; 0: none
	ScopeLimit     int // in-flight calls per scope (ADR 0041 §7.6); 0: none
	// DeclaredWorstCaseCheck (a scope option): admit only if known spend
	// plus Worst, the call's declared worst case, fits (ADR 0044); else
	// known spend below the budget.
	DeclaredWorstCaseCheck bool
	Worst                  int64
	// Key is the display ID of the key that authenticated the call ("" for
	// a token). KeyCap and MemberCap are the accounts below the scope's
	// (org-model.md §4.2): the key's spend cap, and its member's cap in
	// the scope (the member's own, or the scope's default), each checked
	// before the scope's budget and under its BudgetMode; 0: none.
	Key               string
	KeyCap, MemberCap int64
	// Started is when the call began (the request's arrival): it fixes the
	// budget period. Deadline is when it must have ended (the route's
	// maximum duration); zero: none.
	Started, Deadline time.Time
}

// BudgetMode is what a scope's budget does at admission (PRD §9.1, ADR
// 0050). The zero value is Enforce: a call that names no mode is refused
// over its budget, failing closed.
type BudgetMode uint8

// The budget modes.
const (
	// Enforce refuses a call over the budget (budget.pool_exhausted).
	Enforce BudgetMode = iota
	// Warn admits it, its ticket over budget: the ledger row marks it and
	// the response carries a golem-warning header.
	Warn
	// Observe admits it, its ticket over budget: the ledger row marks it,
	// and nothing is added to the response.
	Observe
)

// ModeOf is a snapshot's budget mode ("observe", "warn", "enforce"); any
// other is Enforce.
func ModeOf(s string) BudgetMode {
	switch s {
	case "observe":
		return Observe
	case "warn":
		return Warn
	}
	return Enforce
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
	// OverBudget reports a call admitted over one of its accounts' limits,
	// in a mode that admits one (Warn, Observe): what Enforce would have
	// refused.
	OverBudget() Over
}

// Level is one of a call's accounts (org-model.md §4.2), in the order
// admission checks them.
type Level uint8

// The levels; NoLevel is none.
const (
	NoLevel Level = iota
	KeyLevel
	MemberLevel
	ScopeLevel
)

// Code is the refusal of a call over the level's limit.
func (l Level) Code() contract.Code {
	switch l {
	case KeyLevel:
		return contract.BudgetKeyCap
	case MemberLevel:
		return contract.BudgetMemberCap
	}
	return contract.BudgetPoolExhausted
}

// Over is the account an admitted call is over the limit of: its level
// (NoLevel: none), its known spend as admission checked it, and its limit.
type Over struct {
	Level        Level
	Known, Limit int64
}

// Meter is one per-minute bucket of requests a request is charged to: a
// deployment's share of its provider's quota (PRD §9.2, ADR 0012), or a
// member's or a scope's rate limit (PRD §5.5), each shared by a site's
// gateways. Requests only: tokens are the provider's 429s to enforce and
// budgets' to account (ADR 0044).
type Meter struct {
	Key       string // QuotaMeter, MemberRateMeter, ScopeRateMeter
	PerMinute int64
}

// The meters' keys.
func QuotaMeter(deployment string) string            { return "q:" + deployment }
func MemberRateMeter(scope, principal string) string { return "rm:" + scope + "/" + principal }
func ScopeRateMeter(scope string) string             { return "rs:" + scope }

// Meters takes requests from meters: a call's rate limits once, its
// deployment's quota share each attempt (model/attempts.md's
// QUOTA_PER_ATTEMPT: a failover must not land on a deployment at its
// quota).
type Meters interface {
	// Take takes a request from every meter of ms, or from none, reporting
	// the first without room (its index) and how long until it has; an
	// unreachable store admits (rate limits fail open, PRD §5.5).
	Take(ctx context.Context, ms []Meter) (ok bool, over int, wait time.Duration)
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
