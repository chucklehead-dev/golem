// Package proxy is the request path (gateway.md): authenticate, read and
// scan the body, route by alias, admit against the scope's budgets and
// in-flight limits, splice the model, send upstream (failing over before
// the commit), and stream the response back through a tee that reads
// usage, then account for the call. Hooks are a later step, at the marked
// place.
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

// Snapshots gives the configuration a request uses: taken once, at the
// start, and used to the end (gateway.md).
type Snapshots interface {
	Current() (*snapshot.Compiled, error)
}

// Result is what a finished call reports: the input to pricing, the
// ticket and the access log (gateway.md).
type Result struct {
	RequestID uuid.UUID
	Principal *auth.Principal
	Target    *route.Target
	Cost      int64 // micro-units, from the usage and the snapshot's price
	// CallerGone: the caller went away mid-response (a cancellation).
	CallerGone bool
	// Terminated: the gateway ended the call in flight, its authorization
	// withdrawn (InFlight.Sweep).
	Terminated bool
	Status     int
	Usage      canon.Usage
	Err        error // a malformed usage report or a cut stream
}

// Handler serves one dialect's endpoint.
type Handler struct {
	Dialect   Dialect   // the API served: its body, headers and error shapes
	Snapshots Snapshots // the configuration, taken once per request
	// Limiter admits calls against budgets and in-flight limits
	// (golem's: limit.Limiter.Admitter); nil admits everything uncounted.
	Limiter admission.Admitter
	MaxBody int64 // request body cap in bytes (default 32 MiB)
	// Telemetry records each request's span, metrics and ledger row; nil
	// records nothing.
	Telemetry *telemetry.Telemetry
	// BodyTimeout bounds reading a request's body (default 30 s);
	// MaxResponse, the bytes of a provider's response forwarded (default
	// 64 MiB; past it the response is cut).
	BodyTimeout time.Duration
	MaxResponse int64
	// ProviderTail is how long a call's slot is held after an ambiguous
	// end (a cut, an abandoned response, an upstream failure the provider
	// may have seen), bounded by the call's deadline: the provider may still
	// be working (default 2 minutes).
	ProviderTail time.Duration
	// WriteStall bounds how long one write to the caller may block (default
	// 30 s): a client that stops reading would otherwise hold its slot and
	// its upstream connection for as long as the provider streams.
	WriteStall time.Duration
	// InFlight, if set, registers each admitted call, so a revocation or a
	// denylist entry ends the calls it withdraws (InFlight.Sweep); the
	// gateway's handlers share one.
	InFlight *InFlight
	// Health is the deployments' health and the routes' retry budgets the
	// gateway's handlers share (route.Health); nil: the handler's own.
	Health *route.Health
	// Meters counts each call's requests against its rate limits
	// (gateway/limit's Limiter); nil: none.
	Meters admission.Meters
	// Guard is the floor before authentication the gateway's handlers
	// share (a source's requests and failed authentications); nil: none.
	Guard *guard.Guard
	// MaxKept bounds the request bodies the handler keeps for retries
	// until their calls commit (default 256 MiB); a call past it makes one
	// attempt.
	MaxKept int64
	// Reading bounds the request bodies being read at once, before
	// admission: the gateway's bytes and each principal's bodies (Reading);
	// the gateway's handlers share one. nil: the handler's own, with the
	// defaults.
	Reading *Reading
	// record, if set, is told each call's result (tests).
	record func(*http.Request, Result)
	// beforePick, if set, runs before each attempt's deployment is picked,
	// the first included (tests: the attempt model's traces drive when).
	beforePick func(*http.Request)
	// beforeSend, if set, runs before each attempt's round trip, given the
	// upstream context (tests: a context that ends before the send).
	beforeSend func(context.Context)

	healthOnce  sync.Once
	ownHealth   *route.Health
	readingOnce sync.Once
	ownReading  *Reading
	kept        atomic.Int64 // bytes of bodies kept for retries
}

