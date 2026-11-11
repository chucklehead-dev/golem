# 0013: Budgets across sites as monotonic site ceilings

Status: Accepted (v1 baseline). From archive 0016, 0031.

## Decision

- Every site, EKS included, enforces each pool against a **site ceiling**
  published in its snapshots; its Valkey counts the site's spend per pool
  per period.
- golem-control keeps `Σ site ceilings ≤ pool total` and never lowers a
  ceiling below its site's spend. Every 30 s it reads each site's spend
  from the ClickHouse ledger and moves headroom towards the sites using it.
  Ledger lag affects how tight the rebalance is, not the invariant.
- One site: its ceiling is the pool. Caps are split the same way (a small
  default share per site, the rest where the member is usually active).
- A site cut off from the bucket spends at most its last ceilings.

## Consequences

No lease protocol and no reports from gateways; one invariant, checked in
one place and modelled in Quint with its mutants (testing.md).

## Alternatives

Leases with consumption reports (a second channel from gateways); a
central Valkey for all sites (a cross-site round trip per request); static
shares (wasted budget).
