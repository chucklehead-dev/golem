// Generated from accounting.md by lmt. Edit the markdown, not this file.
module accounting {
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
  var phase: str -> str
  var billed: str -> int
  var known: int
  var buffer: Set[(str, int)]
  var ledger: List[(str, int)]
  var adjustment: int
  var uncounted: int
  var closed: bool
  var slotless: Set[str]
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
  action admit(request: str): bool = all {
    phase.get(request) == "idle",
    if (LIMIT_ON) holdingSlots < LIMIT else true,
    if (WORST_CHECK) known + WORST <= BUDGET else known < BUDGET,
    phase' = phase.put(request, "inflight"),
    billed' = billed, known' = known, buffer' = buffer, ledger' = ledger,
    adjustment' = adjustment, uncounted' = uncounted, closed' = closed,
    slotless' = slotless,
  }
  action complete(request: str, cost: int): bool = all {
    phase.get(request) == "inflight",
    phase' = phase.put(request, "done"),
    billed' = billed.put(request, cost),
    known' = known + cost,
    buffer' = buffer.union(Set((request, cost))),
    ledger' = ledger, adjustment' = adjustment, uncounted' = uncounted,
    closed' = false, slotless' = slotless.exclude(Set(request)),
  }
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
  action expireSlot(request: str): bool = all {
    not(RENEW_SLOTS),
    phase.get(request) == "inflight",
    not(slotless.contains(request)),
    slotless' = slotless.union(Set(request)),
    phase' = phase, billed' = billed, known' = known, buffer' = buffer, ledger' = ledger,
    adjustment' = adjustment, uncounted' = uncounted, closed' = closed,
  }
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
  val slack = if (WORST_CHECK) (LIMIT - 1) * WORST else LIMIT * WORST
  val withinBound = totalBilled <= BUDGET + slack + uncounted
  val noDoubleCount = ledgerSpend <= totalBilled

  val booksMatch = closed implies ledgerSpend + adjustment == totalBilled

  val safe = withinBound and noDoubleCount and booksMatch
  val overshoots = totalBilled > BUDGET
  val refusesWhenSpent = known >= BUDGET and REQUESTS.exists(r => phase.get(r) == "idle")
  val crashThenBalanced = uncounted > 0 and closed and ledgerSpend + adjustment == totalBilled
  val duplicateAbsorbed = ledger.length() > ledgerSet.size() and closed and booksMatch
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
  run crashReconciledTest =
    init
      .then(admit("r1"))
      .then(complete("r1", 2))
      .then(deliver("r1", 2))
      .then(admit("r2"))
      .then(crash(3))
      .then(closeBooks)
      .expect(uncounted == 3 and booksMatch)
  run duplicateCountedTest =
    init
      .then(admit("r1"))
      .then(complete("r1", 2))
      .then(deliver("r1", 2))
      .then(duplicate("r1", 2))
      .expect(not(noDoubleCount))
}

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

module small {
  import accounting(REQUESTS = Set("r1", "r2", "r3"), COSTS = Set(1, 3), WORST = 3, BUDGET = 2, LIMIT = 1,
    LIMIT_ON = true, RENEW_SLOTS = true, WORST_CHECK = false, DEDUP = true, RECONCILE = true).*
}
