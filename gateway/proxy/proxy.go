// Package proxy is the request path. It authenticates a call, reads and
// scans its body, routes it by alias and admits it against the scope's
// budget and in-flight limits. It then sends the call upstream, failing
// over before the commit, and streams the response back while reading its
// usage. Last, it accounts for the call. Hooks will go at the marked place.
// The order of the steps is in gateway.md.
//
// Package variables marked read-only are never written after init.
package proxy

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"maps"
	"math"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"uuid"

	"github.com/chucklehead-dev/golem/gateway/admission"
	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/gateway/envelope"
	"github.com/chucklehead-dev/golem/gateway/guard"
	"github.com/chucklehead-dev/golem/gateway/price"
	"github.com/chucklehead-dev/golem/gateway/route"
	"github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/gateway/telemetry"
	"github.com/chucklehead-dev/golem/lib/contract"
	libsnapshot "github.com/chucklehead-dev/golem/lib/snapshot"
)

// Snapshots gives the configuration a request uses. A request takes it once,
// at its start, and keeps it to its end.
type Snapshots interface {
	Current() (*snapshot.Compiled, error)
}

// Result is what a finished call reports to the tests' record hook.
type Result struct {
	RequestID uuid.UUID
	Principal *auth.Principal
	Target    *route.Target
	Cost      int64 // micro-units, from the usage and the snapshot's price
	// CallerGone is set when the caller went away mid-response.
	CallerGone bool
	// Terminated is set when the gateway ended the call because its
	// authorization was withdrawn (InFlight.Sweep).
	Terminated bool
	Status     int
	Usage      canon.Usage
	Err        error // a malformed usage report or a cut stream
}

// Handler serves one dialect's endpoint.
type Handler struct {
	Dialect   Dialect   // the API served: its body, headers and error shapes
	Snapshots Snapshots // the configuration, taken once per request
	// Limiter admits calls against budgets and in-flight limits; golem's is
	// limit.Limiter.Admitter. Nil admits every call uncounted.
	Limiter admission.Admitter
	MaxBody int64 // request body cap in bytes (default 32 MiB)
	// Telemetry records each request's span, metrics and ledger row. Nil
	// records nothing.
	Telemetry *telemetry.Telemetry
	// BodyTimeout bounds the time to read a request's body (default 30 s).
	BodyTimeout time.Duration
	// MaxResponse bounds the bytes of a provider's response forwarded
	// (default 64 MiB). A longer response is cut.
	MaxResponse int64
	// ProviderTail is how long a call's slot stays held after an ambiguous
	// end, such as a cut or an abandoned response, because the provider may
	// still be working. The call's deadline caps it (default 2 minutes).
	ProviderTail time.Duration
	// WriteStall bounds how long one write to the caller may block (default
	// 30 s). Without it, a client that stops reading would hold its slot and
	// its upstream connection for as long as the provider streams.
	WriteStall time.Duration
	// InFlight, if set, registers each admitted call, so that a revocation
	// or a denylist entry ends the calls it withdraws. The gateway's
	// handlers share one.
	InFlight *InFlight
	// Health holds the deployments' health and the routes' retry budgets,
	// shared by the gateway's handlers. Nil gives the handler its own.
	Health *route.Health
	// Meters takes each call's request from its rate limits; golem's is
	// limit.Limiter. Nil applies no rate limits.
	Meters admission.Meters
	// Guard bounds each source's requests and failed authentications before
	// authentication, shared by the gateway's handlers. Nil applies none.
	Guard *guard.Guard
	// MaxKept bounds the bytes of request bodies kept for retries until
	// their calls commit (default 256 MiB). A call past it makes one attempt.
	MaxKept int64
	// Reading bounds the request bodies being read at once, before
	// admission, shared by the gateway's handlers. Nil gives the handler
	// its own, with the defaults.
	Reading *Reading
	// record, if set, is told each call's result. Tests set it.
	record func(*http.Request, Result)
	// beforePick, if set, runs before each attempt's deployment is picked,
	// the first included. The attempt model's tests use it to pace calls.
	beforePick func(*http.Request)
	// beforeSend, if set, runs before each attempt's round trip with the
	// upstream context. Tests use it to end that context before the send.
	beforeSend func(context.Context)

	healthOnce  sync.Once
	ownHealth   *route.Health
	readingOnce sync.Once
	ownReading  *Reading
	kept        atomic.Int64 // bytes of bodies kept for retries
}

