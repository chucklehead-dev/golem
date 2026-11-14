# A scope's exposure: the second accounting model

A literate Quint specification, the companion of
[accounting.md](accounting.md). That model shows the soft-budget bound for
one principal on one gateway, with a working Valkey. This one asks what
[ADR 0041](../docs/adr/0041-security-boundaries-and-incident-controls.md)
§6 says the first model does not establish:

> A scope's pool is shared by several principals, served by several
> gateways on a site. Known spend is a snapshot from the ledger plus the
> site's counter, and the ledger ingests events late. Valkey can be
> unreachable or lose writes. A client can cancel a call the provider keeps
> working on. Does the scope stay within a stated bound, and when it cannot,
> is the exposure at least recorded?

The design's answer is yes, if four rules hold:

| Switch | When on (the design) | When off (the bug it prevents) |
|---|---|---|
| `SCOPE_LIMIT_ON` | A scope has its own in-flight limit, checked with the principal's | Each principal stays within its limit, but the scope's exposure is their sum: grows with every member who joins |
| `HANDOFF` = `"buckets"` | The counter keeps each minute's spend (by when its calls ended); a snapshot carries a time boundary B, an ingestion allowance before its publication, and the ledger's spend of the calls that ended before B; known spend is that plus the counter's minutes from B on (ADR 0048): exact on every gateway whichever snapshot it holds, while the ledger ingests within the allowance | `"noAllowance"`: B is the publication's own time, and a row still on its way is in neither the snapshot nor the minutes counted. `"resetAtApply"`: applying a snapshot clears the counter, and the spend after B (and a gateway still on an older snapshot) loses it. `"sum"`: the ledger's balance and the whole counter are added |
| `HOLD_ON_CANCEL` | A cancelled call keeps its slot until its upstream has ended | The slot is freed at cancellation while the provider keeps working: cancel and resend escapes the in-flight limit |
| `REACQUIRE` | When Valkey comes back, each gateway re-registers the slots of its calls still running | Slots that expired during the outage are forgotten: new calls are admitted next to calls still streaming |

And one rule that is not a switch, because there is no safe alternative
(ADR 0041 §7.3): while Valkey is unreachable a gateway admits against its
own local in-flight counts, and every call so admitted is recorded as a
**waiver** of the bound. Across several gateways the local counts can add
up past the limits; the waiver is how that exposure is known instead of
silent.

## 1. Reading and running

`make` extracts this file into `exposure.qnt` and runs `check-exposure.sh`:
the design holds, every scenario is reachable, every broken design breaks
its property, every story behaves as §9 says.

```quint exposure.qnt +=
// Generated from exposure.md by lmt. Edit the markdown, not this file.
module exposure {
```

## 2. The world

A scope with a `BUDGET`, several principals each with `LIMIT` calls in
flight, the scope with `SCOPE_LIMIT`, a site's `GATEWAYS`, and a few
`REQUESTS`, each its `OWNER`'s. Costs and `WORST` as in the first model.
Only the soft check: the declared-worst-case check tightens the bound by a call, as
accounting.md shows, and changes nothing here.

Time is in minutes, the counter's grain. `ALLOWANCE` is how many minutes
the ledger may take to ingest a row: the deployment's assumption (the
collector's queue and ClickHouse's insert lag, ADR 0043), stated as the
guard on `tick` (§5.5). golem-control places a snapshot's boundary that
far behind the time it publishes.

```quint exposure.qnt +=
  const REQUESTS: Set[str]
  const OWNER: str -> str
  const GATEWAYS: Set[str]
  const COSTS: Set[int]
  const WORST: int
  const BUDGET: int
  const LIMIT: int
  const SCOPE_LIMIT: int
  const ALLOWANCE: int

  const SCOPE_LIMIT_ON: bool
  const HANDOFF: str
  const HOLD_ON_CANCEL: bool
  const REACQUIRE: bool
```

## 3. State

### 3.1 Calls

A call is `idle`, `inflight`, `cancelled` (the client went away; the
provider may still be working, and billing), or `done`. Each records the
gateway that admitted it, what the provider billed, and the spend event
the gateway emitted for it: its cost at completion, or at cancellation
what the gateway had seen so far, and the minute it was emitted: the
call's end, which its ledger row and its counter minute both carry.

