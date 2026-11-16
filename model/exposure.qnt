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
  const HOLD_ON_CANCEL: bool
  const REACQUIRE: bool
  type Event = (str, int)

  var phase: str -> str
  var gatewayOf: str -> str
  var billed: str -> int
  var seenCost: str -> int
  var latest: int
  var snaps: str -> int
  var gwDown: Set[str]
  var grace: Set[str]
  var slots: Set[str]
  var valkeyUp: bool
  var observed: Set[Event]
  var uncounted: int
  var waived: Set[str]
  var lag: int
  var overAdmit: bool
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
  action keepCalls = all { phase' = phase, gatewayOf' = gatewayOf, billed' = billed, seenCost' = seenCost }
  action keepSpend = all { latest' = latest, snaps' = snaps, gwDown' = gwDown, grace' = grace }
  action keepGhosts = all { observed' = observed, uncounted' = uncounted, waived' = waived, lag' = lag, overAdmit' = overAdmit }
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
  val limitsHonoured = not(overAdmit)
  val withinBound =
    totalBilled <= BUDGET + lag + SCOPE_LIMIT * WORST + uncounted + waived.size() * WORST

  val safe = limitsHonoured and withinBound
  val overshoots = totalBilled > BUDGET
  val waiverExceedsLimit = REQUESTS.exists(r => ownedBy(running, OWNER.get(r)).size() > LIMIT)
  val cancelledBilledMore = REQUESTS.exists(r => billed.get(r) > seenCost.get(r))
  val snapshotLags = lag > 0
  run scopeExposureTest =
    init
      .then(admit("a1", "g1"))
      .then(admit("b1", "g1"))
      .then(admit("c1", "g2"))
      .expect(not(limitsHonoured))
  run lagTest =
    init
      .then(admit("a1", "g1")).then(complete("a1", 3))
      .then(admit("b1", "g2")).then(complete("b1", 3))
      .then(admit("c1", "g1")).then(complete("c1", 3))
      .then(admit("a2", "g2")).then(complete("a2", 3))
      .expect(lag == 9 and totalBilled > BUDGET + SCOPE_LIMIT * WORST and withinBound and limitsHonoured)
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
      .then(recover("g1"))
      .then(recover("g2"))
      .then(graceEnds("g1"))
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
