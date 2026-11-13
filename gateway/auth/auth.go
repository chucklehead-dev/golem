// Package auth turns a request's credential into a Principal: who is
// calling, in which scope, with which rights (ADR 0009, org-model.md §3).
// Everything is decided from the snapshot, with no network call: golem keys
// by their hash, Entra tokens by the tenant's cached signing keys.
package auth

import (
	"cmp"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/chucklehead-dev/golem/lib/contract"
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
	models map[string]bool
	Limits Limits
}

// Limits are the scope's, for the limiter.
type Limits struct {
	BudgetMicros      int64
	WorstCaseCheck    bool
	PrincipalInFlight int
	ScopeInFlight     int
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
	entra    *Entra
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
// entra verifies Entra tokens (nil: they are refused). Whatever names a
// scope the snapshot lacks is refused, never given a zero (unlimited)
// budget.
func Compile(e snapshot.Entitlements, env key.Env, now func() time.Time, entra *Entra) *Keys {
	k := &Keys{env: env, prefix: "glm_" + string(env) + "_", byHash: map[[32]byte]*entry{}, now: now, entra: entra, byEntra: map[string]map[string]*Principal{}}
	if e.Implicit && env == key.Dev { // never for a live installation (golem#5)
		k.implicit = &Principal{ID: "local", Kind: "user", Scope: "local",
			Limits: Limits{PrincipalInFlight: implicitInFlight}}
	}
	scopes := make(map[string]*rights, len(e.Scopes))
	for _, s := range e.Scopes {
		r := &rights{models: make(map[string]bool, len(s.Models)), limits: Limits{BudgetMicros: s.BudgetMicros, WorstCaseCheck: s.WorstCaseCheck,
			PrincipalInFlight: s.PrincipalInFlight, ScopeInFlight: s.ScopeInFlight, StorageAllowed: s.StorageAllowed,
			MaxOutputTokens: s.MaxOutputTokens, MaxInputBytes: s.MaxInputBytes, Features: s.Features}}
		for _, a := range s.Models {
			r.models[a] = true
		}
		scopes[s.ID] = r
	}
	// ownerOf: every listed principal's kind and scopes, by its golem ID.
	type owner struct {
		kind   string
		scopes []string
	}
	ownerOf := map[string]owner{}
	for _, o := range e.KeyOwners {
		ownerOf[o.ID] = owner{o.Kind, o.Scopes}
	}
	member := func(kind string, ms []snapshot.Member) {
		for _, m := range ms {
			o := ownerOf[m.ID]
			ownerOf[m.ID] = owner{kind, append(slices.Clone(o.scopes), m.Scopes...)}
			byScope := map[string]*Principal{}
			for _, id := range m.Scopes {
				if r := scopes[id]; r != nil {
					byScope[id] = &Principal{ID: m.ID, Kind: kind, Scope: id, models: r.models, Limits: r.limits}
				}
			}
			if p := byScope[m.Default]; p != nil {
				byScope[""] = p
			}
			k.byEntra[m.Entra] = byScope
		}
	}
	member("user", e.Users)
	member("service", e.Services)
	for _, sk := range e.Keys {
		var h [32]byte
		if b, err := hex.DecodeString(sk.Hash); err != nil || len(b) != 32 {
			continue // lib/compile wrote it; a bad one cannot match anything
		} else {
			copy(h[:], b)
		}
		en := &entry{p: Principal{ID: sk.Principal, Kind: sk.PrincipalKind, Scope: sk.Scope, Key: sk.Display},
			expires: sk.ExpiresAt, revoked: sk.Revoked}
		k.byHash[h] = en
		r := scopes[sk.Scope]
		o, listed := ownerOf[sk.Principal]
		switch {
		case r == nil:
			en.refused = &Refusal{contract.AuthzScopeNotMember, "this key's scope " + sk.Scope + " no longer exists: mint a key for a scope you are in"}
			continue
		case !listed || o.kind != cmp.Or(sk.PrincipalKind, "user"):
			// Fail closed (golem#5): a key whose owner is gone, or is of
			// another kind than the key says, is no key.
			en.refused = &Refusal{contract.AuthKeyRevoked, "this key's principal is no longer known to golem: mint a new key"}
			continue
		case !slices.Contains(o.scopes, sk.Scope):
			en.refused = &Refusal{contract.AuthzScopeNotMember, "this key's principal is no longer a member of scope " + sk.Scope + ": its managers can add it back, or mint a key for a scope it is in"}
			continue
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
	}
	return k
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

// Authenticate finds the request's principal. The credential is a golem key
// in Authorization: Bearer (OpenAI's SDKs) or x-api-key (Anthropic's).
func (k *Keys) Authenticate(h http.Header) (*Principal, *Refusal) {
	cred, conflict := credential(h)
	if conflict {
		return nil, &Refusal{contract.AuthMissing, "two different credentials (Authorization and x-api-key): send one"}
	}
	if k.implicit != nil {
		// Standalone without a LocalIdentity: the one local principal, for
		// the gateway's own generated key alone (ADR 0023; golem#5). Its
		// browser guard keeps web pages out; the key keeps out other
		// programs and users of the machine.
		if h := key.Hash(cred); k.local != ([32]byte{}) && subtle.ConstantTimeCompare(h[:], k.local[:]) == 1 {
			return k.implicit, nil
		}
		return nil, &Refusal{contract.AuthKeyUnknown, "this standalone gateway takes its own local key: the one it printed at start, in the file it named"}
	}
	if cred == "" {
		return nil, &Refusal{contract.AuthMissing, "no credential: send a golem key as Authorization: Bearer or x-api-key (golemctl keys mint), or an Entra token (golemctl token)"}
	}
	if looksLikeJWS(cred) {
		return k.token(cred, h.Get(contract.HeaderScope))
	}
	// A known hash is a valid key: parsing it (base62 through math/big) is
	// needed only to say why an unknown one is refused. The installation's
	// prefix is checked first all the same, cheaply: the snapshot's keys
	// carry no environment, and a publisher's mistake must not let another
	// installation's key in.
	var e *entry
	if strings.HasPrefix(cred, k.prefix) {
		e = k.byHash[key.Hash(cred)]
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
	return &e.p, nil
}

// token authenticates an Entra token and finds the principal in the scope
// the golem-scope header names, else its default (contracts.md §5).
func (k *Keys) token(raw, scope string) (*Principal, *Refusal) {
	if k.entra == nil {
		return nil, &Refusal{contract.AuthTokenInvalid, "this gateway does not take Entra tokens; use a golem key"}
	}
	id, r := k.entra.Verify(raw)
	if r != nil {
		return nil, r
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
