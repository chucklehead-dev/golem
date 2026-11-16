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

// prepared is a request ready for its route's deployments: authenticated,
// read, scanned, routed, and its features checked.
type prepared struct {
	snap      *snapshot.Compiled
	principal *auth.Principal
	body      []byte // in the pooled buffer readBody was given
	req       scanned
	rt        *route.Route
	betas     []string // its anthropic-beta values (requestBetas)
}

// prepare takes a request to its route: the steps ServeHTTP and
// ServeCountTokens share. Authentication fills g; the body is read into
// in.
func (h *Handler) prepare(w http.ResponseWriter, r *http.Request, ex *exchange, in *[]byte, g *auth.Grant) (prepared, *refusal) {
	var p prepared
	// Before authentication: a credential sent twice must not be judged by
	// its first copy alone.
	if err := contract.CheckSingleHeaders(r.Header); err != nil {
		return p, &refusal{code: contract.RequestInvalid, msg: err.Error()}
	}
	var no *refusal
	if p.snap, p.principal, no = h.authenticate(r, g); no != nil {
		return p, no
	}
	// Before the body is read: a claimed length past the scope's ceiling is
	// refused unread, and the bodies being read at once are bounded, the
	// gateway's bytes and the principal's bodies (Reading).
	maxBody := cmp.Or(h.MaxBody, 32<<20) // zero is the default, not "no body"
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

// authenticate takes the request's snapshot and finds its principal.
func (h *Handler) authenticate(r *http.Request, g *auth.Grant) (*snapshot.Compiled, *auth.Principal, *refusal) {
	// The floor before anything is checked (gateway/guard): a source's
	// requests, and its failed authentications, bounded. A source past its
	// failures (a flood of bad credentials, perhaps behind the same sidecar
	// as valid callers) still has a golem key looked up, cheaply, and is
	// refused only if that fails too; an Entra token from it is refused
	// before its signature is checked.
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
// credential's fault, and so counts against its source's failures
// (gateway/guard): a key unknown, revoked, expired or of another
// installation, a token invalid or expired, none sent. Not the gateway's
// state (gateway.not_ready: Entra's keys not yet fetched), nor a token
// refused for golem's copy of Entra's keys (too old, or without the key
// that signed it: Entra rotated): those are golem's or Entra's, and would
// lock out every caller of a shared address at once. Nor a valid
// credential refused by policy (authz.*, the denylist).
func credentialsFault(no *auth.Refusal) bool {
	switch no.Code {
	case contract.AuthKeyUnknown, contract.AuthKeyRevoked, contract.AuthKeyExpired, contract.AuthKeyWrongInstallation,
		contract.AuthTokenExpired, contract.AuthMissing:
		return true
	case contract.AuthTokenInvalid:
		// lib/entra says these only in its message (its Error has no
		// field for them): TestEntraKeyTroubleIsNoFailure pins them.
		return !strings.Contains(no.Msg, entraKeysTooOld) && !strings.Contains(no.Msg, entraUnknownKey)
	}
	return false
}

// lib/entra's refusals of a token for golem's copy of the signing keys.
const (
	entraKeysTooOld = "signing keys is too old to trust"
	entraUnknownKey = "signed by an unknown key"
)

// sendsGolemKey reports whether the request's credential (x-api-key, or
// Authorization: Bearer) is a golem key, not an Entra token: a key is a
// hash lookup, a token a signature check.
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
// alone: an agent's request (about 200 KB) in one allocation.
const preallocMax = 1 << 20

// readBody reads the request body into the pooled buffer in, at most
// maxBody bytes, counting them in br (the bound on bodies being read).
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request, in *[]byte, maxBody int64, br *bodyRead) ([]byte, *refusal) {
	// One allocation, not a doubling series, for a body up to preallocMax:
	// past it the claimed length is not trusted (a client could claim 32
	// MiB, send two bytes, and hold the buffer until BodyTimeout), and
	// readAll grows the buffer as the bytes actually arrive.
	if n := r.ContentLength; n > 0 && n <= maxBody && int64(cap(*in)) < n+1 {
		*in = make([]byte, 0, min(n+1, preallocMax))
	}
	// The body must arrive within BodyTimeout: a client trickling it in
	// would hold a connection and a buffer before any admission.
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

// features applies the request-feature mode (ADR 0037, 0041):
// in restricted mode (the default, but for agent routes) only approved
// request members, betas and API versions are forwarded; compatible mode
// forwards unknown ones. Known policy (policy below) applies in both.
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

// policy applies the scope's request policy (contracts.md): its
// ceilings on output asked for and on input size (ADR 0031), provider-side
// storage (ADR 0027: turned off where it can be, refused where it cannot),
// and the state M1 never keeps at the provider.
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
	// M1 refers to nothing the provider keeps, whatever the scope allows
	// (security-and-correctness.md): no stored responses, no uploaded
	// files, no item references. A stable, not retryable, policy refusal.
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

// inputCeiling is the scope's ceiling on a request's size (ADR 0031), a
// token count's too.
func inputCeiling(p *auth.Principal, requestBytes int64) *refusal {
	if c := p.Limits.MaxInputBytes; c > 0 && requestBytes > c {
		return &refusal{code: contract.RequestOverCeiling, msg: fmt.Sprintf("the request is %d bytes; this scope accepts at most %d: shorten the context, or ask its managers", requestBytes, c)}
	}
	return nil
}
