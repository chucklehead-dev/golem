package state

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"
)

// Restore is a document with an older one's entitlements brought back
// without widening anyone's rights (security-and-correctness.md §3,
// control-plane.md §3 History): for every right, the narrower of the
// backup's and the current document's.
//
//   - What only the backup has (a principal, scope, membership, role or
//     key since removed) is not restored: re-granting it is a separate,
//     audited change. Report.Withheld names each.
//   - What only the current document has (created since) is rolled back:
//     a scope or a principal removed (a principal that owns keys is kept,
//     with no memberships, as key records name their owner), a key minted
//     since revoked, so no later restore can bring it back.
//   - What both have takes the narrower of each: models, groups and
//     memberships intersected, budgets, in-flight limits and ceilings the
//     lower, storage allowed only if both allow it, the worst-case check
//     if either has it, a key's expiry the earlier.
//
// The fence, the publication records, the revocations and the audit
// outbox are the current document's, never the backup's (model/
// leadership.md's RESTORE=preserve): a restore cannot move publication
// back, depose or reinstate a leader, or revive a revoked key. A backup of
// another schema is refused. The result is written by Update, which checks
// every invariant against the current document.
func Restore(cur, backup *Document, at time.Time) (*Document, Report, error) {
	if backup.Schema != cur.Schema {
		return nil, Report{}, fmt.Errorf("state: the backup is schema %d, the document %d: refused", backup.Schema, cur.Schema)
	}
	r := &restorer{next: cur.clone(), at: at.UTC()}
	n := r.next
	n.Scopes = map[string]Scope{}
	for _, id := range slices.Sorted(maps.Keys(union(cur.Scopes, backup.Scopes))) {
		c, inCur := cur.Scopes[id]
		b, inBackup := backup.Scopes[id]
		switch {
		case !inCur:
			r.withhold("scope %s (removed since the backup)", id)
		case !inBackup:
			r.remove("scope %s (created since the backup)", id)
		default:
			n.Scopes[id] = r.scope(id, b, c)
		}
	}
	n.Principals = map[string]Principal{}
	for _, id := range slices.Sorted(maps.Keys(union(cur.Principals, backup.Principals))) {
		c, inCur := cur.Principals[id]
		b, inBackup := backup.Principals[id]
		switch {
		case !inCur:
			r.withhold("principal %s (removed since the backup)", id)
		case !inBackup && ownsKeys(cur, id):
			n.Principals[id] = Principal{Kind: c.Kind, Entra: c.Entra, UPN: c.UPN}
			r.remove("principal %s's memberships (created since the backup; kept for its keys' records)", id)
		case !inBackup:
			r.remove("principal %s (created since the backup)", id)
		default:
			n.Principals[id] = r.principal(id, b, c)
		}
	}
	n.Roles = nil
	for _, rb := range cur.Roles {
		_, p := n.Principals[rb.Principal]
		_, s := n.Scopes[rb.Scope]
		switch {
		case !slices.Contains(backup.Roles, rb):
			r.remove("%s's %s role on %s (granted since the backup)", rb.Principal, rb.Role, rb.Scope)
		case p && s:
			n.Roles = append(n.Roles, rb)
		}
	}
	for _, rb := range backup.Roles {
		if !slices.Contains(cur.Roles, rb) {
			r.withhold("%s's %s role on %s (removed since the backup)", rb.Principal, rb.Role, rb.Scope)
		}
	}
	n.Keys = maps.Clone(cur.Keys)
	for _, h := range slices.Sorted(maps.Keys(cur.Keys)) {
		c := cur.Keys[h]
		b, inBackup := backup.Keys[h]
		if !inBackup {
			if c.RevokedAt.IsZero() {
				r.remove("key %s (minted since the backup: revoked)", c.Display)
				n.Revoke(h, r.at)
			}
			continue
		}
		r.key(h, b, c)
	}
	for _, h := range slices.Sorted(maps.Keys(backup.Keys)) {
		if _, ok := cur.Keys[h]; !ok {
			r.withhold("key %s (its record is gone)", backup.Keys[h].Display)
		}
	}
	// A live key is honoured only for a member of its scope.
	for _, id := range slices.Sorted(maps.Keys(n.Principals)) {
		n.RevokeOutside(id, n.Principals[id], r.at)
	}
	return n, r.report, nil
}

