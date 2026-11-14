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
// A step that would end more memberships than MaxRemovals is refused, so
// a misconfigured group or a directory answering empty cannot strip a
// team at once (the accidental-deletion guard directory sync tools keep).
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

// User is a group member as the directory reports it.
type User struct {
	ID      string // object ID: the oid in the user's tokens
	UPN     string // userPrincipalName
	Enabled bool   // accountEnabled
}

// Directory lists a group's users, transitively (nested groups' too).
type Directory interface {
	Members(ctx context.Context, group string) ([]User, error)
}

// ErrNoGroup is a group the directory does not have (deleted): it has no
// members, and the scopes it was bound to lose those it gave.
var ErrNoGroup = errors.New("entrasync: no such group")

// ErrTooManyRemovals is a step refused by its removal guard.
var ErrTooManyRemovals = errors.New("entrasync: too many memberships would end")

// Actor is the sync's name in the audit.
const Actor = "system:entra-sync"

// Sync reconciles the state with the directory.
type Sync struct {
	State     *state.Store
	Directory Directory
	// MaxRemovals bounds the memberships one step may end (default 50);
	// a step over it changes nothing and is reported. Raise it for one run
	// when a large removal is intended.
	MaxRemovals int
	Now         func() time.Time
	Log         *slog.Logger
}

// Result is what a step changed.
type Result struct {
	Created, Added, Removed, Revoked int
	changed                          bool // anything at all, a sign-in name included
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
	var res Result
	_, err := s.State.FencedUpdate(ctx, fence, func(d *state.Document) error {
		// Built afresh from the document this attempt is given: the update
		// may retry against a newer one.
		res = reconcile(d, members, s.now())
		if limit := cmp.Or(s.MaxRemovals, 50); res.Removed > limit {
			return fmt.Errorf("%w: %d, over the limit of %d; check the bound groups, or raise -entra-sync-max-removals for one run", ErrTooManyRemovals, res.Removed, limit)
		}
		if !res.changed {
			return errNoChange
		}
		return nil
	})
	if errors.Is(err, errNoChange) {
		return Result{}, nil
	}
	if err != nil {
		return Result{}, err
	}
	return res, nil
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
	upn     string
	enabled bool
	scopes  []string
}

// reconcile makes d's synced memberships those members imply: a member of
// a group bound to a scope is a member of the scope while enabled. A user
// not yet a principal is created (if enabled); one disabled or in no bound
// group loses its synced memberships; every changed principal's keys
// outside its memberships are revoked, and a disabled user's keys all.
// Each change is an audit event by Actor.
func reconcile(d *state.Document, members map[string][]User, at time.Time) Result {
	wants := map[string]*want{} // by object ID
	for _, sid := range slices.Sorted(maps.Keys(d.Scopes)) {
		for _, g := range d.Scopes[sid].Groups {
			for _, u := range members[g] {
				w := wants[u.ID]
				if w == nil {
					w = &want{upn: u.UPN, enabled: true}
					wants[u.ID] = w
				}
				w.enabled = w.enabled && u.Enabled
				w.scopes = append(w.scopes, sid)
			}
		}
	}
	byEntra := map[string]string{}
	for id, p := range d.Principals {
		if p.Kind == "user" && p.Entra != "" {
			byEntra[p.Entra] = id
		}
	}
	var res Result
	audit := func(action, target string, detail map[string]string) {
		d.Audit = append(d.Audit, state.AuditEvent{At: at, Actor: Actor, Action: action, Target: target, Detail: detail})
	}
	set := func(id string, p state.Principal, synced []string, disabled bool) {
		added, removed := diff(p.Synced, synced)
		p.Synced = synced
		before := live(d, id)
		if disabled {
			for _, h := range slices.Sorted(maps.Keys(d.Keys)) {
				if k := d.Keys[h]; k.Owner == id && k.RevokedAt.IsZero() {
					d.Revoke(h, at)
				}
			}
		} else {
			d.RevokeOutside(id, p, at)
		}
		revoked := before - live(d, id)
		lostDefault := p.Default != "" && !p.MemberOf(p.Default)
		if len(added)+len(removed)+revoked == 0 && !lostDefault {
			return
		}
		if lostDefault {
			p.Default = ""
		}
		d.Principals[id] = p
		res.Added += len(added)
		res.Removed += len(removed)
		res.Revoked += revoked
		res.changed = true
		audit("membership.sync", id, map[string]string{"added": strings.Join(added, ","), "removed": strings.Join(removed, ","),
			"keysRevoked": fmt.Sprint(revoked), "disabled": fmt.Sprint(disabled)})
	}
	for _, oid := range slices.Sorted(maps.Keys(wants)) {
		w := wants[oid]
		id, exists := byEntra[oid]
		if !exists && !w.enabled {
			continue // never created for a disabled account
		}
		if !exists {
			if d.Principals == nil {
				d.Principals = map[string]state.Principal{}
			}
			id = principalID(d, w.upn, oid)
			d.Principals[id] = state.Principal{Kind: "user", Entra: oid, UPN: w.upn, Scopes: []string{}}
			res.Created++
			res.changed = true
			audit("principal.create", id, map[string]string{"entra": oid, "upn": w.upn})
		}
		p := d.Principals[id]
		if p.UPN != w.upn {
			p.UPN = w.upn
			d.Principals[id] = p
			res.changed = true
		}
		var synced []string
		if w.enabled {
			synced = slices.Compact(slices.Sorted(slices.Values(w.scopes)))
		}
		set(id, p, synced, !w.enabled)
	}
	for _, oid := range slices.Sorted(maps.Keys(byEntra)) {
		if _, seen := wants[oid]; !seen {
			id := byEntra[oid]
			if p := d.Principals[id]; len(p.Synced) > 0 {
				set(id, p, nil, false)
			}
		}
	}
	return res
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
