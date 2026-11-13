# 0042: Known spend by counter sequence and ledger watermark

Status: Accepted. Amends ADR 0031 (how known spend is computed) and ADR 0041
§7.6 (the hand-off between snapshot and site counter).

## Context

Known spend is the snapshot's spend, from the ledger, plus the site
counter's spend not yet in that snapshot. ADR 0041 §7.6 handed off by event:
the snapshot named the events it included, and the gateway added the
counter's other events. That is exact, and the exposure model showed it
correct. As built, though, it made every admission read and compare every
event in the counter's retention window. The cost grew with traffic, and
it was a design flaw, not an implementation detail.

The first implementation also deleted included events from the counter when
it applied a snapshot. Gateways apply snapshots independently, so a gateway
still on the previous snapshot lost those events: they were in neither its
snapshot nor the counter. The model missed this because it applied each
snapshot to every gateway in one step. A benchmark found it.

## Decision

- **The counter numbers its events.** For each scope, the site counter
  assigns each new event the next sequence number, adds its cost to a
  running total, and records the total as of that number. All three happen
  in one atomic script. Adding an event twice (a replay after an outage) is
  a no-op that returns the same number.
- **Per budget period.** A counter is per scope *and* period (a calendar
  month in the period time zone, contracts.md §3), numbered from 1 each
  period, and so is a snapshot's watermark and spend. A call belongs to
  the period it started in, wherever it completes: the period is worked
  out once, from the request's arrival, by `lib/period` in the one
  installation-wide period zone, and carried on the call to both its
  counter and its ledger row (a call admitted after midnight, its body
  still arriving, would otherwise be numbered in one month's counter and
  booked in the other's). A gateway counts a
  snapshot of another period as none (spend 0, W 0): without this, every
  budgeted scope would carry last month's spend, and from the second month
  be refused. A period's counter keys expire a month plus the retention
  after their last add.
- **One round trip each way.** Admission is one script: known spend
  against the budget, then the principal and scope slots, atomically, so
  two gateways cannot both pass the check before either takes a slot.
  Completion is another: the event numbered and the slot freed together.
  Each store call has a deadline (250 ms); a caller that goes away is not
  an outage.
- **The ledger records the number.** Each attempt row carries `counter_seq`
  with its `site` (contracts.md §3).
- **Each snapshot carries a watermark.** For each site and scope it has W:
  the highest sequence number such that every event up to W has been
  ingested, a contiguous prefix. It also has the scope's spend: the ledger's
  balance with that site's events after W left out.
- **Known spend = snapshot spend + (counter total − counter total at W).**
  That is two values read by one script, so admission is O(1) whatever the
  traffic. Events up to W are counted once, in the snapshot. Events after W
  are counted once, from the counter, whether or not the ledger has
  ingested them yet or out of order. A gateway on an older snapshot uses its
  own, smaller W and is equally exact, so snapshot skew between gateways
  cannot lose or double count.
- **Gaps.** An event a crash keeps from the ledger stalls the watermark, and
  nothing is lost while it does: later events are still counted from the
  counter. After a timeout (15 minutes) golem-control declares the gap lost
  and moves the watermark past it. The lost event's cost then leaves known
  spend, and the monthly reconciliation books it (ADR 0031's accepted
  crash loss).
- **Retention.** The per-number totals are kept longer than any snapshot a
  live gateway can still hold: at least the longest authorization lease (an
  internal pool's four hours, ADR 0032), since a site cut off from the
  bucket but not from its own Valkey keeps admitting on an old snapshot.
  The default is five hours, on the order of 10^5 small hash fields per site
  at golem's scale. A gateway whose W has been trimmed counts the whole
  counter (over-counts: conservative) and reports it.
- **Replication.** A Valkey failover can lose the counter's last writes, and
  their numbers are then given again. A lost event that reached the ledger
  and a new one may share a number, and the new one is missed until
  ingested: at most one call per lost number. The add (at completion, off
  the request's critical path) can wait for a replica's acknowledgement
  (`WAIT`) to narrow this; the bound counts the rest (model/exposure.md
  §5.6).
- **Recovery is per gateway.** After an outage each gateway replays what it
  held and re-registers its running calls on its own. Peers not yet
  recovered hold calls Valkey lacks, so a recovered gateway records its
  admissions as waivers for a grace period (two renewal intervals); the
  model assumes every gateway recovers within it.
- Valkey stays per site (ADR 0012); golem-control still reads no site's
  Valkey. The watermark comes from the ledger alone.

## Consequences

Admission costs one O(1) script regardless of traffic. The ledger gains a
column, and golem-control computes a per-site, per-scope contiguous
watermark (ClickHouse: the largest W with count(seq ≤ W) = W, per site,
scope and period). model/exposure.md models the watermark hand-off
with snapshots applied per gateway. A watermark that is not contiguous (the
highest number ingested) and trimming totals a stale gateway still needs
are its mutants.

## Alternatives

- **Hand-off by event (ADR 0041 §7.6 as first written).** Exact but O(window)
  per admission. Superseded.
- **A time watermark** (every event before T is ingested). Not true under
  out-of-order, at-least-once ingestion without a per-source sequence.
- **Per-gateway sequences** (each gateway numbers its own events; known
  spend = spend + Σ_g (total_g − cum_g[W_g])). It would decouple the ledger
  from Valkey's numbering and a failover could not reuse numbers, but
  admission would read one total per gateway that ever ran at the site,
  and gateways come and go with autoscaling. Kept as the fallback if
  failover reuse proves costly.
- **Resetting the counter at each snapshot.** Loses increments the ledger
  has not ingested: the model's `reset` mutant.
