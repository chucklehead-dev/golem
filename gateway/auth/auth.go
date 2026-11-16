// Package auth turns a request's credential into a Principal: who is
// calling, in which scope, with which rights (ADR 0009, org-model.md §3).
// Everything is decided from the snapshot, with no network call: golem keys
// by their hash, Entra tokens by the tenant's cached signing keys.
package auth

import (
	"cmp"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/chucklehead-dev/golem/gateway/admission"
	"github.com/chucklehead-dev/golem/lib/contract"
	"github.com/chucklehead-dev/golem/lib/entra"
	"github.com/chucklehead-dev/golem/lib/key"
	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// Principal is an authenticated caller and its effective rights.
type Principal struct {
	ID, Kind string
	Scope    string
	Key      string // the key's display prefix, if a key was used
	// models is the scope's aliases narrowed by the key's; nil allows every
	// route (standalone's implicit principal only).
	models  map[string]bool
	entraID string // the Entra ID a token named, for the denylist
	Limits  Limits
}

// Limits are the scope's, for the limiter.
type Limits struct {
	BudgetMicros int64
	// MemberRPM and ScopeRPM are the scope's rate limits, requests a
	// minute: each member's, and the whole scope's (PRD §5.5); 0: none.
	MemberRPM, ScopeRPM int64
	BudgetMode          admission.BudgetMode // the snapshot's; an unknown one is enforce
	PrincipalInFlight   int
	ScopeInFlight       int
	// The scope's request policy (contracts.md §8).
	StorageAllowed  bool
	MaxOutputTokens int64  // max_tokens times n; 0: none
	MaxInputBytes   int64  // 0: none
	Features        string // restricted, compatible, or "" (by the route)
}

// Allows reports whether the principal may call an alias: the intersection
// of the scope's policy and the key's constraints (org-model.md §3).
func (p *Principal) Allows(alias []byte) bool { return p.models == nil || p.models[string(alias)] }

// Keys authenticates against one snapshot's entitlements: golem keys, and
// Entra tokens when an Entra verifier is configured. The chain picks by the
// credential's shape, and whichever recognises it decides (ADR 0009).
type Keys struct {
	env      key.Env
	byHash   map[[32]byte]*entry
	prefix   string // "glm_live_": the installation's keys
	implicit *Principal
	local    [32]byte // the implicit principal's key, hashed (RequireLocalKey)
	now      func() time.Time
	entra    *entra.Verifier
	deny     *DenyFile // the site's emergency denylist, applied over the snapshot
	// byEntra: a user's oid or a service's app ID, then a scope, to the
	// principal acting there; the empty scope is the default's.
	byEntra map[string]map[string]*Principal
}

type entry struct {
	p       Principal
	expires time.Time
	revoked bool
	// refused, if set, is why the key cannot be used whatever its state:
	// its scope is gone from the snapshot, or its principal left it.
	refused *Refusal
}

// The standalone implicit principal's in-flight limit: generous, since one
// developer's agents may run many calls at once.
const implicitInFlight = 16

// rights are a scope's, shared by every principal acting in it.
type rights struct {
	models map[string]bool
	limits Limits
}

// Compile indexes a snapshot's entitlements for an installation: env is the
// key prefix this gateway accepts (Live in production, Dev standalone), and
// v verifies Entra tokens (nil: they are refused). Whatever names a
// scope the snapshot lacks is refused, never given a zero (unlimited)
// budget.
func Compile(e snapshot.Entitlements, env key.Env, now func() time.Time, v *entra.Verifier) *Keys {
	k := &Keys{env: env, prefix: "glm_" + string(env) + "_", byHash: map[[32]byte]*entry{}, now: now, entra: v, byEntra: map[string]map[string]*Principal{}}
	if e.Implicit && env == key.Dev { // never for a live installation (golem#5)
		k.implicit = &Principal{ID: "local", Kind: "user", Scope: "local",
			Limits: Limits{PrincipalInFlight: implicitInFlight}}
	}
	scopes := compileScopes(e.Scopes)
	owners := map[string]owner{}
	for _, o := range e.KeyOwners {
		owners[o.ID] = owner{o.Kind, o.Scopes}
	}
	k.addMembers("user", e.Users, scopes, owners)
	k.addMembers("service", e.Services, scopes, owners)
	for _, sk := range e.Keys {
		b, err := hex.DecodeString(sk.Hash)
		if err != nil || len(b) != 32 {
			continue // lib/compile wrote it; a bad one cannot match anything
		}
		k.byHash[[32]byte(b)] = keyEntry(sk, scopes, owners)
	}
	return k
}

// compileScopes is each scope's rights, by its ID.
func compileScopes(ss []snapshot.Scope) map[string]*rights {
	scopes := make(map[string]*rights, len(ss))
	for _, s := range ss {
		r := &rights{models: make(map[string]bool, len(s.Models)), limits: Limits{BudgetMicros: s.BudgetMicros,
			MemberRPM: s.MemberRPM, ScopeRPM: s.ScopeRPM, BudgetMode: admission.ModeOf(s.BudgetMode),
			PrincipalInFlight: s.PrincipalInFlight, ScopeInFlight: s.ScopeInFlight, StorageAllowed: s.StorageAllowed,
			MaxOutputTokens: s.MaxOutputTokens, MaxInputBytes: s.MaxInputBytes, Features: s.Features}}
		for _, a := range s.Models {
			r.models[a] = true
		}
		scopes[s.ID] = r
	}
	return scopes
}

// owner is a principal a key may belong to: its kind and its scopes.
type owner struct {
	kind   string
	scopes []string
}

// addMembers indexes users' or services' Entra identities by the scopes
// they act in (the empty scope their default's), and records each as an
// owner of kind for its keys, with its membership's scopes added.
func (k *Keys) addMembers(kind string, ms []snapshot.Member, scopes map[string]*rights, owners map[string]owner) {
	for _, m := range ms {
		owners[m.ID] = owner{kind, append(slices.Clone(owners[m.ID].scopes), m.Scopes...)}
		byScope := map[string]*Principal{}
		for _, id := range m.Scopes {
			if r := scopes[id]; r != nil {
				byScope[id] = &Principal{ID: m.ID, Kind: kind, Scope: id, models: r.models, entraID: m.Entra, Limits: r.limits}
			}
		}
		if p := byScope[m.Default]; p != nil {
			byScope[""] = p
		}
		k.byEntra[m.Entra] = byScope
	}
}

// keyEntry is a key's entry: its principal with the scope's rights,
// narrowed by the key's models, or why it is refused whatever its state.
func keyEntry(sk snapshot.Key, scopes map[string]*rights, owners map[string]owner) *entry {
	en := &entry{p: Principal{ID: sk.Principal, Kind: sk.PrincipalKind, Scope: sk.Scope, Key: sk.Display},
		expires: sk.ExpiresAt, revoked: sk.Revoked}
	r := scopes[sk.Scope]
	o, listed := owners[sk.Principal]
	switch {
	case r == nil:
		en.refused = &Refusal{contract.AuthzScopeNotMember, "this key's scope " + sk.Scope + " no longer exists: mint a key for a scope you are in"}
		return en
	case !listed || o.kind != cmp.Or(sk.PrincipalKind, "user"):
		// Fail closed (golem#5): a key whose owner is gone, or is of
		// another kind than the key says, is no key.
		en.refused = &Refusal{contract.AuthKeyRevoked, "this key's principal is no longer known to golem: mint a new key"}
		return en
	case !slices.Contains(o.scopes, sk.Scope):
		en.refused = &Refusal{contract.AuthzScopeNotMember, "this key's principal is no longer a member of scope " + sk.Scope + ": its managers can add it back, or mint a key for a scope it is in"}
		return en
	}
	en.p.Limits, en.p.models = r.limits, r.models
	if len(sk.Models) > 0 { // the key narrows its scope's models
		en.p.models = make(map[string]bool, len(sk.Models))
		for _, m := range sk.Models {
			if r.models[m] {
				en.p.models[m] = true
			}
		}
	}
	return en
}

// Anyone is the principal of a snapshot with no entitlements to check
// (tests): every route, no limits.
var Anyone = &Principal{ID: "anyone", Kind: "user", Scope: "any"} // read-only

// Refusal is why a request was not authenticated: its contract code and a
// message that says what to do.
type Refusal struct {
	Code contract.Code
	Msg  string
}

// Deny applies a site's emergency denylist to every request these keys
// authenticate, over what the snapshot allows.
func (k *Keys) Deny(f *DenyFile) { k.deny = f }

// Authenticate finds the request's principal. The credential, a golem key
// or an Entra token, is in Authorization: Bearer (OpenAI's SDKs) or
// x-api-key (Anthropic's). A caller the site's emergency denylist names is
// refused, whatever the snapshot says.
func (k *Keys) Authenticate(h http.Header) (*Principal, *Refusal) {
	var g Grant
	return k.AuthenticateGrant(h, &g)
}

// AuthenticateGrant is Authenticate, recording in g what it honoured, so
// a call in flight can later ask another snapshot's keys whether they
// still would (Honours). g is the caller's, kept where it keeps the call
// (the request path's pooled exchange), so nothing is allocated for it.
func (k *Keys) AuthenticateGrant(h http.Header, g *Grant) (*Principal, *Refusal) {
	*g = Grant{}
	cred, conflict := credential(h)
	if conflict {
		return nil, &Refusal{contract.AuthMissing, "two different credentials (Authorization and x-api-key): send one"}
	}
	p, r := k.principal(cred, h, g)
	switch {
	case r != nil:
		*g = Grant{}
		return nil, r
	case k.deny.Current().denies(p, keyCred(cred), p.entraID):
		*g = Grant{}
		return nil, denied
	}
	g.p = p
	return p, nil
}

// Grant is what one authentication honoured: the credential's identity (a
// key's hash and text, a token's Entra ID by its principal) and the
// principal it acted as. The zero Grant is none (a snapshot without
// entitlements), which every Keys honours.
type Grant struct {
	p    *Principal
	kind grantKind
	hash [32]byte // a key's
	cred string   // a key's text, for the denylist's prefixes
}

type grantKind uint8

const (
	grantNone grantKind = iota
	grantImplicit
	grantKey
	grantToken
)

// Principal is the principal the grant authenticated; nil for none.
func (g *Grant) Principal() *Principal { return g.p }

// Honours reports whether these keys, with the denylist in force now,
// still honour a grant another snapshot's keys made, by the rules
// Authenticate applies: a key while it is listed and not revoked, its
// scope exists and its principal is a listed member of it; a token while
// its principal is a member of the scope it acted in; neither while the
// denylist names it. It returns the refusal a call in flight is ended
// with, nil if honoured. Expiry (a key's, a token's) is not checked: the
// passing of time ends no call that began in time, as the authorization
// lease's expiry stops only new requests (ADR 0041).
func (k *Keys) Honours(g *Grant) *Refusal {
	var p *Principal
	switch g.kind {
	case grantNone:
		return nil
	case grantImplicit:
		if k.implicit == nil || k.local == ([32]byte{}) {
			return &Refusal{contract.AuthKeyRevoked, "this standalone gateway no longer takes its local key: its configuration names its keys now"}
		}
		p = k.implicit
	case grantKey:
		e := k.byHash[g.hash]
		switch {
		case e == nil:
			return &Refusal{contract.AuthKeyRevoked, "this key is no longer in golem's configuration (revoked or removed): mint a new one (golemctl keys mint)"}
		case e.revoked:
			return &Refusal{contract.AuthKeyRevoked, "this key was revoked: mint a new one (golemctl keys mint)"}
		case e.refused != nil:
			return e.refused
		}
		p = &e.p
	case grantToken:
		q := k.byEntra[g.p.entraID][g.p.Scope]
		if q == nil || q.Kind != g.p.Kind {
			return &Refusal{contract.AuthzScopeNotMember, "no longer a member of scope " + g.p.Scope + ": its managers can add you back"}
		}
		p = q
	}
	if k.deny.Current().denies(p, g.cred, p.entraID) {
		return denied
	}
	return nil
}

// keyCred is cred if it is a golem key, else "" (a token is matched by its
// Entra ID).
func keyCred(cred string) string {
	if looksLikeJWS(cred) {
		return ""
	}
	return cred
}

// principal is the snapshot's principal for cred, before the denylist: the
// implicit principal for the local key, an Entra token's in the scope h's
// golem-scope header names, or a golem key's.
func (k *Keys) principal(cred string, h http.Header, g *Grant) (*Principal, *Refusal) {
	if k.implicit != nil {
		// Standalone without a LocalIdentity: the one local principal, for
		// the gateway's own generated key alone (ADR 0023; golem#5). Its
		// browser guard keeps web pages out; the key keeps out other
		// programs and users of the machine.
		if sum := key.Hash(cred); k.local != ([32]byte{}) && subtle.ConstantTimeCompare(sum[:], k.local[:]) == 1 {
			g.kind, g.cred = grantImplicit, cred
			return k.implicit, nil
		}
		return nil, &Refusal{contract.AuthKeyUnknown, "this standalone gateway takes its own local key: the one it printed at start, in the file it named"}
	}
	if cred == "" {
		return nil, &Refusal{contract.AuthMissing, "no credential: send a golem key as Authorization: Bearer or x-api-key (golemctl keys mint), or an Entra token (golemctl token)"}
	}
	if looksLikeJWS(cred) {
		g.kind = grantToken
		return k.token(cred, h.Get(contract.HeaderScope))
	}
	// A known hash is a valid key: parsing it (base62 through math/big) is
	// needed only to say why an unknown one is refused. The installation's
	// prefix is checked first all the same, cheaply: the snapshot's keys
	// carry no environment, and a publisher's mistake must not let another
	// installation's key in.
	var e *entry
	if strings.HasPrefix(cred, k.prefix) {
		g.hash = key.Hash(cred)
		e = k.byHash[g.hash]
	}
	if e == nil {
		env, err := key.Parse(cred)
		switch {
		case err != nil && !strings.HasPrefix(cred, "glm_"):
			return nil, &Refusal{contract.AuthMissing, "this credential is neither a golem key nor an Entra token: golemctl keys mint, or golemctl token"}
		case err != nil:
			return nil, &Refusal{contract.AuthKeyUnknown, "this golem key is malformed (a mistyped or truncated key): copy it again"}
		case env != k.env:
			return nil, &Refusal{contract.AuthKeyWrongInstallation, "a glm_" + string(env) + "_ key, but this gateway takes glm_" + string(k.env) + "_ keys: use a key of this installation"}
		}
	}
	switch {
	case e == nil:
		return nil, &Refusal{contract.AuthKeyUnknown, "this key is not known to golem (it may never have been minted here); golemctl keys list shows yours"}
	case e.revoked:
		return nil, &Refusal{contract.AuthKeyRevoked, "this key was revoked: mint a new one (golemctl keys mint)"}
	case !e.expires.IsZero() && !k.now().Before(e.expires):
		return nil, &Refusal{contract.AuthKeyExpired, "this key expired at " + e.expires.Format(time.RFC3339) + ": mint a new one (golemctl keys mint)"}
	case e.refused != nil:
		return nil, e.refused
	}
	g.kind, g.cred = grantKey, cred
	return &e.p, nil
}

// token authenticates an Entra token and finds the principal in the scope
// the golem-scope header names, else its default (contracts.md §5).
func (k *Keys) token(raw, scope string) (*Principal, *Refusal) {
	if k.entra == nil {
		return nil, &Refusal{contract.AuthTokenInvalid, "this gateway does not take Entra tokens; use a golem key"}
	}
	id, err := k.entra.Verify(raw)
	if err != nil {
		code := contract.AuthTokenInvalid
		if e, ok := errors.AsType[*entra.Error](err); ok && e.Expired {
			code = contract.AuthTokenExpired
		} else if ok && e.Unavailable {
			// The keys are not fetched yet: the gateway's state, not the
			// token's fault, and retryable (503 with Retry-After).
			code = contract.GatewayNotReady
		}
		return nil, &Refusal{code, err.Error()}
	}
	byScope := k.byEntra[id.ID]
	if byScope == nil {
		what := "user"
		if id.Service {
			what = "service"
		}
		return nil, &Refusal{contract.AuthzScopeNotMember, "this " + what + " is not a member of any golem scope (Entra group sync runs every five minutes; a service must be registered in golem)"}
	}
	p := byScope[scope]
	if p == nil || (p.Kind == "service") != id.Service {
		return nil, &Refusal{contract.AuthzScopeNotMember, "not a member of scope " + scope + ": name one of yours in golem-scope, or leave it out for your default"}
	}
	return p, nil
}

// credential is the request's credential, from Authorization: Bearer or
// x-api-key; conflict reports both present and different, which is
// refused rather than one silently preferred (golem#5).
func credential(h http.Header) (cred string, conflict bool) {
	var bearer string
	if a := h.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		bearer = strings.TrimSpace(a[7:])
	}
	apiKey := strings.TrimSpace(h.Get("X-Api-Key"))
	if bearer != "" && apiKey != "" && bearer != apiKey {
		return "", true
	}
	return cmp.Or(bearer, apiKey), false
}

// RequireLocalKey sets the standalone implicit principal's key: without
// one the implicit principal admits no one.
func (k *Keys) RequireLocalKey(local string) { k.local = key.Hash(local) }

// looksLikeJWS is the chain's test for an Entra token: three base64url
// parts, the first a JSON object.
func looksLikeJWS(s string) bool {
	return strings.Count(s, ".") == 2 && strings.HasPrefix(s, "eyJ")
}
