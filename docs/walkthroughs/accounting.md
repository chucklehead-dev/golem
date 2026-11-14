# Walkthrough: budget accounting

This is a guided reading of how golem decides whether a call may start, and
how what it spent comes to be known: the gateway's limiter
(`gateway/limit`), the site store it runs over (memory or Valkey), the
ledger's numbering records, and golem-control's watermark job
(`control/watermark`). It is for an engineer new to the repository; read it
with the code open. The design is in [ADR 0031](../adr/0031-soft-budgets.md)
(soft budgets), [ADR 0038](../adr/0038-accounting-refinements.md) (slots and
renewal), [ADR 0041](../adr/0041-security-boundaries-and-incident-controls.md)
§7.3 and §7.6 (waivers, the provider's tail), [ADR 0042](../adr/0042-known-spend-by-counter-watermark.md)
(known spend by counter and watermark), [ADR 0043](../adr/0043-ledger-durability.md)
(numbering records, gap reconstruction) and [ADR 0044](../adr/0044-budgets-are-controls-not-ceilings.md)
(what a budget is). Line numbers are as of this writing.

## The pieces at a glance

| Where | What it is |
|---|---|
| `gateway/limit/limit.go` | `Limiter`: one gateway's admission, its tickets, outages, renewal and replay |
| `gateway/limit/store.go` | the `Store` interface, the shared budget check, and `Mem`, the in-memory store |
| `gateway/limit/valkey.go` | `Valkey`, the production store: one Lua script per operation |
| `gateway/proxy/proxy.go` | the request path: `settlement` ends each ticket once (`complete`, `cancel`) |
| `gateway/telemetry/ledger.go` | the ledger rows (`golem.spend`) and numbering records (`golem.spend_seq`) |
| `control/watermark/watermark.go` | `Job`: watermark and known spend per site, scope and period, from the ledger |
| `model/exposure.md`, `model/accounting.md` | the Quint models the design is checked against |

## 1. Budgets are controls, not ceilings

Start with what golem does not claim. A budget is an admission and
monitoring control (ADR 0044): golem refuses new calls once *known spend*
reaches the budget, and bounds how much can be in flight. Calls already in
flight may go past the budget, a provider may go on billing after a cut,
and some costs are estimates. The provider account's own spending limit is
the financial boundary, and the monthly reconciliation is the record.

So the limiter has two checks (`overBudget`, store.go:71):

- **soft** (the default): admit while known spend is below the budget,
  whatever the call then costs;
- **declared-worst-case** (`Call.DeclaredWorstCaseCheck`, once called
  "strict"): admit only if known spend plus the request's declared worst
  case (`Call.Worst`) fits. It narrows the overshoot by about one call per
  principal; it is not a ceiling. Refusing requests that declare no worst
  case, or asks for server-side tools, happens before the limiter, on the
  request path (gateway/proxy/proxy.go:458, :471).

A budget of 0 means none. The same check exists three times: `overBudget`
for `Mem` (store.go:150) and for the outage path (limit.go:231), and in Lua
in the admit script (valkey.go:160). Change one, change all three.

The other half of admission is concurrency: a principal's in-flight limit
and a scope's (`atLimit`, store.go:84). The scope limit matters because a
scope's pool is shared: without it the scope's exposure is the sum of its
members' limits (model/exposure.md's `noScopeLimit` design).

## 2. Known spend: a counter and a watermark

Known spend is what every check reads. It must be cheap (every admission)
and exact across gateways that hold different snapshots. ADR 0042's answer:

- Each site's store keeps a **counter per scope and period**. Each new
  event (a call's cost, keyed by the call's ID) gets the next number; the
  counter keeps a running total and the total as of each number. Adding an
  event twice returns its number and changes nothing.
- golem-control's snapshot carries, per site and scope, a **watermark W**
  (every event of that site's counter numbered 1..W is in the ledger: a
  contiguous prefix) and the **spend**: the ledger's balance with that
  site's events above W left out.