// forwardResponse lists the provider's response headers returned to the
// caller. golem's own headers are never among them (contracts.md).
var forwardResponse = []string{"Content-Type", "Request-Id", "X-Request-Id", "Retry-After"} // read-only

var jsonType = []string{"application/json"} // read-only

// ServeHTTP serves one call, top to bottom: every refusal is written in the
// caller's dialect, and every admitted call is accounted for exactly once,
// whatever ends it.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	ctx, span := h.Telemetry.Start(r.Context(), h.Dialect.Name())
	defer span.End()
	reqID, reqIDs := newRequestID()
	w.Header()[contract.HeaderRequestID] = []string{reqIDs}

	gen := h.InFlight.generation() // read before the keys are, see InFlight.add
	var grant auth.Grant
	ex := getExchange()
	defer ex.release()
	in := getBody()
	defer func() {
		if in != nil {
			putBody(in)
		}
	}()
	// Prepare the request and apply the scope's policy. Hooks go here.
	p, no := h.prepare(w, r, ex, in, &grant)
	if no == nil {
		no = h.policy(ex, p.principal, p.req, int64(len(p.body)))
	}
	if no != nil {
		h.fail(ctx, w, reqIDs, &p, no)
		return
	}

	c := &callState{}
	c.init(h, ctx, w, r, ex, &p, reqID, reqIDs)
	h.runBeforePick(r)
	// Refuse a request no deployment's betas allow, before anything is
	// counted. ADR 0028.
	if no := incapable(p.rt, c.betas); no != nil {
		h.fail(ctx, w, reqIDs, &p, no)
		return
	}
	// Take one request from the rate limits, once per call.
	if no := h.takeRate(c.bg, c.principal); no != nil {
		h.fail(ctx, w, reqIDs, &p, no)
		return
	}
	var retryAt time.Time
	var own bool
	if c.target, c.hold, retryAt, own = c.next(); c.target == nil {
		h.fail(ctx, w, reqIDs, &p, noDeployment(retryAt, own))
		return
	}
	defer func() { c.hold.Release() }() // the last attempt's hold, when the call is over
	// Every admitted call's ticket ends exactly once. If a bug panics before
	// it ends, cancel the ticket at its worst case so the slot is not held
	// forever. The panic's report is the record, so no ledger row is written.
	// A cut response's abort panics only after its ticket ended.
	defer c.cancelUnended()
	defer c.unkeep()

	if no := c.begin(&p, started); no != nil {
		h.fail(ctx, w, reqIDs, &p, no)
		return
	}

	// The route's timeouts bound all the call's attempts together. The
	// caller leaving still cancels them too.
	lim := &ex.lim
	c.upCtx = lim.bound(ctx, started, p.rt)
	defer lim.end()
	if h.InFlight != nil {
		// Register the call for its upstream life. Defers run last in, first
		// out, so it is removed before its context is released.
		t := h.InFlight.add(&ex.call, &grant, lim.cancel, reqID[15], gen)
		defer h.InFlight.remove(&ex.call)
		if t != nil { // withdrawn as it registered, so the provider never had it
			c.endWithdrawn(t)
			return
		}
	}

	resp, upBody, streamed, committed := c.attempts(ex, p.snap, &in, p.body)
	if !committed {
		return
	}
	// The attempt committed, so its response is the caller's and the
	// request body is no longer needed.
	if in != nil {
		putBody(in)
		in = nil
	}
	c.respond(ex, resp, upBody, streamed)
}

