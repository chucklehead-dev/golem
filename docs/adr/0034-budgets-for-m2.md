# 0034: M2 budgets are caps and a cost-center mapping; grants and approvals wait for demand

Status: Accepted. **Applies from M2.** Amends [0011](0011-budgets.md) and org-model.md §4 for M2. M1's budgets are [0031](0031-soft-budgets.md)'s: a scope budget, no caps.

## Decision

- M2 ships: a monthly cap per team, sub-team and project, set by admins or
  by a parent's managers within the parent's cap; per-member and per-key
  caps; threshold alerts in Grafana and notifications; enforcement modes;
  and a mapping from scopes to cost centers, applied in ClickHouse for
  chargeback reports.
- Recurring and one-time grants from cost centers, adjustment requests and
  budget-manager approvals (org-model §4.1, §4.3) are built when a budget
  manager asks for self-service; until then adjustments are a request to
  the platform team, applied as an audited cap change.

## Consequences

The approval workflow, its notifications and its model are not on the M2
critical path. The org model keeps the full design for when it is wanted.
