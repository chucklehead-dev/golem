# Walkthrough: budget accounting

This is a guided reading of how golem decides whether a call may start, and
how what it spent comes to be known: the gateway's limiter
(`gateway/limit`), the site store it runs over (memory or Valkey), the
known-spend job in golem-control (`control/spend`) that reads the ledger,
and how a snapshot's known spend meets the site's counters. It is for an
engineer new to the repository; read it with the code open. The design is
in [ADR 0031](../adr/0031-soft-budgets.md) (soft budgets),
[ADR 0038](../adr/0038-accounting-refinements.md) (slots and renewal),
[ADR 0041](../adr/0041-security-boundaries-and-incident-controls.md) §7.3
and §7.6 (waivers, the provider's tail),
[ADR 0043](../adr/0043-ledger-durability.md) (ledger durability),
[ADR 0044](../adr/0044-budgets-are-controls-not-ceilings.md) (what a budget
is) and [ADR 0048](../adr/0048-known-spend-by-time-boundary.md) (known
spend by a time boundary and minute counters); caps are org-model.md §4.2,
rate limits and quotas PRD §5.5 and §9.2.

## The pieces at a glance

| Where | What it is |
|---|---|
| `gateway/admission/admission.go` | the request path's contract: `Call`, `Admitter`, `Ticket`, `Level`, `Over`, `Meter`, `Meters`, `Refusal` |
| `gateway/limit/limit.go` | `Limiter`: one gateway's admission, its accounts, tickets, outages, renewal and replay |
| `gateway/limit/store.go` | the `Store` interface (`store.go:137`), the shared budget check, and `Mem`, the in-memory store |
| `gateway/limit/valkey.go` | `Valkey`, the production store: one Lua script per operation |
| `gateway/limit/rate.go` | rate limits and quota shares: requests-per-minute buckets, in `Mem` and Valkey, and `Limiter.Take` |
| `gateway/auth/auth.go` | where a principal's `Limits` come from: the scope's budget and mode, its member's cap, its key's cap, rate and in-flight limits |
| `control/spend/spend.go` | the known-spend job: `Job`, the `Ledger` it reads (and `Ingestion`, each site's latest row), and `ClickHouse`, the ledger's queries |
| `deploy/clickhouse/spend.sql` | the ledger (`golem.spend`), its deduplicated view, `golem.spend_by_event`, which the job sums, and `golem.ingest_by_site`, each site's latest row |
| `control/controller/controller.go:347` | where each publication runs the job; `withKnownSpend` (`:549`) stamps every scope, member and capped key with its result |
| `cmd/golem-gateway/main.go:314` | `applyKnown`: each applied snapshot's known spend handed to the limiter, account by account |
| `gateway/proxy/proxy.go:213` | the request path: rate limits, then quota shares at the pick, then `admit` (`:745`); `settlement` (`:517`) ends each ticket once |
| `model/exposure.md`, `model/accounting.md` | the Quint models the design is checked against |

## 1. Budgets are controls, not ceilings

Start with what golem does not claim. A budget is an admission and
monitoring control (ADR 0044): golem refuses new calls once *known spend*
reaches the budget, and bounds how much can be in flight. Calls already in
flight may go past the budget, a provider may go on billing after a cut,
and some costs are estimates. The provider account's own spending limit is
the financial boundary, and the monthly reconciliation is the record.

So the limiter has two checks (`overBudget`, `store.go:119`):

- **soft** (the default): admit while known spend is below the limit,
  whatever the call then costs;
- **declared-worst-case** (`Call.DeclaredWorstCaseCheck`): admit only if
  known spend plus the request's declared worst case (`Call.Worst`) fits.
  It narrows the overshoot by about one call per principal; it is not a
  ceiling. Refusing requests that declare no worst case happens before the
  limiter, on the request path (`admit`, `proxy.go:745`).

A limit of 0 means none; a negative one refuses everything (config
refuses one, so this only fails closed). The same check exists twice:
`overBudget` in Go (for `Mem`, the outage path and a ticket's over-budget
mark), and in Lua in the admit script (`valkey.go:312`). Change one,
change both; the stores' differential test (§8) catches the Lua drifting
from Go, as it did once for a negative budget.

### 1.1 Accounts: the key's cap, the member's, the scope's budget

A call is charged to up to three **accounts** (org-model.md §4.2), in the
order admission checks them (`charged`, `chargedOf`, `limit.go:129`):

1. its **key's** spend cap, `Call.KeyCap`, when the key has one
   (`KeyAccount(display)`, `k:<display>`; a token has no key);
2. its **member's** cap in the scope, `Call.MemberCap`: the member's own,
   else the scope's `defaultMemberCapMicros` (`rights.limitsOf`,
   `auth.go:99`), when there is one (`MemberAccount(scope, principal)`,
   `m:<scope>/<principal>`);
3. its **scope's** budget, `Call.Budget` (the scope's ID).

An uncapped key or member has no account: its spend is its scope's, and
the site keeps no counter for it. Each account is checked with the same
`overBudget` against its own limit, the first over it refusing:
`ErrKeyCap` (`budget.key_cap`), `ErrMemberCap` (`budget.member_cap`) or
`ErrBudget` (`budget.pool_exhausted`), all 402 (`limit.go:19`,
`refusalAt`). A completion adds the call's cost to every account it was
charged to in one write (`Increment.Accounts`, `store.go:38`), so the
accounts never disagree about which calls they hold. `MaxAccounts` is 3.
TestCaps, store_test.go "accounts".

