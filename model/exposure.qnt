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
  type Row = (str, int, int)
  type Snap = { w: int, spend: int, whole: int, mark: int }

  var counter: List[Event]
  var trimmed: int
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
    counter' = [], trimmed' = 0, pending' = GATEWAYS.mapBy(g => Set()), gwDown' = Set(), grace' = Set(),
    buffer' = Set(), ledger' = Set(),
    latest' = blankSnap, snaps' = GATEWAYS.mapBy(g => blankSnap),
    slots' = Set(), valkeyUp' = true, views' = GATEWAYS.mapBy(g => 0),
    observed' = Set(), uncounted' = 0, waived' = Set(), heldEver' = 0, overAdmit' = false,
  }

  val blankSnap: Snap = { w: 0, spend: 0, whole: 0, mark: 0 }
  def sum(events: Set[Event]): int = events.fold(0, (acc, e) => acc + e._2)
  def rowsSum(rows: Set[Row]): int = rows.fold(0, (acc, r) => acc + r._2)
  def running: Set[str] = REQUESTS.filter(r => phase.get(r) == "inflight" or phase.get(r) == "cancelled")
  def ownedBy(rs: Set[str], p: str): Set[str] = rs.filter(r => OWNER.get(r) == p)
  def totalBilled: int = REQUESTS.fold(0, (acc, r) => acc + billed.get(r))
  def counted: int = sum(observed)
  def held: int = GATEWAYS.fold(0, (acc, g) => acc + sum(pending.get(g)))
  // A gateway uses the store if Valkey answers and it has recovered.
  def usesStore(g: str): bool = valkeyUp and not(gwDown.contains(g))
  def counterTotal(from: int): int =
    counter.indices().filter(i => i >= from and i >= trimmed).fold(0, (acc, i) => acc + counter[i]._2)
  def known(g: str): int =
    val sn = snaps.get(g)
    if (HANDOFF == "reset") sn.whole + counterTotal(sn.mark)
    else if (HANDOFF == "sum") sn.whole + counterTotal(0)
    else sn.spend + counterTotal(sn.w)
  action keepCalls = all { phase' = phase, gatewayOf' = gatewayOf, billed' = billed, seenCost' = seenCost }
  action keepSpend = all {
    counter' = counter, trimmed' = trimmed, pending' = pending, gwDown' = gwDown, grace' = grace, buffer' = buffer, ledger' = ledger,
    latest' = latest, snaps' = snaps, views' = views,
  }
  action keepGhosts = all { observed' = observed, uncounted' = uncounted, waived' = waived, heldEver' = heldEver, overAdmit' = overAdmit }
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
    counter' = counter, trimmed' = trimmed, pending' = pending, gwDown' = gwDown, grace' = grace, buffer' = buffer, ledger' = ledger,
    latest' = latest, snaps' = snaps,
  }
  def holds(c: List[Event], e: Event): bool = c.foldl(false, (b, x) => b or x._1 == e._1)

  action emit(e: Event, g: str): bool = all {
    observed' = observed.union(Set(e)),
    val up = usesStore(g) and not(holds(counter, e))
    all {
      counter' = if (up) counter.append(e) else counter,
      buffer' = if (up) buffer.union(Set((e._1, e._2, counter.length() + 1))) else buffer,
    },
    pending' = if (usesStore(g)) pending else pending.put(g, pending.get(g).union(Set(e))),
    heldEver' = if (usesStore(g)) heldEver else heldEver + e._2,
    gwDown' = gwDown, grace' = grace, trimmed' = trimmed, ledger' = ledger, latest' = latest, snaps' = snaps, views' = views,
  }
  action complete(request: str, cost: int): bool = all {
    phase.get(request) == "inflight",
    phase' = phase.put(request, "done"),
    billed' = billed.put(request, cost),
    seenCost' = seenCost.put(request, cost),
    emit((request, cost), gatewayOf.get(request)),
    slots' = slots.exclude(Set(request)),
    gatewayOf' = gatewayOf, valkeyUp' = valkeyUp,
    uncounted' = uncounted, waived' = waived, overAdmit' = overAdmit,
  }
  action cancel(request: str, seen: int): bool = all {
    phase.get(request) == "inflight",
    phase' = phase.put(request, "cancelled"),
    seenCost' = seenCost.put(request, seen),
    emit((request, seen), gatewayOf.get(request)),
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
  action deliver(r: Row): bool = all {
    buffer.contains(r),
    buffer' = buffer.exclude(Set(r)),
    ledger' = ledger.union(Set(r)),
    counter' = counter, trimmed' = trimmed, pending' = pending, gwDown' = gwDown, grace' = grace, latest' = latest, snaps' = snaps, views' = views,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp, keepGhosts,
  }

  def ingested: Set[int] = ledger.map(r => r._3)
  def contiguous: int =
    0.to(ingested.fold(0, (m, n) => if (n > m) n else m))
     .filter(w => 1.to(w).forall(n => ingested.contains(n)))
     .fold(0, (m, n) => if (n > m) n else m)
  def watermark: int =
    if (HANDOFF == "maxIngested") ingested.fold(0, (m, n) => if (n > m) n else m) else contiguous

  action publishSnapshot: bool = all {
    val w = watermark
    latest' = { w: w, spend: rowsSum(ledger.filter(r => r._3 <= w)), whole: rowsSum(ledger), mark: counter.length() },
    counter' = counter, trimmed' = trimmed, pending' = pending, gwDown' = gwDown, grace' = grace, buffer' = buffer, ledger' = ledger,
    snaps' = snaps, views' = views,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp, keepGhosts,
  }

  action applySnapshot(gateway: str): bool = all {
    snaps.get(gateway) != latest,
    snaps' = snaps.put(gateway, latest),
    // The bug: trim the history the new snapshot no longer needs, which
    // a gateway still on an older one does.
    trimmed' = if (HANDOFF == "trimAtApply" and valkeyUp and latest.w > trimmed) latest.w else trimmed,
    counter' = counter, pending' = pending, gwDown' = gwDown, grace' = grace, buffer' = buffer, ledger' = ledger, latest' = latest, views' = views,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp, keepGhosts,
  }
  action valkeyDown: bool = all {
    valkeyUp,
    valkeyUp' = false,
    gwDown' = GATEWAYS,
    grace' = grace,
    slots' = Set(),
    counter' = counter, trimmed' = trimmed, pending' = pending, buffer' = buffer, ledger' = ledger,
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
    val fresh = not(holds(counter, e))
    all {
      counter' = if (fresh) counter.append(e) else counter,
      buffer' = if (fresh) buffer.union(Set((e._1, e._2, counter.length() + 1))) else buffer,
    },
    pending' = pending.put(gateway, pending.get(gateway).exclude(Set(e))),
    gwDown' = gwDown, grace' = grace, trimmed' = trimmed, ledger' = ledger, latest' = latest, snaps' = snaps, views' = views,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp, keepGhosts,
  }

  action recover(gateway: str): bool = all {
    valkeyUp,
    gwDown.contains(gateway),
    pending.get(gateway) == Set(),
    gwDown' = gwDown.exclude(Set(gateway)),
    grace' = grace.union(Set(gateway)),
    slots' = if (REACQUIRE) slots.union(running.filter(r => gatewayOf.get(r) == gateway)) else slots,
    counter' = counter, trimmed' = trimmed, pending' = pending, buffer' = buffer, ledger' = ledger,
    latest' = latest, snaps' = snaps, views' = views,
    keepCalls, valkeyUp' = valkeyUp, keepGhosts,
  }

  // A gateway's grace ends, which the model allows only once every
  // gateway has recovered (each within one renewal interval: §3.2).
  action graceEnds(gateway: str): bool = all {
    grace.contains(gateway),
    gwDown == Set(),
    grace' = grace.exclude(Set(gateway)),
    counter' = counter, trimmed' = trimmed, pending' = pending, gwDown' = gwDown, buffer' = buffer, ledger' = ledger,
    latest' = latest, snaps' = snaps, views' = views,
    keepCalls, keepGhosts, slots' = slots, valkeyUp' = valkeyUp,
  }

  action failover(keep: int): bool = all {
    valkeyUp,
    keep >= 0,
    keep < counter.length(),
    val lost = counter.indices().filter(i => i >= keep).fold(0, (acc, i) => acc + counter[i]._2)
    all {
      counter' = counter.slice(0, keep),
      trimmed' = if (trimmed > keep) keep else trimmed,
      uncounted' = uncounted + lost + (counter.length() - keep) * WORST,
    },
    pending' = pending, gwDown' = gwDown, grace' = grace, buffer' = buffer, ledger' = ledger, latest' = latest, snaps' = snaps, views' = views,
    keepCalls, slots' = slots, valkeyUp' = valkeyUp,
    observed' = observed, waived' = waived, heldEver' = heldEver, overAdmit' = overAdmit,
  }
  action step = {
    nondet request = REQUESTS.oneOf()
    nondet gateway = GATEWAYS.oneOf()
    nondet cost = COSTS.oneOf()
    nondet r = buffer.union(Set(("none", 0, 0))).oneOf()
    nondet e = pending.get(gateway).union(Set(("none", 0))).oneOf()
    nondet keep = 0.to(counter.length()).oneOf()
    any {
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
      failover(keep),
    }
  }
  val limitsHonoured = not(overAdmit)
  val withinBound =
    totalBilled <= BUDGET + SCOPE_LIMIT * WORST + uncounted + waived.size() * WORST + heldEver
  val knownNeverOver = GATEWAYS.forall(g => known(g) <= counted)
  val noLostIncrements = GATEWAYS.forall(g => usesStore(g) implies known(g) + uncounted + held >= counted)

  val safe = limitsHonoured and withinBound and knownNeverOver and noLostIncrements
  val overshoots = totalBilled > BUDGET
  val waiverExceedsLimit = REQUESTS.exists(r => ownedBy(running, OWNER.get(r)).size() > LIMIT)
  val cancelledBilledMore = REQUESTS.exists(r => billed.get(r) > seenCost.get(r))
  // A gateway's snapshot includes some events and the counter holds later ones.
  val handoffInFlight = GATEWAYS.exists(g => snaps.get(g).w > 0 and counter.length() > snaps.get(g).w)
  // Two gateways on different snapshots, both exact.
  val snapshotSkew = GATEWAYS.exists(g => GATEWAYS.exists(h => snaps.get(g).w != snaps.get(h).w))
  run scopeExposureTest =
    init
      .then(admit("a1", "g1"))
      .then(admit("b1", "g1"))
      .then(admit("c1", "g2"))
      .expect(not(limitsHonoured))
  run resetTest =
    init
      .then(admit("a1", "g1")).then(complete("a1", 3)).then(publishSnapshot).then(applySnapshot("g1"))
      .then(admit("a2", "g1")).then(complete("a2", 3)).then(publishSnapshot).then(applySnapshot("g1"))
      .then(admit("b1", "g1")).then(complete("b1", 3)).then(publishSnapshot).then(applySnapshot("g1"))
      .then(admit("b2", "g1")).then(complete("b2", 3))
      .expect(not(withinBound))
  run sumTest =
    init
      .then(admit("a1", "g1"))
      .then(complete("a1", 2))
      .then(deliver(("a1", 2, 1)))
      .then(publishSnapshot)
      .then(applySnapshot("g1"))
      .expect(not(knownNeverOver))
  run maxIngestedTest =
    init
      .then(admit("a1", "g1")).then(complete("a1", 3))
      .then(admit("b1", "g1")).then(complete("b1", 1))
      .then(deliver(("b1", 1, 2)))
      .then(publishSnapshot)
      .then(applySnapshot("g1"))
      .expect(not(noLostIncrements))
  run trimTest =
    init
      .then(admit("a1", "g1")).then(complete("a1", 3))
      .then(deliver(("a1", 3, 1)))
      .then(publishSnapshot)
      .then(applySnapshot("g1"))
      .expect(not(noLostIncrements))
  run skewTest =
    init
      .then(admit("a1", "g1")).then(complete("a1", 3))
      .then(deliver(("a1", 3, 1)))
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
}

