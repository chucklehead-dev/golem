package auth

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

	"github.com/chucklehead-dev/golem/lib/contract"
)

// Entra verifies Entra ID v2 access tokens for golem's API (ADR 0009,
// entra.md #1), locally: the signing keys are cached and refreshed in the
// background, so the request path makes no network call. A token proves
// who is calling, nothing more: rights come from the snapshot.
type Entra struct {
	// Tenant is the directory (tenant) ID tokens must come from.
	Tenant string
	// Audiences are golem API's identifiers: its Application ID URI
	// (api://golem) and its client ID, either of which a v2 token may carry.
	Audiences []string
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

// The delegated scope a user's token must carry, and the app role a
// service's must (entra.md #1).
const (
	ScopeAccess = "golem.access"
	RoleService = "golem.service"
	// RoleAdmin is the app role that makes a caller a golem admin
	// (org-model.md §2): the control API's, not the gateway's.
	RoleAdmin = "golem.admin"
)

// Identity is who a token says is calling.
type Identity struct {
	Service bool   // a service (client credentials, workload identity), else a user
	ID      string // the user's oid, or the service's app ID (azp)
	Admin   bool   // the token carries the golem.admin app role
}

type entraClaims struct {
	Tid   string   `json:"tid"`
	Oid   string   `json:"oid"`
	Azp   string   `json:"azp"`
	Scp   string   `json:"scp"`
	Roles []string `json:"roles"`
	Ver   string   `json:"ver"`
}

// Verify checks a token and says who it names.
func (e *Entra) Verify(raw string) (Identity, *Refusal) {
	invalid := func(why string) (Identity, *Refusal) {
		return Identity{}, &Refusal{contract.AuthTokenInvalid, "the Entra token is not valid for golem: " + why}
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
		return Identity{}, &Refusal{contract.AuthTokenExpired, "the Entra token has expired: get a new one (golemctl token)"}
	case errors.Is(err, jwt.ErrInvalidIssuer):
		return invalid("issued by another tenant or not a v2 token")
	case errors.Is(err, jwt.ErrInvalidAudience):
		return invalid("issued for another API (its audience is not golem's)")
	case err != nil:
		return invalid(err.Error())
	case c.Tid != e.Tenant || c.Ver != "2.0":
		return invalid("not a v2 token of this tenant")
	}
	switch {
	case slices.Contains(strings.Fields(c.Scp), ScopeAccess) && c.Oid != "":
		return Identity{ID: c.Oid, Admin: slices.Contains(c.Roles, RoleAdmin)}, nil
	case c.Scp == "" && slices.Contains(c.Roles, RoleService) && c.Azp != "":
		return Identity{Service: true, ID: c.Azp, Admin: slices.Contains(c.Roles, RoleAdmin)}, nil
	}
	return invalid("it carries neither the " + ScopeAccess + " scope (a user) nor the " + RoleService + " role (a service)")
}

func (e *Entra) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Entra) key(kid string) *rsa.PublicKey {
	m := e.keys.Load()
	if m == nil {
		return nil
	}
	return (*m)[kid]
}

// SetKeys replaces the cached key set (Refresh's, or a test's).
func (e *Entra) SetKeys(set jose.JSONWebKeySet) {
	m := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if pk, ok := k.Key.(*rsa.PublicKey); ok && k.KeyID != "" && (k.Use == "" || k.Use == "sig") {
			m[k.KeyID] = pk
		}
	}
	e.keys.Store(&m)
	e.keysAt.Store(e.now().UnixNano())
}

// keysClient fetches key sets when Entra.Client is nil (read-only): a hung
// endpoint must not hold a gateway's start, nor its refresh loop.
var keysClient = &http.Client{Timeout: 10 * time.Second}

// maxKeySet bounds a key set's body: Entra's is a few kilobytes.
const maxKeySet = 1 << 20

// Refresh fetches the tenant's key set.
func (e *Entra) Refresh(ctx context.Context) error {
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
func (e *Entra) wakeup() chan struct{} {
	e.wakeOne.Do(func() { e.wake = make(chan struct{}, 1) })
	return e.wake
}

// requestRefresh asks Run for a refresh; one already asked for absorbs it.
func (e *Entra) requestRefresh() {
	select {
	case e.wakeup() <- struct{}{}:
	default:
	}
}

// Run keeps the key set fresh: hourly, and when a token names an unknown
// key (at most once a minute).
func (e *Entra) Run(ctx context.Context) {
	wake := e.wakeup()
	hourly := time.NewTicker(time.Hour)
	defer hourly.Stop()
	var last time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-hourly.C:
		case <-wake:
			if e.now().Sub(last) < time.Minute {
				continue
			}
		}
		last = e.now()
		if err := e.Refresh(ctx); err != nil {
			slog.Warn("could not refresh Entra's signing keys; using the cached ones", "err", err)
		}
	}
}

// looksLikeJWS is the chain's test for an Entra token: three base64url
// parts, the first a JSON object.
func looksLikeJWS(s string) bool {
	return strings.Count(s, ".") == 2 && strings.HasPrefix(s, "eyJ")
}
