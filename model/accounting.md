# Every attempt is paid for exactly once: the accounting model

A literate Quint specification, like [publication.md](publication.md): the
prose explains, the `quint` blocks are the model, `make` extracts and
checks it. It models the accounting requirements of
[ADR 0029](../docs/adr/0029-security-and-correctness-revision.md) §9 and
the spool choice left open by
[ADR 0030](../docs/adr/0030-authorization-renewal-and-availability.md),
and the ledger contract of [contracts.md](../docs/contracts.md) §3.

## 1. The question

Every upstream attempt a gateway sends may be billed by the provider,
whether or not golem ever learns the result. The ledger in ClickHouse is
how that cost reaches budgets and chargeback. The question:

> Whatever crashes, lost responses, lost disks, store failovers and
> duplicate deliveries happen, is every billed attempt accounted for, and
> never counted twice?

The design's answer is yes, if five rules hold. As in the publication
model, each rule is a switch, so its absence can be watched:

| Switch | When on (the design) | When off (the bug it prevents) |
|---|---|---|
| `INTENT_FIRST` | An attempt's intent is durably journaled **before** it is dispatched | A crash after dispatch loses the only record of a billed attempt |
| `KEEP_UNCERTAIN` | An attempt whose outcome is unknown (a lost response, a crash, an expired reservation) stays as uncertain liability until evidence resolves it | Timeouts, recovery and reservation expiry silently erase billed usage |
| `TRIM_AFTER_ACK` | The journal drops an event only after the ledger acknowledged it | An export lost in transit takes the event with it |
| `DEDUP` | Balances count each event once (by event ID) and only the head of each supersedes chain | At-least-once delivery counts duplicates, and an estimate plus its settlement both count |
| `DURABLE_MEDIUM` | What the journal calls durable survives the loss of its medium | A lost volume or a store failover loses durable but not yet exported events |

`DURABLE_MEDIUM` is how the model compares the spool candidates of ADR
0030 (§9 below).

## 2. Reading and running

`make` (in this directory) extracts this file into `accounting.qnt` and
runs `check-accounting.sh`: the design must hold, every scenario must be
reachable, every broken design must break its property, and every story
(§8) must pass in its broken design and be impossible in the correct one.

```quint accounting.qnt +=
// Generated from accounting.md by lmt. Edit the markdown, not this file.
module accounting {
```

## 3. The world

A few **attempts**, each billed by the provider an amount from `AMOUNTS`
(the model does not care what the amounts are, only that they are kept).
The reservation estimate is `ESTIMATE`.

```quint accounting.qnt +=
  const ATTEMPTS: Set[str]
  const AMOUNTS: Set[int]
  const ESTIMATE: int

  const INTENT_FIRST: bool
  const KEEP_UNCERTAIN: bool
  const TRIM_AFTER_ACK: bool
  const DEDUP: bool
  const DURABLE_MEDIUM: bool
```

## 4. State

### 4.1 What the provider knows

The ground truth no component of golem sees directly: what each attempt
was billed (0 until dispatched). This is a ghost: it exists so the
properties can compare golem's books with reality.

```quint accounting.qnt +=
  var billed: str -> int
```

### 4.2 The gateway

Each attempt moves through phases in the gateway's memory: `idle`,
`reserved` (a reservation is held in Valkey), `inflight` (sent upstream),
`done` (settled, or handed over as uncertain). A crash loses the phases of
attempts in flight. `responseLost` records whether the provider's answer
for an attempt will ever arrive.

```quint accounting.qnt +=
  var phase: str -> str
  var responseLost: str -> bool
  var reservations: str -> int
```

### 4.3 The journal and the ledger

The **journal** is the durable spool between the gateway and the ledger.
Each event has an ID (the pair of its attempt and kind), its attempt, its kind, an amount,
and how durable it is: `buffered` (in memory, lost on a crash) or
`durable` (survives a crash; survives medium loss only with
`DURABLE_MEDIUM`).

Kinds, and the chain they form for one attempt:

- `intent`: the attempt is about to be dispatched; its amount is the
  estimate, counted as *uncertain exposure*, not spend;
- `settlement`: the attempt's actual usage, superseding the intent;
- `resolution`: the provider's evidence for an attempt whose outcome golem
  never learned (from the daily reconciliation), superseding the intent.

The **ledger** is ClickHouse's table: a list of delivered events, which can
contain the same event more than once (at-least-once delivery). `acked`
are the events the ledger has acknowledged.

