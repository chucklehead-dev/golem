# 0031: Budgets are soft admission controls on the snapshot's known spend

Status: Accepted, consolidated 2026-11-16. This record is the accounting
decision as it stands for M1. It absorbs, and replaces:
[0012](0012-limits-and-accounting.md)'s accounting (reserve, renew,
reconcile), [0013](0013-site-ceilings.md) (site allocations),
[0038](0038-accounting-refinements.md) (dedicated accounts, monthly
reconciliation, renewed slots, request ceilings),
[0042](0042-known-spend-by-counter-watermark.md) (the counter watermark),
[0044](0044-budgets-are-controls-not-ceilings.md) (controls, not ceilings),
[0048](0048-known-spend-by-time-boundary.md) (the time boundary and minute
counters) and [0052](0052-accounting-for-m1.md) (snapshot known spend,
scope budgets only); each of those is now a stub pointing here, and their
full text is in git history. Budget modes are
[0050](0050-scope-policy-is-config.md)'s. Grants, team pools and member
caps are M2's ([0011](0011-budgets.md), [0034](0034-budgets-for-m2.md)).

## Context

A call's true cost is known only when it ends, and a provider can keep
working (and billing) after golem stops seeing a call. The design first
sought hard ceilings (a durable journal before dispatch, money
reservations renewed on long streams, fixed per-site allocations), then a
soft budget with a stated overshoot bound, then exact known spend through a
site counter. Each step showed that the bound rested on an assumption the
real world does not grant: that no call costs more than the worst case golem
computes from the request (token-dense output, server-side tools, files and
images, a provider's tail after a cut, prices behind the provider's). The
enterprise's financial boundary already exists: the provider account's
spending limit. Golem's job is to keep expected exposure small and to know,
after the fact, who spent what. One team's M1 rollout needs no more
machinery than that, and every extra part (counters, replays, caps, quota
shares, a fail-closed mode) was where the reviews kept finding defects.

## Decision

**What a budget is.** A scope's budget for a period is an admission and
monitoring control, not a spending ceiling. Provider-native limits are the
financial boundary; the provider's bill, reconciled, is the record.

**Provider accounts.** golem routes to dedicated provider accounts or
projects (OpenAI projects, Anthropic workspaces, AWS accounts or tagged
Bedrock roles, Azure resources): usage data is per account, so this is
reconciliation's precondition. Every one has a spending limit or budget
alarm set at the provider, sized to what finance accepts losing: a
deployment requirement (operations.md), not a check golem makes.

**What golem enforces**, and claims nothing beyond:

- authentication and authorization (ADR 0009, 0026, 0041);
- in-flight slots per principal (counted per scope) and per scope,
  renewed every 30 s while a call runs (TTL 90 s); a cancelled or cut call
  keeps its slot for the provider's tail (2 minutes, at most to the call's
  deadline);
- rate limits in requests a minute, per member and per scope, taken once
  a call before admission, failing open when the site store is down;
- a deployment's in-flight ceilings (`maxInFlight`,
  `maxInFlightPerPrincipal`); the provider's own 429s fail the attempt over
  (ADR 0051);
- request size (the body limit, a scope's `maxInputBytes`) and declared
  output (`maxOutputTokens` over `max_tokens` × `n`, a missing
  `max_tokens` refused under one);
- request features: provider storage and stateful references, restricted
  mode (ADR 0027, 0037, 0041 §3); server-side tools are admitted;
- call lifetime: first-byte, idle and maximum durations;
- refusal once known spend reaches an `enforce` budget (`budget.pool_exhausted`,
  402), within the snapshot's lag.

**Known spend is the snapshot's.** golem-control's known-spend job
(control/spend) publishes, in every snapshot (about every minute), each
scope's spend this period: the ledger's spend (`golem.spend_dedup`,
deduplicated by `event_id`) of the calls that ended before a boundary, a
whole minute an ingestion allowance (default 5 minutes) before the
snapshot's time. The gateway checks it against the budget at admission; it
keeps no spend counter of its own. A call belongs to the period it started
in (`lib/period`, one installation-wide period zone that every snapshot
states and every gateway checks); a snapshot of another period counts as
none. A failed job keeps its last result for its own period and reports it
stale; a site whose rows arrive later than the allowance is reported
behind (`Job.Lags`), the boundary unmoved.

**Admission is one store call.** The site's Valkey (ElastiCache, one
primary and a replica; in-memory for development and standalone) holds the
slots and the rate buckets. One script checks known spend against the
budget and takes the principal's and the scope's slots, atomically, so two
gateways cannot both pass before either takes a slot. Each store call is
bounded (250 ms); a caller that leaves meanwhile is no outage, and its slot
is freed when the call returns. Admission is a fixed stage after routing,
not a hook (gateway/admission).

