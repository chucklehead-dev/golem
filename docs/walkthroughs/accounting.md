# Walkthrough: budget accounting

This is a guided reading of how golem decides whether a call may start, and
how what it spent comes to be known: the gateway's limiter
(`gateway/limit`), the site store it runs over (memory or Valkey), and how
a snapshot's known spend meets the site's counter. It is for an engineer
new to the repository; read it with the code open. The design is in
[ADR 0031](../adr/0031-soft-budgets.md) (soft budgets),
[ADR 0038](../adr/0038-accounting-refinements.md) (slots and renewal),
[ADR 0041](../adr/0041-security-boundaries-and-incident-controls.md) §7.3
and §7.6 (waivers, the provider's tail),
[ADR 0043](../adr/0043-ledger-durability.md) (ledger durability),
[ADR 0044](../adr/0044-budgets-are-controls-not-ceilings.md) (what a budget
is) and [ADR 0048](../adr/0048-known-spend-by-time-boundary.md) (known
spend by a time boundary and minute counters).

## The pieces at a glance

| Where | What it is |
|---|---|
| `gateway/limit/limit.go` | `Limiter`: one gateway's admission, its tickets, outages, renewal and replay |
| `gateway/limit/store.go` | the `Store` interface, the shared budget check, and `Mem`, the in-memory store |
| `gateway/limit/valkey.go` | `Valkey`, the production store: one Lua script per operation |
| `gateway/proxy/proxy.go` | the request path: `settlement` ends each ticket once (`complete`, `cancel`) |
| `gateway/telemetry/ledger.go` | the ledger rows (`golem.spend`), written at each call's end |
| `deploy/clickhouse/spend.sql` | the ledger's table and `golem.spend_dedup`, from which a snapshot's spend is summed |
| `model/exposure.md`, `model/accounting.md` | the Quint models the design is checked against |

## 1. Budgets are controls, not ceilings

Start with what golem does not claim. A budget is an admission and
monitoring control (ADR 0044): golem refuses new calls once *known spend*
reaches the budget, and bounds how much can be in flight. Calls already in
flight may go past the budget, a provider may go on billing after a cut,
and some costs are estimates. The provider account's own spending limit is
the financial boundary, and the monthly reconciliation is the record.

So the limiter has two checks (`overBudget`):

- **soft** (the default): admit while known spend is below the budget,
  whatever the call then costs;
- **declared-worst-case** (`Call.DeclaredWorstCaseCheck`, once called
  "strict"): admit only if known spend plus the request's declared worst
  case (`Call.Worst`) fits. It narrows the overshoot by about one call per
  principal; it is not a ceiling. Refusing requests that declare no worst
  case, or asks for server-side tools, happens before the limiter, on the
  request path.

A budget of 0 means none. The same check exists three times: `overBudget`
for `Mem` and for the outage path, and in Lua
in the admit script. Change one, change all three.

The other half of admission is concurrency: a principal's in-flight limit
and a scope's (`atLimit`). The scope limit matters because a
scope's pool is shared: without it the scope's exposure is the sum of its
members' limits (model/exposure.md's `noScopeLimit` design).

## 2. Known spend: a snapshot and the site's minutes

Known spend is what every check reads. It must be cheap (every admission)
and exact across gateways that hold different snapshots. ADR 0048's
answer:

- Each site's store keeps a **counter per scope and period**: the spend of
  each minute, by when its calls ended. A completion adds its cost to its
  minute. The counter holds amounts, not events, and keeps a retention's
  minutes (six hours).
- golem-control's snapshot carries, per scope, a **boundary B** (a minute,
  an ingestion allowance before the snapshot's time) and the **spend**:
  the ledger's spend of the calls that ended before B, deduplicated by
  event.
- **Known spend = snapshot spend + the counter's minutes from B on.** A
  call's ledger row and its counter increment carry the same end (the
  increment's minute is stamped after the row's, never before), so a call
  is on one side of B: in the snapshot, or in the minutes counted. A
  gateway on an older snapshot has an earlier B and is just as exact.

It holds while the ledger ingests within the allowance. A row later than
that, or minutes a Valkey failover lost, undercount until a later boundary
passes them or reconciliation books them: soft controls, stated in ADR
0048, not hidden.