// Report is what a restore did not bring back, and what it rolled back.
type Report struct {
	Withheld []string `json:"withheld,omitempty"` // in the backup, not restored: each a separate re-grant
	Removed  []string `json:"removed,omitempty"`  // in the document, rolled back
}

type restorer struct {
	next   *Document
	at     time.Time
	report Report
}

func (r *restorer) withhold(f string, a ...any) {
	r.report.Withheld = append(r.report.Withheld, fmt.Sprintf(f, a...))
}

func (r *restorer) remove(f string, a ...any) {
	r.report.Removed = append(r.report.Removed, fmt.Sprintf(f, a...))
}

// scope is the narrower of the backup's and the current policy.
func (r *restorer) scope(id string, b, c Scope) Scope {
	m := Scope{
		Groups:                 r.meetSet("scope "+id+"'s group", b.Groups, c.Groups),
		Models:                 r.meetSet("scope "+id+"'s model", b.Models, c.Models),
		BudgetMicros:           r.lower("scope "+id+"'s budget", b.BudgetMicros, c.BudgetMicros, 0),
		DeclaredWorstCaseCheck: b.DeclaredWorstCaseCheck || c.DeclaredWorstCaseCheck,
		PrincipalInFlight:      int(r.lower("scope "+id+"'s principal in-flight limit", int64(b.PrincipalInFlight), int64(c.PrincipalInFlight), DefaultPrincipalInFlight)),
		ScopeInFlight:          int(r.lower("scope "+id+"'s in-flight limit", int64(b.ScopeInFlight), int64(c.ScopeInFlight), DefaultScopeInFlight)),
		StorageAllowed:         b.StorageAllowed && c.StorageAllowed,
		MaxOutputTokens:        r.lower("scope "+id+"'s output ceiling", b.MaxOutputTokens, c.MaxOutputTokens, 0),
		MaxInputBytes:          r.lower("scope "+id+"'s input ceiling", b.MaxInputBytes, c.MaxInputBytes, 0),
		Features:               features(b.Features, c.Features),
	}
	if b.StorageAllowed && !c.StorageAllowed {
		r.withhold("scope %s's provider storage (forbidden since the backup)", id)
	}
	if !b.DeclaredWorstCaseCheck && c.DeclaredWorstCaseCheck {
		r.withhold("scope %s without the declared worst-case check (on since the backup)", id)
	}
	if m.Features != b.Features && b.Features == "compatible" {
		r.withhold("scope %s's compatible feature mode (restricted since the backup)", id)
	}
	return m
}

// principal is the backup's record, with memberships both have; its
// identity (kind, Entra ID) is the current one's: only an admin or the
// group sync binds it (ADR 0047).
func (r *restorer) principal(id string, b, c Principal) Principal {
	p := Principal{Kind: c.Kind, Entra: c.Entra, UPN: cmpOr(c.UPN, b.UPN)}
	if b.Kind != c.Kind || b.Entra != c.Entra {
		r.withhold("principal %s's identity in the backup (%s %q; now %s %q)", id, b.Kind, b.Entra, c.Kind, c.Entra)
	}
	exists := func(s string) bool { _, ok := r.next.Scopes[s]; return ok }
	p.Scopes = slices.DeleteFunc(r.meetSet("principal "+id+"'s membership of", b.Scopes, c.Scopes), func(s string) bool { return !exists(s) })
	if p.Kind == "user" && p.Entra != "" {
		p.Synced = slices.DeleteFunc(r.meetSet("principal "+id+"'s synced membership of", b.Synced, c.Synced), func(s string) bool { return !exists(s) })
	}
	for _, d := range []string{b.Default, c.Default} {
		if d != "" && p.MemberOf(d) {
			p.Default = d
			break
		}
	}
	return p
}

