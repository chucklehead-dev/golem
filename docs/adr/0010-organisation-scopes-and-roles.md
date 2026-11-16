# 0010: Organisation: scopes, four roles, rights by intersection

Status: Accepted (v1 baseline).

## Decision

- **Scope**, one entity with `kind: team | project`. A team may have a
  parent team (sub-teams; depth two) and Entra group bindings. A project is
  owned by one team and may include teams, services and users. Services
  are owned by a scope. Cost centers are external references.
- **Four roles**: member and manager (on a scope; a manager binding may be
  narrowed to one service), budget manager (on a cost center), admin.
  A project's owning team's managers, and a sub-team's parent's, manage it
  unless set otherwise. Approving one's own request is refused.
- **Rights** are computed per request, never copied: scope policy ∩
  membership ∩ key constraints. A key can never be wider than its owner.
- The full model, with examples: org-model.md.

## Consequences

One `Scope`, `Membership`, `RoleBinding` and `Cap` in the API; rules that
differ by kind are validations on `kind`.

## Alternatives

Separate team and project entities (twice the surface for the same
behaviour).