The gateway side is `Known`: `Period`, `Spend`, `Since` (B), applied per
scope by `ApplySnapshot` (cmd/golem-gateway's `OnApply` calls it).
`snapshotFor` treats a snapshot of another period as none: a new month
starts from nothing. The period is the month the call *started* in
(`Call.Started`), so its counter and its ledger row agree even if it
completes next month (TestPeriodOfStart). A boundary inside a minute
counts that whole minute (`fromMinute`): golem-control places boundaries
on minutes, and one off a minute errs high.

## 3. The limiter

### 3.1 Admission

`Limiter.Admit`:

1. While the store is reachable (`Down()` false), one store call does the
   whole check atomically: `Store.Admit` with an `Admission` carrying the
   snapshot's boundary and spend, the budget, the worst case and both
   limits. Atomic matters: two gateways must not both
   pass the budget check and then both take the last slot.
2. `admitted` takes the answer: the known spend the store
   computed is remembered in `last` (all an outage will leave), and an
   admitted call is
   tracked (`track`) as a `Ticket`. During the grace after a
   recovery (§3.4) the ticket is marked waived.
3. If the caller's context ended, that is the caller's error, not an
   outage (TestCallerCancelIsNoOutage). Any other store error
   is an outage: `markDown`, then the local check.
4. `admitLocally` checks the last known spend and this
   gateway's own counts (`byPrincipal`, `byScope`, kept so the check is
   O(1)) under the lock, and the call is a **waiver** (§3.4).

Each store call is bounded by `StoreTimeout` (250 ms; `storeContext`). `Mem` is marked `inProcess` and gets no deadline: it has no
I/O, and a deadline costs two allocations per call.

### 3.2 Ending a call: complete, cancel, the tail

A ticket ends once (`claimEnd`; TestTicketEndsOnce).

- `Complete` frees the local slot (`finish`)
  and calls `account` with `release`: `Store.Complete` adds the cost to
  its minute and frees the slot in one step.
