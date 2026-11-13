// Package proxy is the request path (gateway.md §3): authenticate, read and
// scan the body, route by alias, admit against the scope's budget and
// in-flight limits, splice the model, send upstream, and stream the
// response back through a tee that reads usage, then account for the call.
// Hooks, retries and failover are later steps at the marked places.
package proxy

import (
	"cmp"
	"context"
	"errors"
	"io"
	"net/http"
	"time"
	"uuid"

	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/gateway/canon"
	"github.com/chucklehead-dev/golem/gateway/limit"
	"github.com/chucklehead-dev/golem/gateway/price"
	"github.com/chucklehead-dev/golem/gateway/route"
	"github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/gateway/telemetry"
	"github.com/chucklehead-dev/golem/lib/contract"
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
	Status     int
	Usage      canon.Usage
	Err        error // a malformed usage report or a cut stream
}

// Handler serves one dialect's endpoint.
type Handler struct {
	Dialect   Dialect
	Snapshots Snapshots
	// Limiter admits calls against budgets and in-flight limits; nil admits
	// everything.
	Limiter *limit.Limiter
	MaxBody int64 // request body cap in bytes
	// Telemetry records each request's span, metrics and ledger row; nil
	// records nothing.
	Telemetry *telemetry.Telemetry
	// WriteStall bounds how long one write to the caller may block (default
	// 30 s): a client that stops reading would otherwise hold its slot and
	// its upstream connection for as long as the provider streams.
	WriteStall time.Duration
	// Record, if set, is told each call's result (tests).
	Record func(*http.Request, Result)
}

// Request headers a caller may pass to the provider, and response headers
// returned to the caller (contracts.md §5: golem's own are never forwarded).
var (
	forwardRequest  = []string{"Anthropic-Version", "Anthropic-Beta", "Openai-Organization"} // read-only
	forwardResponse = []string{"Content-Type", "Request-Id", "X-Request-Id", "Retry-After"}  // read-only
)

var (
	jsonType   = []string{"application/json"} // read-only
	retryAfter = []string{"5"}                // read-only
)

