// Package proxy is the request path (gateway.md §3): authenticate, read and
// scan the body, route by alias, admit against the scope's budget and
// in-flight limits, splice the model, send upstream, and stream the
// response back through a tee that reads usage, then account for the call.
// Hooks, retries and failover are later steps at the marked places.
package proxy

import (
	"cmp"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
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
	"github.com/chucklehead-dev/golem/gateway/price"
	"github.com/chucklehead-dev/golem/gateway/route"
	"github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/gateway/telemetry"
	"github.com/chucklehead-dev/golem/lib/contract"
	libsnapshot "github.com/chucklehead-dev/golem/lib/snapshot"
)

// Snapshots gives the configuration a request uses: taken once, at the
// start, and used to the end (gateway.md §1).
type Snapshots interface {
	Current() (*snapshot.Compiled, error)
}

// Result is what a finished call reports: the input to pricing, the
// counters and the access log (gateway.md §3 step 9).
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
	// Limiter admits calls against budgets and in-flight limits (golem's:
	// limit.Limiter.Admitter); nil admits everything uncounted.
	Limiter admission.Admitter
	MaxBody int64 // request body cap in bytes (default 32 MiB)
	// Telemetry records each request's span, metrics and ledger row; nil
	// records nothing.
	Telemetry *telemetry.Telemetry
	// BodyTimeout bounds reading a request's body (default 30 s);
	// MaxResponse, the bytes of a provider's response forwarded (default
	// 64 MiB; past it the response is cut, golem#5).
	BodyTimeout time.Duration
	MaxResponse int64
	// ProviderTail is how long a call's slot is held after an ambiguous
	// end (a cut, an abandoned response, an upstream failure the provider
	// may have seen), bounded by the call's deadline: the provider may still
	// be working (default 2 minutes; golem#4).
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
	// MaxKept bounds the request bodies the handler keeps for retries
	// until their calls commit (default 256 MiB); a call past it makes one
	// attempt.
	MaxKept int64
	// record, if set, is told each call's result (tests).
	record func(*http.Request, Result)
	// beforePick, if set, runs before each attempt's deployment is picked,
	// the first included (tests: the attempt model's traces drive when).
	beforePick func(*http.Request)

	healthOnce sync.Once
	ownHealth  *route.Health
	kept       atomic.Int64 // bytes of bodies kept for retries
}

// Response headers returned to the caller (contracts.md §5: golem's own
// are never forwarded); the request headers a caller may pass are its
// dialect's (Dialect.forwardRequest).
var forwardResponse = []string{"Content-Type", "Request-Id", "X-Request-Id", "Retry-After"} // read-only

var (
	jsonType          = []string{"application/json"} // read-only
	defaultRetryAfter = []string{"5"}                // read-only: a retryable refusal's, when its refuser gave none
)

// refusal is a request the path will not serve: a contract code and a
// message that says what to do next.
type refusal struct {
	code       contract.Code
	msg        string
	retryAfter time.Duration // the admitter's, if it said when to retry
	callerGone bool          // nothing to tell: the caller left first
}