// Response headers returned to the caller (contracts.md: golem's own
// are never forwarded); the request headers a caller may pass are its
// dialect's (Dialect.forwardRequest).
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

	gen := h.InFlight.generation() // before the keys are read: see InFlight.add
	var grant auth.Grant
	ex := getExchange()
	defer ex.release()
	in := getBody()
	defer func() {
		if in != nil {
			putBody(in)
		}
	}()
	// Authenticate, read, scan, route and check. (Hooks go here.)
	p, no := h.prepare(w, r, ex, in, &grant)
	if no == nil {
		no = h.policy(ex, p.principal, p.req, int64(len(p.body)))
	}
	if no != nil {
		h.fail(ctx, w, reqIDs, &p, no)
		return
	}
	body, req, rt := p.body, p.req, p.rt

	// The call's attempts (model/attempts.md): the pool in order, each
	// attempt from a deployment its health admits; one refused or never
	// seen by its provider is retried, within the route's attempts and its
	// pool's retry budget, until one commits (commit.go). The budget and
	// the slots are the call's, admitted once at its first deployment (a
	// call bills at most once, so its bound needs no check per attempt).
	ex.tried = append(ex.tried[:0], make([]bool, len(rt.Pool))...) // pooled: held in c, a slice would escape
	c := &callState{h: h, w: w, r: r, ctx: ctx, bg: context.WithoutCancel(ctx), principal: p.principal, req: req, rt: rt,
		reqID: reqID, reqIDs: reqIDs, requestBytes: int64(len(body)), hl: h.health(), tried: ex.tried, betas: p.betas}
	h.paused(r)
	// Only deployments that accept the request's betas are its pool here
	// (ADR 0028): none, and it is refused before anything is counted.
	if no := incapable(rt, c.betas); no != nil {
		h.fail(ctx, w, reqIDs, &p, no)
		return
	}
	// The call takes a request from its rate limits once (prd.md).
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
	defer func() { c.hold.Release() }() // the last attempt's: when the call is over

	var deadline time.Time // the call's end at the latest: renewals stop there
	if rt.MaxDuration > 0 {
		deadline = started.Add(rt.MaxDuration)
	}
	worst, ok := price.Worst(c.requestBytes, req.maxTokens, req.n, p.snap.Prices[c.target.Model])
	if !ok {
		h.fail(ctx, w, reqIDs, &p, &refusal{code: contract.RequestInvalid, msg: "the request's token count is too large to price safely: lower max_tokens or n"})
		return
	}
	ticket, no := h.admit(ctx, c.principal, admission.Call{ID: reqIDs, Alias: rt.Alias, Deployment: c.target.Key(),
		Model: c.target.Model, Started: started, Deadline: deadline})
	if no != nil {
		h.fail(ctx, w, reqIDs, &p, no)
		return
	}
	// Only an admitted call earns its route retries: refused ones would
	// refill the budget a failing pool's retries draw on.
	c.hl.Called(rt.Alias)
	// The accounting runs to its end whatever the caller does.
	c.acct = settlement{ticket: ticket, ctx: c.bg, deadline: deadline, tail: cmp.Or(h.ProviderTail, 2*time.Minute)}
	// Every admitted call's ticket ends exactly once. Should anything below
	// panic before it does (a bug; the abort of a cut response comes after
	// its end), the call is cancelled at its declared worst case and its
	// slot held for the tail, rather than held for ever. Its row is not
	// written: the panic's report is the record.
	defer func() {
		if !c.acct.ended {
			c.acct.cancel(worst)
		}
	}()

	// A retry needs the caller's body, to rewrite for the next deployment:
	// it is kept until the call commits, within the gateway's bound on
	// bodies kept (past it the call makes one attempt, rather than taking
	// more memory).
	c.kept = rt.MaxAttempts > 1 && h.keep(c.requestBytes)
	if c.kept {
		defer h.unkeep(c.requestBytes)
	}

	pr := c.principal
	c.row = telemetry.Call{RequestID: reqIDs, Principal: pr.ID, PrincipalKind: pr.Kind, Key: pr.Key,
		Scope: pr.Scope, Frontend: h.Dialect.Name(), Backend: rt.Dialect, Alias: rt.Alias, Started: started,
		Currency: p.snap.Snapshot.Currency, Waived: ticket != nil && ticket.Waived(), ProviderAccount: p.snap.Accounts[c.target.Backend]}
	if ticket != nil {
		c.row.Period = ticket.Period() // admission's: one period per call
		c.row.OverBudget = h.overBudget(w, pr, ticket)
	}

	// The route's bounds, over all its attempts: the caller's going away
	// still cancels it too.
	lim := &ex.lim
	c.upCtx = lim.bound(ctx, started, rt)
	defer lim.end()
	if h.InFlight != nil {
		// Registered for the call's upstream life, taken off before its
		// context is released (defers run last in, first out).
		t := h.InFlight.add(&ex.call, &grant, lim.cancel, reqID[15], gen)
		defer h.InFlight.remove(&ex.call)
		if t != nil { // withdrawn as it was admitted: the provider never had it
			c.endWithdrawn(t)
			return
		}
	}

	var resp *http.Response
	var upBody io.Reader
	streamed := false
	for c.attempt = 1; ; c.attempt++ {
		t := c.target
		c.price = p.snap.Prices[t.Model]
		c.row.Attempt, c.row.Model, c.row.Deployment, c.row.Provider = c.attempt, t.Model, t.Key(), t.Backend
		c.row.ProviderAccount, c.row.PriceVersion = p.snap.Accounts[t.Backend], c.price.Version
		c.row.Envelope = t.Envelope
		out := getBody()
		if n := len(body) + len(t.Model) + 64; cap(*out) < n {
			*out = make([]byte, 0, n)
		}
		*out = ex.rewrite(h.Dialect, (*out)[:0], body, t.Model)
		if !c.kept && in != nil {
			putBody(in) // the caller's body is no longer needed
			in, body = nil, nil
		}
		if c.attempt > 1 {
			lim.nextAttempt(rt)
		}
		up, ub := upstreamRequest(c.attemptContext(), r, t, out, h.Dialect.forwardRequest())
		if h.beforeSend != nil {
			h.beforeSend(c.upCtx)
		}
		if c.upCtx.Err() != nil {
			// The call's context ended (a route's alarm, the caller, a sweep)
			// before this attempt was sent: the provider never had it. Sent,
			// the transport would fail at once with the context's error,
			// which reads as a request that may have gone out.
			ub.Close()
			c.endUnanswered(errUnsent)
			return
		}
		var err error
		resp, err = t.Transport.RoundTrip(up)
		if err != nil {
			ub.Close()
			if !reachedProvider(err) && c.retry(telemetry.AttemptUnreached, 0, 0) {
				continue
			}
			c.endUnanswered(err)
			return
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
			// Nothing is written until the first event (commit.go): one that
			// is an error is an attempt that failed before commit, retried or
			// refused as retryable, the provider billing nothing for it.
			if ex.first.await(h.Dialect, upBody, ex.buf) {
				resp.Body.Close()
				if c.retry(telemetry.AttemptRefused, 0, providerRetryAfter(resp)) {
					continue
				}
				c.endFailedFirst(providerRetryAfter(resp))
				return
			}
			// The stream failed or ended before its first event (a reset, the
			// idle alarm, the caller leaving): nothing to commit. The provider
			// had the request, so it ends as an attempt with no answer does:
			// uncertain, its hold failed, its slot held for the tail.
			if err := ex.first.unanswered(); err != nil {
				resp.Body.Close()
				c.endUnanswered(err)
				return
			}
			upBody = &ex.first
		}
		c.row.Committed = time.Now() // the first event in hand, or a whole response's headers
		break
	}
	// Committed: this attempt's response is the caller's, and the caller's
	// body is let go.
	if in != nil {
		putBody(in)
		in = nil
	}
	c.respond(ex, resp, upBody, streamed)
}

