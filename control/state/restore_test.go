package state

import (
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"
)

// TestRestore: a backup taken, then rights changed both ways; the restore
// rolls back what widened and keeps what narrowed, the fence, the records
// and every revocation the current document's, and says what it withheld.
func TestRestore(t *testing.T) {
	s := seeded(t)
	ctx := t.Context()
	if _, err := s.Update(ctx, func(d *Document) error {
		d.Fence = &Fence{Holder: "r1", Epoch: 1}
		d.Scopes["a"] = Scope{Groups: []string{"g1"}}
		d.Roles = []RoleBinding{{"alice", "manager", "a"}}
		return put("p", 1)(d)
	}); err != nil {
		t.Fatal(err)
	}
	backup := s.Current()
	at := time.Unix(100, 0)
	if _, err := s.Update(ctx, func(d *Document) error {
		d.Fence = &Fence{Holder: "r2", Epoch: 2}
		if err := put("p", 2)(d); err != nil {
			return err
		}
		// Widened since the backup: a group bound, a new key, a new
		// principal with a role.
		d.Scopes["a"] = Scope{Groups: []string{"g1", "g2"}}
		d.Principals["eve"] = Principal{Kind: "user", Scopes: []string{"a"}}
		d.Roles = append(d.Roles, RoleBinding{"eve", "manager", "a"})
		d.Keys["h3"] = Key{Display: "glm_live_3", Owner: "eve", Scope: "a"}
		d.Keys["h4"] = Key{Display: "glm_live_4", Owner: "alice", Scope: "a"} // by a member the backup had
		// Narrowed since: a key revoked, a membership removed.
		if err := d.Revoke("h2", at); err != nil {
			return err
		}
		p := d.Principals["alice"]
		p.Scopes, p.Default = []string{"a"}, ""
		d.Principals["alice"] = p
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := s.Current()
	report, err := s.Restore(ctx, backup, "admin:ops", false)
	if err != nil {
		t.Fatal(err)
	}
	d := s.Current()
	switch {
	case *d.Fence != (Fence{Holder: "r2", Epoch: 2}) || d.Publications["p"].Seq != 2:
		t.Fatalf("fence %+v, record %+v: a restore keeps the current ones", d.Fence, d.Publications["p"])
	case !slices.Equal(d.Scopes["a"].Groups, []string{"g1"}):
		t.Fatalf("scope a %+v: the group bound since is unbound", d.Scopes["a"])
	case d.Keys["h2"].RevokedAt.IsZero() || d.Keys["h3"].RevokedAt.IsZero() || d.Keys["h4"].RevokedAt.IsZero():
		t.Fatalf("keys %+v: one revoked since stays revoked, those minted since are revoked", d.Keys)
	case !d.Keys["h1"].RevokedAt.IsZero():
		t.Fatal("alice's key from before the backup, still a member, was revoked")
	case !slices.Equal(d.Principals["alice"].Scopes, []string{"a"}):
		t.Fatalf("alice %+v: a membership removed since is not restored", d.Principals["alice"])
	case len(d.Principals["eve"].Scopes) != 0 || slices.ContainsFunc(d.Roles, func(r RoleBinding) bool { return r.Principal == "eve" }):
		t.Fatalf("eve %+v, roles %v: a principal created since keeps no rights", d.Principals["eve"], d.Roles)
	case !slices.Equal(d.Revocations[:len(before.Revocations)], before.Revocations):
		t.Fatal("the revocations were not kept")
	}
	if !contains(report.Withheld, "principal alice's membership of b") || !contains(report.Removed, "key glm_live_3") ||
		!contains(report.Removed, "scope a's group g2 (added since the backup)") {
		t.Fatalf("report %+v: want alice's membership of b withheld, and the new key and group named", report)
	}
	last := d.Audit[len(d.Audit)-1]
	if last.Action != "state.restore" || last.Actor != "admin:ops" {
		t.Fatalf("audit %+v: a restore is audited", last)
	}
	// A dry run writes nothing.
	v := d.StateVersion
	if _, err := s.Restore(ctx, backup, "admin:ops", true); err != nil || s.Current().StateVersion != v {
		t.Fatalf("dry run: %v, version %d → %d", err, v, s.Current().StateVersion)
	}
	other := *backup
	other.Schema = 99
	if _, err := s.Restore(ctx, &other, "admin:ops", false); err == nil {
		t.Fatal("a backup of another schema was restored")
	}
}

func contains(lines []string, sub string) bool {
	return slices.ContainsFunc(lines, func(l string) bool { return strings.Contains(l, sub) })
}

// TestRestoreNeverWidens: over random pairs of valid documents, a
// restore's result passes every invariant against the current document and
// grants nothing the current one does not: no scope, bound group,
// membership or role it lacks, no live key it does not have live, no key
// model it lacks, no later expiry; and its fence and records are the
// current ones. (A scope's policy is config, not state: a restore cannot
// touch it.)
func TestRestoreNeverWidens(t *testing.T) {
	for seed := range uint64(2000) {
		r := rand.New(rand.NewPCG(seed, 1))
		cur, backup := randomDoc(r), randomDoc(r)
		// The backup shares the current keys' history: a key revoked in the
		// backup is revoked now (revocations are never undone).
		for h, k := range backup.Keys {
			if c, ok := cur.Keys[h]; ok && !k.RevokedAt.IsZero() && c.RevokedAt.IsZero() {
				cur.Revoke(h, k.RevokedAt)
			}
		}
		if err := checkEntitlements(cur, cur); err != nil {
			continue // not a valid current document
		}
		next, _, err := Restore(cur, backup, time.Unix(1000, 0))
		if err != nil {
			t.Fatal(err)
		}
		if err := checkEntitlements(cur, next); err != nil {
			t.Fatalf("seed %d: the restore breaks an invariant: %v", seed, err)
		}
		if err := narrower(cur, next); err != nil {
			t.Fatalf("seed %d: %v\ncurrent %+v\nbackup %+v\nrestored %+v", seed, err, cur, backup, next)
		}
	}
}

// narrower reports a right next grants that cur does not.
func narrower(cur, next *Document) error {
	if next.Fence != cur.Fence && (next.Fence == nil || cur.Fence == nil || *next.Fence != *cur.Fence) || !maps.Equal(next.Publications, cur.Publications) {
		return fmt.Errorf("the fence or records changed")
	}
	for id, s := range next.Scopes {
		c, ok := cur.Scopes[id]
		switch {
		case !ok:
			return fmt.Errorf("scope %s is not the current document's", id)
		case !subset(s.Groups, c.Groups):
			return fmt.Errorf("scope %s: groups widened", id)
		}
	}
	for id, p := range next.Principals {
		c, ok := cur.Principals[id]
		switch {
		case !ok:
			return fmt.Errorf("principal %s is not the current document's", id)
		case p.Kind != c.Kind || p.Entra != c.Entra:
			return fmt.Errorf("principal %s's identity changed", id)
		case !subset(p.Scopes, c.Scopes) || !subset(p.Synced, c.Synced):
			return fmt.Errorf("principal %s: memberships widened", id)
		}
		for _, s := range p.Members() {
			if capRaised(c.Caps[s], p.Caps[s]) {
				return fmt.Errorf("principal %s: cap in %s raised or removed (%d, now %d)", id, s, c.Caps[s], p.Caps[s])
			}
		}
	}
	for _, rb := range next.Roles {
		if !slices.Contains(cur.Roles, rb) {
			return fmt.Errorf("role %+v is not the current document's", rb)
		}
	}
	for h, k := range next.Keys {
		c, ok := cur.Keys[h]
		switch {
		case !ok:
			return fmt.Errorf("key %s is not the current document's", h)
		case !k.RevokedAt.IsZero():
		case !c.RevokedAt.IsZero():
			return fmt.Errorf("key %s is live again", h)
		case len(c.Models) > 0 && (len(k.Models) == 0 || !subset(k.Models, c.Models)):
			return fmt.Errorf("key %s: models widened (%v, now %v)", h, k.Models, c.Models)
		case !c.ExpiresAt.IsZero() && (k.ExpiresAt.IsZero() || k.ExpiresAt.After(c.ExpiresAt)):
			return fmt.Errorf("key %s: expiry later", h)
		case capRaised(c.CapMicros, k.CapMicros):
			return fmt.Errorf("key %s: cap raised or removed (%d, now %d)", h, c.CapMicros, k.CapMicros)
		}
	}
	return nil
}

func subset(a, b []string) bool {
	for _, x := range a {
		if !slices.Contains(b, x) {
			return false
		}
	}
	return true
}

// randomDoc is a small document over a few names, valid by construction
// (save the fence and records, which the restore keeps whatever they are).
func randomDoc(r *rand.Rand) *Document {
	pick := func(from []string) []string {
		var out []string
		for _, x := range from {
			if r.IntN(2) == 0 {
				out = append(out, x)
			}
		}
		return out
	}
	d := &Document{Schema: Schema, Scopes: map[string]Scope{}, Principals: map[string]Principal{}, Keys: map[string]Key{},
		Fence: &Fence{Holder: "r", Epoch: uint64(r.IntN(3) + 1)}, Publications: map[string]Record{"p": {Seq: uint64(r.IntN(5) + 1)}}}
	for _, s := range pick([]string{"a", "b", "c"}) {
		d.Scopes[s] = Scope{Groups: pick([]string{"g1", "g2"})}
	}
	scopes := slices.Sorted(maps.Keys(d.Scopes))
	entra := []string{"", "oid-u", "oid-v"}
	used := map[string]bool{}
	for _, id := range pick([]string{"u", "v", "w"}) {
		p := Principal{Kind: []string{"user", "service"}[r.IntN(2)], Entra: entra[r.IntN(3)], Scopes: pick(scopes)}
		if p.Entra != "" && used[p.Entra] {
			p.Entra = ""
		}
		used[p.Entra] = p.Entra != ""
		if p.Kind == "user" && p.Entra != "" {
			p.Synced = pick(scopes)
		}
		if m := p.Members(); len(m) > 0 && r.IntN(2) == 0 {
			p.Default = m[r.IntN(len(m))]
		}
		for _, s := range pick(p.Members()) {
			if p.Caps == nil {
				p.Caps = map[string]int64{}
			}
			p.Caps[s] = int64(5 * (1 + r.IntN(2)))
		}
		d.Principals[id] = p
		for _, s := range pick(scopes) {
			d.Roles = append(d.Roles, RoleBinding{id, "manager", s})
		}
	}
	for i, h := range pick([]string{"k1", "k2", "k3", "k4"}) {
		owners := slices.Sorted(maps.Keys(d.Principals))
		if len(owners) == 0 {
			break
		}
		owner := owners[r.IntN(len(owners))]
		p := d.Principals[owner]
		m := p.Members()
		k := Key{Display: "glm_" + h, Owner: owner, Models: pick([]string{"m1", "m2", "m3"})}
		if r.IntN(3) == 0 {
			k.ExpiresAt = time.Unix(int64(500+100*r.IntN(3)), 0).UTC()
		}
		k.CapMicros = []int64{0, 3, 7}[r.IntN(3)]
		switch {
		case len(m) == 0 || r.IntN(4) == 0: // revoked, in a scope it may no longer be a member of
			k.Scope = []string{"a", "b", "c"}[i%3]
			d.Keys[h] = k
			d.Revoke(h, time.Unix(int64(10+i), 0))
		default:
			k.Scope = m[r.IntN(len(m))]
			d.Keys[h] = k
		}
	}
	return d
}
