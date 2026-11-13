# 0012: Limits and accounting: reserve, renew, reconcile; Valkey at every site

Amended where specified by [ADR 0029](0029-security-and-correctness-revision.md)
and its normative security and correctness requirements. Baseline text below
must be read with those amendments.

Status: Accepted (v1 baseline). Accounting parts superseded by [ADR 0031](0031-soft-budgets.md): no money reservations or renewal; known spend, concurrency limits and an optional worst-case check. Amended by [0038](0038-accounting-refinements.md) and [0044](0044-budgets-are-controls-not-ceilings.md) (budgets are controls, not ceilings).

## Decision

- **Valkey** at every site (ElastiCache for Valkey on AWS; one primary, one
  replica; no cluster mode); the only supported engine. Development uses an
  in-memory limiter.
- **Reserve, then reconcile**: before the upstream call, one script call
  reserves the expected cost at every applicable level (key cap, member
  cap, site ceiling of the pool, the deployment's provider quota share,
  rate and concurrency limits), all or nothing; a refusal in the caller's
  dialect names the level (429 for rate and concurrency, 402 for budget
  levels: ADR 0024). At the end the reservation becomes spend.
- **Expected, not worst case**: input priced at the principal's recent
  cache-hit ratio, output at the smaller of `max_tokens` and the principal's
  recent p95 for the model; a scope may choose worst case. Decision models
  bill input only, so theirs is exact.
- **Long streams** renew their reservation every minute (TTL 5 minutes).
- **Cut streams** are charged a conservative estimate of what was produced
  (every byte received a token, at most the request's output ceiling;
  golem#3; an estimate, not a guarantee: ADR 0044) and
  flagged `usage_estimated`. A client disconnect closes the
  upstream at once, except for requests a scope marks
  `complete_on_disconnect`.
- **Provider quotas** (TPM/RPM per provider account) are split statically
  per site in the `Backend` CRD and shared within a site in its Valkey.
- **Reconciliation**: counters are rebuilt from the ledger after a Valkey
  loss, using data older than the collector's maximum delay; a daily job
  compares the ledger with providers' usage APIs and records adjustments
  (never edits).
- On Valkey outage: budget levels fail open or closed per scope policy;
  rate limits fail open with a warning.

## Consequences

Heavy users are not refused by worst-case arithmetic; overshoot is bounded
by estimation error until reconciliation. Every request touches one Valkey
round trip to reserve and one to reconcile.

## Alternatives

Charging after the fact (unbounded overshoot); a remote rate-limit service
(counts descriptors, not money); worst-case reservations (false refusals).