// callState is an admitted call as its attempts and its end share it.
type callState struct {
	h            *Handler
	w            http.ResponseWriter
	r            *http.Request
	ctx          context.Context // the span's
	bg           context.Context // ctx never cancelled: the meters' and the accounting's
	principal    *auth.Principal
	req          scanned
	rt           *route.Route
	reqID        uuid.UUID
	reqIDs       string
	requestBytes int64 // for an estimate, once the body is gone

	// The attempts: the deployments tried and the one being attempted,
	// with its hold from the deployment's health and its price.
	hl      *route.Health
	tried   []bool
	betas   []string
	target  *route.Target
	hold    *route.Hold
	price   libsnapshot.Price
	attempt int
	kept    bool // the caller's body is kept for a retry

	row   telemetry.Call  // the attempt's ledger row
	acct  settlement      // the call's ticket
	upCtx context.Context // the upstream requests', bounded by the route
}

// attemptContext is the attempt's upstream context. An envelope may pass
// the attempt's attribution to its provider (Bedrock's request metadata:
// the ledger's IDs in its invocation logs, so the two join per request).
// Only an envelope's calls pay for the context.
func (c *callState) attemptContext() context.Context {
	if c.target.Envelope == "" {
		return c.upCtx
	}
	p := c.principal
	return envelope.WithAttribution(c.upCtx, &envelope.Attribution{RequestID: c.reqIDs, AttemptID: c.row.AttemptID(),
		Principal: p.ID, Scope: p.Scope, Key: p.Key})
}

