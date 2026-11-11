// Generated from accounting.md by lmt. Edit the markdown, not this file.
module accounting {
  const ATTEMPTS: Set[str]
  const AMOUNTS: Set[int]
  const ESTIMATE: int

  const INTENT_FIRST: bool
  const KEEP_UNCERTAIN: bool
  const TRIM_AFTER_ACK: bool
  const DEDUP: bool
  const DURABLE_MEDIUM: bool
  var billed: str -> int
  var phase: str -> str
  var responseLost: str -> bool
  var reservations: str -> int
  type Event = { id: (str, str), attempt: str, kind: str, amount: int, durability: str }

  var journal: Set[Event]
  var ledger: List[Event]
  var acked: Set[(str, str)]
  var lostLiability: bool
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
  def eventIds(es: Set[Event]): Set[(str, str)] = es.map(e => e.id)
  def ledgerSet: Set[Event] = ledger.foldl(Set(), (s, e) => s.union(Set(e)))
  def recorded(a: str, j: Set[Event]): bool =
    j.exists(e => e.attempt == a) or ledgerSet.exists(e => e.attempt == a)
  def losesLiability(j: Set[Event]): bool =
    ATTEMPTS.exists(a => billed.get(a) > 0 and not(recorded(a, j)))
  action reserve(attempt: str): bool = all {
    phase.get(attempt) == "idle",
    phase' = phase.put(attempt, "reserved"),
    reservations' = reservations.put(attempt, ESTIMATE),
    billed' = billed, responseLost' = responseLost,
    journal' = journal, ledger' = ledger, acked' = acked, lostLiability' = lostLiability,
  }
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
  action loseMedium: bool = all {
    not(DURABLE_MEDIUM),
    journal.size() > 0,
    journal' = Set(),
    lostLiability' = (lostLiability or losesLiability(Set())),
    billed' = billed, phase' = phase, responseLost' = responseLost,
    reservations' = reservations, ledger' = ledger, acked' = acked,
  }
  action loseReservations: bool = all {
    reservations' = ATTEMPTS.mapBy(a => 0),
    billed' = billed, phase' = phase, responseLost' = responseLost,
    journal' = journal, ledger' = ledger, acked' = acked, lostLiability' = lostLiability,
  }
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
  action duplicate(id: (str, str)): bool = all {
    ledgerSet.exists(e => e.id == id),
    val ev = ledgerSet.filter(e => e.id == id).fold({ id: ("", ""), attempt: "", kind: "", amount: 0, durability: "" },
                                                    (acc, e) => e)
    ledger' = ledger.append(ev),
    billed' = billed, phase' = phase, responseLost' = responseLost, reservations' = reservations,
    journal' = journal, acked' = acked, lostLiability' = lostLiability,
  }
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
  def headSpend(a: str): int =
    val es = ledgerSet.filter(e => e.attempt == a and e.kind != "intent")
    es.fold(0, (acc, e) => if (e.amount > acc) e.amount else acc)

  def spentDedup: int = ATTEMPTS.fold(0, (acc, a) => acc + headSpend(a))
  def spentRaw: int = ledger.foldl(0, (acc, e) => if (e.kind != "intent") acc + e.amount else acc)
  def spent: int = if (DEDUP) spentDedup else spentRaw

  def totalBilled: int = ATTEMPTS.fold(0, (acc, a) => acc + billed.get(a))
  val noLostLiability = not(lostLiability)
  val noDoubleCount = spent <= totalBilled
  val quiet = and {
    ATTEMPTS.forall(a => phase.get(a) != "inflight"),
    journal.size() == 0,
    ATTEMPTS.forall(a => billed.get(a) == 0 or ledgerSet.exists(e => e.attempt == a and e.kind != "intent")),
  }

  val exactWhenQuiet = quiet implies spent == totalBilled
  val safe = noLostLiability and noDoubleCount and exactWhenQuiet
  val settledInLedger = ledgerSet.exists(e => e.kind == "settlement")
  val resolvedAfterLostResponse = ledgerSet.exists(e => e.kind == "resolution")
  val uncertainAfterCrash = ATTEMPTS.exists(a =>
    billed.get(a) > 0 and phase.get(a) == "done"
      and chain(a).exists(e => e.kind == "intent") and not(chain(a).exists(e => e.kind != "intent")))
  val duplicateAbsorbed = ledger.length() > ledgerSet.size() and quiet and spent == totalBilled
  run crashAfterDispatchTest =
    init
      .then(reserve("a1"))
      .then(dispatch("a1", 2, false))
      .then(crash)
      .expect(not(noLostLiability))
  run timeoutErasesTest =
    init
      .then(reserve("a1"))
      .then(writeIntent("a1"))
      .then(flush)
      .then(dispatch("a1", 2, true))
      .then(giveUp("a1"))
      .expect(not(noLostLiability))
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
  run lostVolumeTest =
    init
      .then(reserve("a1"))
      .then(writeIntent("a1"))
      .then(flush)
      .then(dispatch("a1", 2, false))
      .then(loseMedium)
      .expect(not(noLostLiability))
}

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
