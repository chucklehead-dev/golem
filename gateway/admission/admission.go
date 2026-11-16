// Package admission is the request path's contract with whatever admits
// calls against budgets and limits. It is a leaf. The proxy depends on it,
// gateway/limit implements it, and a test double needs nothing else.
//
// Admission is a fixed stage after routing, not a hook. Hooks have no
// phase after routing, are stateless per phase, and may drop work. An
// admitted call holds a slot for its whole life, through the provider's
// tail, and its cost must be counted exactly once. ADR 0014, ADR 0031.
package admission

import (
	"context"
	"time"

	"github.com/chucklehead-dev/golem/lib/contract"
)

// Call is one request as admission sees it. It says who calls, under which
// limits, and to which deployment.
type Call struct {
	ID               string // the request ID, the call's event in the ledger
	Principal, Scope string
	// The route chosen. gateway/limit reads none of these, since rate limits
	// are Meters, taken outside admission.
	Alias, Deployment, Model string
	Budget                   int64 // the scope's budget in micro-units; 0 is none
	// BudgetMode is what the budget does. Enforce refuses a call over it.
	// Warn and Observe admit it, and its ticket says so (OverBudget).
	// In-flight limits apply in every mode.
	BudgetMode     BudgetMode
	PrincipalLimit int // in-flight calls per principal; 0 is none
	ScopeLimit     int // in-flight calls per scope; 0 is none
	// Started is when the request arrived, which fixes the budget period.
	// Deadline is when it must have ended, from the route's maximum
	// duration. Zero is none.
	Started, Deadline time.Time
}

// BudgetMode is what a scope's budget does at admission. The zero value is
// Enforce, so a call that names no mode is refused over its budget,
// failing closed. ADR 0050.
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
type Admitter interface {
	Admit(ctx context.Context, c Call) (Ticket, error)
}

// Ticket is an admitted call's account. It ends exactly once, by Complete
// or Cancel; any later end is ignored.
type Ticket interface {
	// Complete records the call's cost and frees its slot.
	Complete(ctx context.Context, cost int64)
	// Cancel records the cost seen of a call abandoned or cut, and holds
	// its slot until the given time. The provider may go on working and
	// billing after the connection closes. ADR 0041.
	Cancel(ctx context.Context, seen int64, until time.Time)
	// Period is the budget period the call is counted in, for its row.
	Period() string
	// Waived reports an admission made without the store's check, because
	// the store was down.
	Waived() bool
	// OverBudget reports a call admitted over its scope's budget, which only
	// Warn and Observe allow. It also returns the known spend admission
	// read.
	OverBudget() (over bool, knownSpend int64)
}

// Meter is one per-minute bucket of requests that a request is charged to,
// a member's or a scope's rate limit. A site's gateways share each one. It
// counts requests only. Providers enforce tokens with their own 429s, and
// budgets account for them.
type Meter struct {
	Key       string // MemberRateMeter, ScopeRateMeter
	PerMinute int64
}

// The meters' keys.
func MemberRateMeter(scope, principal string) string { return "rm:" + scope + "/" + principal }
func ScopeRateMeter(scope string) string             { return "rs:" + scope }

// Meters takes a call's request from its rate limits, once per call.
type Meters interface {
	// Take takes a request from every meter of ms, or from none. It reports
	// the index of the first meter without room and how long until it has
	// some. An unreachable store admits, since rate limits fail open.
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
