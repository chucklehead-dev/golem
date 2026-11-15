// Package entra verifies Entra ID v2 access tokens, locally (ADR 0009,
// entra.md): the gateway's (golem API's registration) and the control
// API's (its own, ADR 0046), each a Verifier for its audience and scope.
// It names who calls; rights are the caller's business.
package entra

import (
	"cmp"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Verifier verifies Entra ID v2 access tokens for one golem API (ADR 0009,
// entra.md #1), locally: the signing keys are cached and refreshed in the
// background, so the request path makes no network call. A token proves
// who is calling, nothing more: rights come from the snapshot.
type Verifier struct {
	// Tenant is the directory (tenant) ID tokens must come from.
	Tenant string
	// Audiences are the resource's identifiers: its Application ID URI and
	// its client ID, either of which a v2 token may carry. The gateway's
	// resource is golem API (api://golem); the control API's is its own
	// registration (api://golem-control, ADR 0046), so a token for one is
	// never accepted by the other.
	Audiences []string
	// Scope is the delegated scope a user's token must carry: ScopeAccess
	// (the default) for the gateway, ScopeManage for the control API.
	Scope string
	// JWKS is the tenant's key set URL; default
	// https://login.microsoftonline.com/<tenant>/discovery/v2.0/keys.
	JWKS string
	// Client fetches the key set; default a client with a 10-second timeout.
	Client *http.Client
	Now    func() time.Time

	// MaxKeyAge is how old the cached key set may grow while refreshes
	// fail before tokens are refused (default 24 hours): a key Entra has
	// revoked must not be trusted for ever (golem#5).
	MaxKeyAge time.Duration

	keys    atomic.Pointer[map[string]*rsa.PublicKey]
	keysAt  atomic.Int64 // when the key set was fetched, Unix nanoseconds
	wake    chan struct{}
	wakeOne sync.Once
}

// Leeway tolerates clock skew between Entra and the gateway.
const Leeway = time.Minute

// The delegated scopes a user's token must carry, and the app role a
// service's must (entra.md #1, #1b; each registration defines its own).
const (
	ScopeAccess = "golem.access" // golem API's: calling models
	ScopeManage = "golem.manage" // the control API's: golemctl and the console
	RoleService = "golem.service"
	// RoleAdmin is the app role that makes a caller a golem admin
	// (org-model.md §2): the control API registration's, not the gateway's.
	RoleAdmin = "golem.admin"
)

// Identity is who a token says is calling.
type Identity struct {
	Service bool   // a service (client credentials, workload identity), else a user
	ID      string // the user's oid, or the service's app ID (azp)
	Admin   bool   // the token carries the golem.admin app role
}

// entraClaims are the Entra-specific claims Verify reads, beside the
// registered ones (jwt.Claims).
type entraClaims struct {
	Tid   string   `json:"tid"`
	Oid   string   `json:"oid"`
	Azp   string   `json:"azp"`
	Scp   string   `json:"scp"`
	Roles []string `json:"roles"`
	Ver   string   `json:"ver"`
	// Idtyp is "app" on an app-only token (an optional claim each
	// registration must request, entra.md): a service is recognised by it,
	// not only by the absence of a scope.
	Idtyp string `json:"idtyp"`
}

// Error is a token refused: Expired, Unavailable (it cannot be checked
// yet: the signing keys have never been fetched), or invalid for Msg's
// reason (said so the caller can act on it).
type Error struct {
	Expired     bool
	Unavailable bool
	Msg         string
}

// errNoKeys is Verify's refusal before the first key set: not the token's
// fault, so a caller can answer it as a retryable unavailability rather
// than an invalid credential.
var errNoKeys = &Error{Unavailable: true, Msg: "golem cannot check Entra tokens yet: Entra's signing keys not yet fetched (the gateway retries); retry shortly, or call with a golem key"}

// Error implements error.
func (e *Error) Error() string { return e.Msg }

// Verify checks a token and says who it names; a refusal is an *Error.
func (e *Verifier) Verify(raw string) (Identity, error) {
	invalid := func(why string) (Identity, error) {
		return Identity{}, &Error{Msg: "the Entra token is not valid for golem: " + why}
	}
	// RS256 only: anything else (none, HMAC with a public key as its
	// secret) is refused before a key is looked at.
	tok, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return invalid("not an RS256-signed JWT")
	}
	if len(tok.Headers) != 1 {
		return invalid("expected one signature")
	}
	if e.keys.Load() == nil {
		e.requestRefresh() // Run is retrying already; this wakes it
		return Identity{}, errNoKeys
	}
	if at := e.keysAt.Load(); at != 0 && e.now().Sub(time.Unix(0, at)) > cmp.Or(e.MaxKeyAge, 24*time.Hour) {
		return invalid("golem's copy of Entra's signing keys is too old to trust (they have not been refreshed); the platform team is alerted")
	}
	key := e.key(tok.Headers[0].KeyID)
	if key == nil {
		e.requestRefresh() // Entra rotates keys; refresh off the request path
		return invalid("signed by an unknown key (if Entra just rotated its keys, retry in a minute)")
	}
	var std jwt.Claims
	var c entraClaims
	if err := tok.Claims(key, &std, &c); err != nil {
		return invalid("its signature does not verify")
	}
	if std.Expiry == nil {
		return invalid("it has no expiry (exp)") // Entra's always do: one without never expires
	}
	err = std.ValidateWithLeeway(jwt.Expected{
		Issuer:      "https://login.microsoftonline.com/" + e.Tenant + "/v2.0",
		AnyAudience: jwt.Audience(e.Audiences),
		Time:        e.now(),
	}, Leeway)
	switch {
	case errors.Is(err, jwt.ErrExpired):
		return Identity{}, &Error{Expired: true, Msg: "the Entra token has expired: get a new one (golemctl token)"}
	case errors.Is(err, jwt.ErrInvalidIssuer):
		return invalid("issued by another tenant or not a v2 token")
	case errors.Is(err, jwt.ErrInvalidAudience):
		return invalid("issued for another API (its audience is not golem's)")
	case err != nil:
		return invalid(err.Error())
	case c.Tid != e.Tenant || c.Ver != "2.0":
		return invalid("not a v2 token of this tenant")
	}
	scope := cmp.Or(e.Scope, ScopeAccess)
	switch {
	case slices.Contains(strings.Fields(c.Scp), scope) && c.Oid != "" && c.Idtyp != "app":
		return Identity{ID: c.Oid, Admin: slices.Contains(c.Roles, RoleAdmin)}, nil
	case c.Scp == "" && c.Idtyp == "app" && slices.Contains(c.Roles, RoleService) && c.Azp != "":
		return Identity{Service: true, ID: c.Azp, Admin: slices.Contains(c.Roles, RoleAdmin)}, nil
	case c.Scp == "" && c.Idtyp != "app" && slices.Contains(c.Roles, RoleService):
		return invalid("an app-only token without idtyp \"app\" (the registration must request the idtyp optional claim, entra.md)")
	}
	return invalid("it carries neither the " + scope + " scope (a user) nor the " + RoleService + " role (a service)")
}

func (e *Verifier) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Verifier) key(kid string) *rsa.PublicKey {
	m := e.keys.Load()
	if m == nil {
		return nil
	}
	return (*m)[kid]
}

