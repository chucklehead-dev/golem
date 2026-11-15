// Generated from attempts.md by lmt. Edit the markdown, not this file.
module attempts {
  const CALLS: Set[str]
  const DEPLOYMENTS: Set[str]
  const COSTS: Set[int]
  const WORST: int
  const BUDGET: int
  const LIMIT: int
  const QUOTA: int
  const MAX_ATTEMPTS: int
  const POOL_RETRIES: int
  const EJECT_AFTER: int

  const COMMIT_FINAL: bool
  const UNCERTAIN_FINAL: bool
  const ATTEMPT_EVENTS: bool
  const CHARGE_UNCERTAIN: bool
  const QUOTA_PER_ATTEMPT: bool
  const RETRY_BUDGET: bool
  const POOL_CAP: bool
  const EJECTION: bool

  // ORDERED is the gateway's own choice, for the instance its tests drive
  // (§11): an attempt takes the first deployment of ORDER its health
  // admits, one the call has not tried first, and a call gives up only
  // when no attempt can follow. Unset, any deployment may be tried and a
  // call may give up at any point: the design checked for every choice.
  const ORDERED: bool
  const ORDER: List[str]
  var phase: str -> str
  var attempts: str -> int
  var commits: str -> int
  var on: str -> str
  var tails: Set[str]
  var known: int
  var ledger: (str, int) -> int
  var billed: int
  var retries: int
  var load: str -> int
  var fails: str -> int
  var ejected: str -> bool
  var probes: str -> int
  var probing: Set[str]
  var tried: str -> Set[str]
  action init = all {
    phase' = CALLS.mapBy(c => "idle"),
    attempts' = CALLS.mapBy(c => 0),
    commits' = CALLS.mapBy(c => 0),
    on' = CALLS.mapBy(c => ""),
    tails' = Set(),
    known' = 0,
    ledger' = Map(),
    billed' = 0,
    retries' = 0,
    load' = DEPLOYMENTS.mapBy(d => 0),
    fails' = DEPLOYMENTS.mapBy(d => 0),
    ejected' = DEPLOYMENTS.mapBy(d => false),
    probes' = DEPLOYMENTS.mapBy(d => 0),
    probing' = Set(),
    tried' = CALLS.mapBy(c => Set()),
  }

  // A call holds its slot from its start until it ends, and past its end
  // while its tail runs.
  def holds(c: str): bool =
    Set("waiting", "trying", "streaming").contains(phase.get(c)) or tails.contains(c)
  def slots: int = CALLS.filter(c => holds(c)).size()
  // The work the provider has in hand for the principal: attempts in
  // flight and uncertain attempts' tails.
  def work: int = CALLS.filter(c => Set("trying", "streaming").contains(phase.get(c))).size() + tails.size()
  def ledgerSpend: int = ledger.keys().fold(0, (acc, k) => acc + ledger.get(k))

  // An attempt's charge: added to the counter, and written to the ledger
  // under its event ID unless a row with that ID is there (deduplication).
  def eventOf(c: str): (str, int) = if (ATTEMPT_EVENTS) (c, attempts.get(c)) else (c, 1)
  def charged(c: str, amount: int): (str, int) -> int =
    if (ledger.keys().contains(eventOf(c))) ledger else ledger.put(eventOf(c), amount)

