// Package entrasync is golem-control's Entra group sync (ADR 0047,
// control-plane.md §6): while this replica leads, it reads the members of
// every Entra group bound to a scope from the directory (Microsoft Graph),
// and makes the state's synced memberships match. It is the only writer
// of a principal's Synced memberships and, with admins, of its Entra ID:
// a user's identity in golem is what Entra says, never what a manager
// typed.
//
// A step reads every group first and writes nothing unless all were read:
// a directory outage leaves memberships as they were, never empties them.
// A step that would end more memberships of still-active users than
// MaxRemovals is refused, so a misconfigured group or a directory answering
// empty cannot strip a team at once (the accidental-deletion guard
// directory sync tools keep). Offboarding is not held back by it: the
// memberships and keys of users the directory affirmatively reports
// disabled or deleted always end, however many.
package entrasync

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/chucklehead-dev/golem/control/state"
)

// User is a group member, or an account, as the directory reports it.
type User struct {
	ID      string // object ID: the oid in the user's tokens
	UPN     string // userPrincipalName
	Enabled bool   // accountEnabled is true
	// Disabled: accountEnabled is false. Neither it nor Enabled when the
	// directory does not say: such an account is a member of nothing, but
	// its removal is not taken for offboarding (the removal guard counts
	// it), so a directory leaving the field out cannot strip everyone past
	// the guard.
	Disabled bool
	// Deleted: the account is in the directory's deleted items (Directory.User).
	Deleted bool
}

// offboarded reports whether the directory affirmatively says the account
// is gone: disabled or deleted.
func (u User) offboarded() bool { return u.Disabled || u.Deleted }

// Directory lists a group's users, transitively (nested groups' too), and
// looks up one account: a user who is in no bound group any more may have
// left the groups, or been disabled and removed, or deleted (Entra drops a
// deleted user from its groups), and only the account says which.
type Directory interface {
	Members(ctx context.Context, group string) ([]User, error)
	// User is the account with object ID id, Deleted if it is in the
	// deleted items; ErrNoUser if the directory has it in neither.
	User(ctx context.Context, id string) (User, error)
}

// ErrNoGroup is a group the directory does not have (deleted): it has no
// members, and the scopes it was bound to lose those it gave.
var ErrNoGroup = errors.New("entrasync: no such group")

// ErrNoUser is an account the directory has neither live nor in its
// deleted items (purged, or not this tenant's): its removal is not taken
// for offboarding.
var ErrNoUser = errors.New("entrasync: no such user")

// ErrTooManyRemovals is a step refused by its removal guard: only its
// offboarding (users disabled or deleted) was written.
var ErrTooManyRemovals = errors.New("entrasync: too many memberships of active users would end")

// Actor is the sync's name in the audit.
const Actor = "system:entra-sync"

// Sync reconciles the state with the directory.
type Sync struct {
	State     *state.Store
	Directory Directory
	// MaxRemovals bounds the memberships of still-active users one step
	// may end (default 50); a step over it writes only its offboarding (the
	// users the directory reports disabled or deleted) and is reported.
	// Raise it for one run when a large removal is intended.
	MaxRemovals int
	Now         func() time.Time
	Log         *slog.Logger
}

// Result is what a step changed: principals created, memberships added
// and removed, keys revoked, and users offboarded (disabled or deleted in
// the directory: every synced membership and key ended).
type Result struct {
	Created, Added, Removed, Revoked, Offboarded int
	changed                                      bool // anything at all, a sign-in name included
}

// errNoChange ends an update that would change nothing: the store writes
// nothing for it.
var errNoChange = errors.New("no change")