- `Cancel` records what the gateway had seen
  (`Store.Add`) but **keeps the slot**: the provider may still be working,
  and billing, and freeing the slot would let cancel-and-resend escape the
  in-flight limit (model/exposure.md's `releaseOnCancel`). The request
  path's ticket is `tailed`: its `Cancel(ctx, seen, until)`
  also calls `EndAt`, a timer that calls `Ended`
  at the provider-tail time (now plus
  `ProviderTail`, default two minutes, but no later than the call's
  deadline). `Ended` frees the slot locally and at the store.

### 3.3 Renewal

Slots in Valkey expire after a TTL (90 s) unless renewed. `Run`
calls `Renew` every 30 s, which re-registers
the running calls' slots (`reacquire`) without checking
limits: the calls are already running. A call past its deadline plus a
minute is no longer renewed (`renewable`;
TestRenewalStopsAtDeadline).

The subtle part is ordering. `reacquire` copies the slots under the lock,
then calls the store outside it. A call that ends meanwhile would be
registered again: a phantom slot for a TTL. So `finish` records calls
ending while a renewal is in flight (`endedDuring`), and the
last renewal to finish releases them (TestRenewalDoesNotRevive).

### 3.4 Outages, waivers, replay and grace

When the store fails, the gateway goes on admitting on its own counts, and
records every such admission as a waiver (`Ticket.Waived`): across several
gateways local counts can add up past the limits, and the waiver is how
that exposure is known instead of silent (ADR 0041 §7.3). Calls already
running are never cancelled.

While down, completions and cancellations cannot reach the counter, so
`account` holds each as an `Increment` (`hold`), with the minute it ended
in. At most `MaxPending` (100,000) are held; past it the oldest is dropped
and counted (`Dropped`): its ledger row was written at its call's end, so
the snapshots count it once their boundary passes; only the counter (the
site's fast view) loses it (TestRecoverBatches).

`Run` calls `Recover` while down. It replays the held increments in
order, each into its own minute, in batches of 256 (`replayBatch`,
`Store.AddAll`, one script each, so a batch is added whole or not at all),
until at most one batch is left; re-registers every running call's slot
(TTLs lapsed during the outage: model/exposure.md's `REACQUIRE`); marks the
limiter up; and drains the rest. From the mark on, `hold` refuses an
increment (it checks `down` under the lock the mark takes), so a completion
that saw the store down just before goes to the store itself: nothing is
held after the last replay, and the drain can only shrink, so recovery
converges however busy the gateway is (TestRecoverUnderLoad,
TestCompletionRacingRecovery). An increment is an amount: a batch sent
again after a lost reply is counted twice, erring high.

Recovery is per gateway. A recovered gateway cannot know which peers have
not recovered, whose running calls Valkey still lacks, so for `Grace`
(two renewal intervals) its admissions are waivers too.
`EndGrace` ends it at once (the model's `graceEnds`).

At shutdown `Finish` stops the tail timers (their slots expire by TTL),
tries one `Recover` if down, and abandons what is still held (counted in
`Dropped`; the ledger has their rows).

## 4. The stores

`Store`: every method is one atomic operation and one round
trip.

**Mem** is the standalone gateway's store and the tests'. One mutex makes
each method atomic. The counter is a map of minute to spend per scope and
period, and minutes past `Retain` (six hours) go as it adds. `SetDown` and
`Failover` fake Valkey's faults: going down loses every slot (an outage
longer than the TTL), and a failover loses a minute.

**Valkey** keeps every key under the hash tag `{golem:<site>}` so a
cluster puts them in one slot, as the scripts need. Slots are sorted sets
per principal and per scope, scored by expiry on the server's clock
(`TIME`), so no gateway's clock can extend one. The counter is one hash per
scope and period, minute to spend (`scopeKeys`).

The scripts, each run atomically by Valkey:

- `admitScript`: `since` (one `HGET` per minute from the boundary's to
  the gateway's own minute plus one for a peer's clock running ahead, no
  further back than the retention; the hash's `total` field alone for a
  snapshot with no boundary), the budget check, then `take` for the
  slots; returns `{outcome, known}`.
- `completeScript`: `HINCRBY` of the call's minute and of `total`, `trim`
  (once the hash holds more than a retention's minutes and a margin, those
  older go), and the slots freed.
- `addScript`: the same for several increments, for a cancellation and a
  replay's batch.
- `registerScript`: renewal and re-acquisition, in chunks of 256 slots so
  one script never holds Valkey's thread for long.

With `ReplicaAcks`, each add waits for replicas (`WAIT`) so a failover is
less likely to lose it. WAIT counts only its own connection's writes, so
`pipeline` sends the script and WAIT together; a short acknowledgement is
counted in `Unreplicated`, not an error. A connection failure is
`ErrUnavailable` (an outage); a script error stays itself (`wrap`).

## 5. The ledger and the snapshot's spend

The ledger row (`golem.spend`) is written when the call ends, whatever the
counter's state (ADR 0043), and carries `ended_at`. A snapshot's spend for
a scope and period is `sum(cost_micros)` over `golem.spend_dedup` (one row
per `event_id`) with `ended_at` before the boundary; golem-control places
the boundary on a minute, the ingestion allowance behind the time it
publishes. That is the whole known-spend job: no per-site state, no gaps
to time out.

## 6. A worked example

Scope `s`, budget 10, principal limit 1, scope limit 2, two gateways g1 and
g2 on one site, both holding the first snapshot (no spend, no boundary:
every minute counts). The ingestion allowance is two minutes.

1. **12:00, g1 admits a (p1).** `Admit` reads the snapshot and calls the
   store. In Valkey `admitScript` sums the counter's minutes (none): known
   0 < 10, and `take` adds `a` to p1's and s's slot sets. `admitted`
   records last = 0 and tracks `a`.
2. **g2 admits b (p2).** The same; s now has 2 slots. A third call, c
   (p3), on either gateway is refused `ErrConcurrency`: `take` finds the
   scope's set full.
3. **12:00, a completes on g1, costing 3.** The proxy has written a's row
   (ended 12:00:40). `Complete` → `account` → `completeScript` adds 3 to
   minute 12:00 and frees a's slots.
4. **12:01, b's client goes away after 2 has been seen.** b's row is
   written; `tailed.Cancel` → `account` without release → `addScript` adds
   2 to minute 12:01, and the slot stays. `EndAt` sets a timer for the
   provider's tail.
5. **c (p3) is admitted on g1.** s holds one slot (b's). Known spend is
   0 + 3 + 2 = 5 < 10.
6. **12:03, golem-control publishes**: boundary 12:01 (two minutes back),
   spend = the rows that ended before 12:01 = a's 3. g1 applies it; g2 is
   still on the first. g1 computes 3 + minutes from 12:01 (b's 2) = 5; g2
   computes 0 + every minute = 5. Nothing lost, nothing counted twice,
   whichever snapshot.
7. **b's row reaches ClickHouse late**, at 12:02: within the allowance, so
   the next snapshot (boundary 12:02) counts it, and the minutes from 12:02
   on no longer do. Had it arrived after 12:04, a snapshot with boundary
   12:02 would miss it and the counter's minutes from 12:02 lack it: an
   undercount of 2 until a later boundary passes 12:01 with the row
   ingested (ADR 0048's stated limit).
8. **b's tail ends.** The timer calls `Ended`, which frees the slot
   locally and at the store. If the provider billed b more than 2, the
   difference is spend golem never heard of: reconciliation books it
   (model/exposure.md's `uncounted`).

## 7. The models and how the tests use them

- **model/accounting.md**: one principal, one gateway, a working Valkey:
  is the overshoot within `BUDGET + LIMIT × WORST` (`withinBound`), is
  nothing counted twice (`noDoubleCount`), and do the books match after
  reconciliation (`booksMatch`)? Checked by `model/check-accounting.sh`
  (random simulation, and an exhaustive search of the small instance); no
  Go test drives it.
- **model/exposure.md**: a shared scope, several gateways, late ingestion,
  outages, failovers, cancelled calls. Its properties: `limitsHonoured`
  (the limits hold whenever Valkey is reachable), `withinBound` (a scope's
  bound plus what is reported: uncounted spend, waivers, held spend),
  `knownNeverOver` and `noLostIncrements` (the hand-off, while the ledger
  ingests within its allowance, `tick`'s guard). Each broken design
  (`noScopeLimit`, `releaseOnCancel`, `noReacquire`, `noAllowance`,
  `resetAtApply`, `sumHandoff`) must break its property.

The Go tests use exposure.md three ways:

- `gateway/limit/exposure_mbt_test.go` (connect): the model's traces drive
  one `Limiter` per gateway over one store, `Mem` and Valkey, on a clock
  the model's `tick` moves; after every step the counter's minutes, slots,
  waivers, held increments and each gateway's known spend must be the
  model's. The ledger and golem-control are the environment.
- `test/props/exposure_test.go` (Hegel): Hegel drives the real limiter,
  and the model judges every run; `TestNoScopeLimitCaught` shows a planted
  mutant is caught.

Run them with `QUINT_BACKEND=typescript go test ./gateway/limit/...` and
`cd test/props && go test ./...`. The Valkey tests start a `valkey-server`
(or `redis-server`) and skip without one.

## 8. Invariants: where enforced, where tested

| Invariant | Enforced | Tested |
|---|---|---|
| Budget check and slot taken in one atomic step | `admitScript`; `Mem`'s lock | store_test.go "limits", "budget"; exposure MBT |
| Limits hold while the store is reachable | `take`, `Mem.acquire` | props `TestLimiterAgainstExposureModel`, `TestNoScopeLimitCaught` |
| A call on one side of the boundary | the row's end stamped before the increment's minute; `fromMinute`; ledger dedup by event | TestBoundary; both MBT tests |
| Known spend exact on every gateway, any snapshot (within the allowance) | `since`, `Mem.since` | TestBoundary, store_test.go "counter", both MBT tests |
| The counter bounded | retention: `since`'s deletes, `Mem.add` | TestBoundary; Valkey "minutes past retention" |
| A cancelled call keeps its slot for its tail | `Cancel`, `EndAt`, `Ended` | TestLimits, TestAdmitter |
| A renewal never revives an ended call | `endedDuring` | TestRenewalDoesNotRevive |
| Running calls re-registered after an outage | `Recover` → `reacquire` | TestOutage; exposure MBT |
| Outage and grace admissions are waivers | `admitLocally`, `graceUntil` | TestOutage, TestCallerCancelIsNoOutage; MBT `Waived` |
| A ticket ends once | `claimEnd`, `finish` | TestTicketEndsOnce, TestOutageCounts |
| A call is counted in the period it started | `Admit`'s period, `Ticket.period` | TestPeriodOfStart, TestPeriodRollover |
| Held increments bounded and replayed in order | `hold`, `replayBatch` | TestRecoverBatches |
| Recovery converges under load; no increment held after it | `Recover`'s mark before the drain; `hold` refusing once up | TestRecoverUnderLoad, TestCompletionRacingRecovery |

## 9. Known gaps

- **Late rows and lost minutes undercount.** A row later than the
  ingestion allowance, or minutes a Valkey failover lost, are missing from
  known spend until a later boundary passes them or reconciliation books
  them (ADR 0048). `ReplicaAcks` narrows the second; the collector's lag
  is alerted on.
- **A replay counted twice.** A batch whose reply was lost and is sent
  again is added twice: errs high.
- **Grace is an assumption.** The model lets grace end only once every
  gateway has recovered; the code ends it after `Grace`, assuming each
  peer notices within a renewal interval.
- **The models assume `WORST` bounds every call** and have one budget
  period; real calls can exceed their declared worst case (ADR 0044).
- **The known-spend job is not wired in yet**: golem-control publishes
  snapshots without spend or boundary (the first snapshot's "every
  minute counts" is then what gateways use, which within a period and the
  counter's retention is exact).
