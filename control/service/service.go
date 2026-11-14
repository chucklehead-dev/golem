// Package service holds golem-control's rules (control-plane.md §2): each
// method takes an authenticated caller, checks authorization against
// org-model.md §2, validates the change, and applies it with its audit
// events in one state update (control/state), which checks the store's
// invariants and retries the whole change when another write won. The
// transport (control/api) authenticates and calls these; nothing else
// writes the document.
//
// This slice: scopes, memberships, the manager role, keys, and the Entra
// groups bound to scopes, whose members the group sync (control/entrasync)
// makes members (ADR 0047).
package service

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/key"
)

// Caller is who calls, as the transport authenticated them.
type Caller struct {
	// Principal is golem's principal ID; a caller with no principal yet
	// has one that matches none (control/api's "entra:<id>"), so it holds
	// no membership or role and is recorded by its Entra ID in the audit.
	Principal string
	// Admin: the Entra app role golem.admin (org-model.md §2).
	Admin bool
}

// Errors, which the transport maps to its codes.
var (
	ErrPermission = errors.New("permission denied")
	ErrNotFound   = errors.New("not found")
	ErrInvalid    = errors.New("invalid")
)

// Invalid is a field that fails validation: its path, and what to do.
type Invalid struct{ Field, Msg string }

func (e *Invalid) Error() string { return e.Field + ": " + e.Msg }
func (e *Invalid) Unwrap() error { return ErrInvalid }

func invalid(field, f string, a ...any) error { return &Invalid{field, fmt.Sprintf(f, a...)} }

func denied(f string, a ...any) error {
	return fmt.Errorf("%w: "+f, append([]any{ErrPermission}, a...)...)
}

func notFound(f string, a ...any) error {
	return fmt.Errorf("%w: "+f, append([]any{ErrNotFound}, a...)...)
}

// manager is the one role golem binds so far (org-model.md §2); admins
// hold the Entra app role instead of a binding.
const manager = "manager"

// Options are every mutating method's.
type Options struct {
	// ValidateOnly checks the change, its authorization and the store's
	// invariants, and writes nothing.
	ValidateOnly bool
}

