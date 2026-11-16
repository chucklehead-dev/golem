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

// MaxPersonalKeyLife is the longest a personal key lives (org-model.md §3:
// policy, ninety days).
const MaxPersonalKeyLife = 90 * 24 * time.Hour

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
	// ScopeModels, if set, is the route aliases scope's policy allows (its
	// ScopePolicy's, in the config: golem-control's
	// controller.ScopeModels), for refusing a key that names others: a key
	// can only narrow its scope. ok false: the config cannot tell (none
	// read yet), and nothing is checked; the gateway still intersects a
	// key's models with its scope's. nil: not checked.
	ScopeModels func(scope string) (models []string, ok bool)
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

// read is the document as of now (one conditional get).
func (s *Service) read(ctx context.Context) (*state.Document, error) {
	if err := s.State.Refresh(ctx); err != nil {
		return nil, err
	}
	return s.State.Current(), nil
}

// manages reports whether c may manage scope: an admin, or its manager.
func manages(d *state.Document, c Caller, scope string) bool {
	return c.Admin || d.Has(c.Principal, manager, scope)
}

// sees reports whether c may read scope: a member, a manager or an admin.
func sees(d *state.Document, c Caller, scope string) bool {
	return manages(d, c, scope) || d.Principals[c.Principal].MemberOf(scope)
}

// visibleScope is scope's record if it exists and c sees it. Otherwise it
// is not found, never denied: a scope's existence is not an outsider's to
// learn.
func visibleScope(d *state.Document, c Caller, scope string) (state.Scope, error) {
	sc, ok := d.Scopes[scope]
	if !ok || !sees(d, c, scope) {
		return state.Scope{}, notFound("scope %s", scope)
	}
	return sc, nil
}

// requireManager refuses c's change to scope unless c manages it: not
// found if the scope does not exist or c does not see it, else denied,
// since only its managers may do what ("add its members").
func requireManager(d *state.Document, c Caller, scope, what string) error {
	if _, err := visibleScope(d, c, scope); err != nil {
		return err
	}
	if !manages(d, c, scope) {
		return denied("only scope %s's managers %s", scope, what)
	}
	return nil
}

// CreateScope creates a scope: admins only (a top-level team; sub-teams
// and projects come with the scope tree). Its policy (models, budget,
// limits) is its ScopePolicy's, in the config (ADR 0050): until one is
// published the scope can call nothing.
func (s *Service) CreateScope(ctx context.Context, c Caller, o Options, id string) error {
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
		d.Scopes[id] = state.Scope{}
		return nil
	})
}