// init sets up the call's state once its request is prepared. It fills the
// struct in place, so ServeHTTP's callState stays on the stack.
func (c *callState) init(h *Handler, ctx context.Context, w http.ResponseWriter, r *http.Request, ex *exchange, p *prepared, reqID uuid.UUID, reqIDs string) {
	// Reuse the exchange's pooled slice; a new one would be heap-allocated.
	ex.tried = append(ex.tried[:0], make([]bool, len(p.rt.Pool))...)
	*c = callState{h: h, w: w, r: r, ctx: ctx, bg: context.WithoutCancel(ctx), principal: p.principal, req: p.req, rt: p.rt,
		reqID: reqID, reqIDs: reqIDs, requestBytes: int64(len(p.body)), hl: h.health(), tried: ex.tried, betas: p.betas}
}

// begin admits the call at its first deployment and opens its accounting.
// That covers the ticket and its settlement, the body kept for retries, and
// the ledger row. A refusal leaves nothing to settle.
func (c *callState) begin(p *prepared, started time.Time) *refusal {
	h, rt := c.h, c.rt
	var deadline time.Time // the latest the call may end; slot renewals stop there
	if rt.MaxDuration > 0 {
		deadline = started.Add(rt.MaxDuration)
	}
	worst, ok := price.Worst(c.requestBytes, c.req.maxTokens, c.req.n, p.snap.Prices[c.target.Model])
	if !ok {
		return &refusal{code: contract.RequestInvalid, msg: "the request's token count is too large to price safely: lower max_tokens or n"}
	}
	ticket, no := h.admit(c.ctx, c.principal, admission.Call{ID: c.reqIDs, Alias: rt.Alias, Deployment: c.target.Key(),
		Model: c.target.Model, Started: started, Deadline: deadline})
	if no != nil {
		return no
	}
	// Only an admitted call earns its route retries. Refused calls would
	// otherwise refill the budget that a failing pool's retries draw on.
	c.hl.Called(rt.Alias)
	// The settlement uses a context the caller cannot cancel, so the
	// accounting always runs to its end.
	c.acct = settlement{ticket: ticket, ctx: c.bg, deadline: deadline, tail: cmp.Or(h.ProviderTail, 2*time.Minute)}
	c.worst = worst

	// A retry rewrites the caller's body for the next deployment, so the body
	// is kept until the call commits. Past MaxKept the call makes one attempt
	// rather than take more memory.
	c.kept = rt.MaxAttempts > 1 && h.keep(c.requestBytes)

	pr := c.principal
	c.row = telemetry.Call{RequestID: c.reqIDs, Principal: pr.ID, PrincipalKind: pr.Kind, Key: pr.Key,
		Scope: pr.Scope, Frontend: h.Dialect.Name(), Backend: rt.Dialect, Alias: rt.Alias, Started: started,
		Currency: p.snap.Snapshot.Currency, Waived: ticket != nil && ticket.Waived(), ProviderAccount: p.snap.Accounts[c.target.Backend]}
	if ticket != nil {
		c.row.Period = ticket.Period() // admission's period; a call has one
		c.row.OverBudget = h.overBudget(c.w, pr, ticket)
	}
	return nil
}

// cancelUnended cancels the call's ticket at its worst case if nothing has
// ended it, which only a panic causes. A call refused before admission has
// no ticket, so then it counts nothing.
func (c *callState) cancelUnended() {
	if !c.acct.ended {
		c.acct.cancel(c.worst)
	}
}

// unkeep returns the call's kept body to the handler's bound on bodies kept.
func (c *callState) unkeep() {
	if c.kept {
		c.h.unkeep(c.requestBytes)
	}
}

