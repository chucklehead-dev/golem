# Walkthrough: budget accounting

This is a guided reading of how golem decides whether a call may start, and
how what it spent comes to be known: the gateway's limiter
(`gateway/limit`), the site store it runs over (memory or Valkey), the
known-spend job in golem-control (`control/spend`) that reads the ledger,
and how a snapshot's known spend meets the site's counter. It is for an
engineer new to the repository; read it with the code open. The design is
in [ADR 0031](../adr/0031-soft-budgets.md) (soft budgets),
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
| `gateway/limit/store.go` | the `Store` interface (`store.go:105`), the shared budget check, and `Mem`, the in-memory store |
| `gateway/limit/valkey.go` | `Valkey`, the production store: one Lua script per operation |
| `control/spend/spend.go` | the known-spend job: `Job`, the `Ledger` it reads, and `ClickHouse`, the ledger's query |
| `deploy/clickhouse/spend.sql` | the ledger (`golem.spend`), its deduplicated view, and `golem.spend_by_event`, which the job sums |
| `control/controller/controller.go:121` | where each publication runs the job and stamps every scope with its result |
| `cmd/golem-gateway/main.go:146` | `OnApply`: each applied snapshot's known spend handed to the limiter |
| `gateway/proxy/proxy.go:291` | the request path: `settlement` ends each ticket once (`complete`, `cancel`) |
| `model/exposure.md`, `model/accounting.md` | the Quint models the design is checked against |

## 1. Budgets are controls, not ceilings

Start with what golem does not claim. A budget is an admission and
monitoring control (ADR 0044): golem refuses new calls once *known spend*
reaches the budget, and bounds how much can be in flight. Calls already in
flight may go past the budget, a provider may go on billing after a cut,
and some costs are estimates. The provider account's own spending limit is
the financial boundary, and the monthly reconciliation is the record.

So the limiter has two checks (`overBudget`, `store.go:87`):

- **soft** (the default): admit while known spend is below the budget,
  whatever the call then costs;
- **declared-worst-case** (`Call.DeclaredWorstCaseCheck`): admit only if
  known spend plus the request's declared worst case (`Call.Worst`) fits.
  It narrows the overshoot by about one call per principal; it is not a
  ceiling. Refusing requests that declare no worst case, or ask for
  server-side tools, happens before the limiter, on the request path.

A budget of 0 means none; a negative one refuses everything (config
refuses one, so this only fails closed). The same check exists three
times: `overBudget` for `Mem` and for the outage path, and in Lua in the
admit script (`valkey.go:219`). Change one, change all three; the
stores' differential test (§7) catches the Lua drifting from Go, as it
did once for a negative budget.

The other half of admission is concurrency: a principal's in-flight limit
and a scope's (`atLimit`, `store.go:100`). The scope limit matters because
a scope's pool is shared: without it the scope's exposure is the sum of its
members' limits (model/exposure.md §9).

## 2. Known spend: a snapshot and the site's minutes

Known spend is what every check reads. It must be cheap (every admission)
and exact across gateways that hold different snapshots. ADR 0048's
answer:

- Each site's store keeps a **counter per scope and period**: the spend of
  each minute, by when its calls ended, and the period's total. A
  completion adds its cost to its minute. The counter holds amounts, not
  events, and keeps a retention's minutes (six hours: longer than the
  longest authorization lease plus the ingestion allowance, so a gateway
  on the oldest snapshot it may still use finds every minute it needs).
- golem-control's snapshot carries, per scope, a **boundary B** (a whole
  minute, an ingestion allowance before the publication) and the
  **spend**: the ledger's spend of the calls that ended before B,
  deduplicated by event (§5).
- **Known spend = snapshot spend + the counter's minutes from B on.** A
  call's ledger row and its counter increment carry the same end (the row
  is stamped first, `proxy.go:259`, so the increment's minute is never
  before the row's), so a call is on one side of B: in the snapshot, or in
  the minutes counted. A gateway on an older snapshot has an earlier B and
  is just as exact.

