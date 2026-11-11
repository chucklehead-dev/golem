# 0008: Scopes, delegated roles and three-level budgets

Status: Proposed. Amended by [0024](0024-scopes-and-roles.md): teams and projects are one Scope entity; four roles; allocations are caps.

## Context

Users must mint their own keys within their own rights; keys can be
narrowed further; services belong to teams or projects; team and project
managers allocate to members; projects are funded by teams or by external
cost centers with a budget manager's approval; budget managers approve
permanent and one-time adjustments. The model must be enforceable per
request in well under a millisecond.

## Decision

The model in [org-model.md](../org-model.md):

- Every key, and every request, is bound to exactly one **scope** (a team
  or a project). Rights and spend both come from the scope. A user in two
  projects has two keys (or picks the scope per request with an Entra
  token).
- Rights are an intersection computed at request time (scope policy ∩
  membership ∩ key constraints), never copied into keys.
- Money moves between pools only by **grants** (cost center → team or
  project; team → project), recurring or one-time, per month. Grants from
  cost centers need a budget manager. Member **allocations** are caps
  within a pool, not transfers, and may oversubscribe.
- Enforcement has three levels: key cap, member allocation, scope pool.
  Team pools are not charged for project spend (the grant already moved
  the money), so a request never touches more than one pool.
- Attribution of a project's spend to its funding sources is done in the
  ledger (pro rata by grants), not at enforcement time.

## Consequences

- Enforcement cost is constant (three counters) however deep the
  organisation is.
- A team cannot overspend through its projects: grants out reduce its
  available money at grant time.
- A user who wants one key for everything cannot have it; scopes are what
  make the spend attributable. `golemctl login` makes picking a scope cheap.

## Alternatives

- Hierarchical enforcement up a tree (key → user → project → team → cost
  center), charging every ancestor: matches intuition, but a request
  touches an unbounded number of counters and a team's limit couples
  unrelated projects.
- Budgets on users only, aggregated for reporting: no control at team or
  project level, which is what managers asked for.
