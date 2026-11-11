# 0024: Teams and projects are one entity; four roles

Status: Proposed. Amends 0008 and org-model.md. Amended by [0026](0026-design-envelope.md): team scopes may have a parent team (sub-teams, depth two).

## Context

Teams and projects have the same members, managers, pools, caps, keys,
capture settings and policies. They differ in two ways: a team may be bound
to Entra groups, and a project has an owning team and may include other
teams. Modelling them separately doubles the API, the service code and the
tests. Likewise the roles list (member, team manager, project manager,
budget manager, service owner, admin) has three that differ only in what
they are held on.

## Decision

- One entity, **Scope**, with `kind: team | project`. A project has an
  `owner` scope (its primary team); any scope's members may be users,
  services or (for projects) teams. Entra group binding is a property of a
  scope (normally teams).
- Four roles: **member** and **manager** (held on a scope), **budget
  manager** (held on a cost center), **admin** (global). A project's
  owning team's managers are its managers unless set otherwise. A service's
  keys are managed by its scope's managers; a "service owner" is a manager
  binding narrowed to one service, not a role of its own.
- Caps unify: an **allocation** and a key's spend cap are both a `Cap` on a
  subject (member in a scope, or key) within a pool; the default member cap
  is a `Cap` with no subject.

## Consequences

- One `Scope` resource, one `Membership`, one `RoleBinding`, one `Cap` in
  the API; the org service is about half the code.
- Rules that differ by kind (only projects may contain teams; only a team
  may own a project) are validations on `kind`, tested once.

## Alternatives

- Keep two entities: clearer names in the API, twice the surface.