### 1.2 Budget modes

**What a budget does is its scope's budget mode** (PRD §9.1, ADR 0050),
the ScopePolicy's `budgetMode`, which the compiler makes explicit for
every scope with a budget and the gateway reads into `Call.BudgetMode`
(`admission.ModeOf`, `admission.go:72`; any value it does not know is
`Enforce`, failing closed, as is `BudgetMode`'s zero value). One mode
governs every account of the scope: caps as much as the budget.

- **observe** (the default, so a rollout cannot stop a team's work on day
  one): admission never refuses on a budget or cap. The call is admitted
  and counted, and its ledger row's `over_budget` says it was over.
- **warn**: as observe, and the response carries `golem-warning`, the
  level's code and the share of its limit known spend has reached, e.g.
  `budget.pool_exhausted 104%` (`budgetWarning`, `proxy.go:787`).
- **enforce**: refused.

The limiter asks the store to check a limit only when it is enforced
(`refusing`, `limit.go:299`): otherwise the store checks the slots alone
and returns the known spends it computed, and `charged.over`
(`limit.go:154`) checks them against the limits with the same
`overBudget`, the declared-worst-case check included; the ticket answers
`OverBudget()` with the first account over (`admission.Over`: its level,
known spend and limit). So the Valkey script is the same in every mode,
and an outage's local check makes the same decision from its last known
spends. In-flight limits refuse in every mode. `TestBudgetModes`
(gateway/limit, on `Mem`, Valkey and an outage; gateway/proxy, through the
ledger row and the header), `TestWorstCaseOverBudget`, `TestCapModes`.

The other half of admission is concurrency: a principal's in-flight limit
and a scope's (`atLimit`, `store.go:132`). The scope limit matters because
a scope's pool is shared: without it the scope's exposure is the sum of its
members' limits (model/exposure.md §9).

## 2. Known spend: a snapshot and the site's minutes

Known spend is what every check reads. It must be cheap (every admission)
and exact across gateways that hold different snapshots. ADR 0048's
answer:

- Each site's store keeps a **counter per account and period**: the spend
  of each minute, by when its calls ended, and the period's total. A
  completion adds its cost to its minute. The counter holds amounts, not
  events, and keeps a retention's minutes (six hours: longer than the
  longest authorization lease plus the ingestion allowance, so a gateway
  on the oldest snapshot it may still use finds every minute it needs,
  provided the snapshot's boundary was recent when it was published: a
  boundary more than retention − lease − margin old is stale, which
  golem-control reports, §6).
- golem-control's snapshot carries, per scope, a **boundary B** (a whole
  minute, an ingestion allowance before the publication) and the
  **spend** of each account: the ledger's spend of the calls that ended
  before B, deduplicated by event, by scope, by member in the scope, and
  by capped key (§6).
- **Known spend = snapshot spend + the counter's minutes from B on.** A
  call's ledger row and its counter increment carry the same end (the row
  is stamped first, `proxy.go:464`, so the increment's minute is never
  before the row's), so a call is on one side of B: in the snapshot, or in
  the minutes counted. A gateway on an older snapshot has an earlier B and
  is just as exact.
- **Known spend is never below the counter's period total**: known =
  max(spend + minutes from B, total). The total is this site's own calls
  of the period, a floor whatever the snapshot: a B older than the
  retention reads only the minutes retained, and without the floor the
  spend between B and the retention's start would vanish
  (TestStaleBoundary). On a current snapshot the floor never binds (the
  snapshot's spend holds this site's calls before B, and other sites').

It holds while the ledger ingests within the allowance. A row later than
that, or minutes a Valkey failover lost, undercount until a later boundary
passes them or reconciliation books them: soft controls, stated in ADR
0048, not hidden. The job measures the first: each site's ingestion lag,
reported behind once it exceeds the allowance (`Job.Lags`, §6).

The gateway side is `Known` (`limit.go:49`): `Period`, `Spend`, `Since`
(B), applied per account by `ApplySnapshot` (`limit.go:540`): the
gateway's `applyKnown` (`cmd/golem-gateway/main.go:314`) applies each
scope's, each listed member's (`MemberAccount`) and each capped key's
(`KeyAccount`), all with the scope's period and B. The counter is left as
it is at an apply: a gateway still on an older snapshot needs the minutes
from its own boundary. A member's account counts from the snapshot as
soon as it has a cap; a cap set mid-period misses at most the calls
between that snapshot's boundary and the cap's publication, since the
site keeps no counter for an uncapped member. `snapshotLocked`
(`limit.go:251`) treats a snapshot of another period as none: a new month
starts from nothing. The period is the month the call *started* in
(`Call.Started`), so its counters and its ledger row agree even if it
completes next month (TestPeriodOfStart). `fromMinute` (`store.go:64`)
turns B into the first minute counted: B's own minute (golem-control
places boundaries on minutes, and one off a minute errs high), or 0 for
no boundary, which reads the period's whole total.

## 3. The limiter

### 3.1 Admission

`Limiter.Admit` (`limit.go:269`):

1. While the store is reachable (`Down()` false), one store call does the
   whole check atomically: `Store.Admit` with an `Admission`
   (`store.go:84`) carrying, per account, the snapshot's boundary and
   spend and the limit it is refused over (0 unless enforced), the
   gateway's clock, the worst case and both in-flight limits. Atomic
   matters: two gateways must not both pass the budget check and then
   both take the last slot.
