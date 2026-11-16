package proxy

import (
	"cmp"
	"context"
	"time"

	"github.com/chucklehead-dev/golem/gateway/admission"
	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/gateway/price"
	"github.com/chucklehead-dev/golem/gateway/telemetry"
	"github.com/chucklehead-dev/golem/lib/contract"
	libsnapshot "github.com/chucklehead-dev/golem/lib/snapshot"
)

// settlement ends an admitted call's ticket exactly once. It completes the
// ticket when the call's end is clear. Otherwise it cancels the ticket,
// which holds the slot for the provider's tail. With no limiter there is no
// ticket, and it counts nothing.
type settlement struct {
	ticket   admission.Ticket
	ctx      context.Context // the request's, never cancelled
	deadline time.Time       // the call's deadline, if the route sets one
	tail     time.Duration   // how long the provider may go on working
	ended    bool
}

// complete ends the ticket, charging cost.
func (a *settlement) complete(cost int64) {
	if a.ticket != nil {
		a.ticket.Complete(a.ctx, cost)
	}
	a.ended = true
}

// cancel ends the ticket charging seen, and holds the slot for the
// provider's tail from now. The hold ends no later than the call's
// deadline.
func (a *settlement) cancel(seen int64) {
	if a.ticket != nil {
		until := time.Now().Add(a.tail)
		if !a.deadline.IsZero() && a.deadline.Before(until) {
			until = a.deadline
		}
		a.ticket.Cancel(a.ctx, seen, until)
	}
	a.ended = true
}

// chargeUnanswered returns the cost of a call whose round trip failed, and
// sets its row's outcome. Absent response bytes are not absent work, so
// only a failure the provider cannot have seen is free. Otherwise the call
// is charged an estimate, which reconciliation corrects. If a sweep ended
// the call, it is charged as if its caller left, and its row says
// terminated.
func chargeUnanswered(call *telemetry.Call, err error, callerLeft bool, ended *errTerminated, requestBytes int64, req scanned, pr libsnapshot.Price) int64 {
	call.Outcome, call.ErrorOrigin, call.ErrorCode = coded(outcomeFailed, contract.UpstreamUnavailable)
	if ended != nil && !callerLeft {
		call.Outcome, call.ErrorOrigin, call.ErrorCode = coded(outcomeTerminated, ended.code)
	}
	var cost int64
	switch {
	case !reachedProvider(err):
		if callerLeft { // free, and the caller's doing rather than the provider's
			call.Outcome, call.ErrorOrigin, call.ErrorCode = outcomeCancelled, string(contract.OriginClient), ""
		}
	case callerLeft: // the caller left first, but the provider read its input
		call.Outcome, call.ErrorOrigin, call.ErrorCode = outcomeCancelled, string(contract.OriginClient), ""
		cost = price.Cost(canon.Usage{Input: requestBytes}, pr) // a token per byte, as Worst bounds it
	case ended != nil: // a sweep ended it, charged as a caller leaving is
		cost = price.Cost(canon.Usage{Input: requestBytes}, pr)
	default: // a timeout or reset after the request went out, charged its worst case
		call.Outcome = "uncertain"
		var ok bool
		if cost, ok = price.Worst(requestBytes, cmp.Or(req.maxTokens, uncertainOutput), req.n, pr); !ok {
			cost = price.MaxCost // too large to price, so the most a call is charged
		}
	}
	if cost > 0 {
		call.Cost, call.EstimatedFraction = cost, 1
	}
	return cost
}

// uncertainOutput is the output assumed for an uncertain call that set no
// max_tokens, which OpenAI makes optional. It is a long answer.
const uncertainOutput = 8192
