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
  const ALLOWANCE: int

  const SCOPE_LIMIT_ON: bool
  const HANDOFF: str
  const HOLD_ON_CANCEL: bool
  const REACQUIRE: bool
  const MEMBER_CAP: int
  const MEMBER_KNOWN: str
  type Event = (str, int, int)

  var phase: str -> str
  var gatewayOf: str -> str
  var billed: str -> int
  var seenCost: str -> int
  type Row = (str, int, int)
  type Snap = { b: int, spend: int, whole: int, mspend: str -> int, mwhole: str -> int }

  var now: int
  var counter: int -> int
  var mcounter: str -> (int -> int)
  var pending: str -> Set[Event]
  var gwDown: Set[str]
  var grace: Set[str]
  var buffer: Set[Row]
  var ledger: Set[Row]
  var latest: Snap
  var snaps: str -> Snap
  var slots: Set[str]
  var valkeyUp: bool
  var views: str -> int
  var mviews: str -> (str -> int)
  var observed: Set[Event]
  var uncounted: int
  var waived: Set[str]
  var heldEver: int
  var overAdmit: bool
  action init = all {
    phase' = REQUESTS.mapBy(r => "idle"),
    gatewayOf' = REQUESTS.mapBy(r => ""),
    billed' = REQUESTS.mapBy(r => 0),
    seenCost' = REQUESTS.mapBy(r => 0),
    now' = 0, counter' = Map(), mcounter' = MEMBERS.mapBy(p => Map()), pending' = GATEWAYS.mapBy(g => Set()), gwDown' = Set(), grace' = Set(),
    buffer' = Set(), ledger' = Set(),
    latest' = blankSnap, snaps' = GATEWAYS.mapBy(g => blankSnap),
    slots' = Set(), valkeyUp' = true, views' = GATEWAYS.mapBy(g => 0), mviews' = GATEWAYS.mapBy(g => MEMBERS.mapBy(p => 0)),
    observed' = Set(), uncounted' = 0, waived' = Set(), heldEver' = 0, overAdmit' = false,
  }

  // The first snapshot: nothing from the ledger, every minute counted.
  val MEMBERS: Set[str] = REQUESTS.map(r => OWNER.get(r))
  val blankSnap: Snap = { b: 0, spend: 0, whole: 0, mspend: MEMBERS.mapBy(p => 0), mwhole: MEMBERS.mapBy(p => 0) }
  def sum(events: Set[Event]): int = events.fold(0, (acc, e) => acc + e._2)
  def rowsSum(rows: Set[Row]): int = rows.fold(0, (acc, r) => acc + r._2)
  def running: Set[str] = REQUESTS.filter(r => phase.get(r) == "inflight" or phase.get(r) == "cancelled")
  def ownedBy(rs: Set[str], p: str): Set[str] = rs.filter(r => OWNER.get(r) == p)
  def totalBilled: int = REQUESTS.fold(0, (acc, r) => acc + billed.get(r))
  def billedOf(p: str): int = ownedBy(REQUESTS, p).fold(0, (acc, r) => acc + billed.get(r))
  def countedOf(p: str): int = sum(observed.filter(e => OWNER.get(e._1) == p))
  def counted: int = sum(observed)
  def held: int = GATEWAYS.fold(0, (acc, g) => acc + sum(pending.get(g)))
  // A gateway uses the store if Valkey answers and it has recovered.
  def usesStore(g: str): bool = valkeyUp and not(gwDown.contains(g))
  def counterFrom(b: int): int = counter.keys().filter(t => t >= b).fold(0, (acc, t) => acc + counter.get(t))
  def counterAll: int = counter.keys().fold(0, (acc, t) => acc + counter.get(t))
  def addAt(c: int -> int, t: int, cost: int): int -> int =
    if (c.keys().contains(t)) c.set(t, c.get(t) + cost) else c.put(t, cost)
  def known(g: str): int =
    val sn = snaps.get(g)
    if (HANDOFF == "sum") sn.whole + counterAll
    else sn.spend + counterFrom(sn.b)
  def minutesFrom(c: int -> int, b: int): int = c.keys().filter(t => t >= b).fold(0, (acc, t) => acc + c.get(t))
  def mknown(g: str, p: str): int =
    val sn = snaps.get(g)
    if (MEMBER_KNOWN == "snapshotOnly") sn.mspend.get(p)
    else if (HANDOFF == "sum") sn.mwhole.get(p) + minutesFrom(mcounter.get(p), -1)
    else sn.mspend.get(p) + minutesFrom(mcounter.get(p), sn.b)
  def underCap(v: int): bool = MEMBER_CAP == 0 or v < MEMBER_CAP
  action keepCalls = all { phase' = phase, gatewayOf' = gatewayOf, billed' = billed, seenCost' = seenCost }
  action keepSpend = all {
    now' = now, counter' = counter, mcounter' = mcounter, pending' = pending, gwDown' = gwDown, grace' = grace, buffer' = buffer, ledger' = ledger,
    latest' = latest, snaps' = snaps, views' = views, mviews' = mviews,
  }
  action keepGhosts = all { observed' = observed, uncounted' = uncounted, waived' = waived, heldEver' = heldEver, overAdmit' = overAdmit }
  action admit(request: str, gateway: str): bool = all {
    phase.get(request) == "idle",
    val p = OWNER.get(request)
    val local = running.filter(r => gatewayOf.get(r) == gateway)
    if (usesStore(gateway)) all {
      known(gateway) < BUDGET,
      underCap(mknown(gateway, p)), // the member's account, in the same script
      views' = views.put(gateway, known(gateway)), // the check reads the counter
      mviews' = mviews.put(gateway, mviews.get(gateway).put(p, mknown(gateway, p))),
      ownedBy(slots, p).size() < LIMIT,
      if (SCOPE_LIMIT_ON) slots.size() < SCOPE_LIMIT else true,
      slots' = slots.union(Set(request)),
      // In grace, peers may still hold calls Valkey lacks: a waiver.
      waived' = if (grace.contains(gateway)) waived.union(Set(request)) else waived,
      overAdmit' = (overAdmit or (not(grace.contains(gateway)) and (ownedBy(running, p).size() >= LIMIT or running.size() >= SCOPE_LIMIT))),
    } else all {
      views.get(gateway) < BUDGET,
      underCap(mviews.get(gateway).get(p)),
      views' = views, mviews' = mviews,
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
    now' = now, counter' = counter, mcounter' = mcounter, pending' = pending, gwDown' = gwDown, grace' = grace, buffer' = buffer, ledger' = ledger,
    latest' = latest, snaps' = snaps,
  }
  action emit(id: str, cost: int, g: str): bool = all {
    val e = (id, cost, now)
    all {
      observed' = observed.union(Set(e)),
      counter' = if (usesStore(g)) addAt(counter, now, cost) else counter,
      mcounter' = if (usesStore(g)) mcounter.set(OWNER.get(id), addAt(mcounter.get(OWNER.get(id)), now, cost)) else mcounter,
      buffer' = buffer.union(Set(e)),
      pending' = if (usesStore(g)) pending else pending.put(g, pending.get(g).union(Set(e))),
      heldEver' = if (usesStore(g)) heldEver else heldEver + cost,
    },
    now' = now, gwDown' = gwDown, grace' = grace, ledger' = ledger, latest' = latest, snaps' = snaps, views' = views, mviews' = mviews,
  }
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
  action tick: bool = all {
    buffer.forall(r => r._3 >= now + 1 - ALLOWANCE),
    now' = now + 1,
    counter' = counter, mcounter' = mcounter, pending' = pending, gwDown' = gwDown, grace' = grace, buffer' = buffer, ledger' = ledger,
    latest' = latest, snaps' = snaps, views' = views, mviews' = mviews,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp, keepGhosts,
  }

  action deliver(r: Row): bool = all {
    buffer.contains(r),
    buffer' = buffer.exclude(Set(r)),
    ledger' = ledger.union(Set(r)),
    now' = now, counter' = counter, mcounter' = mcounter, pending' = pending, gwDown' = gwDown, grace' = grace, latest' = latest, snaps' = snaps, views' = views, mviews' = mviews,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp, keepGhosts,
  }

  def boundary: int = if (HANDOFF == "noAllowance") now else now - ALLOWANCE

  action publishSnapshot: bool = all {
    val b = boundary
    latest' = { b: b, spend: rowsSum(ledger.filter(r => r._3 < b)), whole: rowsSum(ledger),
      mspend: MEMBERS.mapBy(p => rowsSum(ledger.filter(r => r._3 < b and OWNER.get(r._1) == p))),
      mwhole: MEMBERS.mapBy(p => rowsSum(ledger.filter(r => OWNER.get(r._1) == p))) },
    now' = now, counter' = counter, mcounter' = mcounter, pending' = pending, gwDown' = gwDown, grace' = grace, buffer' = buffer, ledger' = ledger,
    snaps' = snaps, views' = views, mviews' = mviews,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp, keepGhosts,
  }

  action applySnapshot(gateway: str): bool = all {
    snaps.get(gateway) != latest,
    snaps' = snaps.put(gateway, latest),
    // The bug: start the counter again at each snapshot, as if the
    // snapshot held everything before it.
    counter' = if (HANDOFF == "resetAtApply" and valkeyUp) Map() else counter,
    mcounter' = if (HANDOFF == "resetAtApply" and valkeyUp) MEMBERS.mapBy(p => Map()) else mcounter,
    now' = now, pending' = pending, gwDown' = gwDown, grace' = grace, buffer' = buffer, ledger' = ledger, latest' = latest, views' = views, mviews' = mviews,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp, keepGhosts,
  }
  action valkeyDown: bool = all {
    valkeyUp,
    valkeyUp' = false,
    gwDown' = GATEWAYS,
    grace' = grace,
    slots' = Set(),
    now' = now, counter' = counter, mcounter' = mcounter, pending' = pending, buffer' = buffer, ledger' = ledger,
    latest' = latest, snaps' = snaps, views' = views, mviews' = mviews,
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
    mcounter' = mcounter.set(OWNER.get(e._1), addAt(mcounter.get(OWNER.get(e._1)), e._3, e._2)),
    pending' = pending.put(gateway, pending.get(gateway).exclude(Set(e))),
    now' = now, buffer' = buffer, gwDown' = gwDown, grace' = grace, ledger' = ledger, latest' = latest, snaps' = snaps, views' = views, mviews' = mviews,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp, keepGhosts,
  }

  action recover(gateway: str): bool = all {
    valkeyUp,
    gwDown.contains(gateway),
    pending.get(gateway) == Set(),
    gwDown' = gwDown.exclude(Set(gateway)),
    grace' = grace.union(Set(gateway)),
    slots' = if (REACQUIRE) slots.union(running.filter(r => gatewayOf.get(r) == gateway)) else slots,
    now' = now, counter' = counter, mcounter' = mcounter, pending' = pending, buffer' = buffer, ledger' = ledger,
    latest' = latest, snaps' = snaps, views' = views, mviews' = mviews,
    keepCalls, valkeyUp' = valkeyUp, keepGhosts,
  }

  // A gateway's grace ends, which the model allows only once every
  // gateway has recovered (each within one renewal interval: §3.2).
  action graceEnds(gateway: str): bool = all {
    grace.contains(gateway),
    gwDown == Set(),
    grace' = grace.exclude(Set(gateway)),
    now' = now, counter' = counter, mcounter' = mcounter, pending' = pending, gwDown' = gwDown, buffer' = buffer, ledger' = ledger,
    latest' = latest, snaps' = snaps, views' = views, mviews' = mviews,
    keepCalls, keepGhosts, slots' = slots, valkeyUp' = valkeyUp,
  }

  action failover(t: int): bool = all {
    valkeyUp,
    counter.keys().contains(t),
    counter' = counter.keys().exclude(Set(t)).mapBy(m => counter.get(m)),
    mcounter' = mcounter.keys().mapBy(p => mcounter.get(p).keys().exclude(Set(t)).mapBy(m => mcounter.get(p).get(m))),
    uncounted' = uncounted + counter.get(t),
    now' = now, pending' = pending, gwDown' = gwDown, grace' = grace, buffer' = buffer, ledger' = ledger, latest' = latest, snaps' = snaps, views' = views, mviews' = mviews,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp,
    observed' = observed, waived' = waived, heldEver' = heldEver, overAdmit' = overAdmit,
  }
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
  val limitsHonoured = not(overAdmit)
  val withinBound =
    totalBilled <= BUDGET + SCOPE_LIMIT * WORST + uncounted + waived.size() * WORST + heldEver
  val knownNeverOver = GATEWAYS.forall(g => known(g) <= counted)
  val noLostIncrements = GATEWAYS.forall(g => usesStore(g) implies known(g) + uncounted + held >= counted)

  val safe = limitsHonoured and withinBound and knownNeverOver and noLostIncrements and memberWithinBound and memberKnownNeverOver
  val memberWithinBound = MEMBER_CAP == 0 or MEMBERS.forall(p =>
    billedOf(p) <= MEMBER_CAP + LIMIT * WORST + uncounted + waived.size() * WORST + heldEver)
  val memberKnownNeverOver = GATEWAYS.forall(g => MEMBERS.forall(p => mknown(g, p) <= countedOf(p)))
  val overshoots = totalBilled > BUDGET
  val waiverExceedsLimit = REQUESTS.exists(r => ownedBy(running, OWNER.get(r)).size() > LIMIT)
  val cancelledBilledMore = REQUESTS.exists(r => billed.get(r) > seenCost.get(r))
  // A gateway's snapshot holds some spend, and the counter's minutes after
  // its boundary hold more.
  val handoffInFlight = GATEWAYS.exists(g => snaps.get(g).spend > 0 and counterFrom(snaps.get(g).b) > 0)
  // Two gateways on different snapshots, both exact.
  val snapshotSkew = GATEWAYS.exists(g => GATEWAYS.exists(h => snaps.get(g).b != snaps.get(h).b))
  // A member past its cap (by the calls in flight when it reached it).
  val memberOvershoots = MEMBER_CAP > 0 and MEMBERS.exists(p => billedOf(p) > MEMBER_CAP)
  run scopeExposureTest =
    init
      .then(admit("a1", "g1"))
      .then(admit("b1", "g1"))
      .then(admit("c1", "g2"))
      .expect(not(limitsHonoured))
  run resetTest =
    init
      .then(admit("a1", "g1")).then(complete("a1", 3))
      .then(publishSnapshot).then(applySnapshot("g1"))
      .expect(not(noLostIncrements))
  run sumTest =
    init
      .then(admit("a1", "g1"))
      .then(complete("a1", 2))
      .then(deliver(("a1", 2, 0)))
      .then(publishSnapshot)
      .then(applySnapshot("g1"))
      .expect(not(knownNeverOver))
  run noAllowanceTest =
    init
      .then(admit("a1", "g1")).then(complete("a1", 3))
      .then(tick)
      .then(publishSnapshot)
      .then(applySnapshot("g1"))
      .expect(not(noLostIncrements))
  run skewTest =
    init
      .then(admit("a1", "g1")).then(complete("a1", 3))
      .then(deliver(("a1", 3, 0)))
      .then(tick).then(tick)
      .then(publishSnapshot)
      .then(applySnapshot("g1"))
      .expect(snapshotSkew and known("g1") == 3 and known("g2") == 3 and noLostIncrements and knownNeverOver)
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
  run memberCapTest =
    init
      .then(admit("a1", "g1")).then(complete("a1", 3))
      .then(admit("a2", "g1")).then(complete("a2", 3))
      .expect(not(memberWithinBound))
}

