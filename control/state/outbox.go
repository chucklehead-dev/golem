package state

import (
	"cmp"
	"errors"
	"fmt"
	"maps"
	"slices"
)

// The audit outbox's bound (ADR 0041 §1, security-and-correctness.md §7).
// The outbox lives in the document every write and every 500 ms read
// carries, and only the leader's shipper trims it, after archival: with no
// archive, or one unreachable, it would grow without limit. So the store
// bounds it, and the bound is directional:
//
//   - Past the reserve (80% of the bound) a write that grants or widens
//     access is refused (*OutboxFull); restrictive writes go on landing,
//     into the last 20%, which is theirs.
//   - At the bound every write that adds an audit event is refused,
//     restrictive ones too: the document must not grow without limit, and
//     no change is written without its event. A revocation then goes
//     through the site's emergency denylist until the outbox drains.
//   - A write that adds no event (publication records, the fence, the
//     shipper's trim) is never refused by it.
//
// A write is restrictive only if every event it adds is of a Restrictive
// action (actions, below) and the change itself widens nothing, judged
// from the documents before and after (widens): the effective
// authorization diff, not the action's name alone, so an event labelled
// restrictive cannot carry a grant, and a mixed change is a widening one.
// An action not in the table is refused at every write (ErrInvariant):
// a new write path cannot land unclassified (TestEveryActionClassified
// finds them in the source).

// DefaultMaxOutbox is the outbox's bound when the store sets none
// (Store.MaxOutbox; golem-control -audit-outbox-max): about a day of
// heavy administration, and a few MB in the document.
const DefaultMaxOutbox = 10_000

// Reserve is the outbox's length, for a bound, past which only
// restrictive writes land: 80% of it.
func Reserve(bound int) int { return bound - bound/5 }

// Class is an audit action's direction.
type Class int

const (
	// Widening: the change can grant or widen access (a key, a member, a
	// scope, a role, a bound group). Refused past the reserve.
	Widening Class = iota + 1
	// Restrictive: the change only narrows access, or widens nobody's
	// (recovery of the document). Lands up to the bound.
	Restrictive
)

func (c Class) String() string {
	switch c {
	case Widening:
		return "widening"
	case Restrictive:
		return "restrictive"
	}
	return fmt.Sprintf("Class(%d)", int(c))
}

// actions is every audit action golem writes, and its class. A new action
// is added here, Widening unless it can only narrow access.
var actions = map[string]Class{
	// control/service
	"scope.create":      Widening,
	"membership.add":    Widening,
	"membership.remove": Restrictive,
	"group.bind":        Widening,
	"group.unbind":      Restrictive,
	"role.bind":         Widening,
	"key.mint":          Widening,
	"key.revoke":        Restrictive,
	"member.cap":        Widening, // a cap raised or removed widens
	"key.cap":           Widening,
	// control/entrasync: a user created, memberships added; and the
	// removals and offboarding, written as their own event so they land
	// when the additions are held back.
	"principal.create":       Widening,
	"membership.sync.add":    Widening,
	"membership.sync.remove": Restrictive,
	// control/state: a restore never widens (Restore); a bootstrap or an
	// adoption raises publication records and grants nothing.
	"state.restore":   Restrictive,
	"state.bootstrap": Restrictive,
	"state.adopt":     Restrictive,
}

// ClassOf is action's class; ok is false for an action golem does not
// write.
func ClassOf(action string) (c Class, ok bool) {
	c, ok = actions[action]
	return c, ok
}

// Actions is every audit action golem writes, sorted.
func Actions() []string { return slices.Sorted(maps.Keys(actions)) }

// ErrOutboxFull: the audit outbox is past its reserve and the write
// widens access, or at its bound. Nothing was written; it is retryable
// once the outbox drains.
var ErrOutboxFull = errors.New("state: the audit outbox is full")

// OutboxFull is ErrOutboxFull: what the outbox holds, its bound, and the
// write refused.
type OutboxFull struct {
	Len    int    // the events the outbox holds, without the write's
	Max    int    // its bound
	Action string // the refused write's audit action (its first widening one)
	// Widens is how the write widens access; empty at the bound, where
	// every write that adds an event is refused.
	Widens string
}

// Hard reports whether the outbox is at its bound: restrictive writes are
// refused too.
func (e *OutboxFull) Hard() bool { return e.Widens == "" }

func (e *OutboxFull) Unwrap() error { return ErrOutboxFull }