// refusal is a request the path will not serve: a contract code and a
// message that says what to do next.
type refusal struct {
	code contract.Code
	msg  string
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	ctx, span := h.Telemetry.Start(r.Context(), h.Dialect.Name())
	defer span.End()
	reqID := uuid.NewV7() // time-ordered: the ledger and logs sort by it (contracts.md §5)
	var idBuf [36]byte
	text, _ := reqID.AppendText(idBuf[:0]) // never fails
	reqIDs := string(text)
	w.Header()[contract.HeaderRequestID] = []string{reqIDs}

	snap, principal, no := h.authenticate(r)
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
	requestBytes := int64(len(body)) // for an estimate, once body is gone
	if no != nil {
		h.fail(ctx, w, no)
		return
	}

	// Scan and route. (Hooks go here.)
	req, err := ex.scan(h.Dialect, body)
	if err != nil {
		h.fail(ctx, w, &refusal{contract.RequestInvalid, err.Error()})
		return
	}
	rt, no := h.route(snap, principal, req.alias)
	if no != nil {
		h.fail(ctx, w, no)
		return
	}
	target := rt.Pick()
	pr := snap.Prices[target.Model]
	var deadline time.Time // the call's end at the latest: renewals stop there
	if rt.MaxDuration > 0 {
		deadline = started.Add(rt.MaxDuration)
	}
	ticket, no := h.admit(ctx, reqIDs, started, deadline, principal, req, price.Worst(int64(len(body)), req.maxTokens, pr))
	if no != nil {
		h.fail(ctx, w, no)
		return
	}

	out := getBody()
	if n := len(body) + len(target.Model) + 64; cap(*out) < n {
		*out = make([]byte, 0, n)
	}
	*out = ex.rewrite(h.Dialect, (*out)[:0], body, target.Model)
	putBody(in) // the caller's body is no longer needed
	in = nil

	call := telemetry.Call{RequestID: reqIDs, Principal: principal.ID, PrincipalKind: principal.Kind, Key: principal.Key,
		Scope: principal.Scope, Frontend: h.Dialect.Name(), Backend: rt.Dialect, Alias: rt.Alias, Model: target.Model,
		Deployment: target.Backend + "/" + target.Deployment, Provider: target.Backend, Started: started,
		Currency: snap.Snapshot.Currency, Counted: ticket != nil, Waived: ticket != nil && ticket.Waived()}
	if ticket != nil {
		call.Period = ticket.Period() // the counter's: one period per call
	}
	// The accounting runs to its end whatever the caller does.
	acct := context.WithoutCancel(ctx)

	// One attempt. (Retries and failover before the first byte go here; a
	// retry needs the rewritten body kept until the attempt is committed.)
	// The route's bounds: the caller's going away still cancels it too.
	lim := &ex.lim
	upCtx := lim.bound(ctx, started, rt)
	defer lim.end()
	up, ub := upstreamRequest(upCtx, r, target, out)
	resp, err := target.Transport.RoundTrip(up)
	if err != nil {
		ub.Close()
		c, msg := contract.UpstreamUnavailable, "the provider could not be reached; retry shortly"
		if cause := context.Cause(upCtx); cause == errHeadersTimeout || cause == errMaxDuration {
			msg = cause.Error() + "; retry, or ask the platform team about this route's timeouts"
		}
		call.Ended, call.Status, call.Outcome = time.Now(), c.Status, "failed"
		call.ErrorOrigin, call.ErrorCode = string(c.Origin), c.Name
		if r.Context().Err() != nil { // the caller left before the headers: theirs, not the provider's
			call.Outcome, call.ErrorOrigin, call.ErrorCode = "cancelled", string(contract.OriginClient), ""
		}
		h.Telemetry.Ended(ctx, call) // before the ticket numbers its event
		if ticket != nil {
			ticket.Complete(acct, 0) // no upstream work: nothing billed
		}
		Refuse(w, h.Dialect, c, msg)
		return
	}
	defer resp.Body.Close()
	lim.gotHeaders()

	for _, k := range forwardResponse {
		if v := resp.Header[k]; v != nil {
			w.Header()[k] = v
		}
	}
	w.WriteHeader(resp.StatusCode)
	streamed := req.stream && resp.StatusCode == http.StatusOK
	res := Result{RequestID: reqID, Principal: principal, Target: target, Status: resp.StatusCode}
	if streamed {
		res.Usage, res.Err = h.stream(w, lim.bodyOf(resp.Body), ex)
	} else {
		res.Usage, res.Err = h.whole(w, lim.bodyOf(resp.Body), ex)
	}
	// The caller is gone if a write to it failed, or if its request ended
	// first (the upstream read then fails with its context).
	res.CallerGone = ex.callerGone || r.Context().Err() != nil
	_, cut := errors.AsType[errUpstreamCut](res.Err)
	if resp.StatusCode == http.StatusOK { // a provider bills no error response
		// A cut or unreported response is charged what was produced, counted
		// from what was seen (ADR 0012), and its row says how much of its
		// cost is estimated; never zero for work the provider did.
		var estimated canon.Usage
		res.Usage, estimated = ex.estimate(h.Dialect, requestBytes)
		if est := price.Cost(estimated, pr); est > 0 {
			call.EstimatedFraction = float64(est) / float64(max(price.Cost(res.Usage, pr), 1))
		}
	}
	res.Cost = price.Cost(res.Usage, pr)

	call.Ended, call.Status, call.Usage, call.Cost = time.Now(), resp.StatusCode, res.Usage, res.Cost
	call.ProviderRequestID = cmp.Or(resp.Header.Get("Request-Id"), resp.Header.Get("X-Request-Id"))
	call.Outcome, call.ErrorOrigin, call.ErrorCode = outcome(res, cut)
	// The row is prepared before the ticket numbers its event: the
	// limiter's OnNumbered writes it with the number.
	h.Telemetry.Ended(ctx, call)
	if ticket != nil {
		if res.CallerGone {
			// The caller went away: what was seen is charged, and the slot
			// is held until the upstream has ended (ADR 0041 §7.6), here the
			// connection's close.
			ticket.Cancel(acct, res.Cost)
			resp.Body.Close()
			ticket.Ended(acct)
		} else {
			ticket.Complete(acct, res.Cost)
		}
	}
	if h.Record != nil {
		h.Record(r, res)
	}
	if cut && !res.CallerGone {
		// The provider's response ended early. A clean end would read as a
		// complete (and shorter) answer: a stream gets the contract's error
		// event, and then the response is aborted (a reset, not an end), so
		// no client can take it for whole (contracts.md §1).
		if streamed {
			w.Write(streamError(h.Dialect, reqIDs))
			http.NewResponseController(w).Flush()
		}
		panic(http.ErrAbortHandler)
	}
}

