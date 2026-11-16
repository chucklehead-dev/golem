package state

import (
	"cmp"
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"
)

// Restore is a document with an older one's entitlements brought back
// without widening anyone's rights (security-and-correctness.md). Each
// right is the narrower of the backup's and the current document's.
//
//   - What only the backup has (a principal, scope, membership, role or
//     key since removed) is not restored: re-granting it is a separate,
//     audited change. Report.Withheld names each.
//   - What only the current document has (created since) is rolled back:
//     a scope or a principal removed (a principal that owns keys is kept,
//     with no memberships, as key records name their owner), a key minted
//     since revoked, so no later restore can bring it back.
//   - What both have takes the narrower of each: groups, memberships and a
//     key's models intersected, a key's expiry the earlier.
//
// A scope's policy is config, not state (ADR 0050), so a restore neither
// brings back nor narrows it.
//
// The fence, the publication records, the revocations and the audit outbox
// are the current document's, never the backup's. So a restore cannot move
// publication back, depose or reinstate a leader, or revive a revoked key.
// A backup of another schema is refused. Update writes the result, checking
// every invariant against the current document.
//
//quint:enforces revocationsSurviveRestore
func Restore(cur, backup *Document, at time.Time) (*Document, Report, error) {
	if backup.Schema != cur.Schema {
		return nil, Report{}, fmt.Errorf("state: the backup is schema %d, the document %d: refused", backup.Schema, cur.Schema)
	}
	r := &restorer{cur: cur, backup: backup, next: cur.clone(), at: at.UTC()}
	// In this order: memberships are kept only of the scopes restored,
	// roles only of the principals and scopes restored, and the keys
	// narrowed before their owners' memberships revoke what is outside them.
	r.scopes()
	r.principals()
	r.roles()
	r.keys()
	// A live key is honoured only for a member of its scope.
	for _, id := range slices.Sorted(maps.Keys(r.next.Principals)) {
		r.next.RevokeOutside(id, r.next.Principals[id], r.at)
	}
	return r.next, r.report, nil
}

// scopes keeps the scopes both documents have, each with the groups both
// bind.
func (r *restorer) scopes() {
	r.next.Scopes = map[string]Scope{}
	for _, id := range union(r.cur.Scopes, r.backup.Scopes) {
		c, inCur := r.cur.Scopes[id]
		b, inBackup := r.backup.Scopes[id]
		switch {
		case !inCur:
			r.withhold("scope %s (removed since the backup)", id)
		case !inBackup:
			r.remove("scope %s (created since the backup)", id)
		default:
			r.next.Scopes[id] = Scope{Groups: r.meetSet("scope "+id+"'s group", b.Groups, c.Groups)}
		}
	}
}

// principals keeps the principals both documents have, each with the
// memberships both have; one created since is kept, with no memberships,
// only if it owns keys (their records name it).
func (r *restorer) principals() {
	r.next.Principals = map[string]Principal{}
	for _, id := range union(r.cur.Principals, r.backup.Principals) {
		c, inCur := r.cur.Principals[id]
		b, inBackup := r.backup.Principals[id]
		switch {
		case !inCur:
			r.withhold("principal %s (removed since the backup)", id)
		case !inBackup && ownsKeys(r.cur, id):
			r.next.Principals[id] = Principal{Kind: c.Kind, Entra: c.Entra, UPN: c.UPN}
			r.remove("principal %s's memberships (created since the backup; kept for its keys' records)", id)
		case !inBackup:
			r.remove("principal %s (created since the backup)", id)
		default:
			r.next.Principals[id] = r.principal(id, b, c)
		}
	}
}