```quint accounting.qnt +=
  type Event = { id: (str, str), attempt: str, kind: str, amount: int, durability: str }

  var journal: Set[Event]
  var ledger: List[Event]
  var acked: Set[(str, str)]
```

### 4.4 Ghosts

`lostLiability` is set the moment a billed attempt has no event anywhere,
journal or ledger: the bug the model exists to catch.

```quint accounting.qnt +=
  var lostLiability: bool
```

## 5. The initial state

```quint accounting.qnt +=
  action init = all {
    billed' = ATTEMPTS.mapBy(a => 0),
    phase' = ATTEMPTS.mapBy(a => "idle"),
    responseLost' = ATTEMPTS.mapBy(a => false),
    reservations' = ATTEMPTS.mapBy(a => 0),
    journal' = Set(),
    ledger' = [],
    acked' = Set(),
    lostLiability' = false,
  }
```

Helpers: what an attempt has recorded anywhere, and the check that runs
after every action that can lose records.

```quint accounting.qnt +=
  def eventIds(es: Set[Event]): Set[(str, str)] = es.map(e => e.id)
  def ledgerSet: Set[Event] = ledger.foldl(Set(), (s, e) => s.union(Set(e)))
  def recorded(a: str, j: Set[Event]): bool =
    j.exists(e => e.attempt == a) or ledgerSet.exists(e => e.attempt == a)
  def losesLiability(j: Set[Event]): bool =
    ATTEMPTS.exists(a => billed.get(a) > 0 and not(recorded(a, j)))
```

## 6. Actions

### 6.1 Reserve

Before anything else, the gateway reserves the estimate in the site's
Valkey (ADR 0012). The reservation guards budgets; it is not the record of
what was spent.

```quint accounting.qnt +=
  action reserve(attempt: str): bool = all {
    phase.get(attempt) == "idle",
    phase' = phase.put(attempt, "reserved"),
    reservations' = reservations.put(attempt, ESTIMATE),
    billed' = billed, responseLost' = responseLost,
    journal' = journal, ledger' = ledger, acked' = acked, lostLiability' = lostLiability,
  }
```

### 6.2 Journal the intent, and flush

The gateway appends the attempt's intent to the journal. Appends land
`buffered`; `flush` makes everything buffered `durable` (an fsync, an
append-only write acknowledged by Valkey, a PUT). With `INTENT_FIRST` the
gateway may dispatch only once the intent is durable.

```quint accounting.qnt +=
  action writeIntent(attempt: str): bool = all {
    phase.get(attempt) == "reserved",
    not(journal.exists(e => e.attempt == attempt)),
    journal' = journal.union(Set({ id: (attempt, "intent"), attempt: attempt, kind: "intent",
                                   amount: ESTIMATE, durability: "buffered" })),
    billed' = billed, phase' = phase, responseLost' = responseLost,
    reservations' = reservations, ledger' = ledger, acked' = acked, lostLiability' = lostLiability,
  }

  action flush: bool = all {
    journal.exists(e => e.durability == "buffered"),
    journal' = journal.map(e => { ...e, durability: "durable" }),
    billed' = billed, phase' = phase, responseLost' = responseLost,
    reservations' = reservations, ledger' = ledger, acked' = acked, lostLiability' = lostLiability,
  }
```

### 6.3 Dispatch

The attempt goes upstream. From this moment the provider may bill it,
whatever happens to golem; `amount` is what it bills, and `lost` whether
its response will ever reach the gateway.

```quint accounting.qnt +=
  def intentDurable(a: str): bool =
    journal.exists(e => e.attempt == a and e.kind == "intent" and e.durability == "durable")

  action dispatch(attempt: str, amount: int, lost: bool): bool = all {
    phase.get(attempt) == "reserved",
    if (INTENT_FIRST) intentDurable(attempt) else true,
    phase' = phase.put(attempt, "inflight"),
    billed' = billed.put(attempt, amount),
    responseLost' = responseLost.put(attempt, lost),
    reservations' = reservations, journal' = journal, ledger' = ledger, acked' = acked,
    lostLiability' = lostLiability,
  }
```

### 6.4 Settle

The response arrives with its usage. The gateway journals a settlement for
the billed amount and releases the reservation. Without `INTENT_FIRST`
this is the attempt's first and only record.