2. `admitted` (`limit.go:309`) takes the answer: the known spends the store
   computed are remembered in `last` (all an outage will leave), a refusal
   names the account it was over (`Result.Over`), and an admitted call is
   tracked (`track`, `limit.go:377`) as a `Ticket`. During the grace after
   a recovery (§3.4) the ticket is marked waived; a call admitted over a
   limit its scope does not enforce is marked over budget (§1.2).
3. If the caller's context ended, that is the caller's error, not an
   outage (TestCallerCancelIsNoOutage). Any other store error is an
   outage: `MarkDown` (`limit.go:367`), then the local check.
4. `admitLocally` (`limit.go:331`) checks the last known spends and this
   gateway's own counts (`byPrincipal`, `byScope`, kept so the check is
   O(1)) under the lock, and the call is a **waiver** (§3.4).

Each store call is bounded by `StoreTimeout` (250 ms; `storeContext`,
`limit.go:353`). `Mem` is marked `inProcess` and gets no deadline: it has
no I/O, and a deadline costs two allocations per call.

### 3.2 Ending a call: complete, cancel, the tail

A ticket ends once (`claimEnd`, `limit.go:200`; TestTicketEndsOnce).

- `Complete` (`limit.go:424`) frees the local slot (`finish`,
  `limit.go:390`) and calls `account` (`limit.go:454`) with `release`:
  `send` (`limit.go:478`) calls `Store.Complete`, which adds the cost to
  its minute in every account's counter and frees the slot in one step.
- `Cancel` (`limit.go:435`) records what the gateway had seen
  (`Store.Add`) but **keeps the slot**: the provider may still be working,
  and billing, and freeing the slot would let cancel-and-resend escape the
  in-flight limit (model/exposure.md §5.4). The request path's ticket is
  `tailed` (`limit.go:881`): its `Cancel(ctx, seen, until)` also calls
  `EndAt` (`limit.go:490`), a timer that calls `Ended` (`limit.go:502`)
  at the provider-tail time (now plus `ProviderTail`, default two
  minutes, but no later than the call's deadline: `settlement.cancel`,
  `proxy.go:534`). `Ended` frees the slot locally and at the store.

`account` clamps a cost to [0, `snapshot.MaxMicros`] (2^50,
`limit.go:458`): a negative cost would take spend back, and sums past 2^53
are no longer exact in Valkey's Lua numbers. Only a garbled usage report
gives either; config refuses a budget or cap above the same bound
(`scopePolicy`, `lib/compile/policy.go:39`).

### 3.3 Renewal

Slots in Valkey expire after a TTL (90 s) unless renewed. `Run`
(`limit.go:815`) calls `Renew` (`limit.go:550`) every 30 s
(`renewInterval`), which re-registers the running calls' slots
(`reacquire`, `limit.go:563`) without checking limits: the calls are
already running. A call past its deadline plus a minute is no longer
renewed (`renewable`, `limit.go:599`; TestRenewalStopsAtDeadline). A
cancelled call is still running until `Ended`, so its tail is renewed.

The subtle part is ordering. `reacquire` copies the slots under the lock,
then calls the store outside it. A call that ends meanwhile would be
registered again: a phantom slot for a TTL. So `finish` records calls
ending while a renewal is in flight (`endedDuring`), and the last renewal
to finish takes them (`reacquired`, `limit.go:581`) and releases them
(TestRenewalDoesNotRevive).

### 3.4 Outages, waivers, replay and grace

When the store fails, the gateway goes on admitting on its own counts, and
records every such admission as a waiver (`Ticket.Waived`): across several
gateways local counts can add up past the limits, and the waiver is how
that exposure is known instead of silent (ADR 0041 §7.3). Calls already
running are never cancelled.