  // Whether call c's next attempt may go to d: below d's quota (each
  // attempt's, or the first's alone), and not ejected unless it is d's one
  // probe.
  def admits(c: str, d: str): bool = and {
    if (attempts.get(c) == 0 or QUOTA_PER_ATTEMPT) load.get(d) < QUOTA else true,
    if (EJECTION and ejected.get(d)) probes.get(d) == 0 else true,
  }
  // The gateway's choice (ORDERED): the first deployment of ORDER that
  // admits c, an untried one first; "" if none does.
  def firstOf(c: str, untried: bool): str =
    ORDER.foldl("", (acc, d) => if (acc == "" and admits(c, d) and (not(untried) or not(tried.get(c).contains(d)))) d else acc)
  def choiceOf(c: str): str = if (firstOf(c, true) != "") firstOf(c, true) else firstOf(c, false)
  // Whether call c may make another attempt: its budget, the pool's, the
  // budget of spend for a first attempt, and a deployment to take it.
  def canAttempt(c: str): bool = and {
    if (RETRY_BUDGET) attempts.get(c) < MAX_ATTEMPTS else true,
    if (POOL_CAP and attempts.get(c) > 0) retries < POOL_RETRIES else true,
    if (attempts.get(c) == 0) known < BUDGET else true,
    DEPLOYMENTS.exists(d => admits(c, d)),
  }
  action start(c: str): bool = all {
    phase.get(c) == "idle",
    slots < LIMIT,
    phase' = phase.put(c, "waiting"),
    attempts' = attempts, commits' = commits, on' = on, tails' = tails, known' = known,
    ledger' = ledger, billed' = billed, retries' = retries, load' = load, fails' = fails,
    ejected' = ejected, probes' = probes, probing' = probing, tried' = tried,
  }
  action attempt(c: str, d: str): bool = all {
    phase.get(c) == "waiting",
    val first = attempts.get(c) == 0
    all {
      if (RETRY_BUDGET) attempts.get(c) < MAX_ATTEMPTS else true,
      if (POOL_CAP and not(first)) retries < POOL_RETRIES else true,
      if (first) known < BUDGET else true,
      admits(c, d),
      if (ORDERED) d == choiceOf(c) else true,
      retries' = if (first) retries else retries + 1,
    },
    phase' = phase.put(c, "trying"),
    attempts' = attempts.setBy(c, n => n + 1),
    on' = on.put(c, d),
    load' = load.setBy(d, n => n + 1),
    probes' = if (ejected.get(d)) probes.setBy(d, n => n + 1) else probes,
    probing' = if (ejected.get(d)) probing.union(Set(c)) else probing,
    tried' = tried.setBy(c, s => s.union(Set(d))),
    commits' = commits, tails' = tails, known' = known, ledger' = ledger, billed' = billed,
    fails' = fails, ejected' = ejected,
  }
  action failed(c: str, outcome: str): bool = all {
    phase.get(c) == "trying",
    Set("unreached", "refused", "uncertain").contains(outcome),
    val d = on.get(c)
    val uncertain = outcome == "uncertain"
    val charge = if (uncertain and CHARGE_UNCERTAIN) WORST else 0
    all {
      phase' = phase.put(c, if (uncertain and UNCERTAIN_FINAL) "failed" else "waiting"),
      tails' = if (uncertain) tails.union(Set(c)) else tails,
      known' = known + charge,
      ledger' = charged(c, charge),
      load' = load.setBy(d, n => n - 1),
      fails' = fails.setBy(d, n => n + 1),
      ejected' = ejected.put(d, ejected.get(d) or fails.get(d) + 1 >= EJECT_AFTER),
      probes' = if (probing.contains(c)) probes.setBy(d, n => n - 1) else probes,
      probing' = probing.exclude(Set(c)),
    },
    attempts' = attempts, commits' = commits, on' = on, billed' = billed, retries' = retries,
    tried' = tried,
  }
  action commit(c: str): bool = all {
    phase.get(c) == "trying",
    val d = on.get(c)
    all {
      fails' = fails.put(d, 0),
      ejected' = ejected.put(d, false),
      probes' = probes.put(d, 0),
      probing' = probing.filter(x => on.get(x) != d),
    },
    phase' = phase.put(c, "streaming"),
    commits' = commits.setBy(c, n => n + 1),
    attempts' = attempts, on' = on, tails' = tails, known' = known, ledger' = ledger,
    billed' = billed, retries' = retries, load' = load, tried' = tried,
  }