- **Known spend = snapshot spend + (counter total − counter total at W).**
  Events up to W are counted once, in the snapshot; events above W once,
  from the counter, whether or not the ledger has them yet. A gateway on an
  older snapshot has a smaller W and is just as exact.

The gateway side is `Known` (limit.go:28): `Period`, `Spend`, `Watermark`,
applied per scope by `ApplySnapshot` (limit.go:416; cmd/golem-gateway/main.go:76
calls it). `snapshotFor` (limit.go:169) treats a snapshot of another period
as none: a new month starts from nothing. The period is the month the call
*started* in (`Call.Started`, limit.go:184), so its counter and its ledger
row agree even if it completes next month (TestPeriodOfStart).

## 3. The limiter

### 3.1 Admission

`Limiter.Admit` (limit.go:183):

1. While the store is reachable (`Down()` false), one store call does the
   whole check atomically: `Store.Admit` with an `Admission`
   (store.go:45) carrying the snapshot's W and spend, the budget, the
   worst case and both limits. Atomic matters: two gateways must not both
   pass the budget check and then both take the last slot.
2. `admitted` (limit.go:206) takes the answer: the known spend the store
   computed is remembered in `last` (all an outage will leave), an
   over-count is logged once per scope and period, and an admitted call is
   tracked (`track`, limit.go:269) as a `Ticket`. During the grace after a
   recovery (§3.4) the ticket is marked waived.
3. If the caller's context ended, that is the caller's error, not an
   outage (limit.go:195, TestCallerCancelIsNoOutage). Any other store error
   is an outage: `markDown` (limit.go:260), then the local check.
4. `admitLocally` (limit.go:231) checks the last known spend and this
   gateway's own counts (`byPrincipal`, `byScope`, kept so the check is
   O(1)) under the lock, and the call is a **waiver** (§3.4).

Each store call is bounded by `StoreTimeout` (250 ms; `storeContext`,
limit.go:248). `Mem` is marked `inProcess` and gets no deadline: it has no
I/O, and a deadline costs two allocations per call.

### 3.2 Ending a call: complete, cancel, the tail

A ticket ends once (`claimEnd`, limit.go:130; TestTicketEndsOnce).

- `Complete` (limit.go:310) frees the local slot (`finish`, limit.go:279)
  and calls `account` (limit.go:337) with `release`: `Store.Complete`
  numbers the event and frees the slot in one step.
