# 0052: Accounting for M1: snapshot known spend, scope budgets only

Status: Accepted. Supersedes [0048](0048-known-spend-by-time-boundary.md)'s
site counter (its minutes, replays and recovery), and the parts of
[0031](0031-soft-budgets.md), [0038](0038-accounting-refinements.md) and
[0044](0044-budgets-are-controls-not-ceilings.md) listed below. Narrows
[0011](0011-budgets.md) and [0012](0012-limits-and-accounting.md) for M1
(member and key caps, provider quota shares).

## Context

ADR 0044 made budgets admission and monitoring controls: the provider
account's limit is the financial boundary, the monthly reconciliation the
record. The accounting that remained still carried machinery sized for
ceilings: a per-minute spend counter in each site's Valkey, held increments
replayed after an outage, a grace while peers replayed, member and key
caps as accounts beside the scope's, provider quota shares as request
buckets, and a fail-closed declared-worst-case mode. One team's M1 rollout
needs none of it to be safe enough, and each part was a source of defects
the reviews kept finding.

## Decision

1. **No member or key caps.** A call is charged to its scope's budget
   alone. `ScopePolicy.defaultMemberCapMicros` and a `LocalIdentity` key's
   `capMicros` go (v1alpha1, unreleased: no conversion); the gateway reads
   no member or key spend from the snapshot. The refusal codes
   `budget.member_cap` and `budget.key_cap` stay in the contract, unused.
2. **No site spend counter.** Known spend is the snapshot's per-scope
   `knownSpend`, published about every minute with ADR 0048's ingestion
   allowance. Valkey keeps the concurrency slots (admission takes them
   and checks the snapshot's known spend against the budget in one
   script; renewal, re-acquisition after an outage, a cancelled call's slot
   held for the provider's tail) and the requests-a-minute rate buckets.
   Gone: the minute counters and their Lua, held increments and their
   replay, `-valkey-replica-acks` (WAIT), and the `golem.limit.held` and
   `golem.limit.dropped` metrics. During an outage a gateway admits on its
   own in-flight counts and the snapshot's known spend, as waivers. The
   grace after a recovery stays, now only for slots: a peer that has not
   recovered yet runs calls Valkey has no slot for.
3. **No provider quota shares.** A Deployment's `quota` and the
   `limit.provider_quota` refusal's use go; a deployment's in-flight
   ceiling (`maxInFlight`) and the provider's own 429s remain, and
   member and scope rate limits stay.
4. **No declared-worst-case check.** `ScopePolicy.declaredWorstCaseCheck`
   goes, with its refusals (no `max_tokens`, server-side tools, a worst
   case over the budget). `price.Worst` stays: an uncertain call is still
   charged its declared worst case.

## What is lost

- **Spend within a snapshot's lag is not seen at admission.** Calls that
  ended since the snapshot's boundary (the allowance, the publication
  interval, the gateway's application) are not in known spend: a scope
  can pass its budget by that much more. `model/exposure.md`'s bound gains
  a `lag` term for it; the in-flight limits do not bound it, the scope's
  rate limit does (requests a minute × the lag's minutes × the worst
  case). Before, this site's own spend was seen at once.
- **Standalone mode counts no spend.** With no golem-control, nothing
  publishes known spend, so a standalone gateway's budgets never refuse
  or warn; its in-flight and rate limits still apply.
- **No cap below the scope**: one member or one leaked key can spend the
  whole scope's budget, within the rate and in-flight limits.
- **No fail-closed admission mode**: calls without `max_tokens`, or with
  server-side tools, are admitted under every budget mode (a scope's
  `maxOutputTokens` still requires `max_tokens`).
- **No per-site quota split**: a deployment shared by sites can be pushed
  to its provider limit by one; the provider's 429s fail the attempt over.

## Supersedes

- ADR 0031: the overshoot bound's known spend now lags by a snapshot
  (the bound in `model/exposure.md` §6.2 replaces it); the strict mode
  is gone, not renamed.
- ADR 0038: "the worst-case check's declared cost uses them" (the
  `maxOutputTokens` and `maxInputBytes` ceilings stay as ceilings).
- ADR 0044: the `declaredWorstCaseCheck` section and its consequences;
  "refusal once known spend reaches a budget" now has the snapshot's lag
  and no Valkey failover term.
- ADR 0048: the counter, its minutes and retention, replays, recovery's
  convergence and `ReplicaAcks`; the snapshot's boundary and the
  known-spend job stay (golem-control's).

## Alternatives

- **Keep the counter, drop the caps.** Known spend current at the site,
  for the counter's Lua, retention, replay and its defects.
- **Keep the caps from the snapshot alone.** A member's spend within the
  lag is not seen, as `model/exposure.md`'s `memberSnapshotOnly` showed:
  a cap that does not cap.