// endWithdrawn ends a call withdrawn as it registered in flight (t): the
// provider never had it, so it costs nothing.
func (c *callState) endWithdrawn(t *errTerminated) {
	c.row.Model, c.row.Deployment, c.row.Provider = c.target.Model, c.target.Key(), c.target.Backend
	c.row.Ended, c.row.Status = time.Now(), t.code.Status
	c.row.Outcome, c.row.ErrorOrigin, c.row.ErrorCode = coded(outcomeTerminated, t.code)
	c.h.Telemetry.Ended(c.ctx, c.row)
	c.acct.complete(0)
	Refuse(c.w, c.h.Dialect, t.code, t.message())
}

// endUnanswered ends a call whose last round trip failed (err): charged
// what the provider may have seen (chargeUnanswered), and refused as
// upstream.unavailable, naming the route's timeout if one cut it, or with
// the refusal of the sweep that withdrew it. An attempt never sent
// (errUnsent) says nothing of its deployment: its lease is released.
func (c *callState) endUnanswered(err error) {
	callerLeft := c.r.Context().Err() != nil
	code, msg := contract.UpstreamUnavailable, unavailableMessage(context.Cause(c.upCtx)) // the cause as the call failed
	ended := terminatedBy(c.upCtx)
	c.endHold(0, callerLeft || ended != nil || err == errUnsent)
	if ended != nil && !callerLeft { // the headers not yet sent: the dialect's error
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
		c.acct.complete(0) // the provider never had it
	} else {
		c.acct.cancel(cost)
	}
	Refuse(c.w, c.h.Dialect, code, msg)
}

// endFailedFirst ends a stream whose first event was an error, not
// retried: refused as retryable, with the provider's Retry-After (after),
// and charged nothing.
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

// respond relays the committed attempt's response (resp, read as upBody)
// to the caller, then prices it, writes its row and ends its ticket. A
// response the provider cut is aborted, never ended cleanly.
func (c *callState) respond(ex *exchange, resp *http.Response, upBody io.Reader, streamed bool) {
	h, w := c.h, c.w
	defer resp.Body.Close()
	// The deployment answered: a provider's 4xx is the request's fault,
	// not its health.
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
	// first (the upstream read then fails with its context).
	res.CallerGone = ex.callerGone || c.r.Context().Err() != nil
	_, cut := errors.AsType[errUpstreamCut](res.Err)
	// A cut of the gateway's own making: a sweep cancelled the upstream
	// request (its context, not the caller's, so not a caller gone).
	var ended *errTerminated
	if cut {
		ended = terminatedBy(c.upCtx)
		res.Terminated = ended != nil && !res.CallerGone
	}
	if resp.StatusCode == http.StatusOK { // a provider bills no error response
		// A cut or unreported response is charged what was produced, counted
		// from what was seen (ADR 0031), and its row says how much of its
		// cost is estimated; never zero for work the provider did.
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
	case c.acct.ticket == nil: // nothing counted
	case res.CallerGone || cut:
		// The response was abandoned or cut (by the provider, or by the gateway
		// ending the call): what was seen is charged (estimated where
		// unreported), and the slot is held for the provider's tail, which may
		// go on working after the connection closes (ADR 0041).
		resp.Body.Close()
		c.acct.cancel(res.Cost)
	default:
		c.acct.complete(res.Cost)
	}
	if h.record != nil {
		h.record(c.r, res)
	}
	if cut && !res.CallerGone {
		// The provider's response ended early. A clean end would read as a
		// complete (and shorter) answer: a stream gets the contract's error
		// event, and then the response is aborted (a reset, not an end), so
		// no client can take it for whole (contracts.md). A call the
		// gateway ended gets the refusal its caller would now get, as
		// that event (a whole response, already begun, is cut alone).
		if streamed {
			ev := streamError(h.Dialect, c.reqIDs)
			if res.Terminated {
				ev = errorEvent(h.Dialect, ended.code, ended.message(), c.reqIDs)
			}
			ex.send(ev, true) // bounded by the stall, as every write to the caller
		}
		panic(http.ErrAbortHandler)
	}
}