It holds while the ledger ingests within the allowance. A row later than
that, or minutes a Valkey failover lost, undercount until a later boundary
passes them or reconciliation books them: soft controls, stated in ADR
0048, not hidden.

The gateway side is `Known` (`limit.go:34`): `Period`, `Spend`, `Since`
(B), applied per scope by `ApplySnapshot` (`limit.go:421`; the gateway's
`OnApply` calls it, `cmd/golem-gateway/main.go:148`). The counter is left
as it is at an apply: a gateway still on an older snapshot needs the
minutes from its own boundary. `snapshotFor` (`limit.go:170`) treats a
snapshot of another period as none: a new month starts from nothing. The
period is the month the call *started* in (`Call.Started`), so its counter
and its ledger row agree even if it completes next month
(TestPeriodOfStart). `fromMinute` (`store.go:50`) turns B into the first
minute counted: B's own minute (golem-control places boundaries on
minutes, and one off a minute errs high), or 0 for no boundary, which
reads the period's whole total.

## 3. The limiter

### 3.1 Admission

`Limiter.Admit` (`limit.go:184`):

1. While the store is reachable (`Down()` false), one store call does the
   whole check atomically: `Store.Admit` with an `Admission`
   (`store.go:60`) carrying the snapshot's boundary and spend, the
   gateway's clock, the budget, the worst case and both limits. Atomic
   matters: two gateways must not both pass the budget check and then both
   take the last slot.
2. `admitted` (`limit.go:208`) takes the answer: the known spend the store
   computed is remembered in `last` (all an outage will leave), and an
   admitted call is tracked (`track`, `limit.go:263`) as a `Ticket`.
   During the grace after a recovery (§3.4) the ticket is marked waived.
3. If the caller's context ended, that is the caller's error, not an
   outage (TestCallerCancelIsNoOutage). Any other store error is an
   outage: `markDown` (`limit.go:254`), then the local check.
4. `admitLocally` (`limit.go:225`) checks the last known spend and this
   gateway's own counts (`byPrincipal`, `byScope`, kept so the check is
   O(1)) under the lock, and the call is a **waiver** (§3.4).

Each store call is bounded by `StoreTimeout` (250 ms; `storeContext`,
`limit.go:242`). `Mem` is marked `inProcess` and gets no deadline: it has
no I/O, and a deadline costs two allocations per call.

### 3.2 Ending a call: complete, cancel, the tail

A ticket ends once (`claimEnd`, `limit.go:131`; TestTicketEndsOnce).

- `Complete` (`limit.go:307`) frees the local slot (`finish`,
  `limit.go:273`) and calls `account` (`limit.go:337`) with `release`:
  `send` (`limit.go:361`) calls `Store.Complete`, which adds the cost to
  its minute and frees the slot in one step.