// ServeHTTP serves one call, top to bottom: every refusal is written in the
// caller's dialect, and every admitted call is accounted for exactly once,
// whatever ends it.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	ctx, span := h.Telemetry.Start(r.Context(), h.Dialect.Name())
	defer span.End()
	reqID, reqIDs := newRequestID()
	w.Header()[contract.HeaderRequestID] = []string{reqIDs}

	// Before authentication: a credential sent twice must not be judged by
	// its first copy alone.
	if err := contract.CheckSingleHeaders(r.Header); err != nil {
		h.fail(ctx, w, &refusal{code: contract.RequestInvalid, msg: err.Error()})
		return
	}
	gen := h.InFlight.generation() // before the keys are read: see InFlight.add
	var grant auth.Grant
	snap, principal, no := h.authenticate(r, &grant)
	if no != nil {
		h.fail(ctx, w, no)
		return
	}
	ex := getExchange()
	defer ex.release()
	in := getBody()
	defer func() {
		if in != nil {
			putBody(in)
		}
	}()
	body, no := h.readBody(w, r, in)
	if no != nil {
		h.fail(ctx, w, no)
		return
	}
	requestBytes := int64(len(body)) // for an estimate, once body is gone

	// Scan and route. (Hooks go here.)
	req, err := ex.scan(h.Dialect, body)
	if err != nil {
		h.fail(ctx, w, &refusal{code: contract.RequestInvalid, msg: err.Error()})
		return
	}
	rt, no := h.route(snap, principal, req.alias)
	if no != nil {
		h.fail(ctx, w, no)
		return
	}
	if no := h.features(r.Header, principal, rt, req); no != nil {
		h.fail(ctx, w, no)
		return
	}
	if no := h.policy(ex, principal, req, requestBytes); no != nil {
		h.fail(ctx, w, no)
		return
	}
	// The call's attempts (model/attempts.md): the pool in order, each
	// attempt from a deployment its health admits; one refused or never
	// seen by its provider is retried, within the route's attempts and its
	// pool's retry budget, until one commits (commit.go). The budget and
	// the slots are the call's, admitted once at its first deployment (a
	// call bills at most once, so its bound needs no check per attempt).
	hl := h.health()
	hl.Called(rt.Alias)
	tried := make([]bool, len(rt.Pool))
	if h.beforePick != nil {
		h.beforePick(r)
	}
	target, lease, retryAt := pick(hl, rt, tried)
	if target == nil {
		h.fail(ctx, w, &refusal{code: contract.UpstreamUnavailable, retryAfter: time.Until(retryAt),
			msg: "every deployment of this route is resting after failures or at its ceiling; retry shortly"})
		return
	}
	defer func() { lease.Release() }() // the last attempt's: when the call is over
	deployment := target.Key()
	pr := snap.Prices[target.Model]
	var deadline time.Time // the call's end at the latest: renewals stop there
	if rt.MaxDuration > 0 {
		deadline = started.Add(rt.MaxDuration)
	}
	worst, ok := price.Worst(requestBytes, req.maxTokens, req.n, pr)
	if !ok {
		h.fail(ctx, w, &refusal{code: contract.RequestInvalid, msg: "the request's token count is too large to price safely: lower max_tokens or n"})
		return
	}
	ticket, no := h.admit(ctx, principal, req, admission.Call{ID: reqIDs, Alias: rt.Alias, Deployment: deployment,
		Model: target.Model, Worst: worst, Started: started, Deadline: deadline})
	if no != nil {
		h.fail(ctx, w, no)
		return
	}
	// The accounting runs to its end whatever the caller does.
	acct := settlement{ticket: ticket, ctx: context.WithoutCancel(ctx), deadline: deadline,
		tail: cmp.Or(h.ProviderTail, 2*time.Minute)}
	// Every admitted call's ticket ends exactly once. Should anything below
	// panic before it does (a bug; the abort of a cut response comes after
	// its end), the call is cancelled at its declared worst case and its
	// slot held for the tail, rather than held for ever. Its row is not
	// written: the panic's report is the record.
	defer func() {
		if !acct.ended {
			acct.cancel(worst)
		}
	}()

	// A retry needs the caller's body, to rewrite for the next deployment:
	// it is kept until the call commits, within the gateway's bound on
	// bodies kept (past it the call makes one attempt, rather than taking
	// more memory).
	kept := rt.MaxAttempts > 1 && h.keep(int64(len(body)))
	if kept {
		defer h.unkeep(int64(len(body)))
	}

	call := telemetry.Call{RequestID: reqIDs, Principal: principal.ID, PrincipalKind: principal.Kind, Key: principal.Key,
		Scope: principal.Scope, Frontend: h.Dialect.Name(), Backend: rt.Dialect, Alias: rt.Alias, Started: started,
		Currency: snap.Snapshot.Currency, Waived: ticket != nil && ticket.Waived()}
	if ticket != nil {
		call.Period = ticket.Period() // the counter's: one period per call
		call.OverBudget = h.overBudget(w, principal, ticket)
	}

	// The route's bounds, over all its attempts: the caller's going away
	// still cancels it too.
	lim := &ex.lim
	upCtx := lim.bound(ctx, started, rt)
	defer lim.end()
	if h.InFlight != nil {
		// Registered for the call's upstream life, taken off before its
		// context is released (defers run last in, first out).
		t := h.InFlight.add(&ex.call, &grant, lim.cancel, reqID[15], gen)
		defer h.InFlight.remove(&ex.call)
		if t != nil { // withdrawn as it was admitted: the provider never had it
			call.Model, call.Deployment, call.Provider = target.Model, deployment, target.Backend
			call.Ended, call.Status = time.Now(), t.code.Status
			call.Outcome, call.ErrorOrigin, call.ErrorCode = outcomeTerminated, string(t.code.Origin), t.code.Name
			h.Telemetry.Ended(ctx, call)
			acct.complete(0)
			Refuse(w, h.Dialect, t.code, t.message())
			return
		}
	}

	var resp *http.Response
	var upBody io.Reader
	streamed := false
	for attempt := 1; ; attempt++ {
		deployment, pr = target.Key(), snap.Prices[target.Model]
		call.Attempt, call.Model, call.Deployment, call.Provider = attempt, target.Model, deployment, target.Backend
		call.Envelope = target.Envelope
		out := getBody()
		if n := len(body) + len(target.Model) + 64; cap(*out) < n {
			*out = make([]byte, 0, n)
		}
		*out = ex.rewrite(h.Dialect, (*out)[:0], body, target.Model)
		if !kept && in != nil {
			putBody(in) // the caller's body is no longer needed
			in, body = nil, nil
		}
		if attempt > 1 {
			lim.nextAttempt(rt)
		}
		// retry, given the attempt's failure before it committed, takes the
		// pool's next deployment if the call may retry: it was kept, has
		// attempts left, its caller is there, nothing withdrew it, and the
		// pool's budget has a retry. It records the failed attempt's row
		// (charged nothing: the provider saw nothing, or refused before any
		// output) and reports whether the call goes on.
		retry := func(status int, after time.Duration) bool {
			lease.Fail(after)
			if h.beforePick != nil {
				h.beforePick(r)
			}
			if !kept || attempt >= rt.MaxAttempts || r.Context().Err() != nil || terminatedBy(upCtx) != nil {
				return false
			}
			next, nl, _ := pick(hl, rt, tried)
			if next == nil {
				return false
			}
			if !hl.Retry(rt.Alias) {
				nl.Release()
				return false
			}
			c := contract.UpstreamUnavailable
			call.Ended, call.Status = time.Now(), cmp.Or(status, c.Status)
			call.Outcome, call.ErrorOrigin, call.ErrorCode = "failed", string(c.Origin), c.Name
			h.Telemetry.Retried(ctx, call)
			call.ProviderRequestID, call.Outcome, call.ErrorOrigin, call.ErrorCode = "", "", "", ""
			target, lease = next, nl
			return true
		}

		attemptCtx := upCtx
		if target.Envelope != "" {
			// An envelope may pass the attempt's attribution to its provider
			// (Bedrock's request metadata: the ledger's IDs in its invocation
			// logs, so the two join per request). Only an envelope's calls
			// pay for the context.
			attemptCtx = envelope.WithAttribution(upCtx, &envelope.Attribution{RequestID: reqIDs, AttemptID: call.AttemptID(),
				Principal: principal.ID, Scope: principal.Scope, Key: principal.Key})
		}
		up, ub := upstreamRequest(attemptCtx, r, target, out, h.Dialect.forwardRequest())
		var err error
		resp, err = target.Transport.RoundTrip(up)
		if err != nil {
			ub.Close()
			if !reachedProvider(err) && retry(0, 0) {
				continue
			}
			lease.Fail(0)
			c, msg := contract.UpstreamUnavailable, unavailableMessage(context.Cause(upCtx)) // the cause as the call failed
			ended := terminatedBy(upCtx)
			if ended != nil && r.Context().Err() == nil { // the headers not yet sent: the dialect's error
				c, msg = ended.code, ended.message()
			}
			call.Ended, call.Status = time.Now(), c.Status
			cost := chargeUnanswered(&call, err, r.Context().Err() != nil, ended, requestBytes, req, pr)
			h.Telemetry.Ended(ctx, call)
			if cost == 0 {
				acct.complete(0) // the provider never had it
			} else {
				acct.cancel(cost)
			}
			Refuse(w, h.Dialect, c, msg)
			return
		}
		lim.gotHeaders()
		call.ProviderRequestID = cmp.Or(resp.Header.Get("Request-Id"), resp.Header.Get("X-Request-Id"))
		if retryable(resp.StatusCode) && retry(resp.StatusCode, providerRetryAfter(resp)) {
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
				if retry(0, providerRetryAfter(resp)) {
					continue
				}
				c := contract.UpstreamUnavailable
				call.Ended, call.Status = time.Now(), c.Status
				call.Outcome, call.ErrorOrigin, call.ErrorCode = "failed", string(c.Origin), c.Name
				h.Telemetry.Ended(ctx, call)
				acct.complete(0)
				if h.record != nil {
					h.record(r, Result{RequestID: reqID, Principal: principal, Target: target, Status: c.Status})
				}
				refuse(w, h.Dialect, c, "the provider failed before its response began (its first event was an error); retry shortly", providerRetryAfter(resp))
				return
			}
			upBody = &ex.first
		}
		break
	}
	// Committed: this attempt's response is the caller's. The deployment
	// answered (a provider's 4xx included: the request's fault, not its
	// health), and the caller's body is let go.
	defer resp.Body.Close()
	if retryable(resp.StatusCode) || resp.StatusCode >= 500 {
		lease.Fail(providerRetryAfter(resp))
	} else {
		lease.Commit()
	}
	if in != nil {
		putBody(in)
		in, body = nil, nil
	}
	copyHeaders(w.Header(), resp.Header, forwardResponse)
	w.WriteHeader(resp.StatusCode)
	res := Result{RequestID: reqID, Principal: principal, Target: target, Status: resp.StatusCode}
	res.Usage, res.Err = h.relay(w, upBody, ex, streamed)
	// The caller is gone if a write to it failed, or if its request ended
	// first (the upstream read then fails with its context).
	res.CallerGone = ex.callerGone || r.Context().Err() != nil
	_, cut := errors.AsType[errUpstreamCut](res.Err)
	// A cut of the gateway's own making: a sweep cancelled the upstream
	// request (its context, not the caller's, so not a caller gone).
	var ended *errTerminated
	if cut {
		ended = terminatedBy(upCtx)
		res.Terminated = ended != nil && !res.CallerGone
	}
	if resp.StatusCode == http.StatusOK { // a provider bills no error response
		// A cut or unreported response is charged what was produced, counted
		// from what was seen (ADR 0012), and its row says how much of its
		// cost is estimated; never zero for work the provider did.
		var estimated canon.Usage
		res.Usage, estimated = ex.estimate(h.Dialect, requestBytes, outputCeiling(req))
		if est := price.Cost(estimated, pr); est > 0 {
			call.EstimatedFraction = float64(est) / float64(max(price.Cost(res.Usage, pr), 1))
		}
	}
	res.Cost = price.Cost(res.Usage, pr)

	call.Ended, call.Status, call.Usage, call.Cost = time.Now(), resp.StatusCode, res.Usage, res.Cost
	call.Outcome, call.ErrorOrigin, call.ErrorCode = outcome(res, cut, ended)
	// The row's end is stamped before the ticket adds the cost to the
	// counter's minute, so the minute is never before the row's end: a
	// call that ended after a snapshot's boundary is in the counter's
	// minutes from it (ADR 0048). One that ends at a minute's last
	// millisecond may land in both, which errs high.
	h.Telemetry.Ended(ctx, call)
	switch {
	case ticket == nil: // nothing counted
	case res.CallerGone || cut:
		// The response was abandoned or cut (by the provider, or by the
		// gateway ending the call): what was seen is charged (estimated
		// where unreported), and the slot is held for the provider's tail,
		// which may go on working after the connection closes (ADR 0041
		// §7.6; golem#4).
		resp.Body.Close()
		acct.cancel(res.Cost)
	default:
		acct.complete(res.Cost)
	}
	if h.record != nil {
		h.record(r, res)
	}
	if cut && !res.CallerGone {
		// The provider's response ended early. A clean end would read as a
		// complete (and shorter) answer: a stream gets the contract's error
		// event, and then the response is aborted (a reset, not an end), so
		// no client can take it for whole (contracts.md §1). A call the
		// gateway ended gets the refusal its caller would now get, as
		// that event (a whole response, already begun, is cut alone).
		if streamed {
			ev := streamError(h.Dialect, reqIDs)
			if res.Terminated {
				ev = errorEvent(h.Dialect, ended.code, ended.message(), reqIDs)
			}
			w.Write(ev)
			http.NewResponseController(w).Flush()
		}
		panic(http.ErrAbortHandler)
	}
}

