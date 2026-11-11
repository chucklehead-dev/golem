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
- M1: in-memory per instance; each replica's share configurable
  (limit / replicas, or a static per-pod value). M2: Valkey/Redis with one
  Lua script per reserve and per reconcile; on store failure, fall back to
  local shares, with budgets configurable fail-open or fail-closed.
- Budgets (monthly) are limits with a long window plus soft thresholds.
  The ledger, not the limiter, is the record of spend.

## Consequences

- A hard budget can be exceeded by at most the in-flight requests' error
  in estimate; bounded by concurrency limits.
- Estimates need `max_tokens` or a per-model default; requests without
  `max_tokens` reserve the default.

## Alternatives

- Envoy rate limit service (agentgateway's remote mode): standard, but
  counts descriptors, not money, and has no reconcile step.
- Post-hoc only (charge after completion): simple, unbounded overshoot.