// authenticate takes the request's snapshot and finds its principal.
func (h *Handler) authenticate(r *http.Request) (*snapshot.Compiled, *auth.Principal, *refusal) {
	snap, err := h.Snapshots.Current()
	switch {
	case errors.Is(err, snapshot.ErrExpired):
		return nil, nil, &refusal{contract.GatewayAuthorizationExpired,
			"this gateway's authorization has expired and it serves no new requests until it is renewed; the platform team is alerted"}
	case err != nil:
		return nil, nil, &refusal{contract.GatewayNotReady, "this gateway has no configuration yet; retry shortly"}
	case snap.Auth == nil:
		return snap, auth.Anyone, nil
	}
	p, no := snap.Auth.Authenticate(r.Header)
	if no != nil {
		return nil, nil, &refusal{no.Code, no.Msg}
	}
	return snap, p, nil
}

// readBody reads the request body into the pooled buffer in, at most
// MaxBody bytes.
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request, in *[]byte) ([]byte, *refusal) {
	if n := r.ContentLength; n > 0 && n <= h.MaxBody && int64(cap(*in)) < n+1 {
		*in = make([]byte, 0, n+1) // one allocation, not a doubling series
	}
	var err error
	*in, err = readAll((*in)[:0], http.MaxBytesReader(w, r.Body, h.MaxBody))
	switch _, tooBig := errors.AsType[*http.MaxBytesError](err); {
	case tooBig:
		return nil, &refusal{contract.RequestTooLarge, "the request body is larger than this gateway accepts"}
	case err != nil:
		return nil, &refusal{contract.RequestUnreadable, "the request body could not be read"}
	}
	return *in, nil
}

// route finds the alias's route, and checks the principal may call it here.
func (h *Handler) route(snap *snapshot.Compiled, p *auth.Principal, alias []byte) (*route.Route, *refusal) {
	rt := snap.Routes.Lookup(alias)
	switch {
	case rt == nil:
		return nil, &refusal{contract.RouteUnknownModel, "no route for this model; ask the scope's managers which aliases it may use"}
	case rt.Dialect != h.Dialect.Name():
		return nil, &refusal{contract.RouteWrongFrontend,
			"this model speaks " + rt.Dialect + ", not " + h.Dialect.Name() + ": call it at that API's endpoint (golem converts only where a route says so)"}
	case !p.Allows(alias):
		return nil, &refusal{contract.AuthzModelNotAllowed, "this key's scope may not use this model; its managers can allow it, or use an alias it allows"}
	}
	return rt, nil
}

// admit checks the call against the scope's budget and in-flight limits
// (ADR 0031) and takes its slots. A nil ticket with no refusal: no limiter.
func (h *Handler) admit(ctx context.Context, reqID string, started, deadline time.Time, p *auth.Principal, req scanned, worst int64) (*limit.Ticket, *refusal) {
	if h.Limiter == nil {
		return nil, nil
	}
	lim := p.Limits
	if lim.Strict && !req.hasMax {
		return nil, &refusal{contract.RequestInvalid, "this scope is strict: set max_tokens, so the call's worst case can be checked against the budget"}
	}
	t, err := h.Limiter.Admit(ctx, limit.Call{ID: reqID, Principal: p.ID, Scope: p.Scope,
		Budget: lim.BudgetMicros, PrincipalLimit: lim.PrincipalInFlight, ScopeLimit: lim.ScopeInFlight,
		Strict: lim.Strict, Worst: worst, Started: started, Deadline: deadline})
	switch {
	case errors.Is(err, limit.ErrBudget):
		return nil, &refusal{contract.BudgetPoolExhausted, "the scope's budget for this period is spent; its managers or a budget manager can add to it"}
	case errors.Is(err, limit.ErrConcurrency):
		return nil, &refusal{contract.LimitConcurrency, "too many calls in flight for this key's principal or scope; retry when one finishes"}
	case err != nil:
		return nil, &refusal{contract.GatewayInternal, "the limiter failed"}
	}
	return t, nil
}

