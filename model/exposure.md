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
| `HANDOFF` = `"byEvent"` | The counter and the snapshot are reconciled by event: known spend is the snapshot plus the counter's events the snapshot lacks | `"reset"`: the counter restarts with each snapshot and loses increments the ledger has not ingested yet. `"sum"`: the two are added and events in both count twice |
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
Only the soft check: the strict option tightens the bound by a call, as
accounting.md shows, and changes nothing here.

```quint exposure.qnt +=
  const REQUESTS: Set[str]
  const OWNER: str -> str
  const GATEWAYS: Set[str]
  const COSTS: Set[int]
  const WORST: int
  const BUDGET: int
  const LIMIT: int
  const SCOPE_LIMIT: int

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
what the gateway had seen so far.

```quint exposure.qnt +=
  type Event = (str, int)

  var phase: str -> str
  var gatewayOf: str -> str
  var billed: str -> int
  var seenCost: str -> int
```

### 3.2 Known spend: snapshot, counter, ledger

- `snap`: the known spend the last snapshot carried, and which events it
  included (the ledger's, deduplicated, when it was built).
- `counter`: the site counter in Valkey, kept as a set of events, so
  adding one twice is harmless (the real counter is a hash by event ID).
- `pending`: increments a gateway holds while Valkey is unreachable, to
  replay when it returns.
- `buffer` and `ledger`: events on their way to ClickHouse, and there.

```quint exposure.qnt +=
  var snap: { events: Set[Event], spend: int }
  var counter: Set[Event]
  var pending: Set[Event]
  var buffer: Set[Event]
  var ledger: Set[Event]
```

### 3.3 Valkey's slots

`slots` are the calls Valkey holds a concurrency slot for; `valkeyUp`
whether gateways can reach it.

```quint exposure.qnt +=
  var slots: Set[str]
  var valkeyUp: bool
```

### 3.4 Ghosts

- `observed`: every event any gateway emitted, the spend golem knows of.
- `uncounted`: spend golem never heard of, or lost from known spend: a
  failover's lost increments, a cancelled call's billing past what the
  gateway saw.
- `waived`: calls admitted while Valkey was unreachable.
- `overAdmit`: a call admitted while Valkey was reachable although the
  principal or the scope was already at its limit: the bug.

```quint exposure.qnt +=
  var observed: Set[Event]
  var uncounted: int
  var waived: Set[str]
  var overAdmit: bool
```

## 4. The initial state and helpers

```quint exposure.qnt +=
  action init = all {
    phase' = REQUESTS.mapBy(r => "idle"),
    gatewayOf' = REQUESTS.mapBy(r => ""),
    billed' = REQUESTS.mapBy(r => 0),
    seenCost' = REQUESTS.mapBy(r => 0),
    snap' = { events: Set(), spend: 0 },
    counter' = Set(), pending' = Set(), buffer' = Set(), ledger' = Set(),
    slots' = Set(), valkeyUp' = true,
    observed' = Set(), uncounted' = 0, waived' = Set(), overAdmit' = false,
  }

  def sum(events: Set[Event]): int = events.fold(0, (acc, e) => acc + e._2)
  def running: Set[str] = REQUESTS.filter(r => phase.get(r) == "inflight" or phase.get(r) == "cancelled")
  def ownedBy(rs: Set[str], p: str): Set[str] = rs.filter(r => OWNER.get(r) == p)
  def totalBilled: int = REQUESTS.fold(0, (acc, r) => acc + billed.get(r))
  def counted: int = sum(observed)
```

Known spend, the number every check reads. The design adds to the
snapshot only the counter's events the snapshot does not include; the
`"sum"` bug adds the whole counter.

```quint exposure.qnt +=
  def known: int =
    snap.spend + (if (HANDOFF == "sum") sum(counter) else sum(counter.exclude(snap.events)))
