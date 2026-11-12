// Generated from exposure.md by lmt. Edit the markdown, not this file.
module exposure {
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
  type Event = (str, int)

  var phase: str -> str
  var gatewayOf: str -> str
  var billed: str -> int
  var seenCost: str -> int
  var snap: { events: Set[Event], spend: int }
  var counter: Set[Event]
  var pending: Set[Event]
  var buffer: Set[Event]
  var ledger: Set[Event]
  var slots: Set[str]
  var valkeyUp: bool
  var observed: Set[Event]
  var uncounted: int
  var waived: Set[str]
  var overAdmit: bool
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
  def known: int =
    snap.spend + (if (HANDOFF == "sum") sum(counter) else sum(counter.exclude(snap.events)))
  action keepCalls = all { phase' = phase, gatewayOf' = gatewayOf, billed' = billed, seenCost' = seenCost }
  action keepSpend = all { snap' = snap, counter' = counter, pending' = pending, buffer' = buffer, ledger' = ledger }
  action keepGhosts = all { observed' = observed, uncounted' = uncounted, waived' = waived, overAdmit' = overAdmit }
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
  action emit(e: Event): bool = all {
    observed' = observed.union(Set(e)),
    buffer' = buffer.union(Set(e)),
    counter' = if (valkeyUp) counter.union(Set(e)) else counter,
    pending' = if (valkeyUp) pending else pending.union(Set(e)),
    snap' = snap, ledger' = ledger,
  }
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
  val limitsHonoured = not(overAdmit)
  val withinBound =
    totalBilled <= BUDGET + SCOPE_LIMIT * WORST + uncounted + waived.size() * WORST
  val knownNeverOver = known <= counted
  val noLostIncrements = valkeyUp implies known + uncounted >= counted

  val safe = limitsHonoured and withinBound and knownNeverOver and noLostIncrements
  val overshoots = totalBilled > BUDGET
  val waiverExceedsLimit = REQUESTS.exists(r => ownedBy(running, OWNER.get(r)).size() > LIMIT)
  val cancelledBilledMore = REQUESTS.exists(r => billed.get(r) > seenCost.get(r))
  val handoffInFlight = snap.spend > 0 and counter.exclude(snap.events).size() > 0
  run scopeExposureTest =
    init
      .then(admit("a1", "g1"))
      .then(admit("b1", "g1"))
      .then(admit("c1", "g2"))
      .expect(not(limitsHonoured))
  run resetTest =
    init
      .then(admit("a1", "g1")).then(complete("a1", 3)).then(publishSnapshot)
      .then(admit("a2", "g1")).then(complete("a2", 3)).then(publishSnapshot)
      .then(admit("b1", "g1")).then(complete("b1", 3)).then(publishSnapshot)
      .then(admit("b2", "g1")).then(complete("b2", 3)).then(publishSnapshot)
      .expect(not(withinBound))
  run sumTest =
    init
      .then(admit("a1", "g1"))
      .then(complete("a1", 2))
      .then(deliver(("a1", 2)))
      .then(publishSnapshot)
      .expect(not(knownNeverOver))
  run cancelResendTest =
    init
      .then(admit("a1", "g1"))
      .then(cancel("a1", 1))
      .then(admit("a2", "g1"))
      .expect(not(limitsHonoured))
  run forgottenSlotTest =
    init
      .then(admit("a1", "g1"))
      .then(valkeyDown)
      .then(valkeyBack)
      .then(admit("a2", "g1"))
      .expect(not(limitsHonoured))
  run waiverTest =
    init
      .then(valkeyDown)
      .then(admit("a1", "g1"))
      .then(admit("a2", "g2"))
      .expect(waiverExceedsLimit and waived.size() == 2 and withinBound and limitsHonoured)
}

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