```quint exposure.qnt +=
  type Event = (str, int, int)

  var phase: str -> str
  var gatewayOf: str -> str
  var billed: str -> int
  var seenCost: str -> int
```

### 3.2 Known spend: snapshot, counter, ledger

- `now`: the minute.
- `counter`: the site counter in Valkey: each minute's spend, the sum of
  the events that ended in it. It holds amounts, not events: adding is
  `INCRBY`, and nothing names an event in it. The design keeps a minute
  for the retention window, longer than any snapshot a live gateway holds.
- `pending`: the increments each gateway holds while it believes Valkey
  unreachable (`gwDown`), each with its own minute. Its ledger row does not
  wait: rows are written at a call's end (ADR 0043). Each gateway notices
  on its own that Valkey is back and replays its own, into their minutes;
  until then the others admit against a counter that lacks them.
- `grace`: gateways that recovered within the last grace period. A
  recovered gateway cannot know which peers have not recovered yet, whose
  running calls Valkey then lacks, so for a grace period (two renewal
  intervals, 60 seconds) its admissions are waivers too. The grace may end
  only once every gateway has recovered: each renews every 30 seconds, so
  each notices within one interval. That timing is an assumption, and the
  guard on `graceEnds` is where the model states it.
- `buffer` and `ledger`: rows on their way to ClickHouse, and there: the
  event, its cost and the minute its call ended.
- `latest`: the snapshot golem-control last published: its boundary `b`,
  the spend of the ledger's rows that ended before it, and the ledger's
  whole balance `whole` (for the broken hand-off).
- `snaps`: the snapshot each gateway has applied. Gateways apply snapshots
  independently; this is the skew the first version of this model hid by
  applying every snapshot everywhere at once.

```quint exposure.qnt +=
  type Row = (str, int, int)
  type Snap = { b: int, spend: int, whole: int }

  var now: int
  var counter: int -> int
  var pending: str -> Set[Event]
  var gwDown: Set[str]
  var grace: Set[str]
  var buffer: Set[Row]
  var ledger: Set[Row]
  var latest: Snap
  var snaps: str -> Snap
```

### 3.3 Valkey's slots

`slots` are the calls Valkey holds a concurrency slot for; `valkeyUp`
whether gateways can reach it. `views` is each gateway's known spend as it
last computed it: during an outage that is all it has.

```quint exposure.qnt +=
  var slots: Set[str]
  var valkeyUp: bool
  var views: str -> int
```

### 3.4 Ghosts

- `observed`: every event any gateway emitted, the spend golem knows of.
- `uncounted`: spend golem never heard of, or lost from known spend: a
  failover's lost minutes, a cancelled call's billing past what the
  gateway saw.
- `waived`: calls admitted while Valkey was unreachable.
- `heldEver`: the spend gateways ever held during an outage: while a
  gateway holds it, the others admit without it.
- `overAdmit`: a call admitted while Valkey was reachable although the
  principal or the scope was already at its limit: the bug.

```quint exposure.qnt +=
  var observed: Set[Event]
  var uncounted: int
  var waived: Set[str]
  var heldEver: int
  var overAdmit: bool
```

## 4. The initial state and helpers

```quint exposure.qnt +=
  action init = all {
    phase' = REQUESTS.mapBy(r => "idle"),
    gatewayOf' = REQUESTS.mapBy(r => ""),
    billed' = REQUESTS.mapBy(r => 0),
    seenCost' = REQUESTS.mapBy(r => 0),
    now' = 0, counter' = Map(), pending' = GATEWAYS.mapBy(g => Set()), gwDown' = Set(), grace' = Set(),
    buffer' = Set(), ledger' = Set(),
    latest' = blankSnap, snaps' = GATEWAYS.mapBy(g => blankSnap),
    slots' = Set(), valkeyUp' = true, views' = GATEWAYS.mapBy(g => 0),
    observed' = Set(), uncounted' = 0, waived' = Set(), heldEver' = 0, overAdmit' = false,
  }

  // The first snapshot: nothing from the ledger, every minute counted.
  val blankSnap: Snap = { b: 0, spend: 0, whole: 0 }
  def sum(events: Set[Event]): int = events.fold(0, (acc, e) => acc + e._2)
  def rowsSum(rows: Set[Row]): int = rows.fold(0, (acc, r) => acc + r._2)
  def running: Set[str] = REQUESTS.filter(r => phase.get(r) == "inflight" or phase.get(r) == "cancelled")
  def ownedBy(rs: Set[str], p: str): Set[str] = rs.filter(r => OWNER.get(r) == p)
  def totalBilled: int = REQUESTS.fold(0, (acc, r) => acc + billed.get(r))
  def counted: int = sum(observed)
  def held: int = GATEWAYS.fold(0, (acc, g) => acc + sum(pending.get(g)))
  // A gateway uses the store if Valkey answers and it has recovered.
  def usesStore(g: str): bool = valkeyUp and not(gwDown.contains(g))
```