module exposureDesign {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2,
    SCOPE_LIMIT_ON = true, HANDOFF = "watermark", HOLD_ON_CANCEL = true, REACQUIRE = true).*
}

module noScopeLimit {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2,
    SCOPE_LIMIT_ON = false, HANDOFF = "watermark", HOLD_ON_CANCEL = true, REACQUIRE = true).*
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
    SCOPE_LIMIT_ON = true, HANDOFF = "watermark", HOLD_ON_CANCEL = false, REACQUIRE = true).*
}

module maxIngested {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2,
    SCOPE_LIMIT_ON = true, HANDOFF = "maxIngested", HOLD_ON_CANCEL = true, REACQUIRE = true).*
}

module trimAtApply {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2,
    SCOPE_LIMIT_ON = true, HANDOFF = "trimAtApply", HOLD_ON_CANCEL = true, REACQUIRE = true).*
}

module noReacquire {
  import exposure(REQUESTS = Set("a1", "a2", "b1", "b2", "c1"),
    OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2", "b2" -> "p2", "c1" -> "p3"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 2, 3), WORST = 3, BUDGET = 4, LIMIT = 1, SCOPE_LIMIT = 2,
    SCOPE_LIMIT_ON = true, HANDOFF = "watermark", HOLD_ON_CANCEL = true, REACQUIRE = false).*
}

module small {
  import exposure(REQUESTS = Set("a1", "a2", "b1"), OWNER = Map("a1" -> "p1", "a2" -> "p1", "b1" -> "p2"),
    GATEWAYS = Set("g1", "g2"), COSTS = Set(1, 3), WORST = 3, BUDGET = 2, LIMIT = 1, SCOPE_LIMIT = 1,
    SCOPE_LIMIT_ON = true, HANDOFF = "watermark", HOLD_ON_CANCEL = true, REACQUIRE = true).*
}
