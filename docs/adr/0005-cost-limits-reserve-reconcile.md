# 0005: Cost limits reserve an estimate and reconcile with usage

Status: Proposed

## Context

A request's cost is known only when it ends (output tokens, cache reads,
reasoning tokens). Limits checked only after the fact let concurrent
requests overshoot without bound; limits checked on input alone ignore
output, which dominates cost for agents.

## Decision

- Each limit is a window (fixed, sliding, or token bucket via GCRA) over a
  quantity: requests, tokens, or USD micro-units (int64).
- Before the upstream call, the limiter reserves the estimate against
  every applicable limit atomically; if any would exceed, nothing is
  reserved and the caller gets a 429 naming the limit.
- At the end, the reservation is replaced by actual cost (positive or
  negative correction). Reservations carry a TTL so a crashed instance's
  reservations expire.
- Budget levels (org model §4.2): key cap, member allocation, scope pool.
  One reserve checks every applicable level and rate limit in one script
  call and reserves at all of them or none.
- The store speaks RESP: Dragonfly, Valkey or Redis. Scripts declare every
  key they touch in `KEYS` (Dragonfly requires it by default) and keep a
  scope's keys under one hash tag (`{scope:<id>}`) so they also work on
  Redis Cluster. Tested against Valkey and Dragonfly in CI.
- Without a store (M1 option): in-memory per instance, each replica
  enforcing a configured share. With one, on store failure: rate limits
  fall back to local shares; budget levels fail open or closed per scope
  policy.
- Budgets are monthly limits with soft thresholds. The ClickHouse ledger
  (ADR 0010), not the store, is the record of spend: counters are rebuilt
  from it after store loss and corrected against it daily.

## Consequences

- A hard budget can be exceeded by at most the in-flight requests' error
  in estimate; bounded by concurrency limits.
- Estimates need `max_tokens` or a per-model default; requests without
  `max_tokens` reserve the default. Decision models bill input only, so
  their estimate is exact.
- A request touches three or four counters plus rate buckets in one round
  trip; the store is on the latency path (target < 1 ms in-cluster).

## Alternatives

- Envoy rate limit service (agentgateway's remote mode): standard, but
  counts descriptors, not money, and has no reconcile step.
- Post-hoc only (charge after completion): simple, unbounded overshoot.
