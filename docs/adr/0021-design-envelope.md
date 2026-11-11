# 0021: Designed for the enterprise's size, with the envelope stated

Status: Accepted (v1 baseline). From archive 0026.

## Decision

| Quantity | Estimate | Designed for |
|---|---|---|
| Users | < 2,000 (under 100 heavy, under 10 very heavy) | 20,000 |
| Teams | ≤ 10 top-level, ≤ 100 sub-teams | 1,000 |
| Projects | ≤ 1,000 | 10,000 |
| Keys | ~ 5,000 | 50,000 |
| Concurrent streams, peak | ~ 1,000 | 10,000 |
| Requests per second, peak | ~ 50 | 500 |
| Ledger rows per day | 1–2 million | 20 million |
| State document and snapshots | < 2 MB | 20 MB |

Consequences adopted throughout the baseline: whole snapshots, fixed
replicas (three gateways per site across zones, two golem-control replicas
with one leader), one Valkey primary and replica per site, per-principal
limits on scarce local GPU pools, and metadata-only capture by default for
coding agents. The heavy tail, not the average, drives cost and local GPU
demand. Dashboards alert as traffic approaches the envelope.

## Alternatives

Designing for a general product: correct at any size, paid for in every
milestone.