module exposureDesign {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2, ALLOWANCE = 1,
    SCOPE_LIMIT_ON = true, HANDOFF = "buckets", HOLD_ON_CANCEL = true, REACQUIRE = true, MEMBER_CAP = 0, MEMBER_KNOWN = "counter").*
}

module noScopeLimit {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2, ALLOWANCE = 1,
    SCOPE_LIMIT_ON = false, HANDOFF = "buckets", HOLD_ON_CANCEL = true, REACQUIRE = true, MEMBER_CAP = 0, MEMBER_KNOWN = "counter").*
}

module resetAtApply {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2, ALLOWANCE = 1,
    SCOPE_LIMIT_ON = true, HANDOFF = "resetAtApply", HOLD_ON_CANCEL = true, REACQUIRE = true, MEMBER_CAP = 0, MEMBER_KNOWN = "counter").*
}

module sumHandoff {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2, ALLOWANCE = 1,
    SCOPE_LIMIT_ON = true, HANDOFF = "sum", HOLD_ON_CANCEL = true, REACQUIRE = true, MEMBER_CAP = 0, MEMBER_KNOWN = "counter").*
}

module releaseOnCancel {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2, ALLOWANCE = 1,
    SCOPE_LIMIT_ON = true, HANDOFF = "buckets", HOLD_ON_CANCEL = false, REACQUIRE = true, MEMBER_CAP = 0, MEMBER_KNOWN = "counter").*
}

