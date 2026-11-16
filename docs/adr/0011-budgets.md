# 0011: Budgets: pools funded by grants, three levels of enforcement, modes

Status: Accepted (v1 baseline). **Applies from M2**, as narrowed by [0034](0034-budgets-for-m2.md). M1 has one budget per scope and period, with its mode, and no grants, pools or caps ([0031](0031-soft-budgets.md), [0050](0050-scope-policy-is-config.md)); site ceilings are gone (0013).

## Decision

- Each scope has a **pool** per period (calendar month, configured time
  zone). Money enters only by **grants**: cost center → team or project
  (by or with a budget manager's approval), team → sub-team or project (by
  the team's managers, no approval). Grants are recurring or one-time;
  **adjustment requests** change them and need a budget manager when the
  money comes from a cost center (states: pending, approved, rejected,
  expired, withdrawn).
- **Caps** limit members, services and keys within a pool; they do not
  move money and may oversubscribe; a default member cap covers members
  without one.
- **Enforcement** charges one pool per request, at three levels: the key's
  cap, the member's cap, the pool (at the request's site, its site
  ceiling: ADR 0013). Parents are not charged for children's spend: the
  grant moved the money.
- **Modes** per scope: `observe` (record what would be refused), `warn`
  (allow, warn, notify), `enforce`. Rollouts start in `observe`.
- **Attribution**: each ledger row carries principal, key, scope and the
  scope's funding sources; a project's spend is apportioned to its funders
  pro rata by their grants.

## Consequences

Constant enforcement cost whatever the organisation's depth; a team cannot
overspend through its projects.

## Alternatives

Charging every ancestor (unbounded counters, coupled limits); user-only
budgets (no team control).