// attempts tries deployments in pool order. An attempt the provider never
// received, or refused with 429/503/529, is retried on the next untried
// deployment, within MaxAttempts and the retry budget. Admission happens
// once, at the first deployment, because a call is billed once.
//
// It returns the committed attempt's response and the reader of its body.
// committed is false when the call has already ended, refused or
// unanswered. The caller's body is released as soon as no retry can need
// it.
//
//quint:enforces attempts.oneCommit, attempts.withinBound
func (c *callState) attempts(ex *exchange, snap *snapshot.Compiled, in **[]byte, body []byte) (resp *http.Response, upBody io.Reader, streamed, committed bool) {
	h, r, req, rt, lim := c.h, c.r, c.req, c.rt, &ex.lim
	for c.attempt = 1; ; c.attempt++ {
		t := c.target
		c.price = snap.Prices[t.Model]
		c.row.Attempt, c.row.Model, c.row.Deployment, c.row.Provider = c.attempt, t.Model, t.Key(), t.Backend
		c.row.ProviderAccount, c.row.PriceVersion = snap.Accounts[t.Backend], c.price.Version
		c.row.Envelope = t.Envelope
		out := getBody()
		if n := len(body) + len(t.Model) + 64; cap(*out) < n {
			*out = make([]byte, 0, n)
		}
		*out = ex.rewrite(h.Dialect, (*out)[:0], body, t.Model)
		if !c.kept && *in != nil {
			putBody(*in) // no retry can need the caller's body
			*in, body = nil, nil
		}
		if c.attempt > 1 {
			lim.nextAttempt(rt)
		}
		up, ub := upstreamRequest(c.attemptContext(), r, t, out, h.Dialect.forwardRequest())
		if h.beforeSend != nil {
			h.beforeSend(c.upCtx)
		}
		if c.upCtx.Err() != nil {
			// The call ended before this attempt was sent, so the provider
			// never had it. A route's timeout, the caller or a sweep can end it.
			// Sending now would fail with the context's error, which reads as
			// a request that may have gone out.
			ub.Close()
			c.endUnanswered(errUnsent)
			return nil, nil, false, false
		}
		var err error
		resp, err = t.Transport.RoundTrip(up)
		if err != nil {
			ub.Close()
			if !reachedProvider(err) && c.retry(telemetry.AttemptUnreached, 0, 0) {
				continue
			}
			c.endUnanswered(err)
			return nil, nil, false, false
		}
		lim.gotHeaders()
		c.row.ProviderRequestID = cmp.Or(resp.Header.Get("Request-Id"), resp.Header.Get("X-Request-Id"))
		if retryable(resp.StatusCode) && c.retry(telemetry.AttemptRefused, resp.StatusCode, providerRetryAfter(resp)) {
			resp.Body.Close()
			continue
		}
		streamed = req.stream && resp.StatusCode == http.StatusOK
		upBody = lim.bodyOf(resp.Body)
		if streamed {
			// Nothing is written until the first event (commit.go). An error
			// event first is an attempt that failed before the commit. It is
			// retried or refused as retryable, and the provider bills nothing.
			if ex.first.await(h.Dialect, upBody, ex.buf) {
				resp.Body.Close()
				if c.retry(telemetry.AttemptRefused, 0, providerRetryAfter(resp)) {
					continue
				}
				c.endFailedFirst(providerRetryAfter(resp))
				return nil, nil, false, false
			}
			// The stream failed or ended before its first event, so there is
			// nothing to commit. The provider had the request, so the call ends
			// as uncertain. Its hold fails and its slot is held for the tail.
			if err := ex.first.unanswered(); err != nil {
				resp.Body.Close()
				c.endUnanswered(err)
				return nil, nil, false, false
			}
			upBody = &ex.first
		}
		c.row.Committed = time.Now() // a stream's first event, or a whole response's headers
		return resp, upBody, streamed, true
	}
}