  action end(c: str, cost: int, cut: bool): bool = all {
    phase.get(c) == "streaming",
    phase' = phase.put(c, if (cut and not(COMMIT_FINAL)) "waiting" else "done"),
    billed' = billed + cost,
    known' = known + cost,
    ledger' = charged(c, cost),
    load' = load.setBy(on.get(c), n => n - 1),
    attempts' = attempts, commits' = commits, on' = on, tails' = tails, retries' = retries,
    fails' = fails, ejected' = ejected, probes' = probes, probing' = probing, tried' = tried,
  }
  action giveUp(c: str): bool = all {
    phase.get(c) == "waiting",
    if (ORDERED) not(canAttempt(c)) else true,
    phase' = phase.put(c, "failed"),
    attempts' = attempts, commits' = commits, on' = on, tails' = tails, known' = known,
    ledger' = ledger, billed' = billed, retries' = retries, load' = load, fails' = fails,
    ejected' = ejected, probes' = probes, probing' = probing, tried' = tried,
  }

  action tailEnds(c: str, cost: int): bool = all {
    tails.contains(c),
    tails' = tails.exclude(Set(c)),
    billed' = billed + cost,
    phase' = phase, attempts' = attempts, commits' = commits, on' = on, known' = known,
    ledger' = ledger, retries' = retries, load' = load, fails' = fails, ejected' = ejected,
    probes' = probes, probing' = probing, tried' = tried,
  }
  action step = {
    nondet c = CALLS.oneOf()
    nondet d = DEPLOYMENTS.oneOf()
    nondet outcome = Set("unreached", "refused", "uncertain").oneOf()
    nondet cost = COSTS.oneOf()
    nondet cut = Bool.oneOf()
    any {
      start(c),
      attempt(c, d),
      failed(c, outcome),
      commit(c),
      end(c, cost, cut),
      giveUp(c),
      tailEnds(c, cost),
    }
  }
  // A caller gets at most one answer.
  val oneCommit = CALLS.forall(c => commits.get(c) <= 1)
  // Every charge reaches the ledger, once: the ledger's deduplicated sum
  // is the counter's.
  val ledgerMatches = ledgerSpend == known
  // What the provider billed is charged (an uncertain attempt's worst
  // case covers its tail).
  val covered = billed <= known
  // The accounting model's bound, for calls of several attempts. A call is
  // admitted below the budget at its first attempt and bills at most once
  // (refused and unreached attempts are free, an uncertain one ends it),
  // and at most LIMIT admitted calls are unbilled at once, each at most
  // WORST. So a retry need not check the budget again: an earlier version
  // of this model had that check as a switch, and its search found no run
  // the check prevents. A call that bills twice (retried after its commit,
  // or after an uncertain attempt) breaks the bound: §10's table.
  val withinBound = billed <= BUDGET + LIMIT * WORST
  // The provider's work for the principal stays within its slots.
  val workWithinLimit = work <= LIMIT
  val quotaHonoured = DEPLOYMENTS.forall(d => load.get(d) <= QUOTA)
  val attemptsBounded = CALLS.forall(c => attempts.get(c) <= MAX_ATTEMPTS)
  val retriesCapped = retries <= POOL_RETRIES
  // An ejected deployment has at most one probe in flight.
  val probeOnly = DEPLOYMENTS.forall(d => probes.get(d) <= 1)

  val safe = and {
    oneCommit, ledgerMatches, covered, withinBound, workWithinLimit,
    quotaHonoured, attemptsBounded, retriesCapped, probeOnly,
  }
  val failsOver = CALLS.exists(c => phase.get(c) == "done" and attempts.get(c) >= 2)
  val uncertainEndsCall = CALLS.exists(c => phase.get(c) == "failed" and tails.contains(c))
  val ejects = DEPLOYMENTS.exists(d => ejected.get(d))
  val overshoots = billed > BUDGET

