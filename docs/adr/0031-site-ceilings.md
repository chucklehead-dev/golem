# 0031: Budgets across sites as monotonic site ceilings, published in snapshots

Status: Accepted. Supersedes the mechanism of [0016](0016-regional-budget-leases.md).

## Context

ADR 0016 has gateways report lease consumption to the control plane and
ask it synchronously for more. Since ADR 0028 gateways never call the
control plane, so 0016 as written contradicts it. It also has two modes
(direct enforcement in one site, leases with several), and lease
arithmetic that depends on consumption reports arriving on time.

## Decision

- Every site, EKS included, enforces each pool against a **site ceiling**:
  the most that site may spend from the pool this period. Ceilings are
  published in the site's snapshots, so they travel the one channel that
  already exists.
- A site's Valkey counts that site's spend per pool per period
  (cumulative, reset at the period boundary). A reservation succeeds only
  while `site_spent + reserved ≤ site_ceiling`.
- golem-control sets ceilings so that, for every pool,
  `Σ site ceilings ≤ pool total`, and a ceiling never drops below what its
  site has spent. Ceilings only rise during a period, except at a grant
  reduction (when the decrease is taken from unspent headroom first).
- **Consumption comes from the ledger.** Every 30 s golem-control reads each
  site's spend per pool from ClickHouse (all sites' telemetry is there
  already) and moves unspent headroom towards the sites using it: raise a
  busy site's ceiling out of the pool's unallocated remainder, and lower an
  idle site's ceiling towards its spend plus a margin. Ledger lag (seconds)
  affects only how tight the rebalance is, never the invariant: a ceiling
  is an allowance, not a measurement.
- One site: its ceiling is the pool total. The mechanism is the same.
- Member caps and key caps are enforced at the site where the request
  runs, against the site's share (a cap is split the same way, with a small
  default share at each site and the rest at the site where the member is
  usually active, by the same rebalance).
- If a site cannot reach the bucket, it keeps its last ceilings: it can
  spend what it was allowed, no more (budgets fail closed at the ceiling;
  rate limits are unaffected).

## Consequences

- No lease protocol, no consumption reports, no synchronous top-up, no
  second mode for one site. Gateways stay pull-only.
- The invariant (`Σ ceilings ≤ total`, ceilings ≥ spent) is checked in one
  place, in the control plane's write, and is small enough to model in
  Quint with its mutants.
- A burst at one site can be refused for up to one rebalance interval
  while another site holds headroom; the margin tunes this.

## Alternatives

- 0016's leases with consumption reports: tighter, but a second channel
  from gateways to the control plane.
- Static per-site shares: simplest, wastes budget when traffic moves.
