package proxy

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/chucklehead-dev/golem/gateway/auth"
	"github.com/chucklehead-dev/golem/gateway/route"
	"github.com/chucklehead-dev/golem/gateway/snapshot"
	"github.com/chucklehead-dev/golem/lib/contract"
)

// prepared is a request ready for its route's deployments. It has been
// authenticated, read, scanned and routed, and its features checked.
type prepared struct {
	snap      *snapshot.Compiled
	principal *auth.Principal
	body      []byte // in the pooled buffer readBody was given
	req       scanned
	rt        *route.Route
	betas     []string // the request's anthropic-beta values
}

// prepare takes a request to its route, the steps ServeHTTP and
// ServeCountTokens share. Authentication fills the grant, and the body is
// read into the pooled buffer given.
func (h *Handler) prepare(w http.ResponseWriter, r *http.Request, ex *exchange, in *[]byte, g *auth.Grant) (prepared, *refusal) {
	var p prepared
	// Check this before authentication, so a credential sent twice is not
	// judged by its first copy alone.
	if err := contract.CheckSingleHeaders(r.Header); err != nil {
		return p, &refusal{code: contract.RequestInvalid, msg: err.Error()}
	}
	var no *refusal
	if p.snap, p.principal, no = h.authenticate(r, g); no != nil {
		return p, no
	}
	// Before reading, refuse a claimed length past the scope's ceiling, and
	// bound the bodies being read at once (Reading).
	maxBody := cmp.Or(h.MaxBody, 32<<20) // zero means the default, not "no body"
	if c := p.principal.Limits.MaxInputBytes; c > 0 && r.ContentLength > c {
		return p, inputCeiling(p.principal, r.ContentLength)
	}
	br, no := h.reading().start(p.principal.ID)
	if no != nil {
		return p, no
	}
	p.body, no = h.readBody(w, r, in, maxBody, br)
	br.end()
	if no != nil {
		return p, no
	}
	var err error
	if p.req, err = ex.scan(h.Dialect, p.body); err != nil {
		return p, &refusal{code: contract.RequestInvalid, msg: err.Error()}
	}
	if p.rt, no = h.route(p.snap, p.principal, p.req.alias); no != nil {
		return p, no
	}
	p.betas = requestBetas(h.Dialect, r.Header)
	if no := invalidBetas(p.betas); no != nil {
		return p, no
	}
	return p, h.features(r.Header, p.principal, p.rt, p.req, p.betas)
}

// authenticate takes the current snapshot and finds the request's
// principal in it.
func (h *Handler) authenticate(r *http.Request, g *auth.Grant) (*snapshot.Compiled, *auth.Principal, *refusal) {
	// The guard bounds each source's requests and failed authentications.
	// A source past its failures may share a sidecar with valid callers. Its
	// golem keys are still looked up, since that is cheap, and refused only
	// if they fail too. Its Entra tokens are refused before any signature
	// check.
	src := h.Guard.Source(r)
	if no := h.Guard.Admit(src); no != nil {
		return nil, nil, &refusal{code: contract.LimitSource, msg: no.Msg, retryAfter: no.RetryAfter}
	}
	failing := h.Guard.Failing(src)
	if failing != nil && !sendsGolemKey(r.Header) {
		return nil, nil, &refusal{code: contract.LimitSource, msg: failing.Msg, retryAfter: failing.RetryAfter}
	}
	snap, err := h.Snapshots.Current()
	switch {
	//quint:enforces publication.noExpiredServe
	case errors.Is(err, snapshot.ErrExpired):
		return nil, nil, &refusal{code: contract.GatewayAuthorizationExpired, msg: "this gateway's authorization has expired and it serves no new requests until it is renewed; the platform team is alerted"}
	case err != nil:
		return nil, nil, &refusal{code: contract.GatewayNotReady, msg: "this gateway has no configuration yet; retry shortly"}
	case snap.Auth == nil:
		return snap, auth.Anyone, nil
	}
	p, no := snap.Auth.AuthenticateGrant(r.Header, g)
	if no != nil {
		if !credentialsFault(no) {
			return nil, nil, &refusal{code: no.Code, msg: no.Msg}
		}
		h.Guard.Failed(src)
		if failing != nil {
			return nil, nil, &refusal{code: contract.LimitSource, msg: failing.Msg, retryAfter: failing.RetryAfter}
		}
		return nil, nil, &refusal{code: no.Code, msg: no.Msg}
	}
	return snap, p, nil
}

// credentialsFault reports whether an authentication's refusal was the
// credential's fault, so it counts against the source's failures. A key
// unknown, revoked, expired or of another installation counts, as does a
// token invalid or expired, or no credential at all.
//
// Some refusals are golem's or Entra's fault and would lock out every
// caller of a shared address. Those are the gateway not being ready and a
// token refused for golem's copy of Entra's signing keys. A valid
// credential refused by policy does not count either.
func credentialsFault(no *auth.Refusal) bool {
	switch no.Code {
	case contract.AuthKeyUnknown, contract.AuthKeyRevoked, contract.AuthKeyExpired, contract.AuthKeyWrongInstallation,
		contract.AuthTokenExpired, contract.AuthMissing:
		return true
	case contract.AuthTokenInvalid:
		// lib/entra says these only in its message, since its Error has no
		// field for them. TestEntraKeyTroubleIsNoFailure pins the text.
		return !strings.Contains(no.Msg, entraKeysTooOld) && !strings.Contains(no.Msg, entraUnknownKey)
	}
	return false
}