- `Cancel` (`limit.go:318`) records what the gateway had seen
  (`Store.Add`) but **keeps the slot**: the provider may still be working,
  and billing, and freeing the slot would let cancel-and-resend escape the
  in-flight limit (model/exposure.md §5.4). The request path's ticket is
  `tailed` (`limit.go:714`): its `Cancel(ctx, seen, until)` also calls
  `EndAt` (`limit.go:373`), a timer that calls `Ended` (`limit.go:385`)
  at the provider-tail time (now plus `ProviderTail`, default two
  minutes, but no later than the call's deadline: `proxy.go:308`).
  `Ended` frees the slot locally and at the store.

`account` clamps a cost to [0, `snapshot.MaxMicros`] (2^50,
`limit.go:341`): a negative cost would take spend back, and sums past 2^53
are no longer exact in Valkey's Lua numbers. Only a garbled usage report
gives either; config refuses a budget above the same bound
(`lib/compile/entitlements.go:76`).

### 3.3 Renewal

Slots in Valkey expire after a TTL (90 s) unless renewed. `Run`
(`limit.go:648`) calls `Renew` (`limit.go:431`) every 30 s
(`renewInterval`), which re-registers the running calls' slots
(`reacquire`, `limit.go:444`) without checking limits: the calls are
already running. A call past its deadline plus a minute is no longer
renewed (`renewable`, `limit.go:480`; TestRenewalStopsAtDeadline). A
cancelled call is still running until `Ended`, so its tail is renewed.

The subtle part is ordering. `reacquire` copies the slots under the lock,
then calls the store outside it. A call that ends meanwhile would be
registered again: a phantom slot for a TTL. So `finish` records calls
ending while a renewal is in flight (`endedDuring`), and the last renewal
to finish takes them (`reacquired`, `limit.go:462`) and releases them
(TestRenewalDoesNotRevive).

### 3.4 Outages, waivers, replay and grace

When the store fails, the gateway goes on admitting on its own counts, and
records every such admission as a waiver (`Ticket.Waived`): across several
gateways local counts can add up past the limits, and the waiver is how
that exposure is known instead of silent (ADR 0041 §7.3). Calls already
running are never cancelled.

While down, completions and cancellations cannot reach the counter, so
`account` holds each as an `Increment` (`hold`, `limit.go:404`), with the
minute it ended in. At most `MaxPending` (100,000) are held; past it the
oldest is dropped and counted (`Dropped`): its ledger row was written at
its call's end, so the snapshots count it once their boundary passes; only
the counter (the site's fast view) loses it (TestRecoverBatches).

`Run` calls `Recover` (`limit.go:501`) while down. It replays the held
increments in order, each into its own minute, in batches of 256
(`replayBatch`, `limit.go:536`, `Store.AddAll`, one script each, so a
batch is added whole or not at all), until at most one batch is left;
re-registers every running call's slot (their TTLs lapsed during the
outage); marks the limiter up; and drains the rest. From the mark on,
`hold` refuses an increment (it checks `down` under the lock the mark
takes), so a completion that saw the store down just before goes to the
store itself: nothing is held after the last replay, and the drain can
only shrink, so recovery converges however busy the gateway is
(TestRecoverUnderLoad, TestCompletionRacingRecovery). An increment is an
amount: a batch sent again after a lost reply is counted twice, erring
high. Two replays of the same increment (`ReplayOne` racing another) both
send it, but `dropReplayed` (`limit.go:556`) takes it off the held list
once: the later finds it gone and leaves the next held increment alone
(TestOverlappingReplays).

Recovery is per gateway. A recovered gateway cannot know which peers have
not recovered, whose running calls Valkey still lacks, so for `Grace`
(two renewal intervals) its admissions are waivers too. `EndGrace` ends it
at once.

At shutdown `Finish` (`limit.go:677`) stops the tail timers (their slots
expire by TTL), tries one `Recover` if down, and abandons what is still
held (counted in `Dropped`; the ledger has their rows).

## 4. The stores

`Store` (`store.go:105`): every method is one atomic operation and one
round trip.

**Mem** (`store.go:135`) is the standalone gateway's store and the tests'.
One mutex makes each method atomic. The counter (`memCounter`) is a map of
minute to spend per scope and period, plus the total; minutes past
`Retain` (six hours) go as it adds. `Mem.since` (`store.go:281`) reads the
window the Valkey script reads (below), so the two answer alike. `SetDown`
and `Failover` fake Valkey's faults: going down loses every slot (an
outage longer than the TTL), and a failover loses a minute.

