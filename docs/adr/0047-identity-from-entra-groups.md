# 0047: Users and their memberships come from Entra groups

Status: Accepted. Amends [0009](0009-identity-and-authentication.md),
org-model.md §1–§2 and control-plane.md §5–§6.

## Context

A principal's Entra ID is what the control API and the gateway match a
token to. The control API let a scope's manager set it: `members add
<scope> <id> --entra <oid>` bound any object ID to a principal of the
manager's choosing, and a manager could create a principal named
`entra:<someone's oid>`, the ID the control API gives a caller with no
principal yet, which that person would then be. Either way a manager could
claim another person's identity, and with it their rights in other
scopes, or have keys minted in their name. A user's identity is the
directory's to state, not a manager's.

## Decision

- **Scopes bind Entra groups** (`Scope.Groups`, object IDs; `golemctl
  groups bind|unbind`, admins only: binding a group grants its every
  member).
- **The group sync** (`control/entrasync`, run by the leader every five
  minutes) reads each bound group's transitive user members from
  Microsoft Graph (application permissions `GroupMember.Read.All` and
  `User.Read.All`, workload identity, no secret: entra.md #4) and makes
  the state match:
  - a member of a bound group is a member of its scope while their account
    is enabled; a user not yet a principal is created, with the Entra ID
    and sign-in name Graph gives, ID their sign-in name lowercased (with
    part of the object ID if that is taken);
  - memberships from groups are kept apart from those granted in golem
    (`Principal.Synced` and `Principal.Scopes`); the sync writes only the
    former, and a member by both keeps the other when one ends;
  - a user who leaves every bound group of a scope loses that scope's
    synced membership and their keys in it; a disabled account loses all
    synced memberships and every key;
  - a user who is in no bound group any more is looked up: Entra drops a
    deleted user from its groups, so only the account tells a departure
    from a deletion. An account disabled, or in the deleted items, is
    **offboarded** like a disabled member: every synced membership and
    every key;
  - each change is an audit event by `system:entra-sync`.
- **Safe failure.** A step reads every group before it writes; any read
  failure changes nothing. A deleted group (404) has no members. A step
  that would end more than `-entra-sync-max-removals` memberships (50) of
  users still active is refused, the accidental-deletion guard directory
  sync tools keep (a reorg, a group re-bound, Graph answering empty); an
  operator raises it for one run when a large removal is intended. The
  guard never holds back **offboarding**: the memberships and keys of
  users Entra affirmatively reports disabled (`accountEnabled: false`) or
  deleted (in the deleted items) end, however many, and a refused step
  writes those alone. Only affirmative answers count: an account whose
  state Graph omits, or one in neither the users nor the deleted items (a
  404 from both), is a removal the guard counts, so a directory glitch
  cannot pass the guard as offboarding. (Amended: the guard first counted
  every removal and refused the step whole, so disabling 60 people at
  once revoked none of their keys until an operator raised the limit.)
- **Who binds an Entra ID.** Only the sync and admins. A manager adds a
  principal that exists, or creates a service that authenticates by key
  only; a membership from a group is removed in Entra or by unbinding the
  group, not by `members remove`.
- **A reserved namespace.** No principal ID may begin with `entra:`: the
  store refuses one, so no principal created now can be inherited by the
  person whose object ID it names.

## Consequences

- Joining or leaving a team is an Entra group change, seen within five
  minutes (change notifications in M2 would make it seconds).
- Users granted in golem without a group remain possible, by admins, for
  people outside the bound groups; their identity is the admin's
  statement, recorded in the audit.
- Unbinding a group takes effect at the next sync, which recomputes
  memberships from the groups still bound.
- A service's Entra binding (an app ID) is admin-only; services do not
  come from groups.

- A step looks up each user who left every bound group: one Graph call
  per departure (two for a deletion). A directory answering empty costs a
  call per synced user until it recovers; throttling then fails the step,
  which writes nothing.

## Alternatives

- **Bind at first sign-in.** A user's token proves their object ID, so the
  binding could be made when they first call the control API, with
  managers adding members by sign-in name. It leaves membership manual and
  departures to managers; the directory already knows both.
- **SCIM provisioning from Entra.** Entra pushes users and groups to a
  SCIM endpoint golem would host. Standard, but a public endpoint and its
  credential; pulling from Graph needs neither, at five minutes' lag.
- **Admin-only bindings.** Closes the hole, and makes every member an
  admin's ticket.