// Step reads every bound group and reconciles the state with it, as the
// leader at fence: the write is refused (state.ErrFenced) if this replica
// was deposed while it read the directory, so a paused old leader's stale
// read never undoes a newer leader's sync (model/leadership.md's
// noStaleSync: the group sync was unfenced before).
func (s *Sync) Step(ctx context.Context, fence state.Fence) (Result, error) {
	if err := s.State.Refresh(ctx); err != nil {
		return Result{}, err
	}
	groups := boundGroups(s.State.Current())
	members := map[string][]User{}
	for _, g := range groups {
		us, err := s.Directory.Members(ctx, g)
		switch {
		case errors.Is(err, ErrNoGroup):
			s.log().Warn("entra sync: a bound group does not exist; its memberships end", "group", g)
			us = nil
		case err != nil:
			return Result{}, fmt.Errorf("group %s: %w (nothing changed)", g, err)
		}
		members[g] = us
	}
	// The accounts of the users who are in no bound group any more: whether
	// they are offboarded (disabled, deleted) is the account's to say.
	accounts := map[string]User{}
	for _, oid := range left(s.State.Current(), members) {
		u, err := s.Directory.User(ctx, oid)
		switch {
		case errors.Is(err, ErrNoUser):
			continue // neither live nor deleted: a plain removal, guarded
		case err != nil:
			return Result{}, fmt.Errorf("user %s: %w (nothing changed)", oid, err)
		}
		accounts[oid] = u
	}
	limit := cmp.Or(s.MaxRemovals, 50)
	var res Result
	var tripped bool
	var guarded int
	write := func(restrictOnly bool) error {
		_, err := s.State.FencedUpdate(ctx, fence, func(d *state.Document) error {
			// Built afresh from the document this attempt is given: the
			// update may retry against a newer one.
			res, guarded, tripped = reconcile(d, members, accounts, s.now(), limit, restrictOnly)
			if !res.changed {
				return errNoChange
			}
			return nil
		})
		if errors.Is(err, errNoChange) {
			res = Result{}
			return nil
		}
		return err
	}
	err := write(false)
	// Past the audit outbox's reserve the store refuses a write that grants
	// (a user created, a membership added): the removals and offboarding
	// are written alone, into the capacity kept for them, and the additions
	// wait for the outbox to drain.
	var held error
	if full, ok := errors.AsType[*state.OutboxFull](err); ok && !full.Hard() {
		n := res.Created + res.Added
		if err = write(true); err == nil {
			held = fmt.Errorf("%w; only removals and offboarding were written (%d removed, %d keys revoked, %d offboarded), and %d creation(s) and addition(s) wait for the outbox to drain", full, res.Removed, res.Revoked, res.Offboarded, n)
		}
	}
	if err != nil {
		return Result{}, err
	}
	if tripped {
		return res, errors.Join(held, fmt.Errorf("%w: %d, over the limit of %d; only the offboarding of %d disabled or deleted user(s) was written. Check the bound groups (one re-bound or emptied, a directory answering empty), or raise -entra-sync-max-removals for one run if the removal is intended",
			ErrTooManyRemovals, guarded, limit, res.Offboarded))
	}
	return res, held
}

// left is the object ID of every user principal of d's with synced
// memberships who is in none of members' groups, sorted.
func left(d *state.Document, members map[string][]User) []string {
	in := map[string]bool{}
	for _, us := range members {
		for _, u := range us {
			in[u.ID] = true
		}
	}
	var oids []string
	for _, p := range d.Principals {
		if p.Kind == "user" && p.Entra != "" && len(p.Synced) > 0 && !in[p.Entra] {
			oids = append(oids, p.Entra)
		}
	}
	slices.Sort(oids)
	return oids
}

// boundGroups is every group bound to a scope, sorted.
func boundGroups(d *state.Document) []string {
	var gs []string
	for _, sc := range d.Scopes {
		gs = append(gs, sc.Groups...)
	}
	slices.Sort(gs)
	return slices.Compact(gs)
}

// want is one user's synced memberships, as the directory implies them.
type want struct {
	upn      string
	enabled  bool // every group reports the account enabled
	disabled bool // some group reports it disabled
	scopes   []string
}