**Valkey** (`valkey.go:40`) keeps every key under the hash tag
`{golem:<site>}` so a cluster puts them in one slot, as the scripts need.
Slots are sorted sets per principal and per scope, scored by expiry on the
server's clock (`TIME`), so no gateway's clock can extend one. The counter
is one hash per scope and period, minute to spend plus a `total` field
(`scopeKeys`, built once per scope and period). The scripts' duration
arguments are formatted once (`durArgs`, `valkey.go:70`): admission is
hot. `Now` (`valkey.go:50`) is the gateway's clock for a read's last
minute (the stores' differential found reads on the wall clock while
admissions used the gateway's).

The scripts (`valkey.go:128` on) are built from Lua fragments: `take`
(the slots), `since` (the read), `trim` and `add` (the counter's write).
Each script's comment lists its KEYS and ARGV.

- `admitScript` (`valkey.go:211`): `since` reads one `HGET` per minute
  from the boundary's to the gateway's own minute plus one
  (`clockSkewMinutes`, a peer's clock running ahead), no further back than
  the retention, or the `total` field alone for no boundary; then the
  budget check; then `take`. It returns `{outcome, known}`.
- `completeScript` (`valkey.go:260`): `add` (an `HINCRBY` of the call's
  minute and of `total`, the counter's life extended, and `trim`: once the
  hash holds more than a retention's minutes and a margin of 60, the older
  ones go), then the slots freed.
- `addScript` (`valkey.go:249`): `add` for several increments, for a
  cancellation and a replay's batch.
- `registerScript` (`valkey.go:231`): renewal and re-acquisition, in
  chunks of 256 slots so one script never holds Valkey's thread for long.
- `sinceScript` (`valkey.go:272`): `since` alone, for `Limiter.Known`.

With `ReplicaAcks`, each add waits for replicas (`WAIT`) so a failover is
less likely to lose it. WAIT counts only its own connection's writes, so
`write` (`valkey.go:321`) sends the script and WAIT together in one
pipeline (falling back to EVAL by hand on NOSCRIPT); a short
acknowledgement is counted in `Unreplicated`, not an error
(TestReplicaAcks). A connection failure is `ErrUnavailable` (an outage),
keeping its cause; a script error stays itself (`wrap`, `valkey.go:438`;
TestWrap).

## 5. The ledger and the known-spend job

The ledger row (`golem.spend`, `spend.sql:7`) is written when the call
ends, whatever the counter's state (ADR 0043), and carries `ended_at`.
Delivery is at least once, so some events arrive twice.
`golem.spend_by_event` (`spend.sql:79`) is a narrow
`ReplacingMergeTree` filled from `golem.spend` by a materialized view
(`spend.sql:93`), keyed by scope and event in the period's partition, so
duplicates merge away; `FINAL` makes that exact at read time.
(`golem.spend_dedup`, `spend.sql:57`, is the whole-table view balances and
rollups read; the job does not, as it would sort the wide table every
minute.)

The job (`control/spend`):

- `Job.Step` (`spend.go:58`) places B: now minus the `Allowance` (five
  minutes, `-spend-allowance`), truncated to a minute, in the period now
  falls in. A B still in the previous period gives this period nothing,
  without asking the ledger. Otherwise it asks the `Ledger` for each
  scope's spend before B. On a failure the last result is kept if it is of
  this period (its earlier B is still exact) and forgotten if not
  (`forgetOtherThan`, `spend.go:94`): a new month's spend is not the old
  one's (TestJob).
- `ClickHouse.Spend` (`spend.go:134`) is one query (`spendQuery`,
  `spend.go:127`) over ClickHouse's HTTP interface, the period and B
  bound as parameters, never in the query's text; `readSpend`
  (`spend.go:165`) reads its rows (TestClickHouse against a fake;
  TestClickHouseLedger against a real ClickHouse, below).
- `Job.Current` is what a publication carries. The controller runs `Step`
  whenever a publication is due and stamps every scope of the
  entitlements with the result's period, B and spend
  (`controller.go:121-133`; TestKnownSpend). With no result (no
  `-clickhouse`, or a failure in a new period) a snapshot carries none,
  and gateways count their counter's whole total for the period.

TestClickHouseLedger (`control/spend/clickhouse_test.go:27`) runs
spend.sql in a database of its own on a real ClickHouse
(`GOLEM_CLICKHOUSE_URL`; it skips without one) and checks the query: an
event inserted twice and not yet merged counts once, only the period's
rows before B count, and the sum equals `golem.spend_dedup`'s. Without
`FINAL` it fails.

## 6. Worked examples

One site, gateways g1 and g2 over one Valkey; scope `team-a` with budget
10, principal limit 1, scope limit 2; principals p1, p2, p3. Amounts are
small numbers of micro-units, for reading. At 12:00 golem-control
published snapshot S1, which both gateways hold: period 2026-11, B 11:55,
spend 0 (nothing ended before 11:55).

### 6.1 Two gateways admitting for one scope

1. **12:00:05, g1 admits a (p1).** `Admit` (`limit.go:184`) names the
   period from the call's start (`period`, `limit.go:158`), takes S1's
   `Known` (`snapshotFor`) and builds the `Admission`. `Valkey.Admit`
   (`valkey.go:290`) sends `admitScript`: `since` reads minutes 11:55 to
   12:01, all empty, so known is 0; 0 < 10; `take` trims expired slots,
   finds p1's set and the scope's empty, and adds `a` to both, expiring at
   the server's now plus 90 s. It returns `{0, 0}`; `admitted` records
   last = 0 and tracks `a`.
2. **12:00:06, g2 admits b (p2).** The same on the other gateway; the
   scope's set is {a, b}.
3. **12:00:07, g1 tries c (p3).** `take` finds the scope's set full and
   the script returns `{2, 0}`: `ErrConcurrency`. Had g1 and g2 raced for
   the last slot, Valkey would run one script after the other, so one
   wins; the check and the take are never split.
4. **12:00:40, a completes on g1, costing 3.** The proxy writes a's row
   (ended 12:00:40) first (`proxy.go:259`), then `complete`: `Complete`
   (`limit.go:307`) ends the ticket, frees its local counts, and `account`
   stamps the increment 12:00:40. `completeScript` adds 3 to minute 12:00
   and to `total`, and removes `a` from both slot sets.
5. **12:01:10, g2 admits c (p3).** The scope's set is {b}; known is
   0 + the minutes from 11:55 = 3 < 10.

### 6.2 A cut call held for its tail

6. **12:02:10, b's upstream cuts** after 2 has been produced. The proxy
   writes b's row (ended 12:02:10, cut), then `settlement.cancel`
   (`proxy.go:308`) computes the tail's end (12:04:10, two minutes, unless
   the call's deadline is sooner) and calls `tailed.Cancel`
   (`limit.go:716`): `account` without release sends `addScript`, which
   adds 2 to minute 12:02, and `EndAt` sets a timer. b's slot stays in
   both sets, and b stays among g2's running calls.
7. **12:02:30, p2 sends d to g2.** p2's set still holds b:
   `ErrConcurrency`. The provider may still be working on b, and this is
   what stops cancel-and-resend escaping the limit. g2's renewal at about
   that time (`reacquire`) re-registers b's slot with the other running
   calls'.
8. **12:04:10, b's tail ends.** The timer calls `Ended`
   (`limit.go:385`): `finish` drops b locally, and `Store.Release` removes
   it from both sets. p2 can start a call again. Had the provider billed b
   more than 2, the difference is spend golem never heard of:
   reconciliation books it.

### 6.3 A snapshot's known spend and boundary reaching the gateways

9. **12:06:00, golem-control publishes.** `Job.Step` (`spend.go:58`)
   places B at 12:01 (12:06 minus five minutes) and asks ClickHouse for
   the spend of 2026-11's rows that ended before 12:01:00.000: a's 3. The
   controller stamps `team-a` with period 2026-11, B 12:01, spend 3
   (`controller.go:126-133`).
10. **g1 applies the new snapshot S2; g2 is still on S1.**
    `OnApply` → `ApplySnapshot` replaces g1's `Known` for the scope and
    leaves the counter alone. g1's known spend: 3 + the minutes from 12:01
    (b's 2 at 12:02) = 5. g2's: 0 + the minutes from 11:55 (a's 3 and b's
    2) = 5. Each call is counted once on each gateway, whichever snapshot
    it holds.
11. **A late row.** b's row ended at 12:02:10. If it reaches ClickHouse by
    12:07, the 12:08 publication (B 12:03) counts it in its spend, and the
    minutes from 12:03 do not: exact. If it is later than the allowance,
    say at 12:09, the 12:08 snapshot misses it while its minutes from 12:03
    lack it too: an undercount of 2 until a publication after the row is
    ingested (ADR 0048's stated limit).

## 7. The models and how the tests use them

- **model/accounting.md**: one principal, one gateway, a working Valkey:
  is the overshoot within the budget plus one worst case per slot, is
  nothing counted twice, and do the books match after reconciliation?
  Checked by `model/check-accounting.sh` (random simulation, and an
  exhaustive search of the small instance); no Go test drives it.
- **model/exposure.md**: a shared scope, several gateways, late ingestion,
  outages, failovers, cancelled calls. Its properties (§6): the limits
  hold whenever Valkey is reachable; the scope's stated bound, plus what
  is reported (uncounted spend, waivers, held spend); and the hand-off
  (never counted twice, no increment lost) while the ledger ingests
  within its allowance. Each broken design of §9 must break its property.

The Go tests use exposure.md four ways:

- `gateway/limit/exposure_mbt_test.go` (TestExposureModel,
  TestExposureModelValkey; connect): the model's traces drive one
  `Limiter` per gateway over one store, `Mem` or Valkey (whose outages
  and failovers `valkey_faults_test.go` injects), on a clock the model
  moves; after every step the counter's minutes, slots, waivers, held
  increments and each gateway's known spend must be the model's. The
  ledger and golem-control are the environment.
- `gateway/limit/differential_test.go`: the two stores side by side.
  TestStoresAgree (`differential_test.go:95`) runs each trace on Mem and
  Valkey at once: each must match the model, and the two must match each
  other (state and every gateway's known spend). TestStoresAgreeVaried
  (`differential_test.go:112`) replays the traces with inputs the model
  never picks, drawn from a seed of the step so a failure replays (costs
  negative, zero and past 2^53; budgets of zero and below; a retention
  shorter than the trace), and compares what reads from every minute
  return. It found a negative budget admitted without bound by the Lua
  check, sums past 2^53 rounded by Lua (hence the `MaxMicros` clamp),
  `Mem` reading minutes past the retention that Valkey does not, and
  Valkey's reads on the wall clock (hence `Valkey.Now`).
- `test/props/exposure_test.go` (Hegel): Hegel drives the real limiter,
  and the model judges every run; TestNoScopeLimitCaught shows a planted
  mutant is caught. `test/props/stale_test.go` is a scripted regression:
  an outage's admission from a stale view conforms.

Run them with `QUINT_BACKEND=typescript go test ./gateway/limit/` and
`cd test/props && go test ./...`. The Valkey tests use
`GOLEM_VALKEY_ADDR`, or start a `valkey-server` (or `redis-server`), and
skip without one; TestReplicaAcks needs a local server binary to attach a
replica to.

## 8. Invariants: where enforced, where tested

| Invariant | Enforced | Tested |
|---|---|---|
| Budget check and slot taken in one atomic step | `admitScript` (`valkey.go:211`); `Mem`'s lock | store_test.go "limits", "budget"; exposure MBT |
| Limits hold while the store is reachable | `take` (`valkey.go:143`), `Mem.acquire` (`store.go:178`) | props TestLimiterAgainstExposureModel, TestNoScopeLimitCaught |
| The two stores answer alike | one check (`overBudget`, mirrored in Lua); one read window (`since`, `Mem.since`, `clockSkewMinutes`) | TestStoresAgree, TestStoresAgreeVaried |
| A call on one side of the boundary | the row stamped before the increment (`proxy.go:259`); `fromMinute`; the ledger deduplicated by event | TestBoundary; both MBT tests; TestClickHouseLedger |
| Known spend exact on every gateway, any snapshot (within the allowance) | `since`, `Mem.since`; `ApplySnapshot` leaves the counter | TestBoundary, store_test.go "counter", both MBT tests |
| The snapshot's spend counts each event once, before B only | `spendQuery` over `spend_by_event FINAL` | TestClickHouseLedger, TestClickHouse |
| A boundary on a whole minute, an allowance back; a new period from nothing | `Job.Step` | TestJob, TestKnownSpend |
| Sums exact | `account`'s clamp to `MaxMicros`; config's budget bound | TestStoresAgreeVaried |
| The counter bounded | retention: `trim`, `Mem.add` | TestBoundary; Valkey "a counter keeps a retention's minutes and its whole total" |
| A cancelled call keeps its slot for its tail | `Cancel`, `EndAt`, `Ended` | TestLimits, TestAdmitter |
| A renewal never revives an ended call | `endedDuring`, `reacquired` | TestRenewalDoesNotRevive |
| Running calls re-registered after an outage | `Recover` → `reacquire` | TestOutage; exposure MBT |
| Outage and grace admissions are waivers | `admitLocally`, `graceUntil` | TestOutage, TestCallerCancelIsNoOutage; MBT waivers |
| A ticket ends once | `claimEnd`, `finish` | TestTicketEndsOnce, TestOutageCounts |
| A call is counted in the period it started | `Admit`'s period, `Ticket.period` | TestPeriodOfStart, TestPeriodRollover |
| Held increments bounded and replayed in order | `hold`, `replayBatch` | TestRecoverBatches |
| A replayed increment taken off once | `dropReplayed` | TestOverlappingReplays |
| Recovery converges under load; no increment held after it | `Recover`'s mark before the drain; `hold` refusing once up | TestRecoverUnderLoad, TestCompletionRacingRecovery |

## 9. Known gaps

- **Late rows and lost minutes undercount.** A row later than the
  ingestion allowance, or minutes a Valkey failover lost, are missing from
  known spend until a later boundary passes them or reconciliation books
  them (ADR 0048). `ReplicaAcks` narrows the second; the collector's lag
  is alerted on.
- **Other sites' spend after B.** The counter is the site's own: another
  site's calls that ended after B reach a gateway only with a snapshot
  whose B has passed them (ADR 0048).
- **A replay counted twice.** A batch whose reply was lost and is sent
  again, or one increment two replays sent at once, is added twice: errs
  high.
- **Sums are exact per increment, not per scope.** The clamp bounds each
  increment to 2^50 and config bounds a budget; a scope's sum could still
  pass 2^53 (where Lua's numbers round) only with several garbled usage
  reports each counted at the bound.
- **Grace is an assumption.** The model lets grace end only once every
  gateway has recovered; the code ends it after `Grace`, assuming each
  peer notices within a renewal interval.
- **The models assume the declared worst case bounds every call** and
  have one budget period; real calls can exceed their declared worst case
  (ADR 0044).
- **The job's query grows with the month.** `FINAL` reads the period's
  partition of `spend_by_event` each publication (measured: 170 ms for 5
  million rows); past hundreds of millions of calls a month, spend.sql
  names the next step, a per-minute rollup of the merged minutes.
- **No known spend without the job.** A golem-control run without
  `-clickhouse` publishes no spend or boundary, and gateways count their
  counter's whole total for the period: each site counts its own calls
  only, and what a failover or a flush of Valkey loses stays lost.
