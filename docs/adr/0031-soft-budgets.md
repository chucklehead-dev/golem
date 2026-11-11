# 0031: Budgets are soft and eventually consistent; the overshoot is bounded and stated

Status: Accepted. Supersedes the accounting parts of 0012 (money. Amended by [0038](0038-accounting-refinements.md) (dedicated accounts, monthly reconciliation, slot renewal, worst-case ceilings).
reservations, renewal, uncertain liability), 0013 (fixed site allocations),
0029 §9 (durable per-attempt accounting as an M1 gate; failing closed when
the journal is unavailable; site allocations) and 0030 (the spool spike).
Amends 0025 (ledger row kinds).

## Context

A call's true cost is known only when it completes. The design had grown a
durable journal written before dispatch, money reservations renewed on long
streams, uncertain-liability recovery and fixed per-site allocations, all to
make budgets approach a hard ceiling. The accounting model
(`model/accounting.md`, first version) showed what that takes. The enterprise
does not need hard ceilings: it needs budgets that stop spending promptly and
books that match invoices.

## Decision

**Accepted risk** (to be signed off by the platform team, finance and
security):

- Spend is known after the fact and is eventually consistent: on one site
  within seconds; across sites within the snapshot renewal interval (about a
  minute) plus ledger ingestion.
- A principal near its budget can exceed it with the calls already in flight.
- Once known spend reaches a budget, every further call against that key,
  member or scope is refused (402, `budget.*`), on every site, within the
  same lag.
- A gateway crash can lose the spend events it had not yet exported; daily
  provider reconciliation books the difference as an **unattributed
  adjustment**, so totals match invoices even where attribution of those few
  calls is lost.

**Mechanism:**

- **Known spend** for a key, member or scope in a period is the ledger's
  balance (deduplicated by event ID), published in every snapshot renewal,
  plus the site's own counter in Valkey since that snapshot. The gateway
  adds a call's actual cost to the site counter when the call ends.
- **Soft check** (default): admit if known spend is below the budget.
- **Strict check** (a `ScopePolicy` option): admit only if known spend plus
  the request's **worst-case cost** fits: input tokens estimated from the body
  at the full input price (no cache discount), plus `max_tokens` (or the
  model's maximum output) at the output price; server-side tools, which have
  no upper bound, are refused in strict scopes.
- **Concurrency limits** per principal (Valkey slots with a short TTL) are
  the overshoot control. With an in-flight limit `L` and worst-case cost `W`,
  a principal's overshoot on one site is at most `L × W` under the soft check
  and `(L − 1) × W` under the strict check, plus spend lost by a crash or a
  Valkey failover until it is reconciled, plus, across sites, what the
  principal spends during one refresh lag. Strict with `L = 1` is a hard
  ceiling on one site, apart from crash losses.
- **No** journal, intent-before-dispatch, money reservations, reservation
  renewal, uncertain-liability tracking, recovery watermarks or site
  allocations. Spend events go out through the normal access-log path
  (ADR 0015) and are deduplicated by event ID in ClickHouse.
- **Ledger rows**: one `attempt` row per upstream attempt (flagged
  `estimated` when a stream was cut and its usage counted from deltas), and
  `adjustment` rows from reconciliation, attributed where the provider data
  allows, else to an `unattributed` principal under the provider account.

## Consequences

- Gateways stay stateless Deployments; Phase 0 loses the spool spike; M1
  loses the durable-accounting gate; remote sites need no allocations.
- Budget owners are told, in the console and the docs, what a budget means:
  "spending stops within about a minute of reaching the limit; in-flight
  calls may exceed it by up to the stated bound".
- The bound is checked by `model/accounting.md` (second version).

## Alternatives

- Hard ceilings through a durable journal and reservations (the superseded
  design): far more machinery for a guarantee the enterprise does not need.
- Worst-case reservations (a hard ceiling on one site without `L = 1`):
  kept in reserve if finance later requires hard ceilings for some scopes.
