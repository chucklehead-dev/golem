# Retries, failover and ejection: the attempt loop model

A literate Quint specification, like [accounting.md](accounting.md): the
prose explains, the `quint` blocks are the model, `make` extracts and
checks it. It models step 2 of [docs/m1-plan.md](../docs/m1-plan.md): one
call becomes a series of attempts, each to a deployment of the route's
pool, retried or failed over before it commits. It refines the accounting
model's single `admit`/`complete` into those attempts, keeping its
arithmetic (known spend against a budget, a declared worst case, a ledger
deduplicated by event ID) and checking what retries could break in it.

## 1. The question

Today a call is admitted once, holds one slot, is sent once, and ends once
(`gateway/proxy`). Retries change all four. The questions:

> Can a call ever stream twice to its caller? Is every attempt charged,
> once, under its own event? Does the stated bound still hold when one
> call makes several attempts? Can retries put more work on providers than
> the principal's slots allow, or more on a deployment than its quota?
> Are retries bounded per call and per pool? Does an ejected deployment
> see only its probe?

Each rule the answers depend on is a switch: on, the design; off, the bug
it prevents.

| Switch | When on (the design) | When off (the bug it prevents) |
|---|---|---|
| `COMMIT_FINAL` | A call commits at its first event (gateway/proxy/commit.go) and is never retried after it | A stream cut mid-way is retried, and the caller gets a second answer after part of the first |
| `UNCERTAIN_FINAL` | An attempt that was sent and got no answer ends the call: the provider may still be working on it (ADR 0029: commitment is not idempotency) | Its retry runs beside the provider's tail under the same slot: twice the work, twice the bill, for one answer |
| `ATTEMPT_EVENTS` | Each attempt's charge is its own ledger event | Attempts share the call's event ID, and deduplication drops all but the first |
| `CHARGE_UNCERTAIN` | An uncertain attempt is charged its declared worst case at once, as today (golem#4) | It is charged nothing, and the provider's bill for its tail is never counted |
| `QUOTA_PER_ATTEMPT` | Every attempt checks its own deployment's quota share | A failover lands on a deployment already at its quota, checked only for the first |
| `RETRY_BUDGET` | A call makes at most `MAX_ATTEMPTS` attempts (ADR 0002: per request) | A failing call retries without end |
| `POOL_CAP` | A pool's retries are capped (`POOL_RETRIES`), so retries cannot storm a provider already failing | Every call's retries add up |
| `EJECTION` | A deployment ejected after `EJECT_AFTER` failures gets one probe until a success readmits it | It goes on getting traffic |

## 2. Reading and running

`make` extracts this file into `attempts.qnt` and runs `check-attempts.sh`:
the design holds in sampled runs of a mid-sized instance, every scenario
is reachable, the stories behave as §8 says, and the small instance's
design matrix (§10, `attempts.small.yaml`) searches every run of each
design to a depth.

```quint attempts.qnt +=
// Generated from attempts.md by lmt. Edit the markdown, not this file.
module attempts {
```

## 3. The world

One principal with a `BUDGET`, making a few `CALLS` on one route whose pool
has `DEPLOYMENTS`. Each finished attempt costs something from `COSTS`, at
most `WORST` (the declared worst case, which the model takes as true, as
the accounting model does). `LIMIT` is the principal's in-flight limit,
`QUOTA` each deployment's share of its provider's quota (in calls in
flight, for the model).

```quint attempts.qnt +=
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
```

An attempt ends in one of four ways, as the request path classifies them:

| Outcome | What the provider saw | Charged | Next |
|---|---|---|---|
| `unreached` | nothing (DNS, dial, TLS) | 0 | retry |
| `refused` | a refusal before any output: 429, 503, or 200 then an error as the first event | 0 | retry |
| `uncertain` | the request, then no answer (reset, timeout) | `WORST`, at once | the call fails; the provider's tail bills later |
| `commit` | a valid first event: the caller's stream begins | its cost, at its end | the call is done, or cut |

## 4. State

- `phase`: each call is `idle`, `waiting` (holding its slot, between
  attempts), `trying` (an attempt in flight), `streaming` (committed),
  `done` or `failed`.
- `attempts`, `commits`: per call, the attempts made and those committed.
- `on`: the deployment of a call's current attempt.
- `tails`: calls whose uncertain attempt the provider may still be
  working on. A failed call keeps its slot until its tail ends, as today
  (`settlement.cancel`).
- `known`: the known spend admission reads (the counter); `ledger`: the
  rows by event ID, deduplicated; `billed`: what the provider charged (a
  ghost).
- `retries`: the pool's retries so far; `load`: attempts in flight per
  deployment; `fails`: consecutive failures; `ejected`, `probes`: a
  deployment's ejection and its probes in flight; `probing`: the calls
  whose attempt in flight is a probe; `tried`: each call's deployments.

```quint attempts.qnt +=
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
```

## 5. The initial state and helpers

```quint attempts.qnt +=
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
```

## 6. Actions

### 6.1 A call starts

It takes a slot; its attempts check the budget.

```quint attempts.qnt +=
  action start(c: str): bool = all {
    phase.get(c) == "idle",
    slots < LIMIT,
    phase' = phase.put(c, "waiting"),
    attempts' = attempts, commits' = commits, on' = on, tails' = tails, known' = known,
    ledger' = ledger, billed' = billed, retries' = retries, load' = load, fails' = fails,
    ejected' = ejected, probes' = probes, probing' = probing, tried' = tried,
  }
```

### 6.2 An attempt is sent

The first attempt is admitted as today's call is: known spend below the
budget, the deployment below its quota. A retry checks its own
deployment's quota (`QUOTA_PER_ATTEMPT`), within the call's budget of
attempts and the pool's of retries, but not the budget again: the call was
admitted, and the bound holds without it (§7). An ejected deployment is
sent only its probe.

```quint attempts.qnt +=
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
```

### 6.3 An attempt fails before it commits

Unreached and refused attempts are free and retried. An uncertain one is
charged its worst case, and its tail goes on at the provider; with
`UNCERTAIN_FINAL` the call fails, keeping its slot for the tail. Every
failure counts towards its deployment's ejection; a failed probe frees its
probe, the deployment still ejected, so a later one may follow (as
`route.Health` does after its next ejection).

```quint attempts.qnt +=
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
```

### 6.4 An attempt commits, and ends

A valid first event commits the attempt: its deployment is healthy again
(an ejected one is readmitted). The call then ends whole, or is cut
mid-stream; either way the provider bills it and the gateway charges what
it saw (the accounting model's estimate, here exact). With `COMMIT_FINAL`
a cut call is done: its caller has part of an answer and gets the
contract's error event.

```quint attempts.qnt +=
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
```

### 6.5 A call gives up; a tail ends

A waiting call may give up at any point (its attempts or the budget spent,
no deployment left): the caller gets 503 with `Retry-After`. The gateway
(`ORDERED`) gives up only when no attempt can follow. An uncertain
attempt's tail ends when the provider finishes or drops it, billing at
most the worst case; a failed call's slot is then free.

```quint attempts.qnt +=
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
```

### 6.6 The step

```quint attempts.qnt +=
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
```

## 7. Properties

```quint attempts.qnt +=
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
```

What the design must be able to do: fail over (a call done after a failed
attempt), fail a call on an uncertain attempt, eject a deployment (its
readmission is §8's story), and go past the budget within the bound.

```quint attempts.qnt +=
  val failsOver = CALLS.exists(c => phase.get(c) == "done" and attempts.get(c) >= 2)
  val uncertainEndsCall = CALLS.exists(c => phase.get(c) == "failed" and tails.contains(c))
  val ejects = DEPLOYMENTS.exists(d => ejected.get(d))
  val overshoots = billed > BUDGET

  // Where the model means to stop: every call over and no tail running.
  val finished = CALLS.forall(c => Set("done", "failed").contains(phase.get(c))) and tails.size() == 0
```

## 8. The stories

**A refused attempt fails over** (the design). The first deployment
answers 200 and then an overloaded event; the attempt is free, the call is
sent to the other deployment, which streams the answer.

```quint attempts.qnt +=
  run failoverTest =
    init
      .then(start("c1"))
      .then(attempt("c1", "d1"))
      .then(failed("c1", "refused"))
      .then(attempt("c1", "d2"))
      .then(commit("c1"))
      .then(end("c1", 1, false))
      .expect(failsOver and safe and known == 1)
```

**An uncertain attempt is not retried** (the design). Its tail runs on
after the call has failed, under the call's slot, and bills within the
worst case it was charged.

```quint attempts.qnt +=
  run uncertainTest =
    init
      .then(start("c1"))
      .then(attempt("c1", "d1"))
      .then(failed("c1", "uncertain"))
      .expect(uncertainEndsCall and phase.get("c1") == "failed")
      .then(tailEnds("c1", 3))
      .expect(safe and billed == 3 and known == 3)
```

**An ejected deployment gets its probe, and a success readmits it** (the
design). Two refusals eject `d1`; the next attempt there is its probe
(no other may follow it: `probeOnly`, checked by §10's search); the probe
commits and `d1` is back.

```quint attempts.qnt +=
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
```

**A retried uncertain attempt doubles the provider's work** (without
`UNCERTAIN_FINAL`). With two calls at the limit, one's retry runs while
its first attempt's tail does, under one slot: three pieces of work for
two slots.

```quint attempts.qnt +=
  run uncertainRetryTest =
    init
      .then(start("c1"))
      .then(start("c2"))
      .then(attempt("c2", "d1"))
      .then(attempt("c1", "d2"))
      .then(failed("c1", "uncertain"))
      .then(attempt("c1", "d2"))
      .expect(not(workWithinLimit))
```

**A cut stream retried is a second answer** (without `COMMIT_FINAL`).

```quint attempts.qnt +=
  run cutRetryTest =
    init
      .then(start("c1"))
      .then(attempt("c1", "d1"))
      .then(commit("c1"))
      .then(end("c1", 1, true))
      .then(attempt("c1", "d2"))
      .then(commit("c1"))
      .expect(not(oneCommit))
```

**Attempts sharing an event lose a charge** (without `ATTEMPT_EVENTS`).
The free refusal's row takes the call's event ID; the committed attempt's
charge is dropped as its duplicate.

```quint attempts.qnt +=
  run sharedEventTest =
    init
      .then(start("c1"))
      .then(attempt("c1", "d1"))
      .then(failed("c1", "refused"))
      .then(attempt("c1", "d2"))
      .then(commit("c1"))
      .then(end("c1", 1, false))
      .expect(not(ledgerMatches))
```

## 9. What the model leaves out

- Crashes, Valkey failovers, delivery and reconciliation: the accounting
  model's, unchanged by retries (each attempt's row is one more event of
  the same kind).
- Time. `Retry-After`, backoff and probation are orderings here, not
  durations: an ejected deployment's probe may come at any point, and a
  retry may follow at once.
- Several gateways. Ejection is per gateway, so each probes on its own: at
  most one probe per deployment per gateway.
- The body kept for a retry, and its memory bound: the code's (m1-plan.md
  step 2), not a property of the loop.

## 10. The designs to check

The mid-sized instance the simulation samples, the design and its broken
variants:

```quint attempts.qnt +=
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
```

## The small instance

The instance the exhaustive search explores (attempts.small.yaml, run by
check-attempts.sh where it has one): two calls, two deployments, a budget
of 4 against a worst case of 3, one attempt per deployment at once, three
attempts a call and three retries for the pool (so that each budget binds
where the other does not). Its
variants (`simulate.variants`) are the designs, each with the mechanism it
turns off, what it must hold and what it must break; `gotogether simulate
-matrix` searches every run of each to the binding's depth and writes this
table, the properties by the designs (a cell "breaks in N steps" is its
shortest counterexample):

<!-- gotogether:matrix -->
| | design | design, two a deployment | retry after commit | retry uncertain | shared event | uncertain free | quota once | no retry budget | no pool cap | no ejection |
|---|---|---|---|---|---|---|---|---|---|---|
| safe | holds to depth 14 | holds to depth 14 | breaks in 6 steps | breaks in 6 steps | breaks in 5 steps | breaks in 4 steps | breaks in 6 steps | breaks in 8 steps | breaks in 12 steps | breaks in 6 steps |
| oneCommit | holds to depth 14 | holds to depth 14 | breaks in 6 steps | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 |
| ledgerMatches | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | breaks in 5 steps | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 |
| covered | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | breaks in 4 steps | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 |
| withinBound | holds to depth 14 | holds to depth 14 | breaks in 14 steps | breaks in 14 steps | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 |
| workWithinLimit | holds to depth 14 | holds to depth 14 | holds to depth 14 | breaks in 6 steps | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 |
| quotaHonoured | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | breaks in 6 steps | holds to depth 14 | holds to depth 14 | holds to depth 14 |
| attemptsBounded | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | breaks in 8 steps | holds to depth 14 | holds to depth 14 |
| retriesCapped | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | breaks in 12 steps | holds to depth 14 |
| probeOnly | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | holds to depth 14 | breaks in 6 steps |
<!-- /gotogether:matrix -->

```quint attempts.qnt +=

module small {
  import attempts(CALLS = Set("c1", "c2"), DEPLOYMENTS = Set("d1", "d2"), COSTS = Set(1, 3),
    WORST = 3, BUDGET = 4, LIMIT = 2, QUOTA = 1, MAX_ATTEMPTS = 3, POOL_RETRIES = 3, EJECT_AFTER = 1,
    COMMIT_FINAL = true, UNCERTAIN_FINAL = true, ATTEMPT_EVENTS = true, CHARGE_UNCERTAIN = true,
    QUOTA_PER_ATTEMPT = true, RETRY_BUDGET = true, POOL_CAP = true,
    EJECTION = true, ORDERED = false, ORDER = ["d1", "d2"]).*
}
```

## 11. The instance the gateway's tests drive

`gateway/proxy`'s `TestAttemptsModel` replays this instance's traces on the
real request path (connect): two calls on a pool of two deployments, each
with a ceiling of one attempt, ejected after two failures, three attempts
a call and three retries for the route (`gatewayWide`: a ceiling of two, and four retries).
Its choices are the gateway's
(`ORDERED`): the deployment each attempt takes and when a call gives up
are checked against the code's, not chosen for it. The budget and the
slots are the limiter's, already the accounting and exposure models', so
they do not bind here.

```quint attempts.qnt +=

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
```