// roles keeps the role bindings both documents have, of a principal and on
// a scope that were kept.
func (r *restorer) roles() {
	r.next.Roles = nil
	for _, rb := range r.cur.Roles {
		_, p := r.next.Principals[rb.Principal]
		_, s := r.next.Scopes[rb.Scope]
		switch {
		case !slices.Contains(r.backup.Roles, rb):
			r.remove("%s's %s role on %s (granted since the backup)", rb.Principal, rb.Role, rb.Scope)
		case p && s:
			r.next.Roles = append(r.next.Roles, rb)
		}
	}
	for _, rb := range r.backup.Roles {
		if !slices.Contains(r.cur.Roles, rb) {
			r.withhold("%s's %s role on %s (removed since the backup)", rb.Principal, rb.Role, rb.Scope)
		}
	}
}

// keys keeps every current key record: one minted since the backup is
// revoked, one both have is narrowed (key). A key only the backup has is
// not brought back.
func (r *restorer) keys() {
	r.next.Keys = maps.Clone(r.cur.Keys)
	for _, h := range slices.Sorted(maps.Keys(r.cur.Keys)) {
		c := r.cur.Keys[h]
		b, inBackup := r.backup.Keys[h]
		if !inBackup {
			if c.RevokedAt.IsZero() {
				r.remove("key %s (minted since the backup: revoked)", c.Display)
				r.next.Revoke(h, r.at)
			}
			continue
		}
		r.key(h, b, c)
	}
	for _, h := range slices.Sorted(maps.Keys(r.backup.Keys)) {
		if _, ok := r.cur.Keys[h]; !ok {
			r.withhold("key %s (its record is gone)", r.backup.Keys[h].Display)
		}
	}
}

// Report is what a restore did not bring back, and what it rolled back.
type Report struct {
	Withheld []string `json:"withheld,omitempty"` // in the backup, not restored: each a separate re-grant
	Removed  []string `json:"removed,omitempty"`  // in the document, rolled back
}

// restorer builds a restore's document, next, from cur and backup (both
// read, never changed), recording what it withholds and rolls back.
type restorer struct {
	cur, backup *Document
	next        *Document
	at          time.Time
	report      Report
}

func (r *restorer) withhold(f string, a ...any) {
	r.report.Withheld = append(r.report.Withheld, fmt.Sprintf(f, a...))
}

func (r *restorer) remove(f string, a ...any) {
	r.report.Removed = append(r.report.Removed, fmt.Sprintf(f, a...))
}

// principal is the backup's record, with memberships both have; its
// identity (kind, Entra ID) is the current one's: only an admin or the
// group sync binds it (ADR 0047).
func (r *restorer) principal(id string, b, c Principal) Principal {
	p := Principal{Kind: c.Kind, Entra: c.Entra, UPN: cmp.Or(c.UPN, b.UPN)}
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

// ownsKeys reports whether any key record, revoked or not, names principal
// as its owner.
func ownsKeys(d *Document, principal string) bool {
	for _, k := range d.Keys {
		if k.Owner == principal {
			return true
		}
	}
	return false
}

// union is the keys of a and b, sorted.
func union[V any](a, b map[string]V) []string {
	u := maps.Clone(a)
	if u == nil {
		u = map[string]V{}
	}
	maps.Copy(u, b)
	return slices.Sorted(maps.Keys(u))
}

// Restore brings back backup's entitlements as the function Restore does,
// written by Update with a state.restore audit event naming the actor; with
// dryRun, only checked (Validate). It returns what the restore withheld
// and rolled back. A document the store cannot read is not restored over:
// the restore needs the current state to know what not to widen.
func (s *Store) Restore(ctx context.Context, backup *Document, actor string, dryRun bool) (Report, error) {
	var report Report
	change := func(d *Document) error {
		if d.StateVersion == 0 {
			// Restored over nothing, every right would be withheld and an
			// empty document written: a deletion is undone by putting the
			// newest version back, not by a restore.
			return fmt.Errorf("%w: there is no %s to restore over; put its newest version back from the bucket's versions, or run golem-control bootstrap first", ErrInvariant, DocumentKey)
		}
		now := time.Now()
		next, r, err := Restore(d, backup, now)
		if err != nil {
			return err
		}
		report = r
		next.Audit = append(next.Audit, AuditEvent{At: now.UTC(), Actor: actor, Action: "state.restore", Target: "document",
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
