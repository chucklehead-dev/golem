# A scope's exposure: the second accounting model

A literate Quint specification, the companion of
[accounting.md](accounting.md). That model shows the soft-budget bound for
one principal on one gateway, with a working Valkey. This one asks what
[ADR 0041](../docs/adr/0041-security-boundaries-and-incident-controls.md)
§6 says the first model does not establish:

> A scope's pool is shared by several principals, served by several
> gateways on a site. Known spend is a snapshot from the ledger, which
> ingests events late. Valkey can be unreachable. A client can cancel a
> call the provider keeps working on. Does the scope stay within a stated
> bound, and when it cannot, is the exposure at least recorded?

Known spend is the snapshot's alone ([ADR 0052](../docs/adr/0052-accounting-for-m1.md)):
golem-control publishes each scope's spend about every minute, and the
site keeps no spend counter. Valkey holds the concurrency slots. The
design's answer is yes, if three rules hold:

| Switch | When on (the design) | When off (the bug it prevents) |
|---|---|---|
| `SCOPE_LIMIT_ON` | A scope has its own in-flight limit, checked with the principal's | Each principal stays within its limit, but the scope's exposure is their sum: grows with every member who joins |
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

### 3.2 Known spend: the snapshot

- `latest`: the known spend golem-control last published for the scope:
  the spend of the events the ledger had then. Where it places the
  boundary of what it counts (ADR 0048's ingestion allowance) is its
  concern: here a snapshot is the spend of some subset of the events
  emitted before it, which is all a late, out-of-order ingestion can
  give.
- `snaps`: the known spend each gateway has applied. Gateways apply
  snapshots independently.
- `gwDown`: the gateways that believe Valkey unreachable.
- `grace`: gateways that recovered within the last grace period. A
  recovered gateway cannot know which peers have not recovered yet, whose
  running calls' slots Valkey then lacks, so for a grace period (two
  renewal intervals, 60 seconds) its admissions are waivers too. The grace
  may end only once every gateway has recovered: each renews every 30
  seconds, so each notices within one interval. That timing is an
  assumption, and the guard on `graceEnds` is where the model states it.

```quint exposure.qnt +=
  var latest: int
  var snaps: str -> int
  var gwDown: Set[str]
  var grace: Set[str]
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
- `uncounted`: spend golem never heard of: a cancelled call's billing
  past what the gateway saw.
- `waived`: calls admitted while Valkey was unreachable, or in a grace.
- `lag`: the most spend any admission did not see: what golem had
  observed but the admitting gateway's snapshot lacked, the largest over
  every admission.
- `overAdmit`: a call admitted while Valkey was reachable although the
  principal or the scope was already at its limit: the bug.

```quint exposure.qnt +=
  var observed: Set[Event]
  var uncounted: int
  var waived: Set[str]
  var lag: int
  var overAdmit: bool
```

## 4. The initial state and helpers

```quint exposure.qnt +=
  action init = all {
    phase' = REQUESTS.mapBy(r => "idle"),
    gatewayOf' = REQUESTS.mapBy(r => ""),
    billed' = REQUESTS.mapBy(r => 0),
    seenCost' = REQUESTS.mapBy(r => 0),
    latest' = 0, snaps' = GATEWAYS.mapBy(g => 0), gwDown' = Set(), grace' = Set(),
    slots' = Set(), valkeyUp' = true,
    observed' = Set(), uncounted' = 0, waived' = Set(), lag' = 0, overAdmit' = false,
  }

  def sum(events: Set[Event]): int = events.fold(0, (acc, e) => acc + e._2)
  def running: Set[str] = REQUESTS.filter(r => phase.get(r) == "inflight" or phase.get(r) == "cancelled")
  def ownedBy(rs: Set[str], p: str): Set[str] = rs.filter(r => OWNER.get(r) == p)
  def totalBilled: int = REQUESTS.fold(0, (acc, r) => acc + billed.get(r))
  def counted: int = sum(observed)
  // A gateway uses the store if Valkey answers and it has recovered.
  def usesStore(g: str): bool = valkeyUp and not(gwDown.contains(g))
  // Known spend, the number every check reads: the gateway's snapshot.
  def known(g: str): int = snaps.get(g)