// wanted is every group member's synced memberships, by object ID: a
// member of a group bound to a scope is to be a member of the scope, and
// is enabled only if every group reports it so.
func wanted(d *state.Document, members map[string][]User) map[string]*want {
	wants := map[string]*want{}
	for _, sid := range slices.Sorted(maps.Keys(d.Scopes)) {
		for _, g := range d.Scopes[sid].Groups {
			for _, u := range members[g] {
				w := wants[u.ID]
				if w == nil {
					w = &want{upn: u.UPN, enabled: true}
					wants[u.ID] = w
				}
				w.enabled = w.enabled && u.Enabled
				w.disabled = w.disabled || u.Disabled
				w.scopes = append(w.scopes, sid)
			}
		}
	}
	return wants
}

// reconcile makes d's synced memberships those members imply: a member of
// a group bound to a scope is a member of the scope while enabled. A user
// not yet a principal is created (if enabled); one not enabled, or in no
// bound group, loses its synced memberships; every changed principal's
// keys outside its memberships are revoked, and all the keys of a user not
// enabled or whose account (accounts: those in no bound group, looked up)
// is disabled or deleted. Each change is an audit event by Actor.
//
// The guard: guarded counts the memberships it would end of users not
// offboarded (offboarded: disabled or deleted, as the directory
// affirmatively reports). Over limit, only the offboarding is applied, and
// tripped is true.
//
// restrictOnly applies only what narrows access: no user is created and no
// membership added (the audit outbox past its reserve, state.OutboxFull).
func reconcile(d *state.Document, members map[string][]User, accounts map[string]User, at time.Time, limit int, restrictOnly bool) (res Result, guarded int, tripped bool) {
	wants := wanted(d, members)
	byEntra := map[string]string{} // a user's object ID to its principal
	for id, p := range d.Principals {
		if p.Kind == "user" && p.Entra != "" {
			byEntra[p.Entra] = id
		}
	}
	// Every user's change is planned before any is made, so the guard can
	// count them first.
	type change struct {
		oid, id   string // id empty: to be created
		upn       string // the sign-in name to record (a group member's)
		synced    []string
		revokeAll bool // not enabled, or offboarded: every key
		offboard  bool // never held back by the guard
	}
	var plan []change
	for _, oid := range slices.Sorted(maps.Keys(wants)) {
		w := wants[oid]
		id, exists := byEntra[oid]
		if !exists && !w.enabled {
			continue // never created for an account not enabled
		}
		c := change{oid: oid, id: id, upn: w.upn, revokeAll: !w.enabled, offboard: w.disabled}
		if w.enabled {
			c.synced = slices.Compact(slices.Sorted(slices.Values(w.scopes)))
		}
		if exists && !c.offboard {
			_, removed := diff(d.Principals[id].Synced, c.synced)
			guarded += len(removed)
		}
		plan = append(plan, c)
	}
	// A user in no bound group any more keeps none of its synced
	// memberships; one whose account is disabled or deleted, none of its
	// keys either.
	for _, oid := range slices.Sorted(maps.Keys(byEntra)) {
		if _, seen := wants[oid]; seen {
			continue
		}
		id := byEntra[oid]
		if p := d.Principals[id]; len(p.Synced) > 0 {
			off := accounts[oid].offboarded()
			if !off {
				guarded += len(p.Synced)
			}
			plan = append(plan, change{oid: oid, id: id, revokeAll: off, offboard: off})
		}
	}
	tripped = guarded > limit
	r := &reconciler{d: d, at: at}
	for _, c := range plan {
		if tripped && !c.offboard {
			continue
		}
		id := c.id
		switch {
		case id == "" && restrictOnly:
			continue
		case id == "":
			id = r.create(c.upn, c.oid)
		}
		p := d.Principals[id]
		if restrictOnly {
			// Of the memberships wanted, only those it has: removals alone.
			c.synced = slices.DeleteFunc(slices.Clone(c.synced), func(s string) bool { return !slices.Contains(p.Synced, s) })
		}
		if c.upn != "" && p.UPN != c.upn {
			p.UPN = c.upn
			d.Principals[id] = p
			r.res.changed = true
		}
		if r.setSynced(id, p, c.synced, c.revokeAll) && c.offboard {
			r.res.Offboarded++
		}
	}
	return r.res, guarded, tripped
}