- `Cancel` (limit.go:321) records what the gateway had seen
  (`Store.Add`) but **keeps the slot**: the provider may still be working,
  and billing, and freeing the slot would let cancel-and-resend escape the
  in-flight limit (model/exposure.md's `releaseOnCancel`). The request
  path's ticket is `tailed` (limit.go:692): its `Cancel(ctx, seen, until)`
  also calls `EndAt` (limit.go:361), a timer that calls `Ended`
  (limit.go:373) at the provider-tail time (proxy.go:308: now plus
  `ProviderTail`, default two minutes, but no later than the call's
  deadline). `Ended` frees the slot locally and at the store.

Each numbered event goes to `OnNumbered` (limit.go:407) as a `Numbered`
(limit.go:110), which the gateway writes as the ledger's numbering record.

### 3.3 Renewal

Slots in Valkey expire after a TTL (90 s) unless renewed. `Run`
(limit.go:627) calls `Renew` (limit.go:426) every 30 s, which re-registers
the running calls' slots (`reacquire`, limit.go:439) without checking
limits: the calls are already running. A call past its deadline plus a
minute is no longer renewed (`renewable`, limit.go:468;
TestRenewalStopsAtDeadline).

The subtle part is ordering. `reacquire` copies the slots under the lock,
then calls the store outside it. A call that ends meanwhile would be
registered again: a phantom slot for a TTL. So `finish` records calls
ending while a renewal is in flight (`endedDuring`, limit.go:288), and the
last renewal to finish releases them (TestRenewalDoesNotRevive).

### 3.4 Outages, waivers, replay and grace

When the store fails, the gateway goes on admitting on its own counts, and
records every such admission as a waiver (`Ticket.Waived`): across several
gateways local counts can add up past the limits, and the waiver is how
that exposure is known instead of silent (ADR 0041 §7.3). Calls already
running are never cancelled.

While down, completions and cancellations cannot be numbered, so `account`
holds each as an `Increment` (`hold`, limit.go:390). At most `MaxPending`
(100,000) are held; past it the oldest goes to `OnNumbered` with number 0:
its ledger row then counts as it is, and only the counter (the site's fast
view) loses it (TestRecoverBatches).

`Run` calls `Recover` (limit.go:486) while down. It replays the held
increments in order, in batches of 256 per round trip (`replayBatch`,
limit.go:512, `Store.AddAll`), re-registers every running call's slot
(TTLs lapsed during the outage: model/exposure.md's `REACQUIRE`), and only
if nothing more was held meanwhile marks the store up (limit.go:504). No
lock is held across a store call, so completions may add increments while
it runs; `dropReplayed` (limit.go:535) takes a replayed batch off the front
only if it is still there.

Recovery is per gateway. A recovered gateway cannot know which peers have
not recovered, whose running calls Valkey still lacks, so for `Grace`
(two renewal intervals) its admissions are waivers too (limit.go:225).
`EndGrace` ends it at once (the model's `graceEnds`).

At shutdown `Finish` (limit.go:655) stops the tail timers (their slots
expire by TTL), tries one `Recover` if down, and abandons what is still
held (number 0).

## 4. The stores

`Store` (store.go:89): every method is one atomic operation and one round
trip.

**Mem** (store.go:117) is the standalone gateway's store and the tests'.
One mutex makes each method atomic. The counter (`memCounter`) keeps
events, cumulative totals and an index by event ID, and trims events past
`Retain` (five hours) as it adds (store.go:251). `SetDown` and `Failover`
fake Valkey's faults: going down loses every slot (an outage longer than
the TTL), and a failover loses the counter's tail, whose numbers are given
again.

**Valkey** (valkey.go:34) keeps every key under the hash tag
`{golem:<site>}` so a cluster puts them in one slot, as the scripts need.
Slots are sorted sets per principal and per scope, scored by expiry on the
server's clock (`TIME`), so no gateway's clock can extend one. The counter
is six keys per scope and period (`scopeKeys`, valkey.go:85): `seq`,
`total`, `cum` (number → total), `ev` (event → number), `ids` (number →
event) and `times` (number by time, for trimming).

The scripts, each run atomically by Valkey:

- `admitScript` (valkey.go:160): `after` (valkey.go:144) for known spend,
  the budget check, then `take` (valkey.go:127) for the slots; returns
  `{outcome, known, over}`.
- `addScript` and `completeScript` (valkey.go:216, :221) share `add`
  (valkey.go:190): number the event (or return its number), then trim a
  little history past retention, never the newest. Complete also frees the
  slot.
- `registerScript` (valkey.go:175): renewal and re-acquisition, in chunks
  of 256 slots so one script never holds Valkey's thread for long.

If a gateway's W has been trimmed, `after` cannot subtract and returns the
whole total with `over` set: over-counting, never under (the Valkey test
"trimmed history over-counts, never under-counts").

With `ReplicaAcks`, each add waits for replicas (`WAIT`) so a failover
cannot lose it. WAIT counts only its own connection's writes, so
`pipeline` (valkey.go:289) sends the script and WAIT together; a short
acknowledgement is counted in `Unreplicated`, not an error. A connection
failure is `ErrUnavailable` (an outage); a script error stays itself
(`wrap`, valkey.go:413).

## 5. The ledger and the watermark job

The ledger row (`golem.spend`) is written when the call ends and does not
wait for its number. The numbering record (`golem.spend_seq`:
event, site, scope, period, `counter_seq`, gateway/telemetry/ledger.go:45)
follows when the limiter has one, and ClickHouse joins them (ADR 0043).
Number 0 means the event never reached the counter.

`Job.Run` (watermark.go:80) is given the rows (deduplicated by event) and
the numbering records, and works in three steps:

1. `index` (watermark.go:118): each event's number (the lowest, if
   repeated), which numbers each counter's ledger holds, and each counter's
   highest number given. A number whose row has not arrived still stalls
   the mark: the counter has it, the ledger not.
2. `advance` (watermark.go:157): per counter, W climbs the contiguous
   prefix. A missing number stalls it; the job remembers when it first saw
   the gap, and after `GapTimeout` (15 minutes) asks `Counter` (a
   site-local reader) for the event that had the number. If found, the row
   is rebuilt and returned for the caller to write, marked reconstructed;
   if not, the number is declared lost and W moves past it (its cost
   leaves known spend; reconciliation books it).
3. `known` (watermark.go:197): per site, scope and period, the spend is
   every row of the scope and period, less this site's counted rows its
   counter covers: numbered above W, or not numbered yet and younger than
   the timeout. Rows of other sites always count: they reach this site
   only through the ledger.

## 6. A worked example

Scope `s`, budget 10, principal limit 1, scope limit 2, two gateways g1 and
g2 on one site, both holding the snapshot {W 0, spend 0}.

1. **g1 admits a (p1).** `Admit` (limit.go:183) reads the snapshot
   (limit.go:169) and calls the store. In Valkey `admitScript` computes
   `after(0)` = 0 (valkey.go:148), known 0 < 10, and `take` adds `a` to
   p1's and s's slot sets. `admitted` records last = 0 and tracks `a`.
2. **g2 admits b (p2).** The same; s now has 2 slots. A third call, c
   (p3), on either gateway is refused `ErrConcurrency`: `take` finds the
   scope's set full.
3. **a completes on g1, costing 3.** `Complete` (limit.go:310) →
   `account` (limit.go:337) → `completeScript`: `add` gives `a` number 1,
   total 3, cum[1] = 3, and frees a's slots. `OnNumbered` gets {s,
   2026-11, a, 3, 1}; the proxy has already written a's row.