While down, completions and cancellations cannot reach the counters, so
`account` holds each as an `Increment` (`hold`, `limit.go:521`), with the
minute it ended in, and a sequence number (`held.seq`, increasing with
each one held): a replay takes off what it sent by that identity, never by
position or value. At most `MaxPending` (100,000) are held; past it the
oldest is dropped and counted (`Dropped`): its ledger row was written at
its call's end, so the snapshots count it once their boundary passes; only
the counter (the site's fast view) loses it (TestRecoverBatches).

`Run` calls `Recover` (`limit.go:626`) while down. It replays the held
increments in order, each into its own minute, in batches of 256
(`replayBatch`, `limit.go:677`, `Store.AddAll`, one script each, so a
batch is added whole or not at all), until at most one batch is left or
it has replayed everything held when it began (`heldBefore`: at
`MaxPending`, completions held during a replay can outnumber the batch);
re-registers every running call's slot (their TTLs lapsed during the
outage); marks the limiter up; and drains the rest. From the mark on,
`hold` refuses an increment (it checks `down` under the lock the mark
takes), so a completion that saw the store down just before goes to the
store itself: nothing is held after the last replay, and the drain can
only shrink, so recovery converges however busy the gateway is
(TestRecoverUnderLoad, TestCompletionRacingRecovery). A sent batch is
taken off by `dropReplayed` (`limit.go:708`): every increment held with a
seq up to the batch's last. Those gone meanwhile were taken by another
replay or dropped at the cap (an increment dropped while its batch was in
flight is then both counted and in `Dropped`, erring high). Taking a batch
off only while it was still the held list's prefix, as the code once did,
left it held whenever a hold at the cap dropped the oldest meanwhile: it
was sent again every round, and under steady traffic the replay never
ended (TestRecoverAtTheCap). An increment is an amount: a batch sent again
after a lost reply is counted twice, erring high. Two replays of the same
increment (`replayOne`, the tests' single step, racing another) both send
it, but it is taken off the held list once: the later finds it gone and
leaves the next held increment alone (TestOverlappingReplays).

A gateway that starts while its Valkey is unreachable starts in this mode.
`DialValkey` (`valkey.go:114`) dials once and, if that fails, returns the
store without a client: every operation fails as `ErrUnavailable` at once,
allocating nothing. The binary calls `MarkDown`
(`cmd/golem-gateway/main.go:268`), so the gateway admits waivers from its
first call, and runs `Valkey.Connect` (`valkey.go:144`) in the
background, retrying the dial a second after a failure, doubling to 30
seconds. The dial is retried whole, not a lazy single connection, so
valkey-go still decides between a cluster and a single node once Valkey
answers. `Recover` first asks the store whether it answers
(`Valkey.Ping`, `valkey.go:185`): a gateway with nothing held and nothing
running would otherwise touch no store and mark itself up while Valkey was
still down (TestStartWithValkeyDown).

Recovery is per gateway. A recovered gateway cannot know which peers have
not recovered, whose running calls Valkey still lacks, so for `Grace`
(two renewal intervals) its admissions are waivers too. Only a test ends
it early (`endGrace`, the model's graceEnds).

At shutdown `Finish` (`limit.go:844`) stops the tail timers (their slots
expire by TTL), tries one `Recover` if down, and abandons what is still
held (counted in `Dropped`; the ledger has their rows).

## 4. Rate limits and quota shares

Rate limits are requests per minute only (`rate.go`): a provider's token
limits are its 429s, which the attempt loop fails over on, and spend is
the budgets' (ADR 0044). They are not admission: the request path takes
them through `admission.Meters` (`admission.go:158`), which the limiter
implements (`Limiter.Take`, `rate.go:125`), and the store keeps them
beside the slots and counters.

- **The meters** (`admission.Meter`, `admission.go:144`): a member's rate
  limit in its scope (`MemberRateMeter`, `rm:<scope>/<principal>`), the
  scope's (`ScopeRateMeter`, `rs:<scope>`), both from the ScopePolicy's
  `rateLimits` (`Limits.MemberRPM`, `ScopeRPM`, `auth.go:44`), and a
  deployment's share of its provider's quota (`QuotaMeter`,
  `q:<deployment>`, the route target's `QuotaRPM`).
- **A bucket** per meter holds at most a minute's worth, refilled
  continuously, and starts full (`rateBucket`, `rate.go:23`; in Lua,
  `takeScript`, `rate.go:82`, on the server's clock, each bucket a hash
  of its requests and time kept two minutes). `Take` takes one request
  from every meter it is given, or from none, and reports the first
  without room and the wait until it has one.
- **A call's rate limits** are taken once, both together, before the pick
  and admission (`rateMeters`, `proxy.go:801`; `proxy.go:213`): past
  either, `limit.rate` (429) with a Retry-After, saying which, in every
  budget mode.
- **A quota share** is taken per attempt, as the pick tries a deployment
  (`takeQuota`, `gateway/proxy/attempt.go:111`): a deployment at its share
  is passed over as one at its ceiling is (model/attempts.md's
  `QUOTA_PER_ATTEMPT`), and a call that finds every deployment at its
  share is refused `limit.provider_quota` with the earliest Retry-After.
- **They fail open** (PRD §5.5): while the limiter is down, or if the
  store fails, `Limiter.Take` admits; a failure also marks the limiter
  down (so admissions are waivers until it recovers), and is logged at
  most once a minute (`failedOpen`, `rate.go:143`). A provider's own 429
  is the quota's backstop.

TestMetersFailOpen, store_test.go "rate buckets" (both stores),
gateway/proxy's TestRateLimits and TestQuotaFailover.

## 5. The stores

`Store` (`store.go:137`): every method is one atomic operation and one
round trip.

**Mem** (`store.go:171`) is the standalone gateway's store and the tests'.
One mutex makes each method atomic. The counter (`memCounter`) is a map of
minute to spend per account and period, plus the total; minutes past
`Retain` (six hours) go as it adds. `Mem.since` (`store.go:329`) reads the
window the Valkey script reads (below), so the two answer alike. `SetDown`
and `failover` (the tests') fake Valkey's faults: going down loses every
slot (an outage longer than the TTL), and a failover loses a minute of
every account.

**Valkey** (`valkey.go:41`) keeps every key under the hash tag
`{golem:<site>}` so a cluster puts them in one slot, as the scripts need.
Slots are sorted sets per principal and per scope, scored by expiry on the
server's clock (`TIME`), so no gateway's clock can extend one. The counter
is one hash per account and period, minute to spend plus a `total` field
(`counterKey`, `valkey.go:199`, its key built once per account and
period). The scripts' duration arguments are formatted once (`durArgs`,
`valkey.go:76`): admission is hot. `Now` is the gateway's clock for a
read's last minute (the stores' differential found reads on the wall
clock while admissions used the gateway's).

The scripts (`valkey.go:226` on) are built from Lua fragments: `take`
(the slots), `since` (the read), `trim` and `add` (the counter's write).
Each script's comment lists its KEYS and ARGV.

- `admitScript` (`valkey.go:312`): per account, `since` reads one `HGET`
  per minute from the boundary's to the gateway's own minute plus one
  (`clockSkewMinutes`, a peer's clock running ahead), no further back than
  the retention, or the `total` field alone for no boundary; known is the
  snapshot's spend plus that, and with a boundary never below `total`
  (`Mem.known` the same); then each account's check in order; then
  `take`. It returns `{outcome, the account refused over, the known spend
  of each}`.
- `completeScript` (`valkey.go:371`): `add` to each account's counter (an
  `HINCRBY` of the call's minute and of `total`, the counter's life
  extended, and `trim`: once the hash holds more than a retention's
  minutes and a margin of 60, the older ones go), then the slots freed.
- `addScript` (`valkey.go:359`): `add` for several increments, for a
  cancellation and a replay's batch.
- `registerScript` (`valkey.go:341`): renewal and re-acquisition, in
  chunks of 256 slots so one script never holds Valkey's thread for long.
- `sinceScript` (`valkey.go:383`): `since` alone, for `Limiter.Known`.
- `takeScript` (`rate.go:82`): the rate buckets (§4).

With `ReplicaAcks`, each add waits for replicas (`WAIT`) so a failover is
less likely to lose it. WAIT counts only its own connection's writes, so
`write` (`valkey.go:447`) sends the script and WAIT together in one
pipeline (falling back to EVAL by hand on NOSCRIPT); a short
acknowledgement is counted in `Unreplicated`, not an error
(TestReplicaAcks). A connection failure is `ErrUnavailable` (an outage),
keeping its cause; a script error stays itself (`wrap`, `valkey.go:587`;
TestWrap).

## 6. The ledger and the known-spend job

The ledger row (`golem.spend`, `spend.sql:7`) is written when the call
ends, whatever the counters' state (ADR 0043), and carries `ended_at`.
Delivery is at least once, so some events arrive twice.
`golem.spend_by_event` (`spend.sql:84`) is a narrow
`ReplacingMergeTree` filled from `golem.spend` by a materialized view
(`spend.sql:100`), keyed by scope and event in the period's partition, so
duplicates merge away; `FINAL` makes that exact at read time.
(`golem.spend_dedup`, `spend.sql:59`, is the whole-table view balances and
rollups read; the job does not, as it would sort the wide table every
minute.)

The job (`control/spend`):

- `Job.Step` (`spend.go:159`; the spend half `stepSpend`, `:208`) places
  B: now minus the `Allowance` (five minutes, `-spend-allowance`),
  truncated to a minute, in the period now falls in. A B still in the
  previous period gives this period nothing, without asking the ledger.
  Otherwise it asks the `Ledger` for each account's spend before B
  (`Totals`: by scope, by member, by key). On a failure the last result
  is kept if it is of this period (its earlier B is still exact) and
  forgotten if not (`forgetOtherThan`): a new month's spend is not the
  old one's (TestJob). A result whose B is more than `MaxAge` old is
  **stale** (`Job.Stale`): `Retention` − `Lease` − `Margin` (six hours,
  four, ten minutes by default: one hour fifty). A gateway may use a
  snapshot for its whole lease, and reads the counter no further back
  than its retention, so with a B older than that the spend between B
  and the retention's start is in neither the snapshot's spend nor the
  minutes read. A stale result is still published: the gateways floor
  known spend at their counter's period total (§2), so it never counts
  less than no result would (this site's calls), and its spend adds other
  sites' calls before B. What it misses is other sites' spend since, so
  `Step`'s error says the result is stale, and `Stale` and `Job.Age`
  (over `Known.Age`) are exported as `golem.control.spend.stale` and
  `.age` for an alert (TestJobReportsStaleResult). `Job.Validate` refuses
  a configuration whose `MaxAge` is no more than the allowance and a
  minute: every result would be stale from its first publication.
- **Ingestion lag.** B assumes every call that ended before it is in the
  ledger, which holds only while each site's rows arrive within the
  allowance. With a `Ledger` that is also an `Ingestion` (`spend.go:59`;
  `ClickHouse` is), `Step` also reads each site's latest ingested
  `ended_at` (`stepLatest`, `spend.go:166`), and `Job.Lags`
  (`spend.go:191`) reports each site's lag, now less that row's end, as a
  `SiteLag`: **behind** when the lag exceeds the allowance (B is past the
  site's latest row, so its calls that ended between the two and have not
  arrived are in neither the snapshot's spend nor the minutes counted),
  or when a site of an earlier read has no rows any more (`NoRows`: the
  table rebuilt, or another ClickHouse). B is not moved for a lagging
  site; the lag is reported, for an alert. A failed read keeps the last
  one, so the lags grow with the clock while ClickHouse is down. A site
  that served nothing lately is behind as well: the ledger cannot tell
  idle from late, so an alert on it wants the site's traffic beside it
  (TestJobReportsIngestionLag; TestClickHouseIngestion against a real
  ClickHouse).
- `ClickHouse.Spend` (`spend.go:308`) is one query (`spendQuery`,
  `spend.go:301`) over ClickHouse's HTTP interface, grouped by scope,
  principal and key, the period and B bound as parameters, never in the
  query's text; `readSpend` (`spend.go:390`) sums its rows into each
  account (TestClickHouse against a fake; TestClickHouseLedger against a
  real ClickHouse, below). `ClickHouse.Latest` (`spend.go:327`,
  `latestQuery`) reads `golem.ingest_by_site` (`spend.sql:111`), an
  `AggregatingMergeTree` of each site's `max(ended_at)` filled by a
  materialized view from `golem.spend`: a few rows to read whatever the
  ledger's size, exact over unmerged parts since the query takes `max`
  again.
- `Job.Current` is what a publication carries. The controller runs `Step`
  whenever a publication is due (`controller.go:347`) and `withKnownSpend`
  (`controller.go:549`) stamps every scope of the entitlements with the
  result's period, B and spend; every member who has spent (listed even
  without a cap of its own, since the scope's default cap applies to it);
  and every capped key (TestKnownSpend). With no result (no
  `-clickhouse`, or a failure in a new period) a snapshot carries none,
  and gateways count their counters' whole totals for the period.

TestClickHouseLedger (`control/spend/clickhouse_test.go:27`) runs
spend.sql in a database of its own on a real ClickHouse
(`GOLEM_CLICKHOUSE_URL`; it skips without one) and checks the query: an
event inserted twice and not yet merged counts once, only the period's
rows before B count, and the sum equals `golem.spend_dedup`'s. Without
`FINAL` it fails.

## 7. Worked examples

One site, gateways g1 and g2 over one Valkey; scope `team-a` with budget
10, principal limit 1, scope limit 2, no caps and no rate limits;
principals p1, p2, p3. Amounts are small numbers of micro-units, for
reading. At 12:00 golem-control published snapshot S1, which both
gateways hold: period 2026-11, B 11:55, spend 0 (nothing ended before
11:55).

### 7.1 Two gateways admitting for one scope

1. **12:00:05, g1 admits a (p1).** `Admit` (`limit.go:269`) names the
   period from the call's start (`period`, `limit.go:232`), finds one
   account (the scope's: `chargedOf`), takes S1's `Known` for it and
   builds the `Admission`. `Valkey.Admit` (`valkey.go:400`) sends
   `admitScript`: `since` reads minutes 11:55 to 12:01, all empty, so
   known is 0; 0 < 10; `take` trims expired slots, finds p1's set and the
   scope's empty, and adds `a` to both, expiring at the server's now plus
   90 s. It returns `{0, 0, 0}`; `admitted` records last = 0 and tracks
   `a`.
2. **12:00:06, g2 admits b (p2).** The same on the other gateway; the
   scope's set is {a, b}.
3. **12:00:07, g1 tries c (p3).** `take` finds the scope's set full and
   the script returns `{2, 0, 0}`: `ErrConcurrency`. Had g1 and g2 raced
   for the last slot, Valkey would run one script after the other, so one
   wins; the check and the take are never split.
4. **12:00:40, a completes on g1, costing 3.** The proxy writes a's row
   (ended 12:00:40) first (`proxy.go:464`), then `complete`: `Complete`
   (`limit.go:424`) ends the ticket, frees its local counts, and `account`
   stamps the increment 12:00:40. `completeScript` adds 3 to minute 12:00
   and to `total`, and removes `a` from both slot sets.
5. **12:01:10, g2 admits c (p3).** The scope's set is {b}; known is
   0 + the minutes from 11:55 = 3 < 10.

Had p1 a member cap of 2 (its own, or the scope's default), a's
admission would have named two accounts, `m:team-a/p1` then `team-a`,
and a's completion added 3 to both counters in the one script; p1's next
call would find its account's known spend 3 ≥ 2 and be refused
`budget.member_cap` under enforce, or admitted and marked over its cap
under observe or warn.

### 7.2 A cut call held for its tail

6. **12:02:10, b's upstream cuts** after 2 has been produced. The proxy
   writes b's row (ended 12:02:10, cut), then `settlement.cancel`
   (`proxy.go:534`) computes the tail's end (12:04:10, two minutes, unless
   the call's deadline is sooner) and calls `tailed.Cancel`
   (`limit.go:883`): `account` without release sends `addScript`, which
   adds 2 to minute 12:02, and `EndAt` sets a timer. b's slot stays in
   both sets, and b stays among g2's running calls.
7. **12:02:30, p2 sends d to g2.** p2's set still holds b:
   `ErrConcurrency`. The provider may still be working on b, and this is
   what stops cancel-and-resend escaping the limit. g2's renewal at about
   that time (`reacquire`) re-registers b's slot with the other running
   calls'.
8. **12:04:10, b's tail ends.** The timer calls `Ended`
   (`limit.go:502`): `finish` drops b locally, and `Store.Release` removes
   it from both sets. p2 can start a call again. Had the provider billed b
   more than 2, the difference is spend golem never heard of:
   reconciliation books it.

### 7.3 A snapshot's known spend and boundary reaching the gateways

9. **12:06:00, golem-control publishes.** `Job.Step` (`spend.go:159`)
   places B at 12:01 (12:06 minus five minutes) and asks ClickHouse for
   the spend of 2026-11's rows that ended before 12:01:00.000: a's 3. The
   controller stamps `team-a` with period 2026-11, B 12:01, spend 3, and
   p1 (who has spent) as a member with 3 (`withKnownSpend`,
   `controller.go:549`).
10. **g1 applies the new snapshot S2; g2 is still on S1.**
    `applyKnown` → `ApplySnapshot` replaces g1's `Known` for the scope
    (and for p1's member account, unused while p1 has no cap) and leaves
    the counters alone. g1's known spend: 3 + the minutes from 12:01 (b's
    2 at 12:02) = 5. g2's: 0 + the minutes from 11:55 (a's 3 and b's 2) =
    5. Each call is counted once on each gateway, whichever snapshot it
    holds.
11. **A late row.** b's row ended at 12:02:10. If it reaches ClickHouse by
    12:07, the 12:08 publication (B 12:03) counts it in its spend, and the
    minutes from 12:03 do not: exact. If it is later than the allowance,
    say at 12:09, the 12:08 snapshot misses it while its minutes from 12:03
    lack it too: an undercount of 2 until a publication after the row is
    ingested (ADR 0048's stated limit). From 12:07 on, `Job.Lags` reports
    b's site behind (its latest row ended at 12:02:10, more than the
    allowance back) until the row arrives.

## 8. The models and how the tests use them

- **model/accounting.md**: one principal, one gateway, a working Valkey:
  is the overshoot within the budget plus one worst case per slot, is
  nothing counted twice, and do the books match after reconciliation?
  Checked by `model/check-accounting.sh` (random simulation, and an
  exhaustive search of the small instance); no Go test drives it.
- **model/exposure.md**: a shared scope, several gateways, late ingestion,
  outages, failovers, cancelled calls, and member accounts (`MEMBER_CAP`,
  `MEMBER_KNOWN`: a member counted from snapshots alone is blind to its
  calls within a publication's lag). Its properties (§6): the limits hold
  whenever Valkey is reachable; the scope's stated bound, plus what is
  reported (uncounted spend, waivers, held spend); and the hand-off (never
  counted twice, no increment lost) while the ledger ingests within its
  allowance. Each broken design of §9 must break its property.

The Go tests use exposure.md four ways:

- `gateway/limit/exposure_mbt_test.go` (TestExposureModel,
  TestExposureModelValkey; connect): the model's traces drive one
  `Limiter` per gateway over one store, `Mem` or Valkey (whose outages
  and failovers `valkey_faults_test.go` injects), on a clock the model
  moves; after every step the counter's minutes, slots, waivers, held
  increments and each gateway's known spend must be the model's. The
  ledger and golem-control are the environment.
- `gateway/limit/differential_test.go`: the two stores side by side.
  TestStoresAgree (`differential_test.go:112`) runs each trace on Mem and
  Valkey at once: each must match the model, and the two must match each
  other (state and every gateway's known spend). TestStoresAgreeVaried
  (`differential_test.go:129`) replays the traces with inputs the model
  never picks, drawn from a seed of the step so a failure replays (costs
  negative, zero and past 2^53; budgets of zero and below; a retention
  shorter than the trace), and compares what reads from every minute
  return. It found a negative budget admitted without bound by the Lua
  check, sums past 2^53 rounded by Lua (hence the `MaxMicros` clamp),
  `Mem` reading minutes past the retention that Valkey does not, and
  Valkey's reads on the wall clock (hence `Valkey.Now`).
- `exposure_hegel_test.go` (Hegel, behind the `hegel` build tag): Hegel
  drives the real limiter, and the model judges every run;
  TestNoScopeLimitCaught shows a planted mutant is caught.
  `stale_hegel_test.go` (the same tag) is a scripted regression: an
  outage's admission from a stale view conforms.

Run them with `QUINT_BACKEND=typescript go test ./gateway/limit/`, and
the Hegel tests with `-tags hegel` added. The Valkey tests use
`GOLEM_VALKEY_ADDR`, or start a `valkey-server` (or `redis-server`), and
skip without one; TestReplicaAcks needs a local server binary to attach a
replica to.

## 9. Invariants: where enforced, where tested

| Invariant | Enforced | Tested |
|---|---|---|
| Budget check and slot taken in one atomic step | `admitScript` (`valkey.go:312`); `Mem`'s lock | store_test.go "limits", "budget"; exposure MBT |
| Limits hold while the store is reachable | `take` (`valkey.go:240`), `Mem.acquire` (`store.go:223`) | Hegel (TestLimiterAgainstExposureModel, TestNoScopeLimitCaught) |
| A call checked against its key's cap, its member's and its scope's budget, in that order, the first over refusing; each counted in one write | `chargedOf`, `charged.over`; `admitScript`, `completeScript`, `Mem.Admit`, `Mem.add` | TestCaps; store_test.go "accounts"; TestCapModes (gateway/proxy) |
| The two stores answer alike | one check (`overBudget`, mirrored in Lua); one read window (`since`, `Mem.since`, `clockSkewMinutes`) | TestStoresAgree, TestStoresAgreeVaried |
| A call on one side of the boundary | the row stamped before the increment (`proxy.go:464`); `fromMinute`; the ledger deduplicated by event | TestBoundary; both MBT tests; TestClickHouseLedger |
| Known spend exact on every gateway, any snapshot (within the allowance) | `since`, `Mem.since`; `ApplySnapshot` leaves the counter | TestBoundary, store_test.go "counter", both MBT tests |
| Known spend never below the site's period total | `admitScript`'s floor, `Mem.known`, `Limiter.Known` | TestStaleBoundary, store_test.go "known spend is never below the counter's total", TestStoresAgreeVaried (`Admits`) |
| A boundary older than the retention allows is reported stale | `Job.MaxAge`, `Job.Stale`, `Step`'s error | TestJobReportsStaleResult |
| The snapshot's spend counts each event once, before B only | `spendQuery` over `spend_by_event FINAL` | TestClickHouseLedger, TestClickHouse |
| A boundary on a whole minute, an allowance back; a new period from nothing | `Job.Step` | TestJob, TestKnownSpend |
| A site whose rows arrive later than the allowance (or no longer at all) is reported behind; B is not moved for it | `Job.Lags`, `stepLatest`, `ClickHouse.Latest` over `golem.ingest_by_site` | TestJobReportsIngestionLag, TestClickHouseIngestion |
| Sums exact | `account`'s clamp to `MaxMicros`; config's budget and cap bounds | TestStoresAgreeVaried |
| The counter bounded | retention: `trim`, `Mem.add` | TestBoundary; Valkey "a counter keeps a retention's minutes and its whole total" |
| A cancelled call keeps its slot for its tail | `Cancel`, `EndAt`, `Ended` | TestLimits, TestAdmitter |
| A renewal never revives an ended call | `endedDuring`, `reacquired` | TestRenewalDoesNotRevive |
| Running calls re-registered after an outage | `Recover` → `reacquire` | TestOutage; exposure MBT |
| A gateway starts with its Valkey unreachable, admits waivers, and recovers once a retried dial connects; an idle gateway is not marked up while Valkey is down | `DialValkey`, `Valkey.Connect`, `MarkDown`, `Recover`'s `Ping` | TestStartWithValkeyDown; TestPoolModeValkeyDown (cmd/golem-gateway) |
| Outage and grace admissions are waivers | `admitLocally`, `graceUntil` | TestOutage, TestCallerCancelIsNoOutage; MBT waivers |
| Only an enforced budget or cap refuses; observe and warn admit and mark the call over, warn with golem-warning; in-flight limits in every mode | `refusing`, `charged.over`, `track`; `Handler.overBudget` (gateway/proxy) | TestBudgetModes (gateway/limit and gateway/proxy), TestWorstCaseOverBudget, TestCapModes, TestBudgetWarning |
| A ticket ends once | `claimEnd`, `finish` | TestTicketEndsOnce, TestOutageCounts |
| A call is counted in the period it started | `Admit`'s period, `Ticket.period` | TestPeriodOfStart, TestPeriodRollover |
| Held increments bounded and replayed in order | `hold`, `replayBatch` | TestRecoverBatches |
| A replayed increment taken off once, by identity | `held.seq`, `dropReplayed` | TestOverlappingReplays, TestRecoverAtTheCap |
| Recovery converges under load; no increment held after it | `Recover`'s mark before the drain, its first loop bounded (`heldBefore`); `hold` refusing once up | TestRecoverUnderLoad, TestCompletionRacingRecovery, TestRecoverAtTheCap |
| A request taken from every meter or none; a bucket holds a minute's worth | `Mem.Take`, `takeScript` | store_test.go "rate buckets" |
| Rate limits and quota shares fail open | `Limiter.Take` | TestMetersFailOpen |
| A deployment at its quota share passed over; every one at its share refused with the earliest Retry-After | `takeQuota`, the pick | TestQuotaFailover, TestRateLimits (gateway/proxy) |

## 10. Known gaps

- **Late rows and lost minutes undercount.** A row later than the
  ingestion allowance, or minutes a Valkey failover lost, are missing from
  known spend until a later boundary passes them or reconciliation books
  them (ADR 0048). `ReplicaAcks` narrows the second; the first is
  measured: `Job.Lags` reports each site's ingestion lag and whether it is
  past the allowance. It is not yet exported as a metric
  (`golem.control.spend.ingestion_lag`, to come), and a quiet site reads
  as lagging.
- **Other sites' spend after B.** The counters are the site's own: another
  site's calls that ended after B reach a gateway only with a snapshot
  whose B has passed them (ADR 0048).
- **A cap set mid-period** counts from the ledger's spend at the next
  snapshot: the calls between that snapshot's boundary and the cap's
  publication are missing from the member's or key's account (the site
  kept no counter for it while uncapped).
- **A ledger outage longer than `MaxAge`** (about two hours by default):
  the snapshots carry the stale result until ClickHouse answers, and each
  gateway counts at least its own site's period total, so other sites'
  spend since the stale boundary is missing meanwhile. `Job.Stale` and
  `Job.Age` are exported as `golem.control.spend.stale` and `.age`, and
  alerted on (operations.md §2.2, *Known spend stale*).
- **A replay counted twice.** A batch whose reply was lost and is sent
  again, or one increment two replays sent at once, is added twice: errs
  high.
- **Sums are exact per increment, not per account.** The clamp bounds each
  increment to 2^50 and config bounds a budget; an account's sum could
  still pass 2^53 (where Lua's numbers round) only with several garbled
  usage reports each counted at the bound.
- **Grace is an assumption.** The model lets grace end only once every
  gateway has recovered; the code ends it after `Grace`, assuming each
  peer notices within a renewal interval.
- **Rate limits fail open.** While Valkey is unreachable no rate limit or
  quota share is checked; the provider's 429s are the quota's backstop.
  A call's rate limits are taken before admission, so a call then refused
  by a budget or an in-flight limit has still used a request of its
  minute.
- **The models check enforce only.** model/exposure.qnt and the Hegel tests
  drive the limiter with its zero mode, enforce; observe and warn never
  refuse on a budget or cap, so their exposure is unbounded by design
  (they are rollout and monitoring modes, PRD §9.1). The connect tests
  run `exposureDesign`, whose `MEMBER_CAP` is 0: member accounts are
  checked by the model's own instances and by TestCaps, not by a Go
  trace.
- **The models assume the declared worst case bounds every call** and
  have one budget period; real calls can exceed their declared worst case
  (ADR 0044).
- **The job's query grows with the month.** `FINAL` reads the period's
  partition of `spend_by_event` each publication (measured: 170 ms for 5
  million rows); past hundreds of millions of calls a month, spend.sql
  names the next step, a per-minute rollup of the merged minutes.
- **No known spend without the job.** A golem-control run without
  `-clickhouse` publishes no spend or boundary, and gateways count their
  counters' whole totals for the period: each site counts its own calls
  only, and what a failover or a flush of Valkey loses stays lost.