// newRequestID mints a call's ID, a UUIDv7: time-ordered, so the ledger and
// logs sort by it (contracts.md); and its text.
func newRequestID() (uuid.UUID, string) {
	id := uuid.NewV7()
	var buf [36]byte
	text, _ := id.AppendText(buf[:0]) // never fails
	return id, string(text)
}

// unavailableMessage is upstream.unavailable's message: the route's
// timeout, if one cut the call, else a retry.
func unavailableMessage(cause error) string {
	if cause == errHeadersTimeout || cause == errMaxDuration {
		return cause.Error() + "; retry, or ask the platform team about this route's timeouts"
	}
	return "the provider could not be reached; retry shortly"
}

// errUnsent is an attempt not sent: the call's upstream context had ended
// before its round trip began.
var errUnsent = errors.New("the call ended before the attempt was sent")

// reachedProvider reports whether a failed round trip may have reached the
// provider: only a name that did not resolve, a connection that was never
// made, a TLS handshake that failed or an envelope's request that never
// left the gateway (envelope.ErrNotSent: unsigned, unreadable) certainly
// did not.
func reachedProvider(err error) bool {
	if errors.Is(err, envelope.ErrNotSent) || err == errUnsent {
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

// admit checks the call against its scope's budget and its in-flight
// limits (ADR 0031), and takes its slots. c names the call and its route;
// the principal's scope supplies its limits. A nil ticket with no
// refusal: no limiter.
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

// overBudget reports whether an admitted call is over its scope's budget (its
// scope observes or warns rather than enforcing), for its ledger row; in warn
// mode the response says so in golem-warning, with the refusal's code and the
// share of the budget known spend has reached (contracts.md). It is set before
// the provider's headers are copied, which never include golem's.
func (h *Handler) overBudget(w http.ResponseWriter, p *auth.Principal, t admission.Ticket) bool {
	over, known := t.OverBudget()
	if over && p.Limits.BudgetMode == admission.Warn {
		w.Header()[contract.HeaderWarning] = []string{budgetWarning(known, p.Limits.BudgetMicros)}
	}
	return over
}

// budgetWarning is warn mode's golem-warning: the refusal's code, and the
// share of the budget known spend has reached, rounded down.
func budgetWarning(known, limit int64) string {
	var pct int64
	switch {
	case limit <= 0:
	case known > math.MaxInt64/100: // past any limit: not worth exactness
		pct = known / limit * 100
	default:
		pct = known * 100 / limit
	}
	return contract.BudgetPoolExhausted.Name + " " + strconv.FormatInt(pct, 10) + "%"
}

// takeRate takes the call's request from its scope's rate limits (prd.md): its
// member's and its scope's, those the scope sets, together or not at all; past
// one, the call is refused, naming which.
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

// upstreamRequest is the provider's request: the rewritten body, the
// route's credential, and the caller headers the provider may see.
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

// copyHeaders copies the named headers from src to dst, sharing their
// values (neither side changes them).
func copyHeaders(dst, src http.Header, names []string) {
	for _, k := range names {
		if v := src[k]; v != nil {
			dst[k] = v
		}
	}
}

// relay copies the provider's response to the caller as it arrives (a
// stream flushed at each read) and reads its usage from the same bytes: a
// stream's events as they pass, a whole response from a copy of up to
// maxRetained bytes, read at its end.
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
