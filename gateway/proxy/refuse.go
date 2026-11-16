package proxy

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/chucklehead-dev/golem/gateway/telemetry"
	"github.com/chucklehead-dev/golem/lib/contract"
)

// defaultRetryAfter is a retryable refusal's Retry-After when its refuser
// gave none.
var defaultRetryAfter = []string{"5"} // read-only

// refusal is a request the path will not serve. It carries a contract code
// and a message that says what to do next.
type refusal struct {
	code       contract.Code
	msg        string
	retryAfter time.Duration // the refuser's, if it said when to retry
	callerGone bool          // the caller left first, so there is no one to tell
}

// The ledger outcomes of a call that did not complete (contracts.md). A
// terminated call is one the gateway ended because its authorization was
// withdrawn.
const (
	outcomeFailed     = "failed"
	outcomeCancelled  = "cancelled"
	outcomeTerminated = "terminated"
)

// coded returns an outcome with the origin and name of its contract code.
func coded(outcome string, c contract.Code) (string, string, string) {
	return outcome, string(c.Origin), c.Name
}

// outcome returns a finished call's ledger outcome, with its failure's
// origin and code (contracts.md). cut is set for a response that ended
// early, or a stream that sent the provider's error event after output
// began. Either is a failure, however much was delivered and charged.
func outcome(res Result, cut bool, ended *errTerminated) (outcome, origin, code string) {
	switch {
	case res.CallerGone:
		return outcomeCancelled, string(contract.OriginClient), ""
	case res.Terminated:
		return coded(outcomeTerminated, ended.code)
	case cut:
		return coded(outcomeFailed, contract.UpstreamStreamFailed)
	case res.Status >= 400:
		return outcomeFailed, string(contract.OriginProvider), ""
	}
	return "completed", "", ""
}

// fail refuses a request. It records the refusal with the principal and
// route, if the request got that far, and writes it in the caller's
// dialect.
func (h *Handler) fail(ctx context.Context, w http.ResponseWriter, reqID string, p *prepared, no *refusal) {
	if no.callerGone {
		return // no one to tell, and nothing refused
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

// Refuse writes a refusal in a dialect's error shape (contracts.md). It
// carries golem's code, a message that says what to do next, and the
// request ID if the response has one. Only contract codes can be sent.
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