// reconciler is one reconcile's changes to d, counted in res.
type reconciler struct {
	d   *state.Document
	at  time.Time
	res Result
}

func (r *reconciler) audit(action, target string, detail map[string]string) {
	r.d.Audit = append(r.d.Audit, state.AuditEvent{At: r.at, Actor: Actor, Action: action, Target: target, Detail: detail})
}

// create adds the user with object ID oid as a principal with no
// memberships yet, and returns its ID.
func (r *reconciler) create(upn, oid string) string {
	if r.d.Principals == nil {
		r.d.Principals = map[string]state.Principal{}
	}
	id := principalID(r.d, upn, oid)
	r.d.Principals[id] = state.Principal{Kind: "user", Entra: oid, UPN: upn, Scopes: []string{}}
	r.res.Created++
	r.res.changed = true
	r.audit("principal.create", id, map[string]string{"entra": oid, "upn": upn})
	return id
}

// setSynced gives principal id (its record p) the synced memberships
// synced, revoking its live keys outside its memberships (all of them if
// the account is not enabled, or offboarded) and dropping a default it is no
// longer a member of. Nothing is written or audited if nothing changes; it
// reports whether anything did.
func (r *reconciler) setSynced(id string, p state.Principal, synced []string, disabled bool) bool {
	d := r.d
	added, removed := diff(p.Synced, synced)
	p.Synced = synced
	before := live(d, id)
	keep := p // the memberships its keys are honoured in
	if disabled {
		keep = state.Principal{} // a member of nothing: every key revoked
	}
	d.RevokeOutside(id, keep, r.at)
	revoked := before - live(d, id)
	lostDefault := p.Default != "" && !p.MemberOf(p.Default)
	if len(added)+len(removed)+revoked == 0 && !lostDefault {
		return false
	}
	if lostDefault {
		p.Default = ""
	}
	d.Principals[id] = p
	r.res.Added += len(added)
	r.res.Removed += len(removed)
	r.res.Revoked += revoked
	r.res.changed = true
	// Additions and removals are events of their own: a removal is
	// restrictive, and lands when the audit outbox holds back grants
	// (state.OutboxFull).
	if len(added) > 0 {
		r.audit("membership.sync.add", id, map[string]string{"added": strings.Join(added, ",")})
	}
	if len(removed)+revoked > 0 || lostDefault {
		r.audit("membership.sync.remove", id, map[string]string{"removed": strings.Join(removed, ","),
			"keysRevoked": fmt.Sprint(revoked), "disabled": fmt.Sprint(disabled)})
	}
	return true
}

// principalID is a new user's ID: their sign-in name, lowercased (one in
// the reserved namespace is "user-<object ID>"), and if another principal
// has it, with their object ID's first eight characters.
func principalID(d *state.Document, upn, oid string) string {
	id := strings.ToLower(upn)
	if id == "" || strings.HasPrefix(id, state.ReservedPrefix) {
		id = "user-" + oid
	}
	if _, taken := d.Principals[id]; taken {
		id += "#" + oid[:min(8, len(oid))]
	}
	return id
}

// diff is what is in b and not a, and in a and not b; both sorted.
func diff(a, b []string) (added, removed []string) {
	for _, x := range b {
		if !slices.Contains(a, x) {
			added = append(added, x)
		}
	}
	for _, x := range a {
		if !slices.Contains(b, x) {
			removed = append(removed, x)
		}
	}
	return added, removed
}

// live is how many unrevoked keys id owns.
func live(d *state.Document, id string) int {
	n := 0
	for _, k := range d.Keys {
		if k.Owner == id && k.RevokedAt.IsZero() {
			n++
		}
	}
	return n
}

func (s *Sync) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Sync) log() *slog.Logger { return cmp.Or(s.Log, slog.Default()) }