```

"The rest stays as it is", by group:

```quint exposure.qnt +=
  action keepCalls = all { phase' = phase, gatewayOf' = gatewayOf, billed' = billed, seenCost' = seenCost }
  action keepSpend = all { snap' = snap, counter' = counter, pending' = pending, buffer' = buffer, ledger' = ledger }
  action keepGhosts = all { observed' = observed, uncounted' = uncounted, waived' = waived, overAdmit' = overAdmit }
```

## 5. Actions

### 5.1 Admit a call

With Valkey reachable, the check of ADR 0031 and the slot limits: the
principal's and, with `SCOPE_LIMIT_ON`, the scope's. The ghost compares
those limits with the calls really running (the provider's work), not with
Valkey's slots, so a slot freed too early or forgotten shows up here.

While Valkey is unreachable, a gateway admits against what it knows: its
own running calls for the principal and the scope, and known spend as last
seen. The call is waived.

```quint exposure.qnt +=
  action admit(request: str, gateway: str): bool = all {
    phase.get(request) == "idle",
    known < BUDGET,
    val p = OWNER.get(request)
    val local = running.filter(r => gatewayOf.get(r) == gateway)
    if (valkeyUp) all {
      ownedBy(slots, p).size() < LIMIT,
      if (SCOPE_LIMIT_ON) slots.size() < SCOPE_LIMIT else true,
      slots' = slots.union(Set(request)),
      waived' = waived,
      overAdmit' = (overAdmit or ownedBy(running, p).size() >= LIMIT or running.size() >= SCOPE_LIMIT),
    } else all {
      ownedBy(local, p).size() < LIMIT,
      if (SCOPE_LIMIT_ON) local.size() < SCOPE_LIMIT else true,
      slots' = slots,
      waived' = waived.union(Set(request)),
      overAdmit' = overAdmit,
    },
    phase' = phase.put(request, "inflight"),
    gatewayOf' = gatewayOf.put(request, gateway),
    billed' = billed, seenCost' = seenCost,
    valkeyUp' = valkeyUp, keepSpend, observed' = observed, uncounted' = uncounted,
  }
```

### 5.2 Emitting a spend event

Both a completion and a cancellation emit one event: into the counter (or
held while Valkey is unreachable) and towards the ledger.

```quint exposure.qnt +=
  action emit(e: Event): bool = all {
    observed' = observed.union(Set(e)),
    buffer' = buffer.union(Set(e)),
    counter' = if (valkeyUp) counter.union(Set(e)) else counter,
    pending' = if (valkeyUp) pending else pending.union(Set(e)),
    snap' = snap, ledger' = ledger,
  }
```

### 5.3 A call completes

```quint exposure.qnt +=
  action complete(request: str, cost: int): bool = all {
    phase.get(request) == "inflight",
    phase' = phase.put(request, "done"),
    billed' = billed.put(request, cost),
    seenCost' = seenCost.put(request, cost),
    emit((request, cost)),
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
    emit((request, seen)),
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
    keepSpend, observed' = observed, waived' = waived, overAdmit' = overAdmit,
  }
```

### 5.5 The ledger ingests, the control plane publishes

Ingestion is late and in any order. A snapshot carries the ledger as it is
at that moment, including only the events already ingested; the events
still in the buffer are in the counter only. The handoff rule decides what
happens to the counter:

- `"byEvent"` (the design): events the snapshot includes are dropped from
  the counter (garbage collection; the `known` formula would ignore them
  anyway); the rest stay.
- `"reset"`: the counter restarts, as if it counted "since the last
  snapshot", and the events not yet ingested vanish from known spend until
  a later snapshot has them.
- `"sum"`: the counter is left as it is and added whole.

```quint exposure.qnt +=
  action deliver(e: Event): bool = all {
    buffer.contains(e),
    buffer' = buffer.exclude(Set(e)),
    ledger' = ledger.union(Set(e)),
    snap' = snap, counter' = counter, pending' = pending,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp, keepGhosts,
  }

  action publishSnapshot: bool = all {
    snap' = { events: ledger, spend: sum(ledger) },
    counter' = if (HANDOFF == "byEvent") counter.exclude(ledger)
               else if (HANDOFF == "reset") Set()
               else counter,
    pending' = pending, buffer' = buffer, ledger' = ledger,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp, keepGhosts,
  }
```

### 5.6 Valkey

**Unreachable.** Gateways cannot renew slots, so every slot's TTL lapses
(the model takes the outage as longer than the 90-second TTL: the worst
case). The counter itself survives.

**Back.** Gateways replay the increments they held (harmless if repeated:
the counter is by event) and, with `REACQUIRE`, re-register a slot for
each call still running, cancelled ones included.

**A failover.** Valkey is reachable but lost its latest writes: counter
events the snapshot does not include are gone until a later snapshot has
them.

```quint exposure.qnt +=
  action valkeyDown: bool = all {
    valkeyUp,
    valkeyUp' = false,
    slots' = Set(),
    keepCalls, keepSpend, keepGhosts,
  }

  action valkeyBack: bool = all {
    not(valkeyUp),
    valkeyUp' = true,
    counter' = counter.union(pending),
    pending' = Set(),
    slots' = if (REACQUIRE) running else Set(),
    snap' = snap, buffer' = buffer, ledger' = ledger,
    keepCalls, keepGhosts,
  }

  action failover: bool = all {
    valkeyUp,
    val lost = counter.exclude(snap.events)
    all {
      lost.size() > 0,
      counter' = counter.intersect(snap.events),
      uncounted' = uncounted + sum(lost),
    },
    snap' = snap, pending' = pending, buffer' = buffer, ledger' = ledger,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp,
    observed' = observed, waived' = waived, overAdmit' = overAdmit,
  }
```

### 5.7 The step

```quint exposure.qnt +=
  action step = {
    nondet request = REQUESTS.oneOf()
    nondet gateway = GATEWAYS.oneOf()
    nondet cost = COSTS.oneOf()
    nondet e = observed.union(Set(("none", 0))).oneOf()
    any {
      admit(request, gateway),
      complete(request, cost),
      cancel(request, cost),
      providerEnds(request, cost),
      deliver(e),
      publishSnapshot,
      valkeyDown,
      valkeyBack,
      failover,
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
    totalBilled <= BUDGET + SCOPE_LIMIT * WORST + uncounted + waived.size() * WORST
```

So the number ADR 0031 can advertise for a scope is `SCOPE_LIMIT × WORST`
over the budget, not a principal's `LIMIT × WORST`, plus two terms that are
reported, not bounded in advance: spend lost in failures (until
reconciliation books it) and waived admissions.

### 6.3 The handoff: never counted twice, no increment lost

Known spend never exceeds the spend golem has observed; and while Valkey
is reachable, it falls short only by what a failover lost (or a cancelled
call billed beyond what was seen, which `uncounted` also holds). The second
is the sharper check of the handoff: the bound in §6.2 only fails once
enough lost increments pile up, which takes several snapshots in a row;
this fails at the first.

```quint exposure.qnt +=
  val knownNeverOver = known <= counted
  val noLostIncrements = valkeyUp implies known + uncounted >= counted

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
  val handoffInFlight = snap.spend > 0 and counter.exclude(snap.events).size() > 0
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

**A reset counter forgets spend** (`resetHandoff`). Each call completes at
3; a snapshot is published before the ledger has ingested it, the counter
restarts, and known spend is back to 0. Four calls later the scope has
spent 12, past its bound of 4 + 2 × 3 = 10, with nothing lost and nothing
waived. In the design the third call is refused: known spend is 6.

```quint exposure.qnt +=
  run resetTest =
    init
      .then(admit("a1", "g1")).then(complete("a1", 3)).then(publishSnapshot)
      .then(admit("a2", "g1")).then(complete("a2", 3)).then(publishSnapshot)
      .then(admit("b1", "g1")).then(complete("b1", 3)).then(publishSnapshot)
      .then(admit("b2", "g1")).then(complete("b2", 3)).then(publishSnapshot)
      .expect(not(withinBound))
```

**A summed counter counts twice** (`sumHandoff`). One call's event is in
the ledger and so in the snapshot, and still in the counter.

```quint exposure.qnt +=
  run sumTest =
    init
      .then(admit("a1", "g1"))
      .then(complete("a1", 2))
      .then(deliver(("a1", 2)))
      .then(publishSnapshot)
      .expect(not(knownNeverOver))
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
outage; when Valkey returns its slot is gone and p1's next call is
admitted beside it.

```quint exposure.qnt +=
  run forgottenSlotTest =
    init
      .then(admit("a1", "g1"))
      .then(valkeyDown)
      .then(valkeyBack)
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
  known spend lacks the other sites' counters until the ledger has their
  events and a snapshot carries them (about a minute plus ingestion lag).
  Scope limits are per site, so the stated bound becomes, per scope, the
  sum over its active sites of `SCOPE_LIMIT × WORST`, plus what the other
  sites spend within one snapshot's lag, which their rate limits cap. With
  the secondary site a warm standby, that term is zero in normal
  operation. Not modelled.
- **Strict mode** (accounting.md): one call tighter, nothing else changes.
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
| `resetHandoff` | `HANDOFF = "reset"` | `noLostIncrements`, and `withinBound` (by its story: too rare for simulation) |
| `sumHandoff` | `HANDOFF = "sum"` | `knownNeverOver` |
| `releaseOnCancel` | `HOLD_ON_CANCEL` | `limitsHonoured` |
| `noReacquire` | `REACQUIRE` | `limitsHonoured` |

Each story passes in the design it names and fails in `exposureDesign`;
`waiverTest` passes in `exposureDesign`.

```quint exposure.qnt +=

module exposureDesign {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2,
    SCOPE_LIMIT_ON = true, HANDOFF = "byEvent", HOLD_ON_CANCEL = true, REACQUIRE = true).*
}

module noScopeLimit {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2,
    SCOPE_LIMIT_ON = false, HANDOFF = "byEvent", HOLD_ON_CANCEL = true, REACQUIRE = true).*
}

module resetHandoff {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2,
    SCOPE_LIMIT_ON = true, HANDOFF = "reset", HOLD_ON_CANCEL = true, REACQUIRE = true).*
}

module sumHandoff {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2,
    SCOPE_LIMIT_ON = true, HANDOFF = "sum", HOLD_ON_CANCEL = true, REACQUIRE = true).*
}

module releaseOnCancel {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2,
    SCOPE_LIMIT_ON = true, HANDOFF = "byEvent", HOLD_ON_CANCEL = false, REACQUIRE = true).*
}

module noReacquire {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2,
    SCOPE_LIMIT_ON = true, HANDOFF = "byEvent", HOLD_ON_CANCEL = true, REACQUIRE = false).*
}
```