4. **b's client goes away after 2 has been seen.** The proxy's
   `settlement.cancel` (proxy.go:308) calls `tailed.Cancel`
   (limit.go:694): `account` without release → `addScript` gives `b`
   number 2 (total 5), and the slot stays. `EndAt` sets a timer for the
   provider's tail.
5. **c (p3) is admitted on g1.** s holds one slot (b's), so there is room.
   Known spend is 0 + (5 − 0) = 5 < 10.
6. **The ledger has a's row and number, and b's row, but not yet b's
   number.** `Job.Run`: `index` finds number 1 held and highest 1;
   `advance` gives W = 1; `known` takes the balance 3 + 2 = 5 less b's 2
   (counted, unnumbered, younger than the timeout, so the counter's): the
   result {W 1, spend 3}. A gateway that applies it computes 3 + (5 − 3) =
   5, the same as before: nothing lost, nothing counted twice.
7. **b's number arrives**: {W 2, spend 5}, and known spend is 5 + (5 − 5)
   = 5. Had b's *row* been lost instead, number 2 would stall W at 1 (known
   spend still 5, from the counter) until the gap timeout, and then be
   rebuilt from the counter (TestGaps).
8. **b's tail ends.** The timer calls `Ended` (limit.go:373), which frees
   the slot locally and at the store. If the provider billed b more than
   2, the difference is spend golem never heard of: reconciliation books
   it (model/exposure.md's `uncounted`).

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
  `knownNeverOver` and `noLostIncrements` (the hand-off). Each broken
  design (`noScopeLimit`, `releaseOnCancel`, `noReacquire`, `maxIngested`,
  `trimAtApply`, ...) must break its property.

The Go tests use exposure.md three ways:

- `gateway/limit/exposure_mbt_test.go` (connect): the model's traces drive
  one `Limiter` per gateway over one store, `Mem` and Valkey; after every
  step the counter, slots, waivers, held increments and each gateway's
  known spend must be the model's. The ledger and golem-control are the
  environment.
- `control/watermark/exposure_mbt_test.go`: the same traces, but only
  `publishSnapshot` is real: the job must compute the model's W and spend
  from the model's ledger, and the test fails if no trace had a gap.
- `test/props/exposure_test.go` (Hegel): Hegel drives the real limiter,
  and the model judges every run; `TestNoScopeLimitCaught` shows a planted
  mutant is caught.

Run them with `QUINT_BACKEND=typescript go test ./gateway/limit/...
./control/watermark/...` and `cd test/props && go test ./...`. The Valkey
tests start a `valkey-server` and skip without one.

## 8. Invariants: where enforced, where tested

| Invariant | Enforced | Tested |
|---|---|---|
| Budget check and slot taken in one atomic step | `admitScript`; `Mem`'s lock | store_test.go "limits", "budget"; exposure MBT |
| Limits hold while the store is reachable | `take`, `Mem.acquire` | props `TestLimiterAgainstExposureModel`, `TestNoScopeLimitCaught` |
| An event is counted once | `add` (`ev` key), `memCounter.byID`; ledger dedup by event | store_test.go "counter"; TestOutage |
| Known spend exact on every gateway, any snapshot | `after`, `Mem.after`; W contiguous (`advance`) | TestWatermark, watermark TestAgainstBruteForce, both MBT tests |
| A trimmed W over-counts, never under | `after`'s `over` | Valkey "trimmed history over-counts" |
| A cancelled call keeps its slot for its tail | `Cancel`, `EndAt`, `Ended` | TestLimits, TestAdmitter |
| A renewal never revives an ended call | `endedDuring` | TestRenewalDoesNotRevive |
| Running calls re-registered after an outage | `Recover` → `reacquire` | TestOutage; exposure MBT |
| Outage and grace admissions are waivers | `admitLocally`, `graceUntil` | TestOutage, TestCallerCancelIsNoOutage; MBT `Waived` |
| A ticket ends once | `claimEnd`, `finish` | TestTicketEndsOnce, TestOutageCounts |
| A call is counted in the period it started | `Admit`'s period, `Ticket.period` | TestPeriodOfStart, TestPeriodRollover |
| Held increments bounded and replayed in order | `hold`, `replayBatch` | TestRecoverBatches |

## 9. Known gaps

- **Failover reuses numbers.** A lost event that reached the ledger and a
  new one can share a number; the new one is missed until ingested, at
  most one call per lost number (ADR 0042). `ReplicaAcks` narrows it.
- **Grace is an assumption.** The model lets grace end only once every
  gateway has recovered; the code ends it after `Grace`, assuming each
  peer notices within a renewal interval.
- **The models assume `WORST` bounds every call** and have one budget
  period; real calls can exceed their declared worst case (ADR 0044).
- **The watermark job is not wired in yet**: nothing in this checkout
  imports `control/watermark`; its `Job.lost` and `Job.gaps` maps are
  never trimmed for counters that stop appearing.
- **Mem and Valkey differ at the trim boundary**: for a W that is exactly
  the last trimmed number Mem still subtracts exactly, Valkey over-counts;
  for W = 0 after trimming Mem reports `over`, Valkey does not. Both stay
  on the safe side.
- **A completion racing a recovery** can be held just after `Recover`
  marked the store up; it then waits for the next outage or `Finish`
  (number 0), and the counter lacks it until its row counts in a snapshot.