// callState is a call's state from its first pick to its end, shared by
// its attempts and its settlement.
type callState struct {
	h            *Handler
	w            http.ResponseWriter
	r            *http.Request
	ctx          context.Context // the call's, carrying its span
	bg           context.Context // ctx without cancellation, for the meters and the accounting
	principal    *auth.Principal
	req          scanned
	rt           *route.Route
	reqID        uuid.UUID
	reqIDs       string
	requestBytes int64 // for an estimate, once the body is gone

	// The attempts. tried marks the pool's deployments already tried. target
	// is the one being attempted, with its hold and its price.
	hl      *route.Health
	tried   []bool
	betas   []string
	target  *route.Target
	hold    *route.Hold
	price   libsnapshot.Price
	attempt int
	kept    bool  // the caller's body is kept for a retry
	worst   int64 // the call's worst-case cost, charged if a panic ends it

	row   telemetry.Call  // the current attempt's ledger row
	acct  settlement      // ends the call's ticket
	upCtx context.Context // the upstream requests' context, bounded by the route's timeouts
}

// attemptContext is the attempt's upstream context. An envelope may pass
// the attempt's attribution to its provider, so that the provider's logs
// join the ledger per request. Only an envelope's calls pay for this.
func (c *callState) attemptContext() context.Context {
	if c.target.Envelope == "" {
		return c.upCtx
	}
	p := c.principal
	return envelope.WithAttribution(c.upCtx, &envelope.Attribution{RequestID: c.reqIDs, AttemptID: c.row.AttemptID(),
		Principal: p.ID, Scope: p.Scope, Key: p.Key})
}

// endWithdrawn ends a call that a sweep withdrew as it registered in
// flight. The provider never had it, so it costs nothing.
func (c *callState) endWithdrawn(t *errTerminated) {
	c.row.Model, c.row.Deployment, c.row.Provider = c.target.Model, c.target.Key(), c.target.Backend
	c.row.Ended, c.row.Status = time.Now(), t.code.Status
	c.row.Outcome, c.row.ErrorOrigin, c.row.ErrorCode = coded(outcomeTerminated, t.code)
	c.h.Telemetry.Ended(c.ctx, c.row)
	c.acct.complete(0)
	Refuse(c.w, c.h.Dialect, t.code, t.message())
}

// endUnanswered ends a call whose last round trip failed. The call is
// charged what the provider may have seen (chargeUnanswered). It is
// refused as upstream.unavailable, naming the route's timeout if one cut
// it, or with the refusal of the sweep that withdrew it. An attempt never
// sent says nothing of its deployment, so its hold is released.
func (c *callState) endUnanswered(err error) {
	callerLeft := c.r.Context().Err() != nil
	code, msg := contract.UpstreamUnavailable, unavailableMessage(context.Cause(c.upCtx)) // the cause as the call failed
	ended := terminatedBy(c.upCtx)
	c.endHold(0, callerLeft || ended != nil || errors.Is(err, errUnsent))
	if ended != nil && !callerLeft { // no headers sent yet, so the dialect's error
		code, msg = ended.code, ended.message()
	}
	c.row.Ended, c.row.Status = time.Now(), code.Status
	c.row.AttemptResult = telemetry.AttemptUnreached
	if reachedProvider(err) {
		c.row.AttemptResult = telemetry.AttemptUncertain
	}
	cost := chargeUnanswered(&c.row, err, callerLeft, ended, c.requestBytes, c.req, c.price)
	c.h.Telemetry.Ended(c.ctx, c.row)
	if cost == 0 {
		c.acct.complete(0) // the provider never had the request
	} else {
		c.acct.cancel(cost)
	}
	Refuse(c.w, c.h.Dialect, code, msg)
}

// endFailedFirst ends a call whose stream's first event was an error and
// which is not retried. It is refused as retryable, with the provider's
// Retry-After, and charged nothing.
func (c *callState) endFailedFirst(after time.Duration) {
	code := contract.UpstreamUnavailable
	c.row.Ended, c.row.Status = time.Now(), code.Status
	c.row.Outcome, c.row.ErrorOrigin, c.row.ErrorCode = coded(outcomeFailed, code)
	c.row.AttemptResult = telemetry.AttemptRefused
	c.h.Telemetry.Ended(c.ctx, c.row)
	c.acct.complete(0)
	if c.h.record != nil {
		c.h.record(c.r, Result{RequestID: c.reqID, Principal: c.principal, Target: c.target, Status: code.Status})
	}
	refuse(c.w, c.h.Dialect, code, "the provider failed before its response began (its first event was an error); retry shortly", after)
}