```

"The rest stays as it is", by group:

```quint exposure.qnt +=
  action keepCalls = all { phase' = phase, gatewayOf' = gatewayOf, billed' = billed, seenCost' = seenCost }
  action keepSpend = all { latest' = latest, snaps' = snaps, gwDown' = gwDown, grace' = grace }
  action keepGhosts = all { observed' = observed, uncounted' = uncounted, waived' = waived, lag' = lag, overAdmit' = overAdmit }
```

## 5. Actions

### 5.1 Admit a call

With Valkey reachable, the check of ADR 0031 and the slot limits: the
principal's and, with `SCOPE_LIMIT_ON`, the scope's. The ghost compares
those limits with the calls really running (the provider's work), not with
Valkey's slots, so a slot freed too early or forgotten shows up here.

While Valkey is unreachable, a gateway admits against what it knows: its
own running calls for the principal and the scope, and its snapshot's
known spend, which needs no Valkey. The call is waived.

```quint exposure.qnt +=
  action admit(request: str, gateway: str): bool = all {
    phase.get(request) == "idle",
    known(gateway) < BUDGET,
    val p = OWNER.get(request)
    val local = running.filter(r => gatewayOf.get(r) == gateway)
    if (usesStore(gateway)) all {
      ownedBy(slots, p).size() < LIMIT,
      if (SCOPE_LIMIT_ON) slots.size() < SCOPE_LIMIT else true,
      slots' = slots.union(Set(request)),
      // In grace, peers may still run calls Valkey has no slot for: a waiver.
      waived' = if (grace.contains(gateway)) waived.union(Set(request)) else waived,
      overAdmit' = (overAdmit or (not(grace.contains(gateway)) and (ownedBy(running, p).size() >= LIMIT or running.size() >= SCOPE_LIMIT))),
    } else all {
      ownedBy(local, p).size() < LIMIT,
      if (SCOPE_LIMIT_ON) local.size() < SCOPE_LIMIT else true,
      slots' = slots,
      waived' = waived.union(Set(request)),
      overAdmit' = overAdmit,
    },
    lag' = if (counted - known(gateway) > lag) counted - known(gateway) else lag,
    phase' = phase.put(request, "inflight"),
    gatewayOf' = gatewayOf.put(request, gateway),
    billed' = billed, seenCost' = seenCost,
    valkeyUp' = valkeyUp, observed' = observed, uncounted' = uncounted,
    keepSpend,
  }
```

### 5.2 A call completes

Both a completion and a cancellation emit one event, its ledger row, at
the call's gateway. The site keeps no counter of it: known spend has it
once a snapshot does.

```quint exposure.qnt +=
  action complete(request: str, cost: int): bool = all {
    phase.get(request) == "inflight",
    phase' = phase.put(request, "done"),
    billed' = billed.put(request, cost),
    seenCost' = seenCost.put(request, cost),
    observed' = observed.union(Set((request, cost))),
    slots' = slots.exclude(Set(request)),
    gatewayOf' = gatewayOf, valkeyUp' = valkeyUp, keepSpend,
    uncounted' = uncounted, waived' = waived, lag' = lag, overAdmit' = overAdmit,
  }
```

### 5.3 The client cancels, the provider carries on

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
    observed' = observed.union(Set((request, seen))),
    slots' = if (HOLD_ON_CANCEL) slots else slots.exclude(Set(request)),
    gatewayOf' = gatewayOf, billed' = billed, valkeyUp' = valkeyUp, keepSpend,
    uncounted' = uncounted, waived' = waived, lag' = lag, overAdmit' = overAdmit,
  }

  action providerEnds(request: str, cost: int): bool = all {
    phase.get(request) == "cancelled",
    cost >= seenCost.get(request),
    phase' = phase.put(request, "done"),
    billed' = billed.put(request, cost),
    uncounted' = uncounted + (cost - seenCost.get(request)),
    slots' = slots.exclude(Set(request)),
    gatewayOf' = gatewayOf, seenCost' = seenCost, valkeyUp' = valkeyUp,
    keepSpend, observed' = observed, waived' = waived, lag' = lag, overAdmit' = overAdmit,
  }
```

### 5.4 The control plane publishes, gateways apply

A snapshot's known spend is some of the events emitted so far: those the
ledger has ingested, before golem-control's boundary. Ingestion is late
and in any order, so it may be any subset. Publication and application
are separate steps, and each gateway applies on its own.

```quint exposure.qnt +=
  action publishSnapshot(ingested: Set[Event]): bool = all {
    ingested.subseteq(observed),
    latest' = sum(ingested),
    snaps' = snaps, gwDown' = gwDown, grace' = grace,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp, keepGhosts,
  }

  action applySnapshot(gateway: str): bool = all {
    snaps.get(gateway) != latest,
    snaps' = snaps.put(gateway, latest),
    latest' = latest, gwDown' = gwDown, grace' = grace,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp, keepGhosts,
  }
```

### 5.5 Valkey

**Unreachable.** Gateways cannot renew slots, so every slot's TTL lapses
(the model takes the outage as longer than the 90-second TTL: the worst
case), and every gateway notices (its renewal fails within 30 seconds).

**Back.** Valkey answers, and each gateway, on its own, recovers: with
`REACQUIRE` it re-registers a slot for each of its calls still running,
cancelled ones included, and uses the store again, in a grace.

```quint exposure.qnt +=
  action valkeyDown: bool = all {
    valkeyUp,
    valkeyUp' = false,
    gwDown' = GATEWAYS,
    slots' = Set(),
    latest' = latest, snaps' = snaps, grace' = grace,
    keepCalls, keepGhosts,
  }

  action valkeyBack: bool = all {
    not(valkeyUp),
    valkeyUp' = true,
    slots' = slots, keepCalls, keepSpend, keepGhosts,
  }

  action recover(gateway: str): bool = all {
    valkeyUp,
    gwDown.contains(gateway),
    gwDown' = gwDown.exclude(Set(gateway)),
    grace' = grace.union(Set(gateway)),
    slots' = if (REACQUIRE) slots.union(running.filter(r => gatewayOf.get(r) == gateway)) else slots,
    latest' = latest, snaps' = snaps,
    keepCalls, valkeyUp' = valkeyUp, keepGhosts,
  }

  // A gateway's grace ends, which the model allows only once every
  // gateway has recovered (each within one renewal interval: §3.2).
  action graceEnds(gateway: str): bool = all {
    grace.contains(gateway),
    gwDown == Set(),
    grace' = grace.exclude(Set(gateway)),
    latest' = latest, snaps' = snaps, gwDown' = gwDown,
    keepCalls, keepGhosts, slots' = slots, valkeyUp' = valkeyUp,
  }
```

### 5.6 The step

```quint exposure.qnt +=
  action step = {
    nondet request = REQUESTS.oneOf()
    nondet gateway = GATEWAYS.oneOf()
    nondet cost = COSTS.oneOf()
    nondet ingested = observed.powerset().oneOf()
    any {
      admit(request, gateway),
      complete(request, cost),
      cancel(request, cost),
      providerEnds(request, cost),
      publishSnapshot(ingested),
      applySnapshot(gateway),
      valkeyDown,
      valkeyBack,
      recover(gateway),
      graceEnds(gateway),
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
Valkey reachable, outside a grace, saw known spend below the budget, with
fewer than `SCOPE_LIMIT` calls running, each billing at most `WORST`. Its
known spend lacked at most `lag` of what golem had observed. To that add
what golem never heard of and what was admitted under waiver.

```quint exposure.qnt +=
  val withinBound =
    totalBilled <= BUDGET + lag + SCOPE_LIMIT * WORST + uncounted + waived.size() * WORST

  val safe = limitsHonoured and withinBound
```

So the number ADR 0031 can advertise for a scope is `SCOPE_LIMIT × WORST`
over the budget, not a principal's `LIMIT × WORST`, plus terms that are
reported, not bounded in advance by the model: spend golem never heard of
(until reconciliation books it), waived admissions, and the **lag**: the
spend that ended within one snapshot's lag (ingestion, the allowance,
publication about every minute, and application) and so was not yet in the
known spend admission read. The in-flight limits do not bound the lag on
their own, since short calls finish many to a slot in a minute; the
scope's rate limit does: at most the scope's requests a minute over the
lag's minutes, each at most `WORST`. A scope without a rate limit has
that term bounded only by how fast its in-flight calls finish.

### 6.3 Possibility

The design must be able to: go over the budget (the accepted risk); admit
under waiver past a limit, and still hold the bound; bill a cancelled call
more than the gateway saw; and admit a call while spend is outside the
snapshot.

```quint exposure.qnt +=
  val overshoots = totalBilled > BUDGET
  val waiverExceedsLimit = REQUESTS.exists(r => ownedBy(running, OWNER.get(r)).size() > LIMIT)
  val cancelledBilledMore = REQUESTS.exists(r => billed.get(r) > seenCost.get(r))
  val snapshotLags = lag > 0
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

**Spend within a snapshot's lag** (the design). Four calls one after
another each cost 3, and no snapshot has any of them: each is admitted
against known spend 0. The scope has spent 12, past the budget and its
in-flight term (4 + 2 × 3); the bound holds by its lag term alone, the 9
the last admission did not see.

```quint exposure.qnt +=
  run lagTest =
    init
      .then(admit("a1", "g1")).then(complete("a1", 3))
      .then(admit("b1", "g2")).then(complete("b1", 3))
      .then(admit("c1", "g1")).then(complete("c1", 3))
      .then(admit("a2", "g2")).then(complete("a2", 3))
      .expect(lag == 9 and totalBilled > BUDGET + SCOPE_LIMIT * WORST and withinBound and limitsHonoured)
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

- **Several sites.** Each site has its own Valkey and slots. Scope limits
  are per site, so the stated bound becomes, per scope, the sum over its
  active sites of `SCOPE_LIMIT × WORST`; the lag term already holds every
  site's spend outside the snapshot. With the secondary site a warm
  standby, the extra sites' terms are zero in normal operation. Not
  modelled.
- **The time a snapshot takes**: the model lets a snapshot lack any of
  the events emitted, and `lag` measures what it lacked; how long that is
  (ADR 0048's allowance, the publication interval) is the freshness and
  publication models' and golem-control's.
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
| `releaseOnCancel` | `HOLD_ON_CANCEL` | `limitsHonoured` |
| `noReacquire` | `REACQUIRE` | `limitsHonoured` |

Each story passes in the design it names and fails in `exposureDesign`;
`waiverTest` and `lagTest` pass in `exposureDesign`.

```quint exposure.qnt +=

module exposureDesign {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2,
    SCOPE_LIMIT_ON = true, HOLD_ON_CANCEL = true, REACQUIRE = true).*
}

module noScopeLimit {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2,
    SCOPE_LIMIT_ON = false, HOLD_ON_CANCEL = true, REACQUIRE = true).*
}

module releaseOnCancel {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2,
    SCOPE_LIMIT_ON = true, HOLD_ON_CANCEL = false, REACQUIRE = true).*
}

module noReacquire {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2,
    SCOPE_LIMIT_ON = true, HOLD_ON_CANCEL = true, REACQUIRE = false).*
}
```