// upstreamRequest is the provider's request: the rewritten body, the
// route's credential, and the caller headers the provider may see.
func upstreamRequest(ctx context.Context, r *http.Request, target *route.Target, body *[]byte) (*http.Request, *upstreamBody) {
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
	for _, k := range forwardRequest {
		if v := r.Header[k]; v != nil {
			up.Header[k] = v
		}
	}
	for k, v := range target.Header {
		up.Header[k] = v
	}
	return up, ub
}

// outcome is a finished call's ledger outcome, and the failure's origin and
// code (contracts.md §3).
func outcome(res Result, cut bool) (outcome, origin, code string) {
	switch {
	case res.CallerGone:
		return "cancelled", string(contract.OriginClient), ""
	case cut:
		c := contract.UpstreamStreamFailed
		return "failed", string(c.Origin), c.Name
	case res.Status >= 400:
		return "failed", string(contract.OriginProvider), ""
	}
	return "completed", "", ""
}

// stream copies the response as it arrives, flushing each read, and feeds
// the same bytes to the dialect's usage reader.
func (h *Handler) stream(w http.ResponseWriter, body io.Reader, ex *exchange) (canon.Usage, error) {
	ex.startWrites(w, cmp.Or(h.WriteStall, 30*time.Second))
	filter := ex.startStream(h.Dialect)
	send := ex.send
	var cut error
	for {
		n, err := body.Read(ex.buf)
		if n > 0 {
			var ok bool
			if filter {
				ok = send(ex.filter.Pass(ex.buf[:n]))
			} else {
				ok = send(ex.buf[:n])
				ex.parser.Write(ex.buf[:n])
			}
			if !ok {
				cut = errCallerGone
				break
			}
		}
		if err == io.EOF {
			if filter && !send(ex.filter.End()) {
				cut = errCallerGone
			}
			break
		}
		if err != nil {
			cut = errUpstreamCut{err}
			break
		}
	}
	return ex.usage(h.Dialect, cut)
}

// whole copies a non-streamed response, keeping a copy of up to maxRetained
// bytes to read its usage from.
func (h *Handler) whole(w http.ResponseWriter, body io.Reader, ex *exchange) (canon.Usage, error) {
	ex.startWrites(w, cmp.Or(h.WriteStall, 30*time.Second))
	ex.tail = ex.tail[:0]
	var cut error
	for {
		n, err := body.Read(ex.buf)
		if n > 0 {
			if werr := ex.write(ex.buf[:n], false); werr != nil {
				cut, ex.callerGone = errCallerGone, true
				break
			}
			if len(ex.tail)+n <= maxRetained {
				ex.tail = append(ex.tail, ex.buf[:n]...)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			cut = errUpstreamCut{err}
			break
		}
	}
	ex.readWhole(h.Dialect)
	return ex.usage(h.Dialect, cut)
}

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
	h.Telemetry.Refused(ctx, no.code)
	Refuse(w, h.Dialect, no.code, no.msg)
}

// Refuse writes a refusal in a dialect's error shape, with golem's code, a
// message that says what to do next, and the request ID if the response has
// one (contracts.md §1). Only contract codes can be sent.
func Refuse(w http.ResponseWriter, d Dialect, c contract.Code, msg string) {
	w.Header()["Content-Type"] = jsonType
	if c.RetryAfter() {
		w.Header()["Retry-After"] = retryAfter
	}
	w.WriteHeader(c.Status)
	var reqID string
	if v := w.Header()[contract.HeaderRequestID]; len(v) > 0 {
		reqID = v[0]
	}
	w.Write(appendError(nil, d, c, msg, reqID))
}