// respond relays the committed attempt's response to the caller, then
// prices it, writes its row and ends its ticket. A response the provider
// cut is aborted, never ended cleanly.
func (c *callState) respond(ex *exchange, resp *http.Response, upBody io.Reader, streamed bool) {
	h, w := c.h, c.w
	defer resp.Body.Close()
	// The deployment answered. A provider's 4xx is the request's fault, not
	// the deployment's health.
	if retryable(resp.StatusCode) || resp.StatusCode >= 500 {
		c.hold.Fail(providerRetryAfter(resp))
	} else {
		c.hold.Commit()
	}
	copyHeaders(w.Header(), resp.Header, forwardResponse)
	w.WriteHeader(resp.StatusCode)
	res := Result{RequestID: c.reqID, Principal: c.principal, Target: c.target, Status: resp.StatusCode}
	res.Usage, res.Err = h.relay(w, upBody, ex, streamed)
	// The caller is gone if a write to it failed, or if its request ended
	// first. The upstream read then fails with the request's context.
	res.CallerGone = ex.callerGone || c.r.Context().Err() != nil
	_, cut := errors.AsType[errUpstreamCut](res.Err)
	// A sweep may have cut the response by cancelling the upstream context.
	// That is the gateway's doing, not the caller's.
	var ended *errTerminated
	if cut {
		ended = terminatedBy(c.upCtx)
		res.Terminated = ended != nil && !res.CallerGone
	}
	if resp.StatusCode == http.StatusOK { // a provider bills no error response
		// Charge a cut or unreported response for what was produced, counted
		// from what was seen. The row says how much of the cost is an
		// estimate. Work the provider did is never free. ADR 0031.
		var estimated canon.Usage
		res.Usage, estimated = ex.estimate(h.Dialect, c.requestBytes, outputCeiling(c.req))
		if est := price.Cost(estimated, c.price); est > 0 {
			c.row.EstimatedFraction = float64(est) / float64(max(price.Cost(res.Usage, c.price), 1))
		}
	}
	res.Cost = price.Cost(res.Usage, c.price)

	c.row.Ended, c.row.Status, c.row.Usage, c.row.Cost = time.Now(), resp.StatusCode, res.Usage, res.Cost
	c.row.Outcome, c.row.ErrorOrigin, c.row.ErrorCode = outcome(res, cut || streamed && ex.streamErr, ended)
	c.row.AttemptResult = telemetry.AttemptCommitted
	if retryable(resp.StatusCode) { // the last attempt's refusal, relayed
		c.row.AttemptResult = telemetry.AttemptRefused
	}
	h.Telemetry.Ended(c.ctx, c.row)
	switch {
	case c.acct.ticket == nil: // no limiter, nothing counted
	case res.CallerGone || cut:
		// The response was abandoned or cut. Charge what was seen and hold the
		// slot for the provider's tail, since the provider may go on working
		// after the connection closes. ADR 0041.
		resp.Body.Close()
		c.acct.cancel(res.Cost)
	default:
		c.acct.complete(res.Cost)
	}
	if h.record != nil {
		h.record(c.r, res)
	}
	if cut && !res.CallerGone {
		// The response ended early, and a clean end would read as a complete
		// answer. A stream gets the contract's error event, or the refusal of
		// the sweep that ended it. Then the response is aborted, a reset
		// rather than an end, so no client takes it for whole (contracts.md).
		if streamed {
			ev := streamError(h.Dialect, c.reqIDs)
			if res.Terminated {
				ev = errorEvent(h.Dialect, ended.code, ended.message(), c.reqIDs)
			}
			ex.send(ev, true) // bounded by WriteStall, as every write to the caller is
		}
		panic(http.ErrAbortHandler)
	}
}

