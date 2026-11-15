# 0048: Known spend by a time boundary and minute counters

Status: Accepted. Supersedes [0042](0042-known-spend-by-counter-watermark.md),
and the numbering parts of [0043](0043-ledger-durability.md) (the
numbering record, gap reconstruction). Amends
[0044](0044-budgets-are-controls-not-ceilings.md)'s list of what golem
enforces.

## Context

ADR 0042 made known spend exact under snapshot skew: the site counter
numbered every event, the ledger carried each number in a separate
record (ADR 0043), golem-control computed a per-site contiguous watermark,
and a gap stalled the watermark until it was reconstructed or declared
lost. It is exact metering, and it is most of the accounting code: Lua
scripts keeping a sequence, a running total, the total as of each number
and the event of each number; idempotent adds; replays that number; a
watermark job (never wired into golem-control); a gap timeout; an
over-counting flag for a trimmed watermark; the numbering record and its
table.

ADR 0044 then made budgets admission and monitoring controls, not
ceilings: provider-account limits are the financial boundary and the
monthly reconciliation the record. Exactness at the minute is no longer
worth its machinery. Its complexity was also where the defects were: the
Go review found a recovery that never converged under load (a busy
gateway stayed in outage mode, its budgets unenforced, long after Valkey
was back), and a completion racing recovery whose increment was held where
no replay would find it.

## Decision

- **The counter keeps minutes, not events.** Per scope and budget period,
  one hash of minute to spend in the site's Valkey. A completion adds its
  cost to the minute it ended in (`HINCRBY`), in the script that frees its
  slot. Minutes are kept for a retention (six hours: the longest lease a
  live gateway holds, four, plus the ingestion allowance and margin) and
  deleted as admissions pass them.
- **A snapshot carries a time boundary.** For each scope and period,
  golem-control publishes a boundary B, a minute an ingestion allowance
  (the collector's queue and ClickHouse's insert lag: minutes) before the
  snapshot's time, and the ledger's spend of the calls that ended before
  B, from `golem.spend_dedup` (deduplicated by `event_id`). It replaces
  the per-site sequence watermark; the snapshot no longer differs by site.
- **Known spend = the snapshot's spend + the counter's minutes from B on.**
  Read in the admission script that takes the slots: one round trip, one
  `HGET` per minute from B to now (a few, on a current snapshot; at most a
  retention's, on the oldest). The counter also keeps the period's whole
  total, which a snapshot with no boundary (the first) reads instead, in
  one field. A call's ledger row and its counter
  increment carry the same end (the increment's minute is stamped after
  the row's, never before), so a call is on one side of B: in the
  snapshot, or in the minutes counted. A gateway on an older snapshot uses
  its own, earlier B, and is as exact. A snapshot of another period counts
  as none, as before.
- **Replays are plain adds.** Increments held during an outage are added
  to their own minutes, in batches, each batch one script (whole or not at
  all). An add is an amount, not an event: a batch whose reply was lost and
  is sent again is counted twice, which errs high.
- **Recovery converges.** A recovering gateway replays until little is
  left, re-registers its slots, marks itself up, and drains the rest; from
  that mark, completions go to the store themselves (the hold refuses an
  increment once the gateway is up, under the same lock), so the rest can
  only shrink and no increment is held after the last replay.
- **The ledger row drops its number** (`counter_seq`, `counted` and the
  `golem.spend_seq` table go); `site` stays. The row's `ended_at` is what
  B compares.

## What this does not cover, stated

Under ADR 0044 these are accepted, monitored and reconciled, not
prevented:

- **A row later than the allowance** is in neither the snapshot (not yet
  ingested) nor the minutes counted (before B): an undercount of that
  row's cost until a later snapshot's boundary passes it, or
  reconciliation. The allowance is a deployment parameter, sized above
  the collector's observed delivery lag, which is measured: the
  known-spend job reads each site's latest ingested `ended_at`
  (`golem.ingest_by_site`) and reports a site whose lag exceeds the
  allowance, or that has no rows any more, as behind (`spend.Job.Lags`),
  for an alert. B is not moved for it. The ledger cannot tell a quiet
  site from a late one, so the alert reads the site's traffic beside it.
- **A Valkey failover** that loses recent minutes undercounts them until
  the boundary passes them (the ledger has their rows): at most the
  allowance plus a minute's spend at the site. `ReplicaAcks` (`WAIT`)
  narrows it.
- **A boundary older than the retention.** The lease does not prevent
  one: golem-control keeps its last known-spend result through a
  ClickHouse outage and stamps its boundary into every renewed snapshot,
  so after about two hours a fresh snapshot can carry a B whose minutes
  the gateways no longer retain, and the spend between B and the
  retention's start was counted nowhere, silently. The guard: a
  gateway's known spend is never below its counter's period total, so
  even a stale snapshot cannot undercount this site's own calls, and its
  spend still adds other sites' calls before B (more than no known spend
  would give, so the job keeps the stale result). What remains, while
  the ledger is unreachable for longer than retention − lease − margin,
  is other sites' spend since B: missing until it answers. It is not
  silent: the job reports the result stale (`Job.Stale`, its age, and
  each failed step's error), for an alert.
- **Other sites' spend** after B, as before: the next snapshot carries it.

## Consequences

- `control/watermark` is deleted; the known-spend job becomes one
  ClickHouse aggregate per scope and period (`sum(cost_micros)` over
  `golem.spend_dedup` where `ended_at` < B).
- The limiter's store loses its numbering, its idempotent adds, its
  `After` read and the over-counting flag; the limiter loses `OnNumbered`.
- `model/exposure.md` is re-specified for the hand-off by boundary, with
  a clock and the ingestion allowance as `tick`'s guard; its mutants are a
  boundary without the allowance, a counter reset at each snapshot, and
  the snapshot and whole counter summed.
- Snapshots carry `since` (the boundary) instead of `watermark`
  (unreleased: no conversion).

## Alternatives

- **Keep the numbering and fix it** (the Go review's fixes for recovery
  and the stranded increment, and wire the watermark job). Exact, and the
  larger, defect-prone half of the accounting, for exactness ADR 0044 no
  longer asks for.
- **One counter per period, rebased at each snapshot.** Needs the counter
  as of the snapshot's ledger state, which is what the numbering existed
  to give.
- **Coarser buckets** (five minutes). Fewer fields per admission; the
  boundary then moves in coarser steps, and the stated undercount grows
  with them. With minutes, an admission and completion on a current
  snapshot cost about 110 µs against a local Valkey from four parallel
  callers (`BenchmarkAdmitComplete`), against about 80 µs for ADR 0042's
  O(1) script; a gateway on a four-hour-old snapshot pays for 240 reads,
  and is rare.
- **Reading the whole hash** (`HGETALL`) and summing from B. Simple, but
  every admission then pays for the retention's 360 fields whatever its
  boundary: measured at three times the cost.