  // Where the model means to stop: every call over and no tail running.
  val finished = CALLS.forall(c => Set("done", "failed").contains(phase.get(c))) and tails.size() == 0
  run failoverTest =
    init
      .then(start("c1"))
      .then(attempt("c1", "d1"))
      .then(failed("c1", "refused"))
      .then(attempt("c1", "d2"))
      .then(commit("c1"))
      .then(end("c1", 1, false))
      .expect(failsOver and safe and known == 1)
  run uncertainTest =
    init
      .then(start("c1"))
      .then(attempt("c1", "d1"))
      .then(failed("c1", "uncertain"))
      .expect(uncertainEndsCall and phase.get("c1") == "failed")
      .then(tailEnds("c1", 3))
      .expect(safe and billed == 3 and known == 3)
  run ejectionTest =
    init
      .then(start("c1"))
      .then(start("c2"))
      .then(attempt("c1", "d1"))
      .then(failed("c1", "refused"))
      .then(attempt("c1", "d1"))
      .then(failed("c1", "refused"))
      .expect(ejected.get("d1"))
      .then(attempt("c1", "d1"))
      .expect(probes.get("d1") == 1)
      .then(commit("c1"))
      .expect(not(ejected.get("d1")) and safe)
  run uncertainRetryTest =
    init
      .then(start("c1"))
      .then(start("c2"))
      .then(attempt("c2", "d1"))
      .then(attempt("c1", "d2"))
      .then(failed("c1", "uncertain"))
      .then(attempt("c1", "d2"))
      .expect(not(workWithinLimit))
  run cutRetryTest =
    init
      .then(start("c1"))
      .then(attempt("c1", "d1"))
      .then(commit("c1"))
      .then(end("c1", 1, true))
      .then(attempt("c1", "d2"))
      .then(commit("c1"))
      .expect(not(oneCommit))
  run sharedEventTest =
    init
      .then(start("c1"))
      .then(attempt("c1", "d1"))
      .then(failed("c1", "refused"))
      .then(attempt("c1", "d2"))
      .then(commit("c1"))
      .then(end("c1", 1, false))
      .expect(not(ledgerMatches))
}

module attemptsDesign {
  import attempts(CALLS = Set("c1", "c2", "c3"), DEPLOYMENTS = Set("d1", "d2"), COSTS = Set(1, 2, 3),
    WORST = 3, BUDGET = 6, LIMIT = 2, QUOTA = 2, MAX_ATTEMPTS = 3, POOL_RETRIES = 3, EJECT_AFTER = 2,
    COMMIT_FINAL = true, UNCERTAIN_FINAL = true, ATTEMPT_EVENTS = true, CHARGE_UNCERTAIN = true,
    QUOTA_PER_ATTEMPT = true, RETRY_BUDGET = true, POOL_CAP = true,
    EJECTION = true, ORDERED = false, ORDER = ["d1", "d2"]).*
}

module retryUncertain {
  import attempts(CALLS = Set("c1", "c2", "c3"), DEPLOYMENTS = Set("d1", "d2"), COSTS = Set(1, 2, 3),
    WORST = 3, BUDGET = 6, LIMIT = 2, QUOTA = 2, MAX_ATTEMPTS = 3, POOL_RETRIES = 3, EJECT_AFTER = 2,
    COMMIT_FINAL = true, UNCERTAIN_FINAL = false, ATTEMPT_EVENTS = true, CHARGE_UNCERTAIN = true,
    QUOTA_PER_ATTEMPT = true, RETRY_BUDGET = true, POOL_CAP = true,
    EJECTION = true, ORDERED = false, ORDER = ["d1", "d2"]).*
}

module retryCut {
  import attempts(CALLS = Set("c1", "c2", "c3"), DEPLOYMENTS = Set("d1", "d2"), COSTS = Set(1, 2, 3),
    WORST = 3, BUDGET = 6, LIMIT = 2, QUOTA = 2, MAX_ATTEMPTS = 3, POOL_RETRIES = 3, EJECT_AFTER = 2,
    COMMIT_FINAL = false, UNCERTAIN_FINAL = true, ATTEMPT_EVENTS = true, CHARGE_UNCERTAIN = true,
    QUOTA_PER_ATTEMPT = true, RETRY_BUDGET = true, POOL_CAP = true,
    EJECTION = true, ORDERED = false, ORDER = ["d1", "d2"]).*
}