// newRequestID mints a call's ID and its text. A UUIDv7 is time-ordered, so
// the ledger and the logs sort by it.
func newRequestID() (uuid.UUID, string) {
	id := uuid.NewV7()
	var buf [36]byte
	text, _ := id.AppendText(buf[:0]) // never fails
	return id, string(text)
}

// unavailableMessage is upstream.unavailable's message. It names the
// route's timeout if one cut the call, and otherwise asks for a retry.
func unavailableMessage(cause error) string {
	if errors.Is(cause, errHeadersTimeout) || errors.Is(cause, errMaxDuration) {
		return cause.Error() + "; retry, or ask the platform team about this route's timeouts"
	}
	return "the provider could not be reached; retry shortly"
}

// errUnsent is an attempt not sent, because the call's upstream context
// ended before its round trip began.
var errUnsent = errors.New("the call ended before the attempt was sent")

// reachedProvider reports whether a failed round trip may have reached the
// provider. Only a few failures certainly did not. They are a name that did
// not resolve, a connection never made, a failed TLS handshake, and an
// envelope's request that never left the gateway (envelope.ErrNotSent).
//
//quint:enforces attempts.workWithinLimit
func reachedProvider(err error) bool {
	if errors.Is(err, envelope.ErrNotSent) || errors.Is(err, errUnsent) {
		return false
	}
	if _, ok := errors.AsType[*net.DNSError](err); ok {
		return false
	}
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return false
	}
	if _, ok := errors.AsType[tls.RecordHeaderError](err); ok {
		return false
	}
	op, ok := errors.AsType[*net.OpError](err)
	return !ok || op.Op != "dial"
}

// admit checks the call against its scope's budget and in-flight limits,
// and takes its slots. The principal's scope supplies the limits. With no
// limiter it returns a nil ticket and no refusal. ADR 0031.
func (h *Handler) admit(ctx context.Context, p *auth.Principal, c admission.Call) (admission.Ticket, *refusal) {
	if h.Limiter == nil {
		return nil, nil
	}
	lim := p.Limits
	c.Principal, c.Scope = p.ID, p.Scope
	c.Budget, c.BudgetMode, c.PrincipalLimit, c.ScopeLimit = lim.BudgetMicros, lim.BudgetMode, lim.PrincipalInFlight, lim.ScopeInFlight
	t, err := h.Limiter.Admit(ctx, c)
	if no, ok := errors.AsType[*admission.Refusal](err); ok {
		return nil, &refusal{code: no.Code, msg: no.Msg, retryAfter: no.RetryAfter}
	}
	switch {
	case err != nil && ctx.Err() != nil:
		return nil, &refusal{callerGone: true}
	case err != nil:
		return nil, &refusal{code: contract.GatewayInternal, msg: "the limiter failed"}
	}
	return t, nil
}

// overBudget reports whether an admitted call is over its scope's budget,
// for the ledger row. Only a scope that observes or warns admits such a
// call. In warn mode the response's golem-warning header says so
// (contracts.md). It is set before the provider's headers are copied.
func (h *Handler) overBudget(w http.ResponseWriter, p *auth.Principal, t admission.Ticket) bool {
	over, spend := t.OverBudget()
	if over && p.Limits.BudgetMode == admission.Warn {
		w.Header()[contract.HeaderWarning] = []string{budgetWarning(spend, p.Limits.BudgetMicros)}
	}
	return over
}

// budgetWarning is warn mode's golem-warning value. It is the refusal's
// code and the share of the budget that known spend has reached, rounded
// down.
func budgetWarning(spend, limit int64) string {
	var pct int64
	switch {
	case limit <= 0:
	case spend > math.MaxInt64/100: // past any limit, so exactness does not matter
		pct = spend / limit * 100
	default:
		pct = spend * 100 / limit
	}
	return contract.BudgetPoolExhausted.Name + " " + strconv.FormatInt(pct, 10) + "%"
}