func (e *OutboxFull) Error() string {
	const drain = "The outbox drains only as the leader archives it: check that golem-control runs with -audit-archive and that the archive bucket is reachable and writable (the leader's audit: error; operations.md, alert Audit outbox not draining), then retry"
	if e.Hard() {
		return fmt.Sprintf("%v: it holds %d audit events of its bound of %d (-audit-outbox-max), and this change's would pass it: no change that adds an event is written until it drains, revocations included (%s refused). %s. To deny access meanwhile, use the site's emergency denylist (operations.md §2, Emergency revocation)",
			ErrOutboxFull, e.Len, e.Max, e.Action, drain)
	}
	return fmt.Sprintf("%v for changes that grant access: it holds %d audit events of its bound of %d (-audit-outbox-max), past the %d after which the rest is kept for restrictive changes, so %s is refused (%s); revocations and removals still land. %s",
		ErrOutboxFull, e.Len, e.Max, Reserve(e.Max), e.Action, e.Widens, drain)
}

// OutboxMax is the outbox's bound: MaxOutbox, or DefaultMaxOutbox.
func (s *Store) OutboxMax() int { return outboxMax(s.MaxOutbox) }

func outboxMax(n int) int {
	if n <= 0 {
		return DefaultMaxOutbox
	}
	return n
}

// checkOutbox: every event a write adds is of a known action, and the
// outbox stays within its bound, and past its reserve grows only by
// restrictive writes. A write that adds no event is not judged: the trim,
// publication and the fence go on whatever the outbox holds.
func checkOutbox(old, next *Document, bound int) error {
	n := keptLen(old, next)
	added := next.Audit[n:]
	if len(added) == 0 {
		return nil
	}
	widening := ""
	for _, e := range added {
		c, ok := actions[e.Action]
		if !ok {
			return broken("audit action %q has no class: add it to control/state's actions table (outbox.go), Widening unless it can only narrow access", e.Action)
		}
		if c == Widening && widening == "" {
			widening = e.Action
		}
	}
	if len(next.Audit) <= Reserve(bound) {
		return nil
	}
	// Past the reserve: a write that grants is refused as one, even if it
	// would also pass the bound, so its writer can try its restrictive part
	// alone (entrasync); a restrictive write is refused only at the bound.
	full := &OutboxFull{Len: n, Max: bound, Action: cmp.Or(widening, added[0].Action)}
	switch {
	case widening != "":
		full.Widens = "a " + widening + " grants or widens access"
	default:
		full.Widens = widens(old, next)
	}
	if full.Widens != "" || len(next.Audit) > bound {
		return full
	}
	return nil
}

// widens is the first way next grants access old does not, or "": a scope
// created or a group bound to one, a principal created, given an identity
// or made a member, a role bound, or a live key minted, moved, revived or
// widened in models or expiry. A principal's default scope and sign-in
// name are not access.
func widens(old, next *Document) string {
	for _, id := range slices.Sorted(maps.Keys(next.Scopes)) {
		o, ok := old.Scopes[id]
		if !ok {
			return "scope " + id + " created"
		}
		for _, g := range next.Scopes[id].Groups {
			if !slices.Contains(o.Groups, g) {
				return "group " + g + " bound to scope " + id
			}
		}
	}
	for _, id := range slices.Sorted(maps.Keys(next.Principals)) {
		p := next.Principals[id]
		o, ok := old.Principals[id]
		switch {
		case !ok:
			return "principal " + id + " created"
		case p.Kind != o.Kind, p.Entra != "" && p.Entra != o.Entra:
			return "principal " + id + "'s identity changed"
		}
		for _, s := range p.Members() {
			if !o.MemberOf(s) {
				return "principal " + id + " made a member of " + s
			}
		}
	}
	for _, r := range next.Roles {
		if !slices.Contains(old.Roles, r) {
			return r.Principal + " given the " + r.Role + " role on " + r.Scope
		}
	}
	for _, h := range slices.Sorted(maps.Keys(next.Keys)) {
		k := next.Keys[h]
		if !k.RevokedAt.IsZero() {
			continue
		}
		o, ok := old.Keys[h]
		switch {
		case !ok:
			return "key " + k.Display + " minted"
		case !o.RevokedAt.IsZero():
			return "key " + k.Display + " revived"
		case k.Owner != o.Owner || k.Scope != o.Scope:
			return "key " + k.Display + " moved"
		case len(o.Models) > 0 && (len(k.Models) == 0 || slices.ContainsFunc(k.Models, func(m string) bool { return !slices.Contains(o.Models, m) })):
			return "key " + k.Display + "'s models widened"
		case !o.ExpiresAt.IsZero() && (k.ExpiresAt.IsZero() || k.ExpiresAt.After(o.ExpiresAt)):
			return "key " + k.Display + "'s expiry extended"
		}
	}
	return ""
}