Known spend, the number every check reads, for one gateway. In the design:
its snapshot's spend, the calls that ended before `b`, plus the counter's
minutes from `b` on. A call is on one side of `b` only, by the one minute
its row and its counter increment share, so it is counted once; the real
check reads at most a lease's minutes, in one script, whatever the
traffic.

```quint exposure.qnt +=
  def counterFrom(b: int): int = counter.keys().filter(t => t >= b).fold(0, (acc, t) => acc + counter.get(t))
  def counterAll: int = counter.keys().fold(0, (acc, t) => acc + counter.get(t))
  def addAt(c: int -> int, t: int, cost: int): int -> int =
    if (c.keys().contains(t)) c.set(t, c.get(t) + cost) else c.put(t, cost)
  def known(g: str): int =
    val sn = snaps.get(g)
    if (HANDOFF == "sum") sn.whole + counterAll
    else sn.spend + counterFrom(sn.b)
```

"The rest stays as it is", by group:

```quint exposure.qnt +=
  action keepCalls = all { phase' = phase, gatewayOf' = gatewayOf, billed' = billed, seenCost' = seenCost }
  action keepSpend = all {
    now' = now, counter' = counter, pending' = pending, gwDown' = gwDown, grace' = grace, buffer' = buffer, ledger' = ledger,
    latest' = latest, snaps' = snaps, views' = views,
  }
  action keepGhosts = all { observed' = observed, uncounted' = uncounted, waived' = waived, heldEver' = heldEver, overAdmit' = overAdmit }
```

## 5. Actions

### 5.1 Admit a call

