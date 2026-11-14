// Package service holds golem-control's rules (control-plane.md §2): each
// method takes an authenticated caller, checks authorization against
// org-model.md §2, validates the change, and applies it with its audit
// events in one state update (control/state), which checks the store's
// invariants and retries the whole change when another write won. The
// transport (control/api) authenticates and calls these; nothing else
// writes the document.
//
// This slice: scopes, memberships, the manager role, and keys.
package service

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/chucklehead-dev/golem/control/state"
	"github.com/chucklehead-dev/golem/lib/key"
)

// Caller is who calls, as the transport authenticated them.
type Caller struct {
	Principal string // golem's principal ID
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

// apply runs change, with an audit event, as one update (or a validation).
func (s *Service) apply(ctx context.Context, o Options, c Caller, action, target string, detail map[string]string, change func(*state.Document) error) error {
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
	return c.Admin || d.Has(c.Principal, "manager", scope)
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
	return s.apply(ctx, o, c, "scope.create", id, nil, func(d *state.Document) error {
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

// AddMember makes m a member of scope, creating the principal if it is
// new: the scope's managers, or admins. A principal that exists keeps its
// kind and Entra ID; a different one given is refused.
func (s *Service) AddMember(ctx context.Context, c Caller, o Options, scope string, m Member) error {
	if m.ID == "" {
		return invalid("member.id", "required")
	}
	return s.apply(ctx, o, c, "membership.add", m.ID, map[string]string{"scope": scope}, func(d *state.Document) error {
		if _, ok := d.Scopes[scope]; !ok {
			return fmt.Errorf("%w: scope %s", ErrNotFound, scope)
		}
		if !manages(d, c, scope) {
			return denied("only scope %s's managers add its members", scope)
		}
		p, exists := d.Principals[m.ID]
		switch {
		case !exists:
			p = state.Principal{Kind: m.Kind, Entra: m.Entra}
		case m.Kind != "" && m.Kind != p.Kind, m.Entra != "" && m.Entra != p.Entra:
			return invalid("member", "principal %s exists as a %s with another identity", m.ID, p.Kind)
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
func (s *Service) RemoveMember(ctx context.Context, c Caller, o Options, scope, principal string) error {
	return s.apply(ctx, o, c, "membership.remove", principal, map[string]string{"scope": scope}, func(d *state.Document) error {
		if !manages(d, c, scope) {
			return denied("only scope %s's managers remove its members", scope)
		}
		p, ok := d.Principals[principal]
		if !ok || !slices.Contains(p.Scopes, scope) {
			return fmt.Errorf("%w: %s is not a member of %s", ErrNotFound, principal, scope)
		}
		for h, k := range d.Keys {
			if k.Owner == principal && k.Scope == scope {
				if err := d.Revoke(h, s.now()); err != nil {
					return err
				}
			}
		}
		p.Scopes = slices.DeleteFunc(slices.Clone(p.Scopes), func(x string) bool { return x == scope })
		if p.Default == scope {
			p.Default = ""
		}
		d.Principals[principal] = p
		d.Roles = slices.DeleteFunc(slices.Clone(d.Roles), func(r state.RoleBinding) bool { return r.Principal == principal && r.Scope == scope })
		return nil
	})
}

// BindManager makes principal a manager of scope: admins only.
func (s *Service) BindManager(ctx context.Context, c Caller, o Options, scope, principal string) error {
	if !c.Admin {
		return denied("only admins assign managers")
	}
	return s.apply(ctx, o, c, "role.bind", principal, map[string]string{"role": "manager", "scope": scope}, func(d *state.Document) error {
		if d.Has(principal, "manager", scope) {
			return nil
		}
		d.Roles = append(d.Roles, state.RoleBinding{Principal: principal, Role: "manager", Scope: scope})
		return nil
	})
}

// KeyRequest is a key to mint.
type KeyRequest struct {
	Owner   string // the principal the key acts as
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
	plain := key.New(key.Live)
	h := key.Hash(plain)
	m := Minted{Key: plain, Hash: hex.EncodeToString(h[:]), Display: key.Display(plain)}
	err := s.apply(ctx, o, c, "key.mint", m.Display, map[string]string{"owner": r.Owner, "scope": r.Scope}, func(d *state.Document) error {
		sc, ok := d.Scopes[r.Scope]
		if !ok {
			return fmt.Errorf("%w: scope %s", ErrNotFound, r.Scope)
		}
		owner, ok := d.Principals[r.Owner]
		switch {
		case !ok:
			return fmt.Errorf("%w: principal %s", ErrNotFound, r.Owner)
		case !slices.Contains(owner.Scopes, r.Scope):
			return invalid("key.owner", "%s is not a member of %s: add them first", r.Owner, r.Scope)
		case c.Admin:
		case r.Owner == c.Principal:
		case owner.Kind == "service" && d.Has(c.Principal, "manager", r.Scope):
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
	return s.apply(ctx, o, c, "key.revoke", hash, nil, func(d *state.Document) error {
		k, ok := d.Keys[hash]
		if !ok {
			return fmt.Errorf("%w: key %s", ErrNotFound, hash)
		}
		if !c.Admin && k.Owner != c.Principal && !d.Has(c.Principal, "manager", k.Scope) {
			return denied("a key is revoked by its owner, its scope's managers or admins")
		}
		return d.Revoke(hash, s.now())
	})
}
