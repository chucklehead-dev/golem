package state

import (
	"cmp"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"time"

	"github.com/chucklehead-dev/golem/lib/snapshot"
)

// The entitlements part of the state (control-plane.md §3.1): principals,
// their memberships, scopes and their policy, keys by hash, and the
// append-only revocations that a restore must preserve (ADR 0041 §5).

// Principal is a user or a service.
type Principal struct {
	Kind string `json:"kind"` // user or service
	// Entra is the user's object ID or the service's app ID; empty for a
	// principal that authenticates by key only.
	Entra   string   `json:"entra,omitempty"`
	Scopes  []string `json:"scopes"`            // memberships
	Default string   `json:"default,omitempty"` // the scope without a golem-scope header
}

// Scope is a scope's policy (org-model.md §3).
type Scope struct {
	Models                 []string `json:"models"` // route aliases its members may call
	BudgetMicros           int64    `json:"budgetMicros,omitempty"`
	DeclaredWorstCaseCheck bool     `json:"declaredWorstCaseCheck,omitempty"`
	PrincipalInFlight      int      `json:"principalInFlight,omitempty"`
	ScopeInFlight          int      `json:"scopeInFlight,omitempty"`
	StorageAllowed         bool     `json:"storageAllowed,omitempty"`
	MaxOutputTokens        int64    `json:"maxOutputTokens,omitempty"`
	MaxInputBytes          int64    `json:"maxInputBytes,omitempty"`
	Features               string   `json:"features,omitempty"`
}

// Key is a key's record: its hash (never the key), its owner and scope.
type Key struct {
	Display   string    `json:"display"`
	Owner     string    `json:"owner"`
	Scope     string    `json:"scope"`
	Models    []string  `json:"models,omitempty"` // narrows the scope's
	ExpiresAt time.Time `json:"expiresAt,omitzero"`
	RevokedAt time.Time `json:"revokedAt,omitzero"`
}

// Revocation is a key revoked: kept for ever, so no restore of older
// state can bring the key back (§3, History).
type Revocation struct {
	Key string    `json:"key"` // its hash
	At  time.Time `json:"at"`
}

// RoleBinding gives a principal a role on a scope (org-model.md §2):
// "manager" for now. Admins hold the Entra app role golem.admin, not a
// binding.
type RoleBinding struct {
	Principal string `json:"principal"`
	Role      string `json:"role"`
	Scope     string `json:"scope"`
}

// AuditEvent is one change, written with it (control-plane.md §3). Seq
// is the store's: assigned at the write, it names the event in the outbox.
type AuditEvent struct {
	Seq    uint64            `json:"seq"`
	At     time.Time         `json:"at"`
	Actor  string            `json:"actor"`
	Action string            `json:"action"` // e.g. key.mint
	Target string            `json:"target"`
	Detail map[string]string `json:"detail,omitempty"`
}

// Has reports whether principal holds role on scope.
func (d *Document) Has(principal, role, scope string) bool {
	return slices.Contains(d.Roles, RoleBinding{principal, role, scope})
}

// Default in-flight limits when a scope sets none (lib/compile's).
const (
	DefaultPrincipalInFlight = 8
	DefaultScopeInFlight     = 64
)

// checkEntitlements is what every write keeps of the entitlements: every
// key's owner exists, and a live key's owner is a member of its scope; a
// principal's scopes exist and its default is one of them; no two
// principals share an Entra ID; and a revocation is never dropped nor its
// key revived.
func checkEntitlements(old, next *Document) error {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: "+f, append([]any{ErrInvariant}, a...)...) }
	entra := map[string]string{}
	for id, p := range next.Principals {
		if p.Kind != "user" && p.Kind != "service" {
			return bad("principal %s is a %q, not a user or service", id, p.Kind)
		}
		for _, s := range p.Scopes {
			if _, ok := next.Scopes[s]; !ok {
				return bad("principal %s is a member of %s, which does not exist", id, s)
			}
		}
		if p.Default != "" && !slices.Contains(p.Scopes, p.Default) {
			return bad("principal %s's default scope %s is not one of its scopes", id, p.Default)
		}
		if p.Entra != "" {
			if other, dup := entra[p.Entra]; dup {
				return bad("principals %s and %s share Entra ID %s", other, id, p.Entra)
			}
			entra[p.Entra] = id
		}
	}
	for _, k := range next.Keys {
		owner, ok := next.Principals[k.Owner]
		switch {
		case !ok:
			return bad("key %s's owner %s does not exist", k.Display, k.Owner)
		case k.RevokedAt.IsZero() && !slices.Contains(owner.Scopes, k.Scope):
			// A live key's owner must be a member; a revoked key keeps its
			// record after its owner leaves.
			return bad("key %s's owner %s is not a member of its scope %s", k.Display, k.Owner, k.Scope)
		}
	}
	for _, r := range next.Roles {
		switch {
		case r.Role != "manager":
			return bad("role %q is not one golem binds (manager)", r.Role)
		case next.Principals[r.Principal].Kind == "":
			return bad("a role for %s, who does not exist", r.Principal)
		case !hasScope(next, r.Scope):
			return bad("a role on %s, which does not exist", r.Scope)
		}
	}
	if !outboxFollows(old, next) {
		return bad("the audit outbox may lose only its oldest events (shipped and archived) and gain new ones")
	}
	if len(next.Revocations) < len(old.Revocations) || !slices.Equal(next.Revocations[:len(old.Revocations)], old.Revocations) {
		return bad("revocations are append-only")
	}
	// A revocation is never undone: every key revoked is in the
	// revocations, which only grow, and a key in them stays revoked.
	for _, r := range next.Revocations {
		if k, ok := next.Keys[r.Key]; ok && k.RevokedAt.IsZero() {
			return bad("key %s was revoked; a revocation is never undone", k.Display)
		}
	}
	return nil
}