// key narrows a key both have to the backup's and the current models and
// the earlier expiry; one whose models have nothing in common is revoked
// (no models would mean all of its scope's).
func (r *restorer) key(h string, b, c Key) {
	k := r.next.Keys[h]
	switch {
	case len(b.Models) == 0:
		k.Models = c.Models
	case len(c.Models) == 0:
		k.Models = b.Models
		r.remove("key %s's wider models (narrowed again to the backup's)", c.Display)
	default:
		k.Models = r.meetSet("key "+c.Display+"'s model", b.Models, c.Models)
	}
	switch {
	case b.ExpiresAt.IsZero():
	case c.ExpiresAt.IsZero() || b.ExpiresAt.Before(c.ExpiresAt):
		k.ExpiresAt = b.ExpiresAt
		r.remove("key %s's later expiry (back to the backup's %s)", c.Display, b.ExpiresAt.Format(time.RFC3339))
	}
	r.next.Keys[h] = k
	if k.RevokedAt.IsZero() && len(k.Models) == 0 && (len(b.Models) > 0 || len(c.Models) > 0) {
		r.remove("key %s (its backup and current models have none in common: revoked)", c.Display)
		r.next.Revoke(h, r.at)
	}
}

// meetSet is what both lists hold, sorted; each side's extra is reported.
func (r *restorer) meetSet(what string, b, c []string) []string {
	var both []string
	for _, x := range c {
		if slices.Contains(b, x) {
			both = append(both, x)
		} else {
			r.remove("%s %s (added since the backup)", what, x)
		}
	}
	for _, x := range b {
		if !slices.Contains(c, x) {
			r.withhold("%s %s (removed since the backup)", what, x)
		}
	}
	slices.Sort(both)
	return slices.Compact(both)
}

// lower is the stricter of two limits, where 0 is the default def (0 for
// none: no limit). Both unset stays unset.
func (r *restorer) lower(what string, b, c, def int64) int64 {
	eff := func(v int64) int64 {
		if v == 0 {
			return def
		}
		return v
	}
	switch {
	case b == 0 && c == 0:
		return 0
	case eff(b) == 0: // the backup had none
		return c
	case eff(c) == 0:
		r.remove("%s's removal (back to the backup's %d)", what, b)
		return b
	case eff(b) > eff(c):
		r.withhold("%s %d (lowered since the backup to %d)", what, b, eff(c))
		return c
	case eff(b) < eff(c):
		r.remove("%s %d (raised since the backup; back to %d)", what, eff(c), b)
		return b
	}
	return c
}

// features is the stricter feature mode: compatible only if both are,
// restricted if either says so, else unset (restricted, but compatible on
// agent routes).
func features(b, c string) string {
	switch {
	case b == "compatible" && c == "compatible":
		return "compatible"
	case b == "restricted" || c == "restricted":
		return "restricted"
	}
	return ""
}

func ownsKeys(d *Document, principal string) bool {
	for _, k := range d.Keys {
		if k.Owner == principal {
			return true
		}
	}
	return false
}

func union[V any](a, b map[string]V) map[string]bool {
	u := map[string]bool{}
	for k := range a {
		u[k] = true
	}
	for k := range b {
		u[k] = true
	}
	return u
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Restore brings back backup's entitlements as the function Restore does,
// written by Update with a state.restore audit event naming the actor; with
// dryRun, only checked (Validate). It returns what the restore withheld
// and rolled back. A document the store cannot read is not restored over:
// the restore needs the current state to know what not to widen.
func (s *Store) Restore(ctx context.Context, backup *Document, actor string, dryRun bool) (Report, error) {
	var report Report
	change := func(d *Document) error {
		next, r, err := Restore(d, backup, time.Now())
		if err != nil {
			return err
		}
		report = r
		next.Audit = append(next.Audit, AuditEvent{At: time.Now().UTC(), Actor: actor, Action: "state.restore", Target: "document",
			Detail: map[string]string{"backupVersion": strconv.FormatUint(backup.StateVersion, 10),
				"withheld": strconv.Itoa(len(r.Withheld)), "removed": strconv.Itoa(len(r.Removed))}})
		*d = *next
		return nil
	}
	if dryRun {
		return report, s.Validate(ctx, change)
	}
	_, err := s.Update(ctx, change)
	return report, err
}