// Service is the org and key services over one state store.
type Service struct {
	State *state.Store
	Now   func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// apply runs change and appends its audit event, by c, as one update; with
// o.ValidateOnly, it checks them against the store's invariants and writes
// nothing. change may run more than once (the store retries it when
// another write won), so it must work from the document it is given.
func (s *Service) apply(ctx context.Context, c Caller, o Options, action, target string, detail map[string]string, change func(*state.Document) error) error {
	at := s.now()
	f := func(d *state.Document) error {
		if err := change(d); err != nil {
			return err
		}
		d.Audit = append(d.Audit, state.AuditEvent{At: at, Actor: c.Principal, Action: action, Target: target, Detail: detail})
		return nil
	}
	if o.ValidateOnly {
		return s.State.Validate(ctx, f)
	}
	_, err := s.State.Update(ctx, f)
	return err
}

// manages reports whether c may manage scope: an admin, or its manager.
func manages(d *state.Document, c Caller, scope string) bool {
	return c.Admin || d.Has(c.Principal, manager, scope)
}

// CreateScope creates a scope with its policy: admins only (a top-level
// team; sub-teams and projects come with the scope tree).
func (s *Service) CreateScope(ctx context.Context, c Caller, o Options, id string, policy state.Scope) error {
	if !c.Admin {
		return denied("only admins create top-level scopes")
	}
	if id == "" {
		return invalid("scope.id", "required")
	}
	return s.apply(ctx, c, o, "scope.create", id, nil, func(d *state.Document) error {
		if _, exists := d.Scopes[id]; exists {
			return invalid("scope.id", "scope %q exists", id)
		}
		if d.Scopes == nil {
			d.Scopes = map[string]state.Scope{}
		}
		d.Scopes[id] = policy
		return nil
	})
}

// Member is a principal to add to a scope: created on its first add.
type Member struct {
	ID    string
	Kind  string // user or service
	Entra string // the user's object ID or the service's app ID; empty: keys only
}

// unknownScope refuses c's change to scope: not found if c may not see
// it (its existence is not theirs to learn), else denied for why.
func unknownScope(d *state.Document, c Caller, scope, why string) error {
	if _, ok := d.Scopes[scope]; !ok || !sees(d, c, scope) {
		return notFound("scope %s", scope)
	}
	return denied("%s", why)
}

// AddMember makes m a member of scope: the scope's managers, or admins.
// Who a principal is in Entra is never a manager's to say (ADR 0047): users
// come from the groups bound to scopes, and an Entra ID is bound only by
// the sync or an admin. So a manager adds a principal that exists, or
// creates a service that authenticates by key only; an admin may also
// create a user or service with its Entra ID. A principal that exists
// keeps its kind and Entra ID; a different one given is refused.
func (s *Service) AddMember(ctx context.Context, c Caller, o Options, scope string, m Member) error {
	if m.ID == "" {
		return invalid("member.id", "required")
	}
	if strings.HasPrefix(m.ID, state.ReservedPrefix) {
		return invalid("member.id", "IDs beginning %q name callers with no principal; choose another", state.ReservedPrefix)
	}
	return s.apply(ctx, c, o, "membership.add", m.ID, map[string]string{"scope": scope}, func(d *state.Document) error {
		if _, ok := d.Scopes[scope]; !ok || !manages(d, c, scope) {
			return unknownScope(d, c, scope, "only scope "+scope+"'s managers add its members")
		}
		p, exists := d.Principals[m.ID]
		switch {
		case exists && (m.Kind != "" && m.Kind != p.Kind || m.Entra != "" && m.Entra != p.Entra):
			return invalid("member", "principal %s exists as a %s with another identity", m.ID, p.Kind)
		case exists:
		case c.Admin:
			p = state.Principal{Kind: m.Kind, Entra: m.Entra}
		case m.Entra != "":
			return denied("an Entra ID is bound by the group sync or an admin, never by a manager: bind a group to the scope (golemctl groups bind), or ask an admin")
		case m.Kind != "service":
			return invalid("member", "no principal %s: users come from the Entra groups bound to a scope (ADR 0047); a manager can create only a service that uses keys", m.ID)
		default:
			p = state.Principal{Kind: "service"}
		}
		if slices.Contains(p.Scopes, scope) {
			return nil
		}
		p.Scopes = append(slices.Clone(p.Scopes), scope)
		if d.Principals == nil {
			d.Principals = map[string]state.Principal{}
		}
		d.Principals[m.ID] = p
		return nil
	})
}

// RemoveMember ends principal's membership of scope, and revokes its keys
// in the scope (a key is honoured only for a member): the scope's
// managers, or admins.
//
// A membership from a bound group is the group's: it is removed in Entra,
// or by unbinding the group, not here. A principal that is a member both
// ways keeps the synced membership, and its keys.
func (s *Service) RemoveMember(ctx context.Context, c Caller, o Options, scope, principal string) error {
	return s.apply(ctx, c, o, "membership.remove", principal, map[string]string{"scope": scope}, func(d *state.Document) error {
		if _, ok := d.Scopes[scope]; !ok || !manages(d, c, scope) {
			return unknownScope(d, c, scope, "only scope "+scope+"'s managers remove its members")
		}
		p, ok := d.Principals[principal]
		switch {
		case !ok || !p.MemberOf(scope):
			return notFound("%s is not a member of %s", principal, scope)
		case !slices.Contains(p.Scopes, scope):
			return invalid("member", "%s is a member of %s through an Entra group bound to it: remove them from the group in Entra, or unbind the group", principal, scope)
		}
		p.Scopes = slices.DeleteFunc(slices.Clone(p.Scopes), func(x string) bool { return x == scope })
		if p.MemberOf(scope) {
			d.Principals[principal] = p
			return nil
		}
		d.RevokeOutside(principal, p, s.now())
		if p.Default == scope {
			p.Default = ""
		}
		d.Principals[principal] = p
		d.Roles = slices.DeleteFunc(slices.Clone(d.Roles), func(r state.RoleBinding) bool { return r.Principal == principal && r.Scope == scope })
		return nil
	})
}

// BindGroup binds an Entra group to scope: its members, transitively,
// become the scope's members at the next sync. Admins only: a group's
// members are whoever Entra says, and binding one grants all of them.
func (s *Service) BindGroup(ctx context.Context, c Caller, o Options, scope, group string) error {
	if !c.Admin {
		return denied("only admins bind Entra groups to scopes")
	}
	if group == "" {
		return invalid("group", "required: the group's object ID")
	}
	return s.apply(ctx, c, o, "group.bind", group, map[string]string{"scope": scope}, func(d *state.Document) error {
		sc, ok := d.Scopes[scope]
		if !ok {
			return notFound("scope %s", scope)
		}
		if !slices.Contains(sc.Groups, group) {
			sc.Groups = slices.Sorted(slices.Values(append(slices.Clone(sc.Groups), group)))
			d.Scopes[scope] = sc
		}
		return nil
	})
}

// UnbindGroup unbinds an Entra group from scope: admins only. The
// memberships it gave end at the next sync, which recomputes them from the
// groups still bound.
func (s *Service) UnbindGroup(ctx context.Context, c Caller, o Options, scope, group string) error {
	if !c.Admin {
		return denied("only admins unbind Entra groups")
	}
	return s.apply(ctx, c, o, "group.unbind", group, map[string]string{"scope": scope}, func(d *state.Document) error {
		sc, ok := d.Scopes[scope]
		if !ok || !slices.Contains(sc.Groups, group) {
			return notFound("group %s is not bound to scope %s", group, scope)
		}
		sc.Groups = slices.DeleteFunc(slices.Clone(sc.Groups), func(g string) bool { return g == group })
		d.Scopes[scope] = sc
		return nil
	})
}

// BindManager makes principal a manager of scope: admins only.
func (s *Service) BindManager(ctx context.Context, c Caller, o Options, scope, principal string) error {
	if !c.Admin {
		return denied("only admins assign managers")
	}
	return s.apply(ctx, c, o, "role.bind", principal, map[string]string{"role": manager, "scope": scope}, func(d *state.Document) error {
		if d.Has(principal, manager, scope) {
			return nil
		}
		d.Roles = append(d.Roles, state.RoleBinding{Principal: principal, Role: manager, Scope: scope})
		return nil
	})
}

// KeyRequest is a key to mint.
type KeyRequest struct {
	Owner   string // the principal the key acts as; empty: the caller
	Scope   string
	Models  []string // narrows the scope's; empty: all of them
	Expires time.Time
}

// Minted is a new key: its plaintext, shown once and never stored, and
// its hash, which names it from then on.
type Minted struct {
	Key, Hash, Display string
}

// MintKey mints a key for r.Owner in r.Scope. A member mints its own keys;
// a scope's managers mint its services' keys; admins mint any. The owner
// must be a member of the scope, and the models within the scope's.
func (s *Service) MintKey(ctx context.Context, c Caller, o Options, r KeyRequest) (Minted, error) {
	if r.Owner == "" {
		r.Owner = c.Principal // the caller's own key
	}
	plain := key.New(key.Live)
	h := key.Hash(plain)
	m := Minted{Key: plain, Hash: hex.EncodeToString(h[:]), Display: key.Display(plain)}
	err := s.apply(ctx, c, o, "key.mint", m.Display, map[string]string{"owner": r.Owner, "scope": r.Scope}, func(d *state.Document) error {
		sc, ok := d.Scopes[r.Scope]
		if !ok || !sees(d, c, r.Scope) {
			return notFound("scope %s", r.Scope)
		}
		owner, ok := d.Principals[r.Owner]
		switch {
		case !ok:
			return notFound("principal %s", r.Owner)
		case !owner.MemberOf(r.Scope):
			return invalid("key.owner", "%s is not a member of %s: add them first", r.Owner, r.Scope)
		case c.Admin:
		case r.Owner == c.Principal:
		case owner.Kind == "service" && manages(d, c, r.Scope):
		default:
			return denied("a member mints its own keys; a scope's managers mint its services' keys")
		}
		for i, model := range r.Models {
			if !slices.Contains(sc.Models, model) {
				return invalid(fmt.Sprintf("key.models[%d]", i), "scope %s does not allow %q: a key can only narrow its scope", r.Scope, model)
			}
		}
		if !r.Expires.IsZero() && !r.Expires.After(s.now()) {
			return invalid("key.expires", "already past")
		}
		if d.Keys == nil {
			d.Keys = map[string]state.Key{}
		}
		d.Keys[m.Hash] = state.Key{Display: m.Display, Owner: r.Owner, Scope: r.Scope,
			Models: slices.Clone(r.Models), ExpiresAt: r.Expires.UTC()}
		return nil
	})
	if err != nil || o.ValidateOnly {
		return Minted{}, err
	}
	return m, nil
}

// RevokeKey revokes a key by its hash: its owner, its scope's managers, or
// admins. Revoking a revoked key is no change.
func (s *Service) RevokeKey(ctx context.Context, c Caller, o Options, hash string) error {
	return s.apply(ctx, c, o, "key.revoke", hash, nil, func(d *state.Document) error {
		k, ok := d.Keys[hash]
		if !ok {
			return notFound("key %s", hash)
		}
		if k.Owner != c.Principal && !manages(d, c, k.Scope) {
			return denied("a key is revoked by its owner, its scope's managers or admins")
		}
		return d.Revoke(hash, s.now())
	})
}

// Me is who the caller is to golem.
type Me struct {
	Principal string
	Admin     bool
	Kind      string   // user or service; empty for a caller with no principal yet
	Scopes    []string // memberships
	Manages   []string // scopes it manages
}

// Whoami is the caller as golem sees them: anyone signed in.
func (s *Service) Whoami(ctx context.Context, c Caller) (Me, error) {
	d, err := s.read(ctx)
	if err != nil {
		return Me{}, err
	}
	p := d.Principals[c.Principal]
	me := Me{Principal: c.Principal, Admin: c.Admin, Kind: p.Kind, Scopes: p.Members()}
	for _, r := range d.Roles {
		if r.Principal == c.Principal {
			me.Manages = append(me.Manages, r.Scope)
		}
	}
	slices.Sort(me.Manages)
	return me, nil
}

// read is the document as of now (one conditional get).
func (s *Service) read(ctx context.Context) (*state.Document, error) {
	if err := s.State.Refresh(ctx); err != nil {
		return nil, err
	}
	return s.State.Current(), nil
}

// sees reports whether c may read scope: a member, a manager or an admin.
func sees(d *state.Document, c Caller, scope string) bool {
	return manages(d, c, scope) || d.Principals[c.Principal].MemberOf(scope)
}

// ScopeView is a scope as its members see it.
type ScopeView struct {
	ID       string
	Policy   state.Scope
	Members  []string
	Managers []string
}

// ListScopes is the scopes the caller may read, by ID.
func (s *Service) ListScopes(ctx context.Context, c Caller) ([]string, error) {
	d, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, id := range slices.Sorted(maps.Keys(d.Scopes)) {
		if sees(d, c, id) {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// GetScope is a scope's policy, members and managers: for its members,
// its managers and admins.
func (s *Service) GetScope(ctx context.Context, c Caller, id string) (ScopeView, error) {
	d, err := s.read(ctx)
	if err != nil {
		return ScopeView{}, err
	}
	sc, ok := d.Scopes[id]
	// Not found, not denied, for an outsider: a scope's existence is not
	// theirs to learn.
	if !ok || !sees(d, c, id) {
		return ScopeView{}, notFound("scope %s", id)
	}
	v := ScopeView{ID: id, Policy: sc}
	for _, pid := range slices.Sorted(maps.Keys(d.Principals)) {
		if d.Principals[pid].MemberOf(id) {
			v.Members = append(v.Members, pid)
		}
	}
	for _, r := range d.Roles {
		if r.Scope == id {
			v.Managers = append(v.Managers, r.Principal)
		}
	}
	slices.Sort(v.Managers)
	return v, nil
}

// KeyView is a key as its owner and managers see it: never the key.
type KeyView struct {
	Hash string
	state.Key
}

// ListKeys is the keys the caller may see, in scope if it is given: its
// own, those of the scopes it manages, or, for admins, every key.
func (s *Service) ListKeys(ctx context.Context, c Caller, scope string) ([]KeyView, error) {
	d, err := s.read(ctx)
	if err != nil {
		return nil, err
	}
	var out []KeyView
	for _, h := range slices.Sorted(maps.Keys(d.Keys)) {
		k := d.Keys[h]
		if scope != "" && k.Scope != scope {
			continue
		}
		if k.Owner == c.Principal || manages(d, c, k.Scope) {
			out = append(out, KeyView{Hash: h, Key: k})
		}
	}
	return out, nil
}