// newRequestID mints a call's ID, a UUIDv7: time-ordered, so the ledger and
// logs sort by it (contracts.md §5); and its text.
func newRequestID() (uuid.UUID, string) {
	id := uuid.NewV7()
	var buf [36]byte
	text, _ := id.AppendText(buf[:0]) // never fails
	return id, string(text)
}

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
// its row's outcome. Absent response bytes are not absent work (golem#4):
// only a failure the provider cannot have seen is free. Else the call is
// charged an estimate, which reconciliation corrects, and its slot is held
// for the provider's tail. ended is the sweep that cancelled the call, if
// one did: it is ended as a caller gone is, and its row says terminated.
func chargeUnanswered(call *telemetry.Call, err error, callerLeft bool, ended *errTerminated, requestBytes int64, req scanned, pr libsnapshot.Price) int64 {
	c := contract.UpstreamUnavailable
	call.Outcome, call.ErrorOrigin, call.ErrorCode = "failed", string(c.Origin), c.Name
	if ended != nil && !callerLeft {
		call.Outcome, call.ErrorOrigin, call.ErrorCode = outcomeTerminated, string(ended.code.Origin), ended.code.Name
	}
	var cost int64
	switch {
	case !reachedProvider(err):
	case callerLeft: // the caller left first: its input, at least, was read
		call.Outcome, call.ErrorOrigin, call.ErrorCode = "cancelled", string(contract.OriginClient), ""
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

// unavailableMessage is upstream.unavailable's message: the route's
// timeout, if one cut the call, else a retry.
func unavailableMessage(cause error) string {
	if cause == errHeadersTimeout || cause == errMaxDuration {
		return cause.Error() + "; retry, or ask the platform team about this route's timeouts"
	}
	return "the provider could not be reached; retry shortly"
}

// uncertainOutput is the output assumed of an uncertain call that set no
// max_tokens (OpenAI's is optional): a long answer.
const uncertainOutput = 8192

// reachedProvider reports whether a failed round trip may have reached the
// provider: only a name that did not resolve, a connection that was never
// made or a TLS handshake that failed certainly did not.
func reachedProvider(err error) bool {
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

// authenticate takes the request's snapshot and finds its principal.
func (h *Handler) authenticate(r *http.Request, g *auth.Grant) (*snapshot.Compiled, *auth.Principal, *refusal) {
	snap, err := h.Snapshots.Current()
	switch {
	case errors.Is(err, snapshot.ErrExpired):
		return nil, nil, &refusal{code: contract.GatewayAuthorizationExpired, msg: "this gateway's authorization has expired and it serves no new requests until it is renewed; the platform team is alerted"}
	case err != nil:
		return nil, nil, &refusal{code: contract.GatewayNotReady, msg: "this gateway has no configuration yet; retry shortly"}
	case snap.Auth == nil:
		return snap, auth.Anyone, nil
	}
	p, no := snap.Auth.AuthenticateGrant(r.Header, g)
	if no != nil {
		return nil, nil, &refusal{code: no.Code, msg: no.Msg}
	}
	return snap, p, nil
}

// preallocMax is the most readBody allocates on a body's claimed length
// alone: an agent's request (about 200 KB) in one allocation.
const preallocMax = 1 << 20

// readBody reads the request body into the pooled buffer in, at most
// MaxBody bytes.
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request, in *[]byte) ([]byte, *refusal) {
	maxBody := cmp.Or(h.MaxBody, 32<<20) // zero is the default, not "no body"
	// One allocation, not a doubling series, for a body up to preallocMax:
	// past it the claimed length is not trusted (a client could claim 32
	// MiB, send two bytes, and hold the buffer until BodyTimeout), and
	// readAll grows the buffer as the bytes actually arrive.
	if n := r.ContentLength; n > 0 && n <= maxBody && int64(cap(*in)) < n+1 {
		*in = make([]byte, 0, min(n+1, preallocMax))
	}
	// The body must arrive within BodyTimeout: a client trickling it in
	// would hold a connection and a buffer before any admission (golem#5).
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(cmp.Or(h.BodyTimeout, 30*time.Second))) // a test's recorder has none
	var err error
	*in, err = readAll((*in)[:0], http.MaxBytesReader(w, r.Body, maxBody))
	rc.SetReadDeadline(time.Time{})
	switch _, tooBig := errors.AsType[*http.MaxBytesError](err); {
	case tooBig:
		return nil, &refusal{code: contract.RequestTooLarge, msg: "the request body is larger than this gateway accepts"}
	case err != nil:
		return nil, &refusal{code: contract.RequestUnreadable, msg: "the request body could not be read"}
	}
	return *in, nil
}

// route finds the alias's route, and checks the principal may call it here.
func (h *Handler) route(snap *snapshot.Compiled, p *auth.Principal, alias []byte) (*route.Route, *refusal) {
	rt := snap.Routes.Lookup(alias)
	switch {
	case rt == nil:
		return nil, &refusal{code: contract.RouteUnknownModel, msg: "no route for this model; ask the scope's managers which aliases it may use"}
	case rt.Dialect != h.Dialect.Name():
		return nil, &refusal{code: contract.RouteWrongFrontend, msg: "this model speaks " + rt.Dialect + ", not " + h.Dialect.Name() + ": call it at that API's endpoint (golem converts only where a route says so)"}
	case !p.Allows(alias):
		return nil, &refusal{code: contract.AuthzModelNotAllowed, msg: "this key's scope may not use this model; its managers can allow it, or use an alias it allows"}
	}
	return rt, nil
}

// features applies the request-feature mode (ADR 0037, 0041 §3, §7.1):
// in restricted mode (the default, but for agent routes) only approved
// request members, betas and API versions are forwarded; compatible mode
// forwards unknown ones. Known policy (policy below) applies in both.
func (h *Handler) features(hdr http.Header, p *auth.Principal, rt *route.Route, req scanned) *refusal {
	mode := p.Limits.Features
	if mode == "" {
		mode = "restricted"
		if rt.Agent {
			mode = "compatible"
		}
	}
	if mode == "compatible" {
		return nil
	}
	if req.unknown > 0 {
		return &refusal{code: contract.AuthzFeatureForbidden, msg: fmt.Sprintf("%d request member(s) golem does not know: this route forwards approved features only (restricted mode); its managers can approve them", req.unknown)}
	}
	return h.Dialect.restrictedHeaders(hdr, rt)
}

// policy applies the scope's request policy (contracts.md §8): its
// ceilings on output asked for and on input size (ADR 0038), provider-side
// storage (ADR 0027: turned off where it can be, refused where it cannot),
// and server-side tools, refused under the declared-worst-case check, since a
// request that asks for them declares no worst case (ADR 0044).
func (h *Handler) policy(ex *exchange, p *auth.Principal, req scanned, requestBytes int64) *refusal {
	lim := p.Limits
	if c := lim.MaxInputBytes; c > 0 && requestBytes > c {
		return &refusal{code: contract.RequestOverCeiling, msg: fmt.Sprintf("the request is %d bytes; this scope accepts at most %d: shorten the context, or ask its managers", requestBytes, c)}
	}
	if c := lim.MaxOutputTokens; c > 0 {
		switch {
		case !req.hasMax:
			return &refusal{code: contract.RequestOverCeiling, msg: fmt.Sprintf("this scope caps output at %d tokens a request: set max_tokens, at most that", c)}
		case req.maxTokens > c/req.n:
			return &refusal{code: contract.RequestOverCeiling, msg: fmt.Sprintf("max_tokens %d for %d completions is over this scope's ceiling of %d tokens a request", req.maxTokens, req.n, c)}
		}
	}
	if !lim.StorageAllowed {
		if req.state {
			return &refusal{code: contract.AuthzProviderStorageForbidden, msg: "this scope forbids provider-side storage, which a container keeps; its managers can allow it (ADR 0027)"}
		}
		if req.store {
			ex.forbidStorage(h.Dialect)
		}
	}
	if lim.DeclaredWorstCaseCheck && req.serverTools {
		return &refusal{code: contract.AuthzFeatureForbidden, msg: "this scope checks each call's declared worst case, and server-side tools (web search or fetch, code execution, MCP servers) declare none: use client-side tools, or a scope without declaredWorstCaseCheck"}
	}
	return nil
}

// admit checks the call against the scope's budget and in-flight limits
// (ADR 0031) and takes its slots. c names the call and its route; the
// principal's scope supplies its limits. A nil ticket with no refusal: no
// limiter.
func (h *Handler) admit(ctx context.Context, p *auth.Principal, req scanned, c admission.Call) (admission.Ticket, *refusal) {
	if h.Limiter == nil {
		return nil, nil
	}
	lim := p.Limits
	if lim.DeclaredWorstCaseCheck && !req.hasMax {
		return nil, &refusal{code: contract.RequestInvalid, msg: "this scope checks each call's declared worst case: set max_tokens, so it can be checked against the budget"}
	}
	c.Principal, c.Scope = p.ID, p.Scope
	c.Budget, c.BudgetMode, c.PrincipalLimit, c.ScopeLimit = lim.BudgetMicros, lim.BudgetMode, lim.PrincipalInFlight, lim.ScopeInFlight
	c.DeclaredWorstCaseCheck = lim.DeclaredWorstCaseCheck
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

// overBudget reports whether an admitted call is over its scope's budget
// (its scope observes or warns rather than enforcing), for its ledger row;
// in warn mode the response says so in golem-warning, with the share of
// the budget known spend has reached (contracts.md §1). It is set before
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
func budgetWarning(known, budget int64) string {
	var pct int64
	switch {
	case budget <= 0:
	case known > math.MaxInt64/100: // past any budget: not worth exactness
		pct = known / budget * 100
	default:
		pct = known * 100 / budget
	}
	return contract.BudgetPoolExhausted.Name + " " + strconv.FormatInt(pct, 10) + "%"
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

// outcomeTerminated is the ledger outcome of a call the gateway ended in
// flight, its authorization withdrawn (contracts.md §3).
const outcomeTerminated = "terminated"

// outcome is a finished call's ledger outcome, and the failure's origin and
// code (contracts.md §3).
func outcome(res Result, cut bool, ended *errTerminated) (outcome, origin, code string) {
	switch {
	case res.CallerGone:
		return "cancelled", string(contract.OriginClient), ""
	case res.Terminated:
		return outcomeTerminated, string(ended.code.Origin), ended.code.Name
	case cut:
		c := contract.UpstreamStreamFailed
		return "failed", string(c.Origin), c.Name
	case res.Status >= 400:
		return "failed", string(contract.OriginProvider), ""
	}
	return "completed", "", ""
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

// fail refuses a request: the refusal recorded, and written in the
// caller's dialect.
func (h *Handler) fail(ctx context.Context, w http.ResponseWriter, no *refusal) {
	if no.callerGone {
		return // no one to tell, nothing refused
	}
	h.Telemetry.Refused(ctx, no.code)
	refuse(w, h.Dialect, no.code, no.msg, no.retryAfter)
}

// Refuse writes a refusal in a dialect's error shape, with golem's code, a
// message that says what to do next, and the request ID if the response has
// one (contracts.md §1). Only contract codes can be sent.
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