**Outages.** With Valkey unreachable a gateway admits on its own in-flight
counts and the snapshot's known spend, each admission recorded as a
waiver; calls already running continue (no scope cancels calls on a failed
renewal). On recovery it re-registers its running calls' slots and, for a
grace of two renewal intervals, records its admissions as waivers while
peers recover. A gateway starts, and is ready, with Valkey down.

**Estimates err high, and are only estimates.** A call whose usage is
missing or ambiguous is charged by rules chosen to over-count: every byte
received a token (at most `max_tokens` × `n`), every request byte a token,
a call sent and unanswered its declared worst case (`price.Worst`); the
ledger row records the estimated share. They can still be low (a provider's
tail, a modality bytes do not bound, a unit golem does not price).

**Reconciliation is authoritative and, in M1, a query.** Monthly, by
provider account and model, against provider usage and cost data (AWS CUR,
a day or two late; the OpenAI usage API; Anthropic's and Azure's exports),
within a 3–5% tolerance, with daily previews; differences beyond it are
investigated. In M1 it is a SQL query over `golem.spend_dedup` and a
written record (docs/acceptance/m1-exit.md); no job writes `adjustment`
rows yet (the row kind stays in the contract). Chargeback uses the
provider's cost split by golem's attribution; golem's prices need only be
good enough for admission.

**Budget modes** (ADR 0050): `observe` (the default: admitted, the row
marked `over_budget`), `warn` (as observe, with `golem-warning`),
`enforce` (refused). In-flight and rate limits refuse in every mode.

## What is stated, not prevented

- **Spend within the snapshot's lag** (the allowance, the publication
  interval, the gateway's poll) is not seen at admission: a scope can pass
  an `enforce` budget by what it spends in that time, bounded by its rate
  limit (requests a minute × the lag's minutes × the worst case), not by
  its in-flight limits. `model/exposure.md` §6.2 states the bound with
  its `lag` term.
- **Calls in flight** when known spend reaches the budget finish.
- **A row later than the allowance** is in no snapshot until a later one
  includes it.
- **No cap below the scope**: one member or one leaked key can spend the
  scope's budget, within the rate and in-flight limits.
- **No fail-closed mode**: calls without `max_tokens`, or with server-side
  tools, are admitted under every mode (a scope's `maxOutputTokens` still
  requires `max_tokens`).
- **No per-site quota split**: a deployment shared by sites can be pushed
  to its provider limit by one; its 429s fail attempts over.
- **Standalone counts no spend**: with no golem-control nothing publishes
  known spend, so its budgets never refuse; in-flight and rate limits apply.
- **A crash** loses the rows a gateway had not exported (ADR 0043);
  reconciliation finds the difference.

Budget owners are told: "spending stops within a few minutes of reaching
the limit; calls in flight may go past it; the provider's bill, reconciled
monthly, is the record". A review finding of the form "an estimate can be
low when …" is judged by its expected cost against reconciliation and the
provider's limit, not as a breach of a ceiling golem does not claim.

## Removed on the way (history in git)

- Durable journal, intent before dispatch, money reservations and their
  renewal, uncertain-liability tracking (the first design).
- Fixed per-site allocations (0013).
- A strict mode, later `declaredWorstCaseCheck`, refusing calls whose
  declared worst case passed the budget (0044, removed by 0052).
- The site spend counter: by event with a ledger watermark (0042), then by
  minute from a time boundary with held increments replayed after an
  outage (0048); its retention, `WAIT` and gap reconstruction (0043).
- Member and key caps, and provider quota shares (0052). The refusal codes
  `budget.member_cap`, `budget.key_cap` and `limit.provider_quota` stay in
  the contract, unused.

## Alternatives

- **Hard ceilings through a journal and reservations**: the same
  assumption about the worst case, at far greater cost. Kept in reserve
  only if finance requires hard ceilings for some scope.
- **A site counter for known spend current at the site**: the counter's
  Lua, retention and replays, and their defects, for spend within a
  snapshot's lag.
- **Caps from the snapshot alone**: a member's spend within the lag is not
  seen (an exposure.md design shown broken before the caps were
  removed): a cap that does not cap.
