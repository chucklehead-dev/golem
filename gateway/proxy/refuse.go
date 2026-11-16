package proxy

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/chucklehead-dev/golem/gateway/telemetry"
	"github.com/chucklehead-dev/golem/lib/contract"
)

var defaultRetryAfter = []string{"5"} // read-only: a retryable refusal's, when its refuser gave none

// refusal is a request the path will not serve: a contract code and a
// message that says what to do next.
type refusal struct {
	code       contract.Code
	msg        string
	retryAfter time.Duration // the admitter's, if it said when to retry
	callerGone bool          // nothing to tell: the caller left first
}

// The ledger outcomes of a call that did not complete (contracts.md);
// terminated: the gateway ended it in flight, its authorization withdrawn.
const (
	outcomeFailed     = "failed"
	outcomeCancelled  = "cancelled"
	outcomeTerminated = "terminated"
)

// coded is an outcome with its failure's origin and code, a contract code's.
func coded(outcome string, c contract.Code) (string, string, string) {
	return outcome, string(c.Origin), c.Name
}

// outcome is a finished call's ledger outcome, and the failure's origin and
// code (contracts.md). cut is a response that ended early, or a stream
// that carried its provider's error event after its output began: either
// is a failure, however much was delivered (and charged) before it.
func outcome(res Result, cut bool, ended *errTerminated) (outcome, origin, code string) {
	switch {
	case res.CallerGone:
		return outcomeCancelled, string(contract.OriginClient), ""
	case res.Terminated:
		return coded(outcomeTerminated, ended.code)
	case cut: // or a stream whose provider sent its error event
		return coded(outcomeFailed, contract.UpstreamStreamFailed)
	case res.Status >= 400:
		return outcomeFailed, string(contract.OriginProvider), ""
	}
	return "completed", "", ""
}

// fail refuses a request: the refusal recorded (with as much of p as the
// request got to: its principal, its route), and written in the caller's
// dialect.
func (h *Handler) fail(ctx context.Context, w http.ResponseWriter, reqID string, p *prepared, no *refusal) {
	if no.callerGone {
		return // no one to tell, nothing refused
	}
	r := telemetry.Refusal{Code: no.code, Message: no.msg, RequestID: reqID}
	if pr := p.principal; pr != nil {
		r.Principal, r.Scope, r.Key = pr.ID, pr.Scope, pr.Key
	}
	if p.rt != nil {
		r.Alias = p.rt.Alias
	}
	h.Telemetry.Refused(ctx, r)
	refuse(w, h.Dialect, no.code, no.msg, no.retryAfter)
}

// Refuse writes a refusal in a dialect's error shape, with golem's code, a
// message that says what to do next, and the request ID if the response has
// one (contracts.md). Only contract codes can be sent.
func Refuse(w http.ResponseWriter, d Dialect, c contract.Code, msg string) { refuse(w, d, c, msg, 0) }

// refuse is Refuse with the refuser's own Retry-After, when it gave one.
func refuse(w http.ResponseWriter, d Dialect, c contract.Code, msg string, after time.Duration) {
	w.Header()["Content-Type"] = jsonType
	switch {
	case after > 0:
		w.Header()["Retry-After"] = []string{strconv.FormatInt(int64((after+time.Second-1)/time.Second), 10)}
	case c.RetryAfter():
		w.Header()["Retry-After"] = defaultRetryAfter
	}
	w.WriteHeader(c.Status)
	var reqID string
	if v := w.Header()[contract.HeaderRequestID]; len(v) > 0 {
		reqID = v[0]
	}
	w.Write(appendError(nil, d, c, msg, reqID))
}