// SetKeys replaces the cached key set (Refresh's, or a test's).
func (e *Verifier) SetKeys(set jose.JSONWebKeySet) {
	m := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if pk, ok := k.Key.(*rsa.PublicKey); ok && k.KeyID != "" && (k.Use == "" || k.Use == "sig") {
			m[k.KeyID] = pk
		}
	}
	e.keys.Store(&m)
	e.keysAt.Store(e.now().UnixNano())
}

// keysClient fetches key sets when Verifier.Client is nil (read-only): a hung
// endpoint must not hold a gateway's start, nor its refresh loop.
var keysClient = &http.Client{Timeout: 10 * time.Second}

// maxKeySet bounds a key set's body: Entra's is a few kilobytes.
const maxKeySet = 1 << 20

// Refresh fetches the tenant's key set.
func (e *Verifier) Refresh(ctx context.Context) error {
	url := e.JWKS
	if url == "" {
		url = "https://login.microsoftonline.com/" + e.Tenant + "/discovery/v2.0/keys"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := e.Client
	if client == nil {
		client = keysClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("entra keys: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("entra keys: %s from %s", resp.Status, url)
	}
	var set jose.JSONWebKeySet
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxKeySet)).Decode(&set); err != nil {
		return fmt.Errorf("entra keys: %w", err)
	}
	e.SetKeys(set)
	return nil
}

// wakeup is the channel requestRefresh wakes Run by, made on first use.
func (e *Verifier) wakeup() chan struct{} {
	e.wakeOne.Do(func() { e.wake = make(chan struct{}, 1) })
	return e.wake
}

// requestRefresh asks Run for a refresh; one already asked for absorbs it.
func (e *Verifier) requestRefresh() {
	select {
	case e.wakeup() <- struct{}{}:
	default:
	}
}

// Ready reports whether a key set has been fetched: before it, Verify
// refuses every token as Unavailable.
func (e *Verifier) Ready() bool { return e.keys.Load() != nil }

// The refresh schedule (Run).
const (
	refreshEvery   = time.Hour       // a key set fetched is refreshed this often
	refreshSpacing = time.Minute     // a refresh asked for waits this long after the last
	retryFirst     = time.Second     // a failed refresh is retried this soon,
	retryMax       = 5 * time.Minute // doubling to this
)

// Run keeps the key set fresh until ctx ends: at once if none has been
// fetched (a gateway may start while Entra is unreachable), hourly, and
// when a token names an unknown key, at most once a minute: a request
// within the minute after the last refresh is deferred to the minute's
// end, not dropped, so a key Entra rotated in just after a refresh is
// fetched then rather than at the next hour. A failed refresh is retried
// from a second, doubling to five minutes.
func (e *Verifier) Run(ctx context.Context) {
	wake := e.wakeup()
	var last time.Time // the last refresh tried
	retry := retryFirst
	next := e.now()
	if e.Ready() { // fetched before Run (golem-control fetches at its start)
		last = time.Unix(0, e.keysAt.Load())
		next = last.Add(refreshEvery)
	}
	timer := time.NewTimer(max(next.Sub(e.now()), 0))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake:
			if at := last.Add(refreshSpacing); !last.IsZero() && e.now().Before(at) {
				if at.Before(next) {
					next = at
					timer.Reset(at.Sub(e.now()))
				}
				continue
			}
		case <-timer.C:
		}
		last = e.now()
		if err := e.Refresh(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			if e.Ready() {
				slog.Warn("could not refresh Entra's signing keys; using the cached ones", "retry_in", retry, "err", err)
			} else {
				slog.Error("could not fetch Entra's signing keys; Entra tokens are refused until they are (golem keys work)", "retry_in", retry, "err", err)
			}
			next = last.Add(retry)
			retry = min(2*retry, retryMax)
		} else {
			next, retry = last.Add(refreshEvery), retryFirst
		}
		timer.Reset(max(next.Sub(e.now()), 0))
	}
}