With Valkey reachable, the check of ADR 0031 and the slot limits: the
principal's and, with `SCOPE_LIMIT_ON`, the scope's. The ghost compares
those limits with the calls really running (the provider's work), not with
Valkey's slots, so a slot freed too early or forgotten shows up here.

While Valkey is unreachable, a gateway admits against what it knows: its
own running calls for the principal and the scope, and known spend from
the counter as it last read it (`views`). The call is waived. (An earlier
version checked the true counter here, which no gateway can read during an
outage; the limiter's Hegel test found the model refusing a call any
implementation would admit.)

```quint exposure.qnt +=
  action admit(request: str, gateway: str): bool = all {
    phase.get(request) == "idle",
    val p = OWNER.get(request)
    val local = running.filter(r => gatewayOf.get(r) == gateway)
    if (usesStore(gateway)) all {
      known(gateway) < BUDGET,
      views' = views.put(gateway, known(gateway)), // the check reads the counter
      ownedBy(slots, p).size() < LIMIT,
      if (SCOPE_LIMIT_ON) slots.size() < SCOPE_LIMIT else true,
      slots' = slots.union(Set(request)),
      // In grace, peers may still hold calls Valkey lacks: a waiver.
      waived' = if (grace.contains(gateway)) waived.union(Set(request)) else waived,
      overAdmit' = (overAdmit or (not(grace.contains(gateway)) and (ownedBy(running, p).size() >= LIMIT or running.size() >= SCOPE_LIMIT))),
    } else all {
      views.get(gateway) < BUDGET,
      views' = views,
      ownedBy(local, p).size() < LIMIT,
      if (SCOPE_LIMIT_ON) local.size() < SCOPE_LIMIT else true,
      slots' = slots,
      waived' = waived.union(Set(request)),
      overAdmit' = overAdmit,
    },
    phase' = phase.put(request, "inflight"),
    gatewayOf' = gatewayOf.put(request, gateway),
    billed' = billed, seenCost' = seenCost,
    valkeyUp' = valkeyUp, observed' = observed, uncounted' = uncounted, heldEver' = heldEver,
    now' = now, counter' = counter, pending' = pending, gwDown' = gwDown, grace' = grace, buffer' = buffer, ledger' = ledger,
    latest' = latest, snaps' = snaps,
  }
```

### 5.2 Emitting a spend event

Both a completion and a cancellation emit one event, at the call's
gateway, in the minute it happens. Its ledger row is written at once and
travels on its own. If the gateway uses the store, the event's cost is
added to its minute in the counter; otherwise the increment waits with the
gateway (§5.6).

```quint exposure.qnt +=
  action emit(id: str, cost: int, g: str): bool = all {
    val e = (id, cost, now)
    all {
      observed' = observed.union(Set(e)),
      counter' = if (usesStore(g)) addAt(counter, now, cost) else counter,
      buffer' = buffer.union(Set(e)),
      pending' = if (usesStore(g)) pending else pending.put(g, pending.get(g).union(Set(e))),
      heldEver' = if (usesStore(g)) heldEver else heldEver + cost,
    },
    now' = now, gwDown' = gwDown, grace' = grace, ledger' = ledger, latest' = latest, snaps' = snaps, views' = views,
  }
```

### 5.3 A call completes

```quint exposure.qnt +=
  action complete(request: str, cost: int): bool = all {
    phase.get(request) == "inflight",
    phase' = phase.put(request, "done"),
    billed' = billed.put(request, cost),
    seenCost' = seenCost.put(request, cost),
    emit(request, cost, gatewayOf.get(request)),
    slots' = slots.exclude(Set(request)),
    gatewayOf' = gatewayOf, valkeyUp' = valkeyUp,
    uncounted' = uncounted, waived' = waived, overAdmit' = overAdmit,
  }
```

### 5.4 The client cancels, the provider carries on

The gateway closes its upstream connection and emits what it had seen
(the stream's deltas so far). The provider may go on generating, and
billing, for a while; the gateway cannot know when it stops. With
`HOLD_ON_CANCEL` the slot stays until the upstream has ended
(`providerEnds`: in practice the connection's close, or the deployment's
maximum duration).

```quint exposure.qnt +=
  action cancel(request: str, seen: int): bool = all {
    phase.get(request) == "inflight",
    phase' = phase.put(request, "cancelled"),
    seenCost' = seenCost.put(request, seen),
    emit(request, seen, gatewayOf.get(request)),
    slots' = if (HOLD_ON_CANCEL) slots else slots.exclude(Set(request)),
    gatewayOf' = gatewayOf, billed' = billed, valkeyUp' = valkeyUp,
    uncounted' = uncounted, waived' = waived, overAdmit' = overAdmit,
  }

  action providerEnds(request: str, cost: int): bool = all {
    phase.get(request) == "cancelled",
    cost >= seenCost.get(request),
    phase' = phase.put(request, "done"),
    billed' = billed.put(request, cost),
    uncounted' = uncounted + (cost - seenCost.get(request)),
    slots' = slots.exclude(Set(request)),
    gatewayOf' = gatewayOf, seenCost' = seenCost, valkeyUp' = valkeyUp,
    keepSpend, observed' = observed, waived' = waived, heldEver' = heldEver, overAdmit' = overAdmit,
  }
```

### 5.5 Time passes, the ledger ingests, the control plane publishes, gateways apply

Ingestion is late and in any order, but within `ALLOWANCE` minutes: the
clock does not move past a row's minute plus the allowance while the row
is still on its way. That is the deployment's assumption, not a property
golem enforces; a row later than that is an undercount until the next
snapshot whose boundary passes it, or reconciliation (ADR 0048).

golem-control places a snapshot's boundary `b` the allowance behind the
time it publishes, so every row before `b` is in the ledger, and the
snapshot's spend is those rows (`"noAllowance"`: `b` is the publication's
own time). Publication and application are separate steps, and each
gateway applies on its own.

```quint exposure.qnt +=
  action tick: bool = all {
    buffer.forall(r => r._3 >= now + 1 - ALLOWANCE),
    now' = now + 1,
    counter' = counter, pending' = pending, gwDown' = gwDown, grace' = grace, buffer' = buffer, ledger' = ledger,
    latest' = latest, snaps' = snaps, views' = views,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp, keepGhosts,
  }

  action deliver(r: Row): bool = all {
    buffer.contains(r),
    buffer' = buffer.exclude(Set(r)),
    ledger' = ledger.union(Set(r)),
    now' = now, counter' = counter, pending' = pending, gwDown' = gwDown, grace' = grace, latest' = latest, snaps' = snaps, views' = views,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp, keepGhosts,
  }

  def boundary: int = if (HANDOFF == "noAllowance") now else now - ALLOWANCE

  action publishSnapshot: bool = all {
    val b = boundary
    latest' = { b: b, spend: rowsSum(ledger.filter(r => r._3 < b)), whole: rowsSum(ledger) },
    now' = now, counter' = counter, pending' = pending, gwDown' = gwDown, grace' = grace, buffer' = buffer, ledger' = ledger,
    snaps' = snaps, views' = views,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp, keepGhosts,
  }

  action applySnapshot(gateway: str): bool = all {
    snaps.get(gateway) != latest,
    snaps' = snaps.put(gateway, latest),
    // The bug: start the counter again at each snapshot, as if the
    // snapshot held everything before it.
    counter' = if (HANDOFF == "resetAtApply" and valkeyUp) Map() else counter,
    now' = now, pending' = pending, gwDown' = gwDown, grace' = grace, buffer' = buffer, ledger' = ledger, latest' = latest, views' = views,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp, keepGhosts,
  }
```

### 5.6 Valkey

**Unreachable.** Gateways cannot renew slots, so every slot's TTL lapses
(the model takes the outage as longer than the 90-second TTL: the worst
case), and every gateway notices (its renewal fails within 30 seconds).
The counter itself survives.

**Back.** Valkey answers, and each gateway, on its own, replays the
increments it held, one at a time, each into its own minute, and then
recovers: with `REACQUIRE` it re-registers a slot for each of its calls
still running, cancelled ones included, and uses the store again.

**A failover.** Valkey is reachable but lost some of its latest writes:
here, a minute's spend. What it held is uncounted until the snapshots'
boundary passes that minute, when the ledger's rows count it again; the
model counts it lost for good, the worst case. The implementation narrows
the window by waiting for a replica to acknowledge each increment, which
the model, like the real failure, need not honour.

```quint exposure.qnt +=
  action valkeyDown: bool = all {
    valkeyUp,
    valkeyUp' = false,
    gwDown' = GATEWAYS,
    grace' = grace,
    slots' = Set(),
    now' = now, counter' = counter, pending' = pending, buffer' = buffer, ledger' = ledger,
    latest' = latest, snaps' = snaps, views' = views,
    keepCalls, keepGhosts,
  }

  action valkeyBack: bool = all {
    not(valkeyUp),
    valkeyUp' = true,
    slots' = slots, keepCalls, keepSpend, keepGhosts,
  }

  action replayOne(gateway: str, e: Event): bool = all {
    valkeyUp,
    gwDown.contains(gateway),
    pending.get(gateway).contains(e),
    counter' = addAt(counter, e._3, e._2),
    pending' = pending.put(gateway, pending.get(gateway).exclude(Set(e))),
    now' = now, buffer' = buffer, gwDown' = gwDown, grace' = grace, ledger' = ledger, latest' = latest, snaps' = snaps, views' = views,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp, keepGhosts,
  }

  action recover(gateway: str): bool = all {
    valkeyUp,
    gwDown.contains(gateway),
    pending.get(gateway) == Set(),
    gwDown' = gwDown.exclude(Set(gateway)),
    grace' = grace.union(Set(gateway)),
    slots' = if (REACQUIRE) slots.union(running.filter(r => gatewayOf.get(r) == gateway)) else slots,
    now' = now, counter' = counter, pending' = pending, buffer' = buffer, ledger' = ledger,
    latest' = latest, snaps' = snaps, views' = views,
    keepCalls, valkeyUp' = valkeyUp, keepGhosts,
  }

  // A gateway's grace ends, which the model allows only once every
  // gateway has recovered (each within one renewal interval: §3.2).
  action graceEnds(gateway: str): bool = all {
    grace.contains(gateway),
    gwDown == Set(),
    grace' = grace.exclude(Set(gateway)),
    now' = now, counter' = counter, pending' = pending, gwDown' = gwDown, buffer' = buffer, ledger' = ledger,
    latest' = latest, snaps' = snaps, views' = views,
    keepCalls, keepGhosts, slots' = slots, valkeyUp' = valkeyUp,
  }

  action failover(t: int): bool = all {
    valkeyUp,
    counter.keys().contains(t),
    counter' = counter.keys().exclude(Set(t)).mapBy(m => counter.get(m)),
    uncounted' = uncounted + counter.get(t),
    now' = now, pending' = pending, gwDown' = gwDown, grace' = grace, buffer' = buffer, ledger' = ledger, latest' = latest, snaps' = snaps, views' = views,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp,
    observed' = observed, waived' = waived, heldEver' = heldEver, overAdmit' = overAdmit,
  }
```

### 5.7 The step

```quint exposure.qnt +=
  action step = {
    nondet request = REQUESTS.oneOf()
    nondet gateway = GATEWAYS.oneOf()
    nondet cost = COSTS.oneOf()
    nondet r = buffer.union(Set(("none", 0, 0))).oneOf()
    nondet e = pending.get(gateway).union(Set(("none", 0, 0))).oneOf()
    nondet t = counter.keys().union(Set(-1)).oneOf()
    any {
      tick,
      admit(request, gateway),
      complete(request, cost),
      cancel(request, cost),
      providerEnds(request, cost),
      deliver(r),
      publishSnapshot,
      applySnapshot(gateway),
      valkeyDown,
      valkeyBack,
      replayOne(gateway, e),
      recover(gateway),
      graceEnds(gateway),
      failover(t),
    }
  }
```

## 6. Properties

### 6.1 The limits hold whenever Valkey is reachable

The in-flight limits must bound the provider's work, not the gateway's
bookkeeping: a call the provider is still running counts, whatever Valkey
thinks.

```quint exposure.qnt +=
  val limitsHonoured = not(overAdmit)
```

### 6.2 The scope's stated bound

The first model's argument, at scope level: the last admission made with
Valkey reachable saw known spend below the budget, with fewer than
`SCOPE_LIMIT` calls running, each billing at most `WORST`. To that add
what golem never heard of and what was admitted under waiver.

```quint exposure.qnt +=
  val withinBound =
    totalBilled <= BUDGET + SCOPE_LIMIT * WORST + uncounted + waived.size() * WORST + heldEver
```

So the number ADR 0031 can advertise for a scope is `SCOPE_LIMIT × WORST`
over the budget, not a principal's `LIMIT × WORST`, plus two terms that are
reported, not bounded in advance: spend lost in failures (until
reconciliation books it), waived admissions, and spend gateways held during
an outage (which the others admitted without, until each recovered).

### 6.3 The handoff: never counted twice, no increment lost

Known spend never exceeds the spend golem has observed; and while Valkey
is reachable, it falls short only by what a failover lost (or a cancelled
call billed beyond what was seen, which `uncounted` also holds), while the
ledger ingests within its allowance. The second
is the sharper check of the handoff: the bound in §6.2 only fails once
enough lost increments pile up, which takes several snapshots in a row;
this fails at the first.

On every gateway, whichever snapshot it holds:

```quint exposure.qnt +=
  val knownNeverOver = GATEWAYS.forall(g => known(g) <= counted)
  val noLostIncrements = GATEWAYS.forall(g => usesStore(g) implies known(g) + uncounted + held >= counted)

  val safe = limitsHonoured and withinBound and knownNeverOver and noLostIncrements
```

### 6.4 Possibility

The design must be able to: go over the budget (the accepted risk); admit
under waiver past a limit, and still hold the bound; bill a cancelled call
more than the gateway saw; and hand a counter over to a snapshot while an
event is still on its way to the ledger.

```quint exposure.qnt +=
  val overshoots = totalBilled > BUDGET
  val waiverExceedsLimit = REQUESTS.exists(r => ownedBy(running, OWNER.get(r)).size() > LIMIT)
  val cancelledBilledMore = REQUESTS.exists(r => billed.get(r) > seenCost.get(r))
  // A gateway's snapshot holds some spend, and the counter's minutes after
  // its boundary hold more.
  val handoffInFlight = GATEWAYS.exists(g => snaps.get(g).spend > 0 and counterFrom(snaps.get(g).b) > 0)
  // Two gateways on different snapshots, both exact.
  val snapshotSkew = GATEWAYS.exists(g => GATEWAYS.exists(h => snaps.get(g).b != snaps.get(h).b))
```

## 7. The stories

**Three principals fill a scope** (`noScopeLimit`). Each of p1, p2 and p3
admits one call: each is within its own limit of one, but the scope's limit
is two.

```quint exposure.qnt +=
  run scopeExposureTest =
    init
      .then(admit("a1", "g1"))
      .then(admit("b1", "g1"))
      .then(admit("c1", "g2"))
      .expect(not(limitsHonoured))
```

**A counter reset at each snapshot forgets spend** (`resetAtApply`). A
call completes at 3 in minute 0; a snapshot is published at once, its
boundary the allowance before, so it holds nothing; g1 applies it and the
counter starts again. Known spend is 0 though 3 was spent. In the design
the counter keeps minute 0, which is after the boundary: known spend 3.

```quint exposure.qnt +=
  run resetTest =
    init
      .then(admit("a1", "g1")).then(complete("a1", 3))
      .then(publishSnapshot).then(applySnapshot("g1"))
      .expect(not(noLostIncrements))
```

**A summed counter counts twice** (`sumHandoff`). One call's row is in
the ledger and so in the snapshot, and its minute still in the counter.

```quint exposure.qnt +=
  run sumTest =
    init
      .then(admit("a1", "g1"))
      .then(complete("a1", 2))
      .then(deliver(("a1", 2, 0)))
      .then(publishSnapshot)
      .then(applySnapshot("g1"))
      .expect(not(knownNeverOver))
```

**A boundary without the allowance loses a row on its way**
(`noAllowance`). A call completes in minute 0; a minute later its row is
still on its way, within the allowance. A snapshot whose boundary is its
own time, minute 1, leaves the call out (it has no row before minute 1),
and the counter from minute 1 on lacks it too. In the design the boundary
is minute 0, and the counter's minute 0 counts the call.

```quint exposure.qnt +=
  run noAllowanceTest =
    init
      .then(admit("a1", "g1")).then(complete("a1", 3))
      .then(tick)
      .then(publishSnapshot)
      .then(applySnapshot("g1"))
      .expect(not(noLostIncrements))
```

**Two gateways, two snapshots, both exact** (the design). The call's row
is ingested and two minutes pass; g1 applies a snapshot with the call in
its spend (boundary minute 1), while g2, on the first snapshot, counts it
from the counter's minute 0. Both know 3.

```quint exposure.qnt +=
  run skewTest =
    init
      .then(admit("a1", "g1")).then(complete("a1", 3))
      .then(deliver(("a1", 3, 0)))
      .then(tick).then(tick)
      .then(publishSnapshot)
      .then(applySnapshot("g1"))
      .expect(snapshotSkew and known("g1") == 3 and known("g2") == 3 and noLostIncrements and knownNeverOver)
```

**Cancel and resend** (`releaseOnCancel`). p1 cancels its call; the
provider is still generating; the freed slot admits p1's next call.

```quint exposure.qnt +=
  run cancelResendTest =
    init
      .then(admit("a1", "g1"))
      .then(cancel("a1", 1))
      .then(admit("a2", "g1"))
      .expect(not(limitsHonoured))
```

**Forgotten slots** (`noReacquire`). p1's call streams through a Valkey
outage; when Valkey returns and g1 recovers (and its grace has ended), its
slot is gone and p1's next call is admitted beside it.

```quint exposure.qnt +=
  run forgottenSlotTest =
    init
      .then(admit("a1", "g1"))
      .then(valkeyDown)
      .then(valkeyBack)
      .then(recover("g1"))
      .then(recover("g2"))
      .then(graceEnds("g1"))
      .then(admit("a2", "g1"))
      .expect(not(limitsHonoured))
```

**A waiver** (the design). During an outage, two gateways each admit one
of p1's calls against their own counts: p1 has two calls running with a
limit of one. Nothing is hidden: both are waived, and the bound holds.

```quint exposure.qnt +=
  run waiverTest =
    init
      .then(valkeyDown)
      .then(admit("a1", "g1"))
      .then(admit("a2", "g2"))
      .expect(waiverExceedsLimit and waived.size() == 2 and withinBound and limitsHonoured)
```

The model ends here:

```quint exposure.qnt +=
}
```

## 8. What this model leaves out

- **Several sites.** Each site has its own Valkey and counter; a site's
  known spend lacks the other sites' minutes after the boundary until a
  later snapshot's boundary passes them (the allowance plus about a
  minute).
  Scope limits are per site, so the stated bound becomes, per scope, the
  sum over its active sites of `SCOPE_LIMIT × WORST`, plus what the other
  sites spend within one snapshot's lag, which their rate limits cap. With
  the secondary site a warm standby, that term is zero in normal
  operation. Not modelled.