```quint accounting.qnt +=
  action settle(attempt: str): bool = all {
    phase.get(attempt) == "inflight",
    not(responseLost.get(attempt)),
    journal' = journal.union(Set({ id: (attempt, "settlement"), attempt: attempt, kind: "settlement",
                                   amount: billed.get(attempt), durability: "buffered" })),
    phase' = phase.put(attempt, "done"),
    reservations' = reservations.put(attempt, 0),
    billed' = billed, responseLost' = responseLost, ledger' = ledger, acked' = acked,
    lostLiability' = lostLiability,
  }
```

### 6.5 Give up on an answer

The response never comes (or the reservation's TTL runs out mid-stream).
With `KEEP_UNCERTAIN` the intent stays as uncertain liability; without, the
gateway drops it, and with it the only record of a billed attempt.

```quint accounting.qnt +=
  action giveUp(attempt: str): bool = all {
    phase.get(attempt) == "inflight",
    responseLost.get(attempt),
    val j = if (KEEP_UNCERTAIN) journal else journal.filter(e => e.attempt != attempt)
    all {
      journal' = j,
      lostLiability' = (lostLiability or losesLiability(j)),
    },
    phase' = phase.put(attempt, "done"),
    reservations' = reservations.put(attempt, 0),
    billed' = billed, responseLost' = responseLost, ledger' = ledger, acked' = acked,
  }
```

### 6.6 The gateway crashes and restarts

Memory is gone: buffered events are lost, and attempts in flight are
forgotten (their responses will never be read). On restart, recovery reads
the journal: with `KEEP_UNCERTAIN` every intent without a settlement stays
as uncertain liability; without, recovery treats such intents as
abandoned and deletes them.

```quint accounting.qnt +=
  action crash: bool = all {
    val survived = journal.filter(e => e.durability == "durable")
    val settledAttempts = survived.filter(e => e.kind != "intent").map(e => e.attempt)
    val j = if (KEEP_UNCERTAIN) survived
            else survived.filter(e => e.kind != "intent" or settledAttempts.contains(e.attempt))
    all {
      journal' = j,
      lostLiability' = (lostLiability or losesLiability(j)),
    },
    phase' = ATTEMPTS.mapBy(a => if (phase.get(a) == "inflight") "done" else phase.get(a)),
    reservations' = reservations,
    billed' = billed, responseLost' = responseLost, ledger' = ledger, acked' = acked,
  }
```

### 6.7 The journal's medium is lost

A gateway's volume is lost with its node, or the site's Valkey fails over
and loses its last unreplicated writes. With `DURABLE_MEDIUM` that cannot
lose anything durable (the medium is replicated synchronously, or is the
bucket); without, every durable event not yet exported is gone.

```quint accounting.qnt +=
  action loseMedium: bool = all {
    not(DURABLE_MEDIUM),
    journal.size() > 0,
    journal' = Set(),
    lostLiability' = (lostLiability or losesLiability(Set())),
    billed' = billed, phase' = phase, responseLost' = responseLost,
    reservations' = reservations, ledger' = ledger, acked' = acked,
  }
```

### 6.8 Valkey loses reservations

Losing the site's Valkey loses reservations, which hurts budget
enforcement for a moment but must not lose any record of spend: the journal
is the record, not Valkey.

```quint accounting.qnt +=
  action loseReservations: bool = all {
    reservations' = ATTEMPTS.mapBy(a => 0),
    billed' = billed, phase' = phase, responseLost' = responseLost,
    journal' = journal, ledger' = ledger, acked' = acked, lostLiability' = lostLiability,
  }
```

### 6.9 Ship to the ledger

(`export` is a Quint keyword, so the action is `ship`.)

The journal ships durable events to ClickHouse. A delivery may fail
(`delivered` false: the collector was down, the connection dropped). With
`TRIM_AFTER_ACK` the journal keeps an event until the ledger acknowledges
it; without, it drops the event as soon as it is sent.

```quint accounting.qnt +=
  action ship(id: (str, str), delivered: bool): bool = all {
    journal.exists(e => e.id == id and e.durability == "durable"),
    val ev = journal.filter(e => e.id == id).fold({ id: ("", ""), attempt: "", kind: "", amount: 0, durability: "" },
                                                  (acc, e) => e)
    val j = if (delivered and TRIM_AFTER_ACK) journal.filter(e => e.id != id)
            else if (TRIM_AFTER_ACK) journal
            else journal.filter(e => e.id != id)
    all {
      ledger' = if (delivered) ledger.append(ev) else ledger,
      acked' = if (delivered) acked.union(Set(id)) else acked,
      journal' = j,
      lostLiability' = (lostLiability or (not(delivered) and losesLiability(j))),
    },
    billed' = billed, phase' = phase, responseLost' = responseLost, reservations' = reservations,
  }
```

### 6.10 A duplicate delivery

At-least-once transport delivers an event the ledger already has, again.

```quint accounting.qnt +=
  action duplicate(id: (str, str)): bool = all {
    ledgerSet.exists(e => e.id == id),
    val ev = ledgerSet.filter(e => e.id == id).fold({ id: ("", ""), attempt: "", kind: "", amount: 0, durability: "" },
                                                    (acc, e) => e)
    ledger' = ledger.append(ev),
    billed' = billed, phase' = phase, responseLost' = responseLost, reservations' = reservations,
    journal' = journal, acked' = acked, lostLiability' = lostLiability,
  }
```

### 6.11 Reconciliation resolves an uncertain attempt

The daily job finds an attempt whose only record is its intent, asks the
provider's usage data what it really cost, and journals a resolution for
that amount (ADR 0012).

```quint accounting.qnt +=
  def chain(a: str): Set[Event] = journal.union(ledgerSet).filter(e => e.attempt == a)

  action reconcile(attempt: str): bool = all {
    chain(attempt).exists(e => e.kind == "intent"),
    not(chain(attempt).exists(e => e.kind != "intent")),
    phase.get(attempt) == "done",
    journal' = journal.union(Set({ id: (attempt, "resolution"), attempt: attempt, kind: "resolution",
                                   amount: billed.get(attempt), durability: "durable" })),
    billed' = billed, phase' = phase, responseLost' = responseLost, reservations' = reservations,
    ledger' = ledger, acked' = acked, lostLiability' = lostLiability,
  }
```

### 6.12 The step

```quint accounting.qnt +=
  action step = {
    nondet attempt = ATTEMPTS.oneOf()
    nondet amount = AMOUNTS.oneOf()
    nondet lost = Set(true, false).oneOf()
    nondet delivered = Set(true, false).oneOf()
    nondet id = Set((attempt, "intent"), (attempt, "settlement"), (attempt, "resolution")).oneOf()
    any {
      reserve(attempt),
      writeIntent(attempt),
      flush,
      dispatch(attempt, amount, lost),
      settle(attempt),
      giveUp(attempt),
      crash,
      loseMedium,
      loseReservations,
      ship(id, delivered),
      duplicate(id),
      reconcile(attempt),
    }
  }
```

## 7. Properties

### 7.1 How the ledger computes a balance

The **spent** balance of the ledger is what chargeback and budgets read.
With `DEDUP` it is computed as contracts.md §3 requires: each event counted
once by its ID, and per attempt only the head of its chain (a settlement or
resolution supersedes the intent; intents are exposure, not spend). Without
it, the balance is a plain sum over the rows ClickHouse holds.

```quint accounting.qnt +=
  def headSpend(a: str): int =
    val es = ledgerSet.filter(e => e.attempt == a and e.kind != "intent")
    es.fold(0, (acc, e) => if (e.amount > acc) e.amount else acc)

  def spentDedup: int = ATTEMPTS.fold(0, (acc, a) => acc + headSpend(a))
  def spentRaw: int = ledger.foldl(0, (acc, e) => if (e.kind != "intent") acc + e.amount else acc)
  def spent: int = if (DEDUP) spentDedup else spentRaw

  def totalBilled: int = ATTEMPTS.fold(0, (acc, a) => acc + billed.get(a))
```

### 7.2 Safety

**No billed attempt is ever without a record.** Checked the moment any
action drops records.

```quint accounting.qnt +=
  val noLostLiability = not(lostLiability)
```

**Never counted twice.** The ledger's spent balance never exceeds what the
provider billed.

```quint accounting.qnt +=
  val noDoubleCount = spent <= totalBilled
```

**Exact once quiet.** When nothing is in flight, nothing is waiting in the
journal, and no attempt is still uncertain, the ledger's balance is exactly
what the provider billed.

```quint accounting.qnt +=
  val quiet = and {
    ATTEMPTS.forall(a => phase.get(a) != "inflight"),
    journal.size() == 0,
    ATTEMPTS.forall(a => billed.get(a) == 0 or ledgerSet.exists(e => e.attempt == a and e.kind != "intent")),
  }

  val exactWhenQuiet = quiet implies spent == totalBilled
```

All together:

```quint accounting.qnt +=
  val safe = noLostLiability and noDoubleCount and exactWhenQuiet
```

### 7.3 Possibility

The correct design must be able to:

- settle an attempt and land it in the ledger;
- lose a response and still end with the provider's amount, through
  reconciliation;
- survive a crash after dispatch with the intent kept as uncertain;
- absorb a duplicate delivery and still balance exactly.

```quint accounting.qnt +=
  val settledInLedger = ledgerSet.exists(e => e.kind == "settlement")
  val resolvedAfterLostResponse = ledgerSet.exists(e => e.kind == "resolution")
  val uncertainAfterCrash = ATTEMPTS.exists(a =>
    billed.get(a) > 0 and phase.get(a) == "done"
      and chain(a).exists(e => e.kind == "intent") and not(chain(a).exists(e => e.kind != "intent")))
  val duplicateAbsorbed = ledger.length() > ledgerSet.size() and quiet and spent == totalBilled
```

## 8. The bugs, as stories

As in the publication model: each story passes in its broken design and
is impossible in the correct one. Here each story passes in exactly one
design, its own: the five rules are independent, and each is necessary.

| Story | design | noIntentFirst | dropUncertain | trimOnSend | rawSum | fragileMedium |
|---|---|---|---|---|---|---|
| crash after dispatch | impossible | **passes** | impossible | impossible | impossible | impossible |
| timeout erases usage | impossible | impossible | **passes** | impossible | impossible | impossible |
| failed exports | impossible | impossible | impossible | **passes** | impossible | impossible |
| duplicate counted | impossible | impossible | impossible | impossible | **passes** | impossible |
| lost volume | impossible | impossible | impossible | impossible | impossible | **passes** |

**A crash after dispatch loses a billed attempt** (`noIntentFirst`). The
gateway reserves, dispatches without journaling an intent first, and
crashes before the response is settled. The provider billed it; golem has
no record of it at all.

```quint accounting.qnt +=
  run crashAfterDispatchTest =
    init
      .then(reserve("a1"))
      .then(dispatch("a1", 2, false))
      .then(crash)
      .expect(not(noLostLiability))
```

**A timeout erases billed usage** (`dropUncertain`). The intent is
journaled and flushed, the attempt dispatched, its response lost; the
gateway gives up and, without `KEEP_UNCERTAIN`, deletes the intent.

```quint accounting.qnt +=
  run timeoutErasesTest =
    init
      .then(reserve("a1"))
      .then(writeIntent("a1"))
      .then(flush)
      .then(dispatch("a1", 2, true))
      .then(giveUp("a1"))
      .expect(not(noLostLiability))
```

**Failed exports take the events with them** (`trimOnSend`). The intent
and the settlement are both flushed and sent; both deliveries fail, and the
journal had already dropped each as it was sent. (One failed delivery is
not enough: if the intent reaches the ledger, reconciliation can still
resolve the attempt. The intent-first rule makes the books more robust than
any single export.)

```quint accounting.qnt +=
  run failedExportTest =
    init
      .then(reserve("a1"))
      .then(writeIntent("a1"))
      .then(flush)
      .then(dispatch("a1", 2, false))
      .then(settle("a1"))
      .then(flush)
      .then(ship(("a1", "intent"), false))
      .then(ship(("a1", "settlement"), false))
      .expect(not(noLostLiability))
```

**A duplicate is counted twice** (`rawSum`). The settlement reaches the
ledger, then is delivered again; a plain sum counts it twice.

```quint accounting.qnt +=
  run duplicateCountedTest =
    init
      .then(reserve("a1"))
      .then(writeIntent("a1"))
      .then(flush)
      .then(dispatch("a1", 2, false))
      .then(settle("a1"))
      .then(flush)
      .then(ship(("a1", "settlement"), true))
      .then(duplicate(("a1", "settlement")))
      .expect(not(noDoubleCount))
```

**A lost volume loses what was durable** (`fragileMedium`). The intent is
durable on the gateway's volume and the attempt dispatched; the node dies
with its volume before anything is exported.

```quint accounting.qnt +=
  run lostVolumeTest =
    init
      .then(reserve("a1"))
      .then(writeIntent("a1"))
      .then(flush)
      .then(dispatch("a1", 2, false))
      .then(loseMedium)
      .expect(not(noLostLiability))
```

The model ends here:

```quint accounting.qnt +=
}
```

## 9. Comparing the spool candidates

ADR 0030 names three candidates for the journal. The model separates two
questions about each: is the intent durable **before** dispatch
(`INTENT_FIRST`), and does durable survive the loss of the medium
(`DURABLE_MEDIUM`)?

| Candidate | `INTENT_FIRST` | `DURABLE_MEDIUM` | What the model says |
|---|---|---|---|
| A write-ahead log on the gateway pod's volume, fsync before dispatch | yes | no: the volume can be lost with its node | The `lostVolumeTest` story applies: an attempt dispatched and not yet exported is lost with the node. Shrinking the window (exporting intents at once) narrows it; it does not close it |
| Valkey with append-only persistence and a replica | yes, if the write is acknowledged only after `WAIT` for the replica | only with synchronous replication (`WAIT 1`) and `appendfsync always`; with asynchronous replication a failover loses the last writes | The same story applies to an asynchronous failover |
| The bucket, one object per intent (or per small batch, flushed before dispatch) | yes, if the PUT completes before dispatch | yes | Holds; costs a PUT on the request path (tens of milliseconds, small next to a model's time to first token) and one object per attempt or batch |

What the model settles, whatever the spike measures: every candidate must
make the intent durable before dispatch; and a candidate whose medium can
be lost must either replicate synchronously or accept, by recorded policy,
that a node or failover loss can drop the attempts in flight on it (to be
caught later only by provider reconciliation). The spike then measures
latency and cost among the candidates that pass.

## 10. What the model leaves out

- **Budgets.** Whether a reservation should have been granted is the
  limits model's question; here reservations only show that losing them
  loses no record.
- **Amounts' arithmetic** (prices, cache tiers): one number per attempt.
- **Retries.** Each retry is its own attempt with its own events (ADR
  0029); the model treats attempts independently.
- **Provider reconciliation's own failure.** It is assumed to find every
  attempt the provider billed, given time.

## 11. The designs to check

| Instance | Switch off | Should break |
|---|---|---|
| `design` | none | nothing |
| `noIntentFirst` | `INTENT_FIRST` | `noLostLiability` |
| `dropUncertain` | `KEEP_UNCERTAIN` | `noLostLiability` |
| `trimOnSend` | `TRIM_AFTER_ACK` | `noLostLiability` |
| `rawSum` | `DEDUP` | `noDoubleCount` |
| `fragileMedium` | `DURABLE_MEDIUM` | `noLostLiability` |

```quint accounting.qnt +=

module accountingDesign {
  import accounting(ATTEMPTS = Set("a1", "a2"), AMOUNTS = Set(1, 2), ESTIMATE = 2,
    INTENT_FIRST = true, KEEP_UNCERTAIN = true, TRIM_AFTER_ACK = true, DEDUP = true,
    DURABLE_MEDIUM = true).*
}

module noIntentFirst {
  import accounting(ATTEMPTS = Set("a1", "a2"), AMOUNTS = Set(1, 2), ESTIMATE = 2,
    INTENT_FIRST = false, KEEP_UNCERTAIN = true, TRIM_AFTER_ACK = true, DEDUP = true,
    DURABLE_MEDIUM = true).*
}

module dropUncertain {
  import accounting(ATTEMPTS = Set("a1", "a2"), AMOUNTS = Set(1, 2), ESTIMATE = 2,
    INTENT_FIRST = true, KEEP_UNCERTAIN = false, TRIM_AFTER_ACK = true, DEDUP = true,
    DURABLE_MEDIUM = true).*
}

module trimOnSend {
  import accounting(ATTEMPTS = Set("a1", "a2"), AMOUNTS = Set(1, 2), ESTIMATE = 2,
    INTENT_FIRST = true, KEEP_UNCERTAIN = true, TRIM_AFTER_ACK = false, DEDUP = true,
    DURABLE_MEDIUM = true).*
}

module rawSum {
  import accounting(ATTEMPTS = Set("a1", "a2"), AMOUNTS = Set(1, 2), ESTIMATE = 2,
    INTENT_FIRST = true, KEEP_UNCERTAIN = true, TRIM_AFTER_ACK = true, DEDUP = false,
    DURABLE_MEDIUM = true).*
}

module fragileMedium {
  import accounting(ATTEMPTS = Set("a1", "a2"), AMOUNTS = Set(1, 2), ESTIMATE = 2,
    INTENT_FIRST = true, KEEP_UNCERTAIN = true, TRIM_AFTER_ACK = true, DEDUP = true,
    DURABLE_MEDIUM = false).*
}
```
