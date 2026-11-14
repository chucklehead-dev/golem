package proxy

import (
	"context"
	"time"

	"github.com/chucklehead-dev/golem/gateway/limit"
)

// Admitter is the request path's view of the limiter (ADR 0031): admission
// after routing, and an account the call keeps to its end. The proxy
// depends on this, not on gateway/limit's implementation, so a gateway
// without budgets, another accounting backend or a test double plugs in
// here. Budgets, concurrency and accounting are a fixed stage of the path,
// not a hook (ADR 0014's hooks are stateless per phase, and their Complete
// phase may drop work); limits that only refuse can be Request hooks.
//
// Admit refuses with limit.ErrBudget (402) or limit.ErrConcurrency (429).
type Admitter interface {
	Admit(ctx context.Context, c limit.Call) (Ticket, error)
}

// Ticket is an admitted call's account: completed with its cost, or
// cancelled with the cost seen, its slot then held to EndAt (the provider's
// tail). Every admitted call ends in exactly one of Complete or Cancel.
type Ticket interface {
	Complete(ctx context.Context, cost int64)
	Cancel(ctx context.Context, seen int64)
	EndAt(at time.Time)
	// Period is the budget period the call was counted in, for its row.
	Period() string
	// Waived reports an admission outside the bound (the store was down).
	Waived() bool
}

// Limits is golem's limiter as an Admitter.
func Limits(l *limit.Limiter) Admitter { return limiter{l} }

type limiter struct{ l *limit.Limiter }

func (l limiter) Admit(ctx context.Context, c limit.Call) (Ticket, error) {
	t, err := l.l.Admit(ctx, c)
	if t == nil { // a nil *limit.Ticket is not a nil Ticket
		return nil, err
	}
	return t, err
}
