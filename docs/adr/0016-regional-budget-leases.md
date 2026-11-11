# 0016: Budgets are enforced through regional leases

Status: Proposed, for a second region or a remote site. Deferred by [0025](0025-smaller-surfaces.md); brought forward by [0029](0029-one-s3-store-for-every-site.md) for a remote site whose round trip to the central Valkey is too long.

## Context

Gateways in several regions charge the same pools. The RESP store is
regional (Dragonfly, Valkey), and a cross-region round trip per request is
not acceptable. The store is too far and too slow to take a write per
request.

## Decision

- A pool's available amount for the period is divided into **leases**,
  one per region, held in the store by golem-control. Each region's RESP
  store enforces its lease as the pool level of ADR 0005.
- Gateways report lease consumption (through the control API's `Lease` method) every few seconds; the worker rebalances every 30 s,
  moving unused lease to regions that consume it, in store transactions
  that keep `Σ leases + spent ≤ available`.
- A region near exhaustion asks for more synchronously (one control API
  call, not per request). If the control plane is unreachable, the region
  can spend only what it holds: budgets fail closed at the lease, rate
  limits are unaffected.
- Single-region deployments have one lease equal to the pool.
- Member allocations and key caps are rarely shared across regions; they
  use the same mechanism, with a small default lease per region and the
  rest held centrally.

## Consequences

- No request-path cross-region traffic; overshoot is bounded by in-flight
  reservations within each region.
- A pool can be refused in one region while another region holds unused
  lease, for up to one rebalance interval.

## Alternatives

- One global RESP store: cross-region latency on every request.
- Per-region static shares of budgets: simple, wastes money when traffic
  moves between regions.
- Track only, enforce after the fact from ClickHouse: unbounded overshoot.
