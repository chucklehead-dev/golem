# Soft budgets with a stated bound: the accounting model

A literate Quint specification, like [publication.md](publication.md): the
prose explains, the `quint` blocks are the model, `make` extracts and
checks it. It models [ADR 0031](../docs/adr/0031-soft-budgets.md): budgets
are soft and eventually consistent, by accepted risk, and the overshoot is
bounded so long as no call costs more than its declared worst case. That
condition is the model's, not the world's: [ADR 0044](../docs/adr/0044-budgets-are-controls-not-ceilings.md)
makes budgets controls, not ceilings, and this model checks the
mechanism's arithmetic, not a guarantee about spend.

(An earlier version of this model checked a durable journal written before
every dispatch. It showed what a hard ceiling would cost; ADR 0031 decided
not to pay it. That version is in the repository's history.)

## 1. The question

A call's true cost is known only when it ends. golem checks each call
against the spend it already knows about and adds the call's cost when it
ends. So a principal near its budget can go over it with the calls it has
in flight. ADR 0031 accepts that, and states how far over it can go. The
questions:

> Is the overshoot really within the stated bound? Is anything ever
> counted twice? And do the books still match the provider's bill after a
> crash loses some events?

The answers hold if four rules do, each a switch as in the publication
model, plus a fourth switch for the declared-worst-case check:

| Switch | When on (the design) | When off (the bug it prevents) |
|---|---|---|
| `LIMIT_ON` | A principal has at most `LIMIT` calls in flight | Any number of calls admitted at the same moment overshoot without bound |
| `RENEW_SLOTS` | A call renews its concurrency slot while it is in flight (ADR 0038) | A long stream's slot expires mid-call, a new call takes it, and the in-flight limit is silently exceeded |
| `DEDUP` | Balances count each event once, by event ID | At-least-once delivery counts duplicates |
| `RECONCILE` | The daily job books the difference between the provider's bill and the ledger as an adjustment | Events a crash lost are never paid for in golem's books |
| `WORST_CHECK` | (a scope option, not a fix) A call is admitted only if known spend plus its declared worst case fits | The soft check: still bounded, one call looser |

## 2. Reading and running

`make` extracts this file into `accounting.qnt` and runs
`check-accounting.sh`, with the same four checks as the publication model:
the designs hold, every scenario is reachable, every broken design breaks
its property, every story behaves as §10's table says.

```quint accounting.qnt +=
// Generated from accounting.md by lmt. Edit the markdown, not this file.
module accounting {
```

## 3. The world

One principal with a `BUDGET` for the period, on one site, making a few
`REQUESTS`. Each call costs something from `COSTS`; the largest any call
can cost is `WORST` (the declared worst case the declared-worst-case check computes;
the model takes it as a true bound, which the real one is not: §9).
`LIMIT` is the principal's in-flight limit.

```quint accounting.qnt +=
  const REQUESTS: Set[str]
  const COSTS: Set[int]
  const WORST: int
  const BUDGET: int
  const LIMIT: int

  const LIMIT_ON: bool
  const RENEW_SLOTS: bool
  // The declared-worst-case check (ADR 0044): admission against the
  // request's declared bound, not a real-world spending bound.
  const WORST_CHECK: bool
  const DEDUP: bool
  const RECONCILE: bool
```

Across sites, known spend also lags by one snapshot renewal; the model
keeps one site, where the lag is zero, and §9 says what the lag adds.

## 4. State

- `phase`: each request is `idle`, `inflight`, `done` (its cost is known
  and counted) or `lost` (the gateway crashed while it was in flight: the
  provider billed it, golem never heard).
- `billed`: what the provider charged for each request (a ghost: golem
  never sees it directly).
- `known`: the known spend every check reads. In production it is the
  ledger's balance from the snapshot plus the site's Valkey counter since;
  with one site and no lag, one number.
- `buffer`: spend events a gateway has emitted but not yet delivered (the
  OTLP exporter's queue), lost on a crash.
- `ledger`: the rows ClickHouse holds, duplicates included; `adjustment`:
  what reconciliation has booked.
- `uncounted`: a ghost, the spend that never reached `known` (crash-lost
  calls, a Valkey failover's lost increments). The bound has to allow for
  it until reconciliation and the next snapshot catch up.
- `closed`: whether the books have been closed (reconciled) since the last
  change.
- `slotless`: calls still in flight whose concurrency slot has expired
  (only possible without `RENEW_SLOTS`); they no longer count against the
  limit.

```quint accounting.qnt +=
  var phase: str -> str
  var billed: str -> int
  var known: int
  var buffer: Set[(str, int)]
  var ledger: List[(str, int)]
  var adjustment: int
  var uncounted: int
  var closed: bool
  var slotless: Set[str]
```

## 5. The initial state and helpers

```quint accounting.qnt +=
  action init = all {
    phase' = REQUESTS.mapBy(r => "idle"),
    billed' = REQUESTS.mapBy(r => 0),
    known' = 0,
    buffer' = Set(),
    ledger' = [],
    adjustment' = 0,
    uncounted' = 0,
    closed' = false,
    slotless' = Set(),
  }

  def inflight: int = REQUESTS.filter(r => phase.get(r) == "inflight").size()
  def holdingSlots: int =
    REQUESTS.filter(r => phase.get(r) == "inflight" and not(slotless.contains(r))).size()
  def totalBilled: int = REQUESTS.fold(0, (acc, r) => acc + billed.get(r))
  def ledgerSet: Set[(str, int)] = ledger.foldl(Set(), (s, e) => s.union(Set(e)))
  def ledgerSpend: int =
    if (DEDUP) ledgerSet.fold(0, (acc, e) => acc + e._2)
    else ledger.foldl(0, (acc, e) => acc + e._2)
```

## 6. Actions

### 6.1 Admit a call

The check of ADR 0031. Soft: known spend is still below the budget.
Declared-worst-case check: known spend plus the call's worst case fits. Either way the
principal needs a free in-flight slot (`LIMIT_ON`).

```quint accounting.qnt +=
  action admit(request: str): bool = all {
    phase.get(request) == "idle",
    if (LIMIT_ON) holdingSlots < LIMIT else true,
    if (WORST_CHECK) known + WORST <= BUDGET else known < BUDGET,
    phase' = phase.put(request, "inflight"),
    billed' = billed, known' = known, buffer' = buffer, ledger' = ledger,
    adjustment' = adjustment, uncounted' = uncounted, closed' = closed,
    slotless' = slotless,
  }
```

### 6.2 A call ends

The provider bills the call; the gateway prices its usage, adds it to the
site's counter and emits the spend event.

```quint accounting.qnt +=
  action complete(request: str, cost: int): bool = all {
    phase.get(request) == "inflight",
    phase' = phase.put(request, "done"),
    billed' = billed.put(request, cost),
    known' = known + cost,
    buffer' = buffer.union(Set((request, cost))),
    ledger' = ledger, adjustment' = adjustment, uncounted' = uncounted,
    closed' = false, slotless' = slotless.exclude(Set(request)),
  }
```

### 6.3 A gateway crashes

Every call in flight is billed by the provider (at `cost`, here the same
for all of them) and never counted; the exporter's queue is lost. This is
the risk ADR 0031 accepts instead of a durable journal.

```quint accounting.qnt +=
  action crash(cost: int): bool = all {
    val lostNow = REQUESTS.filter(r => phase.get(r) == "inflight")
    all {
      phase' = REQUESTS.mapBy(r => if (lostNow.contains(r)) "lost" else phase.get(r)),
      billed' = REQUESTS.mapBy(r => if (lostNow.contains(r)) cost else billed.get(r)),
      uncounted' = uncounted + lostNow.size() * cost,
    },
    buffer' = Set(),
    known' = known, ledger' = ledger, adjustment' = adjustment,
    closed' = false, slotless' = Set(),
  }
```

### 6.4 Valkey fails over

The site's counter falls back to what the ledger knows (the next
snapshot's known spend): increments the ledger does not have yet are out of
`known` until it catches up.

```quint accounting.qnt +=
  action loseCounter: bool = all {
    val fallback = ledgerSpend + adjustment
    all {
      known > fallback,
      known' = fallback,
      uncounted' = uncounted + (known - fallback),
    },
    phase' = phase, billed' = billed, buffer' = buffer, ledger' = ledger,
    adjustment' = adjustment, closed' = closed, slotless' = slotless,
  }
```

### 6.5 Delivery to the ledger

An event leaves the exporter's queue and lands in ClickHouse; separately,
at-least-once transport may deliver an event a second time.

```quint accounting.qnt +=
  action deliver(request: str, cost: int): bool = all {
    buffer.contains((request, cost)),
    buffer' = buffer.exclude(Set((request, cost))),
    ledger' = ledger.append((request, cost)),
    phase' = phase, billed' = billed, known' = known, adjustment' = adjustment,
    uncounted' = uncounted, closed' = closed, slotless' = slotless,
  }

  action duplicate(request: str, cost: int): bool = all {
    ledgerSet.contains((request, cost)),
    ledger' = ledger.append((request, cost)),
    phase' = phase, billed' = billed, known' = known, buffer' = buffer,
    adjustment' = adjustment, uncounted' = uncounted, closed' = closed,
    slotless' = slotless,
  }
```

### 6.6 Closing the books

Once nothing is in flight or queued, the daily job compares the provider's
usage with the ledger and, with `RECONCILE`, books the difference as an
adjustment (unattributed where the provider's data cannot name the
principal). The next snapshot then carries the corrected known spend.

```quint accounting.qnt +=
  action closeBooks: bool = all {
    inflight == 0,
    buffer.size() == 0,
    not(closed),
    val adj = if (RECONCILE) totalBilled - ledgerSpend else adjustment
    all {
      adjustment' = adj,
      known' = if (known < ledgerSpend + adj) ledgerSpend + adj else known,
    },
    closed' = true,
    phase' = phase, billed' = billed, buffer' = buffer, ledger' = ledger,
    uncounted' = uncounted, slotless' = slotless,
  }
```

### 6.7 A slot expires

Concurrency slots carry a TTL so that a crashed gateway's slots free
themselves. With `RENEW_SLOTS`, a live call renews its slot (every 30
seconds, TTL 90: ADR 0038), so only a dead gateway's slots expire. Without
it, a long agent stream's slot can expire while the stream is still
running.

```quint accounting.qnt +=
  action expireSlot(request: str): bool = all {
    not(RENEW_SLOTS),
    phase.get(request) == "inflight",
    not(slotless.contains(request)),
    slotless' = slotless.union(Set(request)),
    phase' = phase, billed' = billed, known' = known, buffer' = buffer, ledger' = ledger,
    adjustment' = adjustment, uncounted' = uncounted, closed' = closed,
  }
```

### 6.8 The step

```quint accounting.qnt +=
  action step = {
    nondet request = REQUESTS.oneOf()
    nondet cost = COSTS.oneOf()
    any {
      admit(request),
      complete(request, cost),
      crash(cost),
      loseCounter,
      deliver(request, cost),
      duplicate(request, cost),
      closeBooks,
      expireSlot(request),
    }
  }
```

## 7. Properties

### 7.1 The bound

Every admitted call passed its check when known spend excluded only the
calls then in flight. Under the soft check the last admission saw known
spend below the budget, with up to `LIMIT` calls in flight, each at most
`WORST`; under the declared-worst-case check the last admission's own worst case fit,
which saves one call. Spend that never reached `known` (crashes, failovers)
widens the bound until the books catch up.

```quint accounting.qnt +=
  val slack = if (WORST_CHECK) (LIMIT - 1) * WORST else LIMIT * WORST
  val withinBound = totalBilled <= BUDGET + slack + uncounted
```

The declared-worst-case check with `LIMIT = 1` has no slack here: spend stays
within the budget, apart from crash losses, because every call costs at
most `WORST`. A real call can cost more than its declared worst case (§9),
and each such call widens the bound by its excess: nothing in the design
prevents it, and reconciliation books it after the fact (ADR 0044).

### 7.2 Never counted twice, and books that match

```quint accounting.qnt +=
  val noDoubleCount = ledgerSpend <= totalBilled

  val booksMatch = closed implies ledgerSpend + adjustment == totalBilled

  val safe = withinBound and noDoubleCount and booksMatch
```

### 7.3 Possibility

The design must be able to show the accepted risk (spend really does go
past the budget, within the bound), refuse once the budget is reached,
lose events in a crash and still close the books exactly, and absorb a
duplicate.

```quint accounting.qnt +=
  val overshoots = totalBilled > BUDGET
  val refusesWhenSpent = known >= BUDGET and REQUESTS.exists(r => phase.get(r) == "idle")
  val crashThenBalanced = uncounted > 0 and closed and ledgerSpend + adjustment == totalBilled
  val duplicateAbsorbed = ledger.length() > ledgerSet.size() and closed and booksMatch
```

## 8. The stories

**Two calls in flight take a principal past its budget** (the accepted
risk). With 4 of 6 spent, two calls are admitted under the soft check and
both cost 3: the principal ends at 10, over its budget by 4, within the
bound of `LIMIT × WORST = 6`. Under the declared-worst-case check neither is admitted
(4 + 3 exceeds 6).

```quint accounting.qnt +=
  run siblingsOvershootTest =
    init
      .then(admit("r1"))
      .then(complete("r1", 2))
      .then(admit("r2"))
      .then(complete("r2", 2))
      .then(admit("r3"))
      .then(admit("r4"))
      .then(complete("r3", 3))
      .then(complete("r4", 3))
      .expect(overshoots and withinBound)
```

**Without a concurrency limit there is no bound** (`noLimit`). Nothing is
spent yet, so all five calls pass the soft check at once; each costs 3: the
principal ends at 15, past the budget plus `LIMIT × WORST` (12). With more
calls admitted together it would go further still.

```quint accounting.qnt +=
  run unlimitedTest =
    init
      .then(admit("r1"))
      .then(admit("r2"))
      .then(admit("r3"))
      .then(admit("r4"))
      .then(admit("r5"))
      .then(complete("r1", 3))
      .then(complete("r2", 3))
      .then(complete("r3", 3))
      .then(complete("r4", 3))
      .then(complete("r5", 3))
      .expect(not(withinBound))
```

**An expired slot lets extra calls in** (`noSlotRenewal`). Two calls are in
flight at the limit; their slots expire while they stream; two more calls
are admitted. Four calls at 3 each on top of 3 already spent take the
principal to 15, past the bound of 12.

```quint accounting.qnt +=
  run slotExpiryTest =
    init
      .then(admit("r1"))
      .then(complete("r1", 3))
      .then(admit("r2"))
      .then(admit("r3"))
      .then(expireSlot("r2"))
      .then(expireSlot("r3"))
      .then(admit("r4"))
      .then(admit("r5"))
      .then(complete("r2", 3))
      .then(complete("r3", 3))
      .then(complete("r4", 3))
      .then(complete("r5", 3))
      .expect(not(withinBound))
```

**A crash loses events, and reconciliation books them** (the design). A
call is in flight when the gateway crashes; the provider bills it; the
ledger never hears of it; closing the books adds it as an adjustment and
the totals match. Without `RECONCILE` they do not.

```quint accounting.qnt +=
  run crashReconciledTest =
    init
      .then(admit("r1"))
      .then(complete("r1", 2))
      .then(deliver("r1", 2))
      .then(admit("r2"))
      .then(crash(3))
      .then(closeBooks)
      .expect(uncounted == 3 and booksMatch)
```

**A duplicate is counted twice** (`rawSum`). One event is delivered twice;
a plain sum books it twice.

```quint accounting.qnt +=
  run duplicateCountedTest =
    init
      .then(admit("r1"))
      .then(complete("r1", 2))
      .then(deliver("r1", 2))
      .then(duplicate("r1", 2))
      .expect(not(noDoubleCount))
```

The model ends here:

```quint accounting.qnt +=
}
```

## 9. What the model leaves out

- **Several sites.** Each site's known spend lags the others' by one
  snapshot renewal (about a minute). The bound gains a term: what the
  principal can spend in that interval, which its in-flight and rate limits
  cap. The model's single site has no lag.
- **Several principals and levels.** Key caps, member caps and scope pools
  are the same check at three levels; one level shows the bound.
- **Prices, and calls that cost more than declared.** One number per call,
  at most `WORST`. The real worst case is a declaration: every input byte
  a token, plus `max_tokens` × `n`, at list prices, no cache discount, no
  server-side tools (ADR 0038, 0044). It is not a bound on what a call can
  cost: images, audio and files take tokens that bytes do not bound, some
  units are billed outside tokens, a provider can go on working after a
  cut, prices change. The model assumes the declaration holds; every
  bound it checks is conditional on that.

## 10. The designs to check

| Instance | Settings | Should |
|---|---|---|
| `softDesign` | soft check, `LIMIT = 2` | hold `safe`; reach every scenario |
| `worstCheckDesign` | declared-worst-case check, `LIMIT = 2` | hold `safe`, with the tighter slack |
| `worstCheckSingle` | declared-worst-case check, `LIMIT = 1` | hold `withinDeclaredWorst` |
| `noLimit` | no concurrency limit | break `withinBound` |
| `rawSum` | no deduplication | break `noDoubleCount` |
| `noReconcile` | no reconciliation | break `booksMatch` |
| `noSlotRenewal` | slots expire mid-call | break `withinBound` |

Which story passes where (as `check-accounting.sh` confirms):

| Story | softDesign | worstCheckDesign | worstCheckSingle | noLimit | rawSum | noReconcile | noSlotRenewal |
|---|---|---|---|---|---|---|---|
| siblings overshoot (accepted risk) | **passes** | impossible | impossible | **passes** | **passes** | **passes** | **passes** |
| unlimited overshoot | impossible | impossible | impossible | **passes** | impossible | impossible | impossible |
| expired slots let calls in | impossible | impossible | impossible | impossible | impossible | impossible | **passes** |
| crash reconciled | **passes** | **passes** | **passes** | **passes** | **passes** | impossible | **passes** |
| duplicate counted | impossible | impossible | impossible | impossible | **passes** | impossible | impossible |

Two lessons in it. The accepted risk is real and bounded: the siblings
story passes in every soft design and is impossible under the declared-worst-case check.
And the concurrency bugs (`noLimit`, `noSlotRenewal`) have the rare kind
of counterexample: several calls must be admitted, or slots expire, before
any call completes, which random simulation did not find in tens of
thousands of runs. A search of every run does: on the small instance
(`accounting.small.yaml`: three requests, a budget of 2, one call in
flight), `check-accounting.sh` explores every run of up to 12 steps, finds
each bug's shortest counterexample (which are these stories, step for
step), and shows the soft and worst-case designs hold in all of them.

```quint accounting.qnt +=

module softDesign {
  import accounting(REQUESTS = Set("r1", "r2", "r3", "r4", "r5"), COSTS = Set(1, 2, 3), WORST = 3,
    BUDGET = 6, LIMIT = 2, LIMIT_ON = true, RENEW_SLOTS = true, WORST_CHECK = false, DEDUP = true, RECONCILE = true).*
}

module worstCheckDesign {
  import accounting(REQUESTS = Set("r1", "r2", "r3", "r4", "r5"), COSTS = Set(1, 2, 3), WORST = 3,
    BUDGET = 6, LIMIT = 2, LIMIT_ON = true, RENEW_SLOTS = true, WORST_CHECK = true, DEDUP = true, RECONCILE = true).*
}

module worstCheckSingle {
  import accounting(REQUESTS = Set("r1", "r2", "r3", "r4", "r5"), COSTS = Set(1, 2, 3), WORST = 3,
    BUDGET = 6, LIMIT = 1, LIMIT_ON = true, RENEW_SLOTS = true, WORST_CHECK = true, DEDUP = true, RECONCILE = true).*

  // Within the budget, apart from crash losses, while every call costs at
  // most WORST: the model's assumption, not a ceiling on real spend.
  val withinDeclaredWorst = totalBilled <= BUDGET + uncounted
}

module noLimit {
  import accounting(REQUESTS = Set("r1", "r2", "r3", "r4", "r5"), COSTS = Set(1, 2, 3), WORST = 3,
    BUDGET = 6, LIMIT = 2, LIMIT_ON = false, RENEW_SLOTS = true, WORST_CHECK = false, DEDUP = true, RECONCILE = true).*
}

module rawSum {
  import accounting(REQUESTS = Set("r1", "r2", "r3", "r4", "r5"), COSTS = Set(1, 2, 3), WORST = 3,
    BUDGET = 6, LIMIT = 2, LIMIT_ON = true, RENEW_SLOTS = true, WORST_CHECK = false, DEDUP = false, RECONCILE = true).*
}

module noSlotRenewal {
  import accounting(REQUESTS = Set("r1", "r2", "r3", "r4", "r5"), COSTS = Set(1, 2, 3), WORST = 3,
    BUDGET = 6, LIMIT = 2, LIMIT_ON = true, RENEW_SLOTS = false, WORST_CHECK = false, DEDUP = true, RECONCILE = true).*
}

module noReconcile {
  import accounting(REQUESTS = Set("r1", "r2", "r3", "r4", "r5"), COSTS = Set(1, 2, 3), WORST = 3,
    BUDGET = 6, LIMIT = 2, LIMIT_ON = true, RENEW_SLOTS = true, WORST_CHECK = false, DEDUP = true, RECONCILE = false).*
}
```

## The small instance

The instance the exhaustive search explores (accounting.small.yaml, run by
check-accounting.sh where it has one): the design at sizes small enough to
search every run to a depth. The binding names this module rather than
repeating its constants. Its variants (`simulate.variants`) are the
designs, each with the mechanism it turns off, what it must hold and
what it must break; `gotogether simulate -matrix` searches every run of
each to the binding's depth and writes this table, the properties by the
designs (a cell "breaks in N steps" is its shortest counterexample):

<!-- gotogether:matrix -->
| | soft | worst check | no limit | no slot renewal | no dedup | no reconcile |
|---|---|---|---|---|---|---|
| safe | holds to depth 12 | holds to depth 12 | breaks in 4 steps | breaks in 5 steps | breaks in 4 steps | breaks in 3 steps |
| withinBound | holds to depth 12 | holds to depth 12 | breaks in 4 steps | breaks in 5 steps | holds to depth 12 | holds to depth 12 |
| noDoubleCount | holds to depth 12 | holds to depth 12 | holds to depth 12 | holds to depth 12 | breaks in 4 steps | holds to depth 12 |
| booksMatch | holds to depth 12 | holds to depth 12 | holds to depth 12 | holds to depth 12 | breaks in 5 steps | breaks in 3 steps |
<!-- /gotogether:matrix -->

```quint accounting.qnt +=

module small {
  import accounting(REQUESTS = Set("r1", "r2", "r3"), COSTS = Set(1, 3), WORST = 3, BUDGET = 2, LIMIT = 1,
    LIMIT_ON = true, RENEW_SLOTS = true, WORST_CHECK = false, DEDUP = true, RECONCILE = true).*
}
```
