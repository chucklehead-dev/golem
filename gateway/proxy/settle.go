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

// settlement ends an admitted call's ticket, exactly once: by Complete when
// the call's end is clear, else by Cancel, which holds the slot for the
// provider's tail. With no ticket (no limiter) it counts nothing.
type settlement struct {
	ticket   admission.Ticket
	ctx      context.Context // the request's, never cancelled
	deadline time.Time       // the call's, if the route bounds it
	tail     time.Duration   // the provider's tail
	ended    bool
}

func (a *settlement) complete(cost int64) {
	if a.ticket != nil {
		a.ticket.Complete(a.ctx, cost)
	}
	a.ended = true
}

// cancel charges seen and holds the slot until the provider's tail from
// now, but no later than the call's deadline.
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

// chargeUnanswered is the cost of a call whose round trip failed, and sets
// its row's outcome. Absent response bytes are not absent work:
// only a failure the provider cannot have seen is free. Else the call is
// charged an estimate, which reconciliation corrects, and its slot is held
// for the provider's tail. ended is the sweep that cancelled the call, if
// one did: it is ended as a caller gone is, and its row says terminated.
func chargeUnanswered(call *telemetry.Call, err error, callerLeft bool, ended *errTerminated, requestBytes int64, req scanned, pr libsnapshot.Price) int64 {
	call.Outcome, call.ErrorOrigin, call.ErrorCode = coded(outcomeFailed, contract.UpstreamUnavailable)
	if ended != nil && !callerLeft {
		call.Outcome, call.ErrorOrigin, call.ErrorCode = coded(outcomeTerminated, ended.code)
	}
	var cost int64
	switch {
	case !reachedProvider(err):
		if callerLeft { // free, and the caller's doing, not the provider's
			call.Outcome, call.ErrorOrigin, call.ErrorCode = outcomeCancelled, string(contract.OriginClient), ""
		}
	case callerLeft: // the caller left first: its input, at least, was read
		call.Outcome, call.ErrorOrigin, call.ErrorCode = outcomeCancelled, string(contract.OriginClient), ""
		cost = price.Cost(canon.Usage{Input: requestBytes}, pr) // a token a byte, as Worst bounds it
	case ended != nil: // the gateway ended it: likewise
		cost = price.Cost(canon.Usage{Input: requestBytes}, pr)
	default: // a timeout or a reset after the request went out: the worst it may have cost
		call.Outcome = "uncertain"
		var ok bool
		if cost, ok = price.Worst(requestBytes, cmp.Or(req.maxTokens, uncertainOutput), req.n, pr); !ok {
			cost = price.MaxCost // too large to price: the most a call is charged
		}
	}
	if cost > 0 {
		call.Cost, call.EstimatedFraction = cost, 1
	}
	return cost
}

// uncertainOutput is the output assumed of an uncertain call that set no
// max_tokens (OpenAI's is optional): a long answer.
const uncertainOutput = 8192