// Revoke revokes a key, recording the revocation: for use in Update.
func (d *Document) Revoke(hash string, at time.Time) error {
	k, ok := d.Keys[hash]
	if !ok {
		return fmt.Errorf("state: no key %s", hash)
	}
	if !k.RevokedAt.IsZero() {
		return nil
	}
	k.RevokedAt = at.UTC()
	d.Keys[hash] = k
	d.Revocations = append(d.Revocations, Revocation{Key: hash, At: at.UTC()})
	return nil
}

// Entitlements is the snapshot's view of the state: keys revoked are
// listed revoked (a gateway refuses them by name), principals with an
// Entra ID are users or services, and every principal is a key owner of
// its kind. aliases are the routes that exist: a scope's models outside
// them are dropped (it cannot call a route that is not there).
func (d *Document) Entitlements(aliases []string) snapshot.Entitlements {
	var e snapshot.Entitlements
	known := func(ms []string) []string {
		return slices.Sorted(func(yield func(string) bool) {
			for _, m := range ms {
				if slices.Contains(aliases, m) && !yield(m) {
					return
				}
			}
		})
	}
	for _, id := range slices.Sorted(maps.Keys(d.Scopes)) {
		s := d.Scopes[id]
		e.Scopes = append(e.Scopes, snapshot.Scope{ID: id, Models: known(s.Models), BudgetMicros: s.BudgetMicros,
			DeclaredWorstCaseCheck: s.DeclaredWorstCaseCheck,
			PrincipalInFlight:      cmp.Or(s.PrincipalInFlight, DefaultPrincipalInFlight),
			ScopeInFlight:          cmp.Or(s.ScopeInFlight, DefaultScopeInFlight),
			StorageAllowed:         s.StorageAllowed, MaxOutputTokens: s.MaxOutputTokens, MaxInputBytes: s.MaxInputBytes,
			Features: s.Features})
	}
	for _, id := range slices.Sorted(maps.Keys(d.Principals)) {
		p := d.Principals[id]
		scopes := slices.Sorted(slices.Values(p.Scopes))
		e.KeyOwners = append(e.KeyOwners, snapshot.Owner{ID: id, Kind: p.Kind, Scopes: scopes})
		if p.Entra == "" {
			continue
		}
		m := snapshot.Member{ID: id, Entra: p.Entra, Scopes: scopes, Default: cmp.Or(p.Default, firstOr(scopes))}
		if p.Kind == "service" {
			e.Services = append(e.Services, m)
		} else {
			e.Users = append(e.Users, m)
		}
	}
	byEntra := func(a, b snapshot.Member) int { return cmp.Compare(a.Entra, b.Entra) }
	slices.SortFunc(e.Users, byEntra)
	slices.SortFunc(e.Services, byEntra)
	for _, h := range slices.Sorted(maps.Keys(d.Keys)) {
		k := d.Keys[h]
		e.Keys = append(e.Keys, snapshot.Key{Hash: h, Display: k.Display, Principal: k.Owner,
			PrincipalKind: d.Principals[k.Owner].Kind, Scope: k.Scope, Models: known(k.Models),
			ExpiresAt: k.ExpiresAt, Revoked: !k.RevokedAt.IsZero()})
	}
	return e
}

func firstOr(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

func hasScope(d *Document, id string) bool { _, ok := d.Scopes[id]; return ok }

// outboxFollows reports whether next's outbox is old's with some of its
// oldest events trimmed (shipped and archived) and new ones appended: the
// events kept are unchanged and a contiguous run ending at old's newest,
// and the new ones are numbered on from old's last, without a gap.
func outboxFollows(old, next *Document) bool {
	var oldAt = map[uint64]AuditEvent{}
	for _, e := range old.Audit {
		oldAt[e.Seq] = e
	}
	want := uint64(0) // the next seq expected; 0 until the first kept or new event
	for _, e := range next.Audit {
		switch {
		case e.Seq <= old.AuditSeq:
			o, ok := oldAt[e.Seq]
			if !ok || !reflect.DeepEqual(o, e) || (want != 0 && e.Seq != want) {
				return false
			}
		case want == 0 && e.Seq != old.AuditSeq+1, want != 0 && e.Seq != want:
			return false
		}
		want = e.Seq + 1
	}
	// The kept run must reach old's newest event: nothing kept may be
	// followed by a gap where old events were dropped.
	if len(next.Audit) > 0 && next.Audit[0].Seq <= old.AuditSeq {
		last := next.Audit[0].Seq
		for _, e := range next.Audit {
			if e.Seq > old.AuditSeq {
				break
			}
			last = e.Seq
		}
		if len(old.Audit) > 0 && last != old.Audit[len(old.Audit)-1].Seq {
			return false
		}
	}
	return next.AuditSeq >= old.AuditSeq
}
