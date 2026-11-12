// Package auth turns a request's credential into a Principal: who is
// calling, in which scope, with which rights (ADR 0009, org-model.md §3).
// Everything is decided from the snapshot, with no network call. This first
// version authenticates golem keys; Entra tokens are next.
package auth

import (
	"encoding/hex"
	"net/http"
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
	Strict            bool
	PrincipalInFlight int
	ScopeInFlight     int
}

// Allows reports whether the principal may call an alias: the intersection
// of the scope's policy and the key's constraints (org-model.md §3).
func (p *Principal) Allows(alias []byte) bool { return p.models == nil || p.models[string(alias)] }

// Keys authenticates against one snapshot's entitlements.
type Keys struct {
	env      key.Env
	byHash   map[[32]byte]*entry
	implicit *Principal
	now      func() time.Time
}

type entry struct {
	p       Principal
	expires time.Time
	revoked bool
}

// The standalone implicit principal's in-flight limit: generous, since one
// developer's agents may run many calls at once.
const implicitInFlight = 16

// Compile indexes a snapshot's entitlements for an installation: env is the
// key prefix this gateway accepts (Live in production, Dev standalone).
func Compile(e snapshot.Entitlements, env key.Env, now func() time.Time) *Keys {
	k := &Keys{env: env, byHash: map[[32]byte]*entry{}, now: now}
	if e.Implicit {
		k.implicit = &Principal{ID: "local", Kind: "user", Scope: "local",
			Limits: Limits{PrincipalInFlight: implicitInFlight}}
	}
	scopes := map[string]snapshot.Scope{}
	for _, s := range e.Scopes {
		scopes[s.ID] = s
	}
	for _, sk := range e.Keys {
		var h [32]byte
		if b, err := hex.DecodeString(sk.Hash); err != nil || len(b) != 32 {
			continue // lib/compile wrote it; a bad one cannot match anything
		} else {
			copy(h[:], b)
		}
		sc := scopes[sk.Scope]
		models := map[string]bool{}
		for _, m := range sc.Models {
			if len(sk.Models) == 0 || contains(sk.Models, m) {
				models[m] = true
			}
		}
		k.byHash[h] = &entry{
			p: Principal{ID: sk.Principal, Kind: sk.PrincipalKind, Scope: sk.Scope, Key: sk.Display, models: models,
				Limits: Limits{BudgetMicros: sc.BudgetMicros, Strict: sc.Strict,
					PrincipalInFlight: sc.PrincipalInFlight, ScopeInFlight: sc.ScopeInFlight}},
			expires: sk.ExpiresAt, revoked: sk.Revoked,
		}
	}
	return k
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
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
	cred := credential(h)
	if k.implicit != nil && (cred == "" || !strings.HasPrefix(cred, "glm_")) {
		return k.implicit, nil // standalone: whatever the SDK sends (ADR 0023)
	}
	if cred == "" {
		return nil, &Refusal{contract.AuthMissing, "no credential: send a golem key as Authorization: Bearer or x-api-key (golemctl keys mint)"}
	}
	env, err := key.Parse(cred)
	if err != nil {
		if !strings.HasPrefix(cred, "glm_") {
			return nil, &Refusal{contract.AuthMissing, "this credential is not a golem key; golem takes its own keys (Entra tokens are coming): golemctl keys mint"}
		}
		return nil, &Refusal{contract.AuthKeyUnknown, "this golem key is malformed (a mistyped or truncated key): copy it again"}
	}
	if env != k.env {
		return nil, &Refusal{contract.AuthKeyWrongInstallation, "a glm_" + string(env) + "_ key, but this gateway takes glm_" + string(k.env) + "_ keys: use a key of this installation"}
	}
	e := k.byHash[key.Hash(cred)]
	switch {
	case e == nil:
		return nil, &Refusal{contract.AuthKeyUnknown, "this key is not known to golem (it may never have been minted here); golemctl keys list shows yours"}
	case e.revoked:
		return nil, &Refusal{contract.AuthKeyRevoked, "this key was revoked: mint a new one (golemctl keys mint)"}
	case !e.expires.IsZero() && !k.now().Before(e.expires):
		return nil, &Refusal{contract.AuthKeyExpired, "this key expired at " + e.expires.Format(time.RFC3339) + ": mint a new one (golemctl keys mint)"}
	}
	return &e.p, nil
}

func credential(h http.Header) string {
	if a := h.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "bearer ") {
		return strings.TrimSpace(a[7:])
	}
	return strings.TrimSpace(h.Get("X-Api-Key"))
}