module sharedEvent {
  import attempts(CALLS = Set("c1", "c2", "c3"), DEPLOYMENTS = Set("d1", "d2"), COSTS = Set(1, 2, 3),
    WORST = 3, BUDGET = 6, LIMIT = 2, QUOTA = 2, MAX_ATTEMPTS = 3, POOL_RETRIES = 3, EJECT_AFTER = 2,
    COMMIT_FINAL = true, UNCERTAIN_FINAL = true, ATTEMPT_EVENTS = false, CHARGE_UNCERTAIN = true,
    QUOTA_PER_ATTEMPT = true, RETRY_BUDGET = true, POOL_CAP = true,
    EJECTION = true, ORDERED = false, ORDER = ["d1", "d2"]).*
}

module small {
  import attempts(CALLS = Set("c1", "c2"), DEPLOYMENTS = Set("d1", "d2"), COSTS = Set(1, 3),
    WORST = 3, BUDGET = 4, LIMIT = 2, QUOTA = 1, MAX_ATTEMPTS = 3, POOL_RETRIES = 3, EJECT_AFTER = 1,
    COMMIT_FINAL = true, UNCERTAIN_FINAL = true, ATTEMPT_EVENTS = true, CHARGE_UNCERTAIN = true,
    QUOTA_PER_ATTEMPT = true, RETRY_BUDGET = true, POOL_CAP = true,
    EJECTION = true, ORDERED = false, ORDER = ["d1", "d2"]).*
}

module gateway {
  import attempts(CALLS = Set("c1", "c2"), DEPLOYMENTS = Set("d1", "d2"), COSTS = Set(1, 3),
    WORST = 3, BUDGET = 1000, LIMIT = 2, QUOTA = 1, MAX_ATTEMPTS = 3, POOL_RETRIES = 3, EJECT_AFTER = 2,
    COMMIT_FINAL = true, UNCERTAIN_FINAL = true, ATTEMPT_EVENTS = true, CHARGE_UNCERTAIN = true,
    QUOTA_PER_ATTEMPT = true, RETRY_BUDGET = true, POOL_CAP = true,
    EJECTION = true, ORDERED = true, ORDER = ["d1", "d2"]).*

  // What the traces must reach: a failover, a call ended by an uncertain
  // attempt, a deployment ejected and then readmitted by its probe, and the
  // route's retries spent.
  val failedOver = failsOver
  val ejectedThenProbed = DEPLOYMENTS.exists(d => ejected.get(d) and probes.get(d) == 1)
  val retriesSpent = retries == POOL_RETRIES
  val attemptsSpent = CALLS.exists(c => attempts.get(c) == MAX_ATTEMPTS and phase.get(c) == "failed")
}

// The same with a ceiling of two attempts a deployment, where ejection's
// one probe is its own guard (with a ceiling of one, the ceiling already
// allows a single probe).
module gatewayWide {
  import attempts(CALLS = Set("c1", "c2"), DEPLOYMENTS = Set("d1", "d2"), COSTS = Set(1, 3),
    WORST = 3, BUDGET = 1000, LIMIT = 2, QUOTA = 2, MAX_ATTEMPTS = 3, POOL_RETRIES = 4, EJECT_AFTER = 2,
    COMMIT_FINAL = true, UNCERTAIN_FINAL = true, ATTEMPT_EVENTS = true, CHARGE_UNCERTAIN = true,
    QUOTA_PER_ATTEMPT = true, RETRY_BUDGET = true, POOL_CAP = true,
    EJECTION = true, ORDERED = true, ORDER = ["d1", "d2"]).*

  // The pool's first deployment ejected, its probe in flight, and another
  // call's last attempt sent past it: the call had tried every deployment,
  // so the gateway's order would have taken it to the first but for the
  // probe (the state is the step after that choice, which a witness run
  // must take).
  val probePassedBy = ejected.get("d1") and probes.get("d1") == 1 and
    CALLS.exists(c => phase.get(c) == "trying" and on.get(c) == "d2" and attempts.get(c) == MAX_ATTEMPTS)
}