// takeRate takes the call's request from the rate limits its scope sets,
// the member's and the scope's. It takes from both or neither. A call past
// either is refused, naming which.
func (h *Handler) takeRate(ctx context.Context, p *auth.Principal) *refusal {
	var ms []admission.Meter
	if n := p.Limits.MemberRPM; n > 0 {
		ms = append(ms, admission.Meter{Key: admission.MemberRateMeter(p.Scope, p.ID), PerMinute: n})
	}
	if n := p.Limits.ScopeRPM; n > 0 {
		ms = append(ms, admission.Meter{Key: admission.ScopeRateMeter(p.Scope), PerMinute: n})
	}
	if len(ms) == 0 || h.Meters == nil {
		return nil
	}
	ok, over, wait := h.Meters.Take(ctx, ms)
	if ok {
		return nil
	}
	msg := "your rate limit in this scope is reached for this minute; retry after the Retry-After"
	if ms[over].Key == admission.ScopeRateMeter(p.Scope) {
		msg = "this scope's rate limit is reached for this minute; retry after the Retry-After"
	}
	return &refusal{code: contract.LimitRate, retryAfter: wait, msg: msg}
}

// upstreamRequest builds the provider's request from the rewritten body,
// the deployment's credential, and the caller's headers the provider may
// see.
func upstreamRequest(ctx context.Context, r *http.Request, target *route.Target, body *[]byte, forward []string) (*http.Request, *upstreamBody) {
	ub := newUpstreamBody(body)
	up := (&http.Request{
		Method:        http.MethodPost,
		URL:           target.URL,
		Header:        make(http.Header, 4+len(target.Header)),
		Body:          ub,
		ContentLength: int64(len(*body)),
		Host:          target.URL.Host,
	}).WithContext(ctx)
	up.Header["Content-Type"] = jsonType
	copyHeaders(up.Header, r.Header, forward)
	maps.Copy(up.Header, target.Header)
	return up, ub
}

// copyHeaders copies the named headers from src to dst. The two share the
// values, which neither side changes.
func copyHeaders(dst, src http.Header, names []string) {
	for _, k := range names {
		if v := src[k]; v != nil {
			dst[k] = v
		}
	}
}

// relay copies the provider's response to the caller as it arrives,
// flushing a stream at each read. It reads the usage from the same bytes.
// A stream's events are read as they pass. A whole response is read at its
// end, from a copy of up to maxRetained bytes.
func (h *Handler) relay(w http.ResponseWriter, body io.Reader, ex *exchange, streamed bool) (canon.Usage, error) {
	ex.startWrites(w, cmp.Or(h.WriteStall, 30*time.Second))
	filter := false
	if streamed {
		filter = ex.startStream(h.Dialect)
	} else {
		ex.kept, ex.overflow = ex.kept[:0], false
	}
	limit := cmp.Or(h.MaxResponse, defaultMaxResponse)
	var cut error
	for {
		n, err := body.Read(ex.buf)
		if ex.received += int64(n); ex.received > limit {
			cut = errUpstreamCut{errResponseTooLarge}
			break
		}
		if n > 0 && !ex.forward(ex.buf[:n], streamed, filter) {
			cut = errCallerGone
			break
		}
		if err == io.EOF {
			if filter && !ex.send(ex.filter.End(), true) {
				cut = errCallerGone
			}
			break
		}
		if err != nil {
			cut = errUpstreamCut{err}
			break
		}
	}
	if !streamed {
		ex.readWhole(h.Dialect)
	}
	return ex.usage(h.Dialect, cut)
}

// errResponseTooLarge cuts a response past MaxResponse.
var errResponseTooLarge = errors.New("the provider's response is larger than this gateway forwards")

const defaultMaxResponse = 64 << 20

// errCallerGone is a write to the caller that failed.
var errCallerGone = errors.New("proxy: the caller went away")

// errUpstreamCut is the provider's response ending before it was complete.
type errUpstreamCut struct{ err error }

func (e errUpstreamCut) Error() string {
	return "proxy: the provider's response was cut: " + e.err.Error()
}
func (e errUpstreamCut) Unwrap() error { return e.err }