module noAllowance {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2, ALLOWANCE = 1,
    SCOPE_LIMIT_ON = true, HANDOFF = "noAllowance", HOLD_ON_CANCEL = true, REACQUIRE = true, MEMBER_CAP = 0, MEMBER_KNOWN = "counter").*
}

module memberCaps {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 100, LIMIT = 1, SCOPE_LIMIT = 2, ALLOWANCE = 1,
    SCOPE_LIMIT_ON = true, HANDOFF = "buckets", HOLD_ON_CANCEL = true, REACQUIRE = true, MEMBER_CAP = 1, MEMBER_KNOWN = "counter").*
}

module memberSnapshotOnly {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 100, LIMIT = 1, SCOPE_LIMIT = 2, ALLOWANCE = 1,
    SCOPE_LIMIT_ON = true, HANDOFF = "buckets", HOLD_ON_CANCEL = true, REACQUIRE = true, MEMBER_CAP = 1, MEMBER_KNOWN = "snapshotOnly").*
}

module noReacquire {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2, ALLOWANCE = 1,
    SCOPE_LIMIT_ON = true, HANDOFF = "buckets", HOLD_ON_CANCEL = true, REACQUIRE = false, MEMBER_CAP = 0, MEMBER_KNOWN = "counter").*
}

module small {
  import exposure(REQUESTS = Set("a1", "a2", "b1"), OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 3), WORST = 3, BUDGET = 2, LIMIT = 1, SCOPE_LIMIT = 1, ALLOWANCE = 1,
    SCOPE_LIMIT_ON = true, HANDOFF = "buckets", HOLD_ON_CANCEL = true, REACQUIRE = true, MEMBER_CAP = 0, MEMBER_KNOWN = "counter").*
}