// Member is a principal to add to a scope: created on its first add.
type Member struct {
	ID    string
	Kind  string // user or service
	Entra string // the user's object ID or the service's app ID; empty: keys only
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
		if err := requireManager(d, c, scope, "add its members"); err != nil {
			return err
		}
		p, err := principalToAdd(d, c, m)
		if err != nil {
			return err
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

// principalToAdd is the principal m names, as AddMember will write it:
// the one that exists, if m does not contradict its kind or Entra ID, or
// a new one that c may create.
func principalToAdd(d *state.Document, c Caller, m Member) (state.Principal, error) {
	if p, exists := d.Principals[m.ID]; exists {
		if m.Kind != "" && m.Kind != p.Kind || m.Entra != "" && m.Entra != p.Entra {
			return state.Principal{}, invalid("member", "principal %s exists as a %s with another identity", m.ID, p.Kind)
		}
		return p, nil
	}
	switch {
	case m.Kind != "user" && m.Kind != "service":
		return state.Principal{}, invalid("member.kind", "a new principal is a user or a service, not %q", m.Kind)
	case c.Admin:
		return state.Principal{Kind: m.Kind, Entra: m.Entra}, nil
	case m.Entra != "":
		return state.Principal{}, denied("an Entra ID is bound by the group sync or an admin, never by a manager: bind a group to the scope (golemctl groups bind), or ask an admin")
	case m.Kind != "service":
		return state.Principal{}, invalid("member", "no principal %s: users come from the Entra groups bound to a scope (ADR 0047); a manager can create only a service that uses keys", m.ID)
	}
	return state.Principal{Kind: "service"}, nil // keys only: no Entra ID
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
		if err := requireManager(d, c, scope, "remove its members"); err != nil {
			return err
		}
		p, ok := d.Principals[principal]
		switch {
		case !ok || !p.MemberOf(scope):
			return notFound("%s is not a member of %s", principal, scope)
		case !slices.Contains(p.Scopes, scope):
			return invalid("member", "%s is a member of %s through an Entra group bound to it: remove them from the group in Entra, or unbind the group", principal, scope)
		case !c.Admin && d.Has(principal, manager, scope):
			// Managers are bound by admins; a manager cannot unseat another.
			return denied("%s manages %s: only admins remove a scope's managers", principal, scope)
		}
		p.Scopes = slices.DeleteFunc(slices.Clone(p.Scopes), func(x string) bool { return x == scope })
		if p.MemberOf(scope) {
			// Still a member through a bound group: its keys and role stay.
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
		sc, err := visibleScope(d, c, scope) // an admin sees every scope
		if err != nil {
			return err
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

// BindManager makes principal a manager of scope: admins only. The scope
// and the principal must exist (not found otherwise). The principal need
// not be a member: managing is its own right (org-model.md §2: a parent
// team's managers manage its sub-teams), and a manager reads the scope
// without being one.
func (s *Service) BindManager(ctx context.Context, c Caller, o Options, scope, principal string) error {
	if !c.Admin {
		return denied("only admins assign managers")
	}
	if principal == "" {
		return invalid("principal", "required: the principal to make a manager")
	}
	return s.apply(ctx, c, o, "role.bind", principal, map[string]string{"role": manager, "scope": scope}, func(d *state.Document) error {
		if _, err := visibleScope(d, c, scope); err != nil {
			return err
		}
		if _, ok := d.Principals[principal]; !ok {
			return notFound("principal %s: add them to a scope first (golemctl members add), or bind their Entra group and let the sync create them", principal)
		}
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
	Key, Hash string
	// Display is the key's shown prefix, which names its ledger rows.
	Display string
}

// MintKey mints a key for r.Owner in r.Scope. A member mints its own keys;
// a scope's managers mint its services' keys; admins mint any. The owner
// must be a member of the scope, and the models within the scope's. A
// personal key (a user's) must expire (org-model.md §3): within
// MaxPersonalKeyLife, which is also its expiry when none is asked for.
func (s *Service) MintKey(ctx context.Context, c Caller, o Options, r KeyRequest) (Minted, error) {
	if r.Owner == "" {
		r.Owner = c.Principal // the caller's own key
	}
	// A key's display ID names its ledger rows: a new key whose ID another key has (about one in 10^4 at 50,000 keys)
	// is minted again.
	for range 4 {
		m, err := s.mintKey(ctx, c, o, r)
		if !errors.Is(err, errDisplayTaken) {
			return m, err
		}
	}
	return Minted{}, errors.New("four keys minted in a row had display IDs other keys have: report this")
}

// errDisplayTaken is mintKey's, for MintKey to mint again; never returned.
var errDisplayTaken = errors.New("service: the new key's display ID is taken")

// mintKey is one try of MintKey's, with a new key.
func (s *Service) mintKey(ctx context.Context, c Caller, o Options, r KeyRequest) (Minted, error) {
	plain := key.New(key.Live)
	h := key.Hash(plain)
	m := Minted{Key: plain, Hash: hex.EncodeToString(h[:]), Display: key.Display(plain)}
	err := s.apply(ctx, c, o, "key.mint", m.Display, map[string]string{"owner": r.Owner, "scope": r.Scope}, func(d *state.Document) error {
		for _, k := range d.Keys {
			if k.Display == m.Display {
				return errDisplayTaken
			}
		}
		if _, err := visibleScope(d, c, r.Scope); err != nil {
			return err
		}
		owner, ok := d.Principals[r.Owner]
		if !ok {
			return notFound("principal %s", r.Owner)
		}
		if !owner.MemberOf(r.Scope) {
			return invalid("key.owner", "%s is not a member of %s: add them first", r.Owner, r.Scope)
		}
		if !mayMint(d, c, r, owner) {
			return denied("a member mints its own keys; a scope's managers mint its services' keys")
		}
		if err := s.narrows(r); err != nil {
			return err
		}
		expires, err := keyExpiry(owner, r.Expires, s.now())
		if err != nil {
			return err
		}
		if d.Keys == nil {
			d.Keys = map[string]state.Key{}
		}
		d.Keys[m.Hash] = state.Key{Display: m.Display, Owner: r.Owner, Scope: r.Scope,
			Models: slices.Clone(r.Models), ExpiresAt: expires.UTC()}
		return nil
	})
	if err != nil || o.ValidateOnly {
		return Minted{}, err
	}
	return m, nil
}

// narrows refuses r if it names a model its scope's policy does not
// allow, when the config can tell.
func (s *Service) narrows(r KeyRequest) error {
	if s.ScopeModels == nil || len(r.Models) == 0 {
		return nil
	}
	allowed, ok := s.ScopeModels(r.Scope)
	if !ok {
		return nil
	}
	for i, model := range r.Models {
		if !slices.Contains(allowed, model) {
			return invalid(fmt.Sprintf("key.models[%d]", i), "scope %s does not allow %q: a key can only narrow its scope's models, its ScopePolicy's spec.models in the config", r.Scope, model)
		}
	}
	return nil
}

// mayMint reports whether c may mint r for owner: admins any key, a
// member its own, and a scope's managers its services'.
func mayMint(d *state.Document, c Caller, r KeyRequest, owner state.Principal) bool {
	return c.Admin || r.Owner == c.Principal || owner.Kind == "service" && manages(d, c, r.Scope)
}

// keyExpiry is when a key for owner expires, asked to at asked (zero: not
// asked): never in the past, and a personal key (a user's) within
// MaxPersonalKeyLife of now, at that limit when none is asked for. A
// service's key lives as long as asked, or until revoked.
func keyExpiry(owner state.Principal, asked, now time.Time) (time.Time, error) {
	if !asked.IsZero() && !asked.After(now) {
		return time.Time{}, invalid("key.expires", "already past")
	}
	if owner.Kind != "user" {
		return asked, nil
	}
	limit := now.Add(MaxPersonalKeyLife)
	switch {
	case asked.IsZero():
		return limit, nil
	case asked.After(limit):
		return time.Time{}, invalid("key.expires", "a personal key lives at most %d days (org-model.md §3); a service's key has no limit", int(MaxPersonalKeyLife.Hours()/24))
	}
	return asked, nil
}

// RevokeKey revokes a key by its hash: its owner, admins, and, for a
// service's key, its scope's managers (org-model.md §2: managers mint,
// rotate and revoke service keys; a member's own keys end with their
// membership, which a manager can remove). Revoking a revoked key is no
// change.
func (s *Service) RevokeKey(ctx context.Context, c Caller, o Options, hash string) error {
	return s.apply(ctx, c, o, "key.revoke", hash, nil, func(d *state.Document) error {
		if _, err := keyToChange(d, c, hash, "a key is revoked by its owner or an admin, and a service's key by its scope's managers; to end a member's keys, remove the member"); err != nil {
			return err
		}
		return d.Revoke(hash, s.now())
	})
}

// keyToChange is the key hash names, if c may change it: its owner, an
// admin, or, for a service's key, its scope's managers. Anyone else who
// sees the scope is refused with why; anyone who
// does not gets not found, since a key's existence is not an outsider's
// to learn.
func keyToChange(d *state.Document, c Caller, hash string, why string) (state.Key, error) {
	k, ok := d.Keys[hash]
	switch {
	case !ok:
		return state.Key{}, notFound("key %s", hash)
	case k.Owner == c.Principal, c.Admin:
	case d.Principals[k.Owner].Kind == "service" && manages(d, c, k.Scope):
	case !sees(d, c, k.Scope):
		return state.Key{}, notFound("key %s", hash)
	default:
		return state.Key{}, denied("%s", why)
	}
	return k, nil
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

// ScopeView is a scope as its members see it: its policy is its
// ScopePolicy's, read in the config.
type ScopeView struct {
	ID       string
	Groups   []string // the Entra groups bound to it
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

// GetScope is a scope's bound groups, members and managers: for its
// members, its managers and admins.
func (s *Service) GetScope(ctx context.Context, c Caller, id string) (ScopeView, error) {
	d, err := s.read(ctx)
	if err != nil {
		return ScopeView{}, err
	}
	sc, err := visibleScope(d, c, id)
	if err != nil {
		return ScopeView{}, err
	}
	v := ScopeView{ID: id, Groups: sc.Groups}
	for _, pid := range slices.Sorted(maps.Keys(d.Principals)) {
		if p := d.Principals[pid]; p.MemberOf(id) {
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