- **The declared-worst-case check** (accounting.md): one call tighter, nothing else changes.
- **Rows later than the allowance**, and **retention**: the model holds the
  ledger to its allowance (`tick`'s guard) and keeps every minute; a later
  row undercounts until the next boundary passes it, and a boundary older
  than the retention would lose the minutes between it and the
  retention's start, but golem-control publishes none that old and known
  spend is never below the site's counter total (ADR 0048), which in the
  model, keeping every minute, never binds.
- **An increment added twice**: a replay whose reply was lost may be sent
  again and added twice; that errs high, and the model's gateways see
  every reply.
- **Crashes and reconciliation** (accounting.md): a gateway crash is
  another source of `uncounted`; reconciliation books it.
- **Prices**: `WORST` is a true bound only for the modalities and billing
  categories ADR 0041 §6 lists as verified.
- **How long a cancelled call's provider work lasts**: the model lets it
  last until `providerEnds`; the implementation must choose the slot's
  hold (the upstream connection's close where the provider stops on close,
  else the deployment's maximum duration).

## 9. The designs to check

| Instance | Switch off | Should break |
|---|---|---|
| `exposureDesign` | none | nothing (`safe` holds) |
| `noScopeLimit` | `SCOPE_LIMIT_ON` | `limitsHonoured` |
| `resetAtApply` | `HANDOFF = "resetAtApply"` | `noLostIncrements` |
| `sumHandoff` | `HANDOFF = "sum"` | `knownNeverOver` |
| `noAllowance` | `HANDOFF = "noAllowance"` | `noLostIncrements` |
| `releaseOnCancel` | `HOLD_ON_CANCEL` | `limitsHonoured` |
| `noReacquire` | `REACQUIRE` | `limitsHonoured` |

Each story passes in the design it names and fails in `exposureDesign`;
`waiverTest` and `skewTest` pass in `exposureDesign`.

```quint exposure.qnt +=

module exposureDesign {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2, ALLOWANCE = 1,
    SCOPE_LIMIT_ON = true, HANDOFF = "buckets", HOLD_ON_CANCEL = true, REACQUIRE = true).*
}

module noScopeLimit {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2, ALLOWANCE = 1,
    SCOPE_LIMIT_ON = false, HANDOFF = "buckets", HOLD_ON_CANCEL = true, REACQUIRE = true).*
}

module resetAtApply {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2, ALLOWANCE = 1,
    SCOPE_LIMIT_ON = true, HANDOFF = "resetAtApply", HOLD_ON_CANCEL = true, REACQUIRE = true).*
}

module sumHandoff {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2, ALLOWANCE = 1,
    SCOPE_LIMIT_ON = true, HANDOFF = "sum", HOLD_ON_CANCEL = true, REACQUIRE = true).*
}

module releaseOnCancel {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2, ALLOWANCE = 1,
    SCOPE_LIMIT_ON = true, HANDOFF = "buckets", HOLD_ON_CANCEL = false, REACQUIRE = true).*
}

module noAllowance {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2, ALLOWANCE = 1,
    SCOPE_LIMIT_ON = true, HANDOFF = "noAllowance", HOLD_ON_CANCEL = true, REACQUIRE = true).*
}

module noReacquire {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2, ALLOWANCE = 1,
    SCOPE_LIMIT_ON = true, HANDOFF = "buckets", HOLD_ON_CANCEL = true, REACQUIRE = false).*
}
```

## The small instance

The instance the exhaustive search explores (exposure.small.yaml, run by
check-exposure.sh where it has one): the design at sizes small enough to
search every run to a depth. The binding names this module rather than
repeating its constants, and the check scripts vary one switch at a time
with `-const`.

```quint exposure.qnt +=

module small {
  import exposure(REQUESTS = Set("a1", "a2", "b1"), OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 3), WORST = 3, BUDGET = 2, LIMIT = 1, SCOPE_LIMIT = 1, ALLOWANCE = 1,
    SCOPE_LIMIT_ON = true, HANDOFF = "buckets", HOLD_ON_CANCEL = true, REACQUIRE = true).*
}
```