// lib/entra's refusals of a token for golem's copy of the signing keys.
const (
	entraKeysTooOld = "signing keys is too old to trust"
	entraUnknownKey = "signed by an unknown key"
)

// sendsGolemKey reports whether the request's credential, in x-api-key or
// Authorization: Bearer, is a golem key rather than an Entra token. A key
// costs a hash lookup, a token a signature check.
func sendsGolemKey(h http.Header) bool {
	cred := h.Get("X-Api-Key")
	if cred == "" {
		if v := h.Get("Authorization"); len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
			cred = v[7:]
		}
	}
	return strings.HasPrefix(strings.TrimSpace(cred), "glm_")
}

// preallocMax is the most readBody allocates on a body's claimed length
// alone. It fits an agent's request, about 200 KB, in one allocation.
const preallocMax = 1 << 20

// readBody reads at most maxBody bytes of the request body into the pooled
// buffer given. It counts them against the bound on bodies being read.
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request, in *[]byte, maxBody int64, br *bodyRead) ([]byte, *refusal) {
	// Allocate once for a body up to preallocMax, rather than doubling.
	// Larger claims are not trusted, since a client could claim 32 MiB and
	// send two bytes. readAll then grows the buffer as bytes arrive.
	if n := r.ContentLength; n > 0 && n <= maxBody && int64(cap(*in)) < n+1 {
		*in = make([]byte, 0, min(n+1, preallocMax))
	}
	// The body must arrive within BodyTimeout. A client trickling it in
	// would otherwise hold a connection and a buffer before any admission.
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(cmp.Or(h.BodyTimeout, 30*time.Second))) // a test's recorder has none
	var err error
	*in, err = readAll((*in)[:0], br.reader(http.MaxBytesReader(w, r.Body, maxBody)))
	rc.SetReadDeadline(time.Time{})
	switch _, tooBig := errors.AsType[*http.MaxBytesError](err); {
	case errors.Is(err, errReadingFull):
		return nil, readingFull
	case tooBig:
		return nil, &refusal{code: contract.RequestTooLarge, msg: "the request body is larger than this gateway accepts"}
	case err != nil:
		return nil, &refusal{code: contract.RequestUnreadable, msg: "the request body could not be read"}
	}
	return *in, nil
}

// route finds the alias's route and checks that the principal may call it
// at this endpoint.
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

// features applies the scope's request-feature mode. Restricted mode
// forwards only approved request members, betas and API versions. It is
// the default, except on agent routes. Compatible mode forwards unknown
// ones. The scope's policy applies in both modes. ADR 0037.
func (h *Handler) features(hdr http.Header, p *auth.Principal, rt *route.Route, req scanned, betas []string) *refusal {
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
	return h.Dialect.restrictedHeaders(hdr, rt, betas)
}

// policy applies the scope's request policy (contracts.md). It enforces the
// ceilings on output and input size, and refuses state kept at the
// provider. Provider-side storage is turned off where it can be and
// refused where it cannot. ADR 0027.
func (h *Handler) policy(ex *exchange, p *auth.Principal, req scanned, requestBytes int64) *refusal {
	lim := p.Limits
	if no := inputCeiling(p, requestBytes); no != nil {
		return no
	}
	if c := lim.MaxOutputTokens; c > 0 {
		switch {
		case !req.hasMax:
			return &refusal{code: contract.RequestOverCeiling, msg: fmt.Sprintf("this scope caps output at %d tokens a request: set max_tokens, at most that", c)}
		case req.maxTokens > c/req.n:
			return &refusal{code: contract.RequestOverCeiling, msg: fmt.Sprintf("max_tokens %d for %d completions is over this scope's ceiling of %d tokens a request", req.maxTokens, req.n, c)}
		}
	}
	// M1 refers to nothing the provider keeps, whatever the scope allows.
	// That rules out stored responses, uploaded files and item references.
	// The refusal is stable policy, not retryable.
	switch {
	case req.stateful != "":
		return &refusal{code: contract.AuthzProviderStorageForbidden, msg: "golem refers to nothing the provider keeps, and " + req.stateful +
			" does: send the content itself in the request"}
	case req.storeTrue:
		return &refusal{code: contract.AuthzProviderStorageForbidden, msg: "golem serves stateless responses only: send store false, or leave it out (golem sends false)"}
	}
	if !lim.StorageAllowed {
		if req.state {
			return &refusal{code: contract.AuthzProviderStorageForbidden, msg: "this scope forbids provider-side storage, which a container keeps; its managers can allow it (ADR 0027)"}
		}
		if req.store {
			ex.forbidStorage(h.Dialect)
		}
	}
	return nil
}

// inputCeiling refuses a request past the scope's ceiling on its size,
// which applies to a token count too.
func inputCeiling(p *auth.Principal, requestBytes int64) *refusal {
	if c := p.Limits.MaxInputBytes; c > 0 && requestBytes > c {
		return &refusal{code: contract.RequestOverCeiling, msg: fmt.Sprintf("the request is %d bytes; this scope accepts at most %d: shorten the context, or ask its managers", requestBytes, c)}
	}
	return nil
}
