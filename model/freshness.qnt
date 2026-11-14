// Generated from freshness.md by lmt. Edit the markdown, not this file.
module freshness {
  const REPLICAS: Set[str]
  const GATEWAYS: Set[str]
  const RIGHTS: Set[str]
  const LEASE: int
  const MAX_TIME: int

  const CONFIG_FRESH: bool
  const ANCHOR_EARLIEST: bool
  const FENCE_BOTH: bool
  const PRESERVE_ON_RESTORE: bool
  var clock: int
  var stateRights: Set[str]
  var stateVersion: int
  var stateHistory: List[Set[str]]
  var configRights: Set[str]
  var configRevision: int
  var configHistory: List[Set[str]]

  def inForce: Set[str] = stateRights.intersect(configRights)
  type View = { sv: int, sRights: Set[str], sAt: int, cv: int, cRights: Set[str], cAt: int }

  var leaseHolder: str
  var leaseEpoch: int
  var believes: str -> int
  var views: str -> View
  type Pub = { seq: int, epoch: int, sv: int, cv: int, rights: Set[str], validUntil: int }
  type Gw = { applied: Pub, connected: bool }

  var record: { epoch: int, seq: int, sv: int, cv: int }
  var pointer: Pub
  var gws: str -> Gw
  var stateGoneAt: str -> int
  var configGoneAt: str -> int
  var forceGoneAt: str -> int
  var everRemoved: Set[str]
  var regrantServed: bool
  var restored: bool
  var staleWin: bool
  var compositeGap: bool
  var badServe: bool
  var configRollback: bool
  var widenedByRestore: bool
  val genesis: Pub = { seq: 0, epoch: 1, sv: 0, cv: 0, rights: RIGHTS, validUntil: LEASE }

  action initWith(first: str): bool = all {
    clock' = 0,
    stateRights' = RIGHTS, stateVersion' = 0, stateHistory' = [RIGHTS],
    configRights' = RIGHTS, configRevision' = 0, configHistory' = [RIGHTS],
    leaseHolder' = first, leaseEpoch' = 1,
    believes' = REPLICAS.mapBy(r => if (r == first) 1 else 0),
    views' = REPLICAS.mapBy(r => { sv: 0, sRights: RIGHTS, sAt: 0, cv: 0, cRights: RIGHTS, cAt: 0 }),
    record' = { epoch: 1, seq: 0, sv: 0, cv: 0 },
    pointer' = genesis,
    gws' = GATEWAYS.mapBy(g => { applied: genesis, connected: true }),
    stateGoneAt' = RIGHTS.mapBy(r => -1),
    configGoneAt' = RIGHTS.mapBy(r => -1),
    forceGoneAt' = RIGHTS.mapBy(r => -1),
    everRemoved' = Set(),
    regrantServed' = false, restored' = false, staleWin' = false, compositeGap' = false,
    badServe' = false, configRollback' = false, widenedByRestore' = false,
  }

  action init = {
    nondet first = REPLICAS.oneOf()
    initWith(first)
  }
  def goneAt(old: str -> int, before: Set[str], after: Set[str]): str -> int =
    RIGHTS.mapBy(r =>
      if (after.contains(r)) -1
      else if (before.contains(r)) clock
      else old.get(r))

  action trackSources(s: Set[str], c: Set[str]): bool = all {
    stateGoneAt' = goneAt(stateGoneAt, stateRights, s),
    configGoneAt' = goneAt(configGoneAt, configRights, c),
    forceGoneAt' = goneAt(forceGoneAt, inForce, s.intersect(c)),
    everRemoved' = everRemoved.union(inForce.exclude(s.intersect(c))),
  }

  action keepSources = all {
    stateRights' = stateRights, stateVersion' = stateVersion, stateHistory' = stateHistory,
    configRights' = configRights, configRevision' = configRevision, configHistory' = configHistory,
    stateGoneAt' = stateGoneAt, configGoneAt' = configGoneAt, forceGoneAt' = forceGoneAt,
    everRemoved' = everRemoved,
  }
  action keepLeadership = all { leaseHolder' = leaseHolder, leaseEpoch' = leaseEpoch, believes' = believes }
  action keepPublication = all { record' = record, pointer' = pointer }
  action keepFlags = all {
    regrantServed' = regrantServed, restored' = restored, staleWin' = staleWin, compositeGap' = compositeGap,
    badServe' = badServe, configRollback' = configRollback, widenedByRestore' = widenedByRestore,
  }
  action stateChange(right: str, grant: bool): bool = all {
    val s = if (grant) stateRights.union(Set(right)) else stateRights.exclude(Set(right))
    all {
      s != stateRights,
      stateRights' = s,
      stateVersion' = stateVersion + 1,
      stateHistory' = stateHistory.append(s),
      trackSources(s, configRights),
    },
    configRights' = configRights, configRevision' = configRevision, configHistory' = configHistory,
    clock' = clock, views' = views, gws' = gws, keepLeadership, keepPublication, keepFlags,
  }
  action configChange(right: str, grant: bool): bool = all {
    val c = if (grant) configRights.union(Set(right)) else configRights.exclude(Set(right))
    all {
      c != configRights,
      configRights' = c,
      configRevision' = configRevision + 1,
      configHistory' = configHistory.append(c),
      trackSources(stateRights, c),
    },
    stateRights' = stateRights, stateVersion' = stateVersion, stateHistory' = stateHistory,
    clock' = clock, views' = views, gws' = gws, keepLeadership, keepPublication, keepFlags,
  }
  action restore(version: int): bool = all {
    version >= 0,
    version < stateVersion,
    val old = stateHistory[version]
    val s = if (PRESERVE_ON_RESTORE) old.intersect(stateRights) else old
    all {
      stateRights' = s,
      stateVersion' = stateVersion + 1,
      stateHistory' = stateHistory.append(s),
      trackSources(s, configRights),
      widenedByRestore' = (widenedByRestore or s.exclude(stateRights).size() > 0),
    },
    restored' = true,
    regrantServed' = regrantServed, staleWin' = staleWin, compositeGap' = compositeGap, badServe' = badServe,
    configRollback' = configRollback,
    configRights' = configRights, configRevision' = configRevision, configHistory' = configHistory,
    clock' = clock, views' = views, gws' = gws, keepLeadership, keepPublication,
  }
  action readState(replica: str): bool = all {
    val v = views.get(replica)
    views' = views.put(replica, { ...v, sv: stateVersion, sRights: stateRights, sAt: clock }),
    clock' = clock, gws' = gws, keepSources, keepLeadership, keepPublication, keepFlags,
  }

  action listConfig(replica: str): bool = all {
    CONFIG_FRESH,
    val v = views.get(replica)
    views' = views.put(replica, { ...v, cv: configRevision, cRights: configRights, cAt: clock }),
    clock' = clock, gws' = gws, keepSources, keepLeadership, keepPublication, keepFlags,
  }
  action watchSync(replica: str, rev: int): bool = all {
    not(CONFIG_FRESH),
    val v = views.get(replica)
    all {
      rev >= v.cv,
      rev <= configRevision,
      views' = views.put(replica, { ...v, cv: rev, cRights: configHistory[rev], cAt: clock }),
    },
    clock' = clock, gws' = gws, keepSources, keepLeadership, keepPublication, keepFlags,
  }
  action takeLease(replica: str): bool = all {
    replica != leaseHolder,
    leaseHolder' = replica,
    leaseEpoch' = leaseEpoch + 1,
    believes' = believes.put(replica, leaseEpoch + 1),
    clock' = clock, views' = views, gws' = gws, keepSources, keepPublication, keepFlags,
  }

  action noticeLoss(replica: str): bool = all {
    replica != leaseHolder,
    believes.get(replica) > 0,
    believes' = believes.put(replica, 0),
    leaseHolder' = leaseHolder, leaseEpoch' = leaseEpoch,
    clock' = clock, views' = views, gws' = gws, keepSources, keepPublication, keepFlags,
  }
  action publish(replica: str): bool = all {
    believes.get(replica) > 0,
    val e = believes.get(replica)
    val v = views.get(replica)
    val fenced = e >= record.epoch and v.sv >= record.sv and (if (FENCE_BOTH) v.cv >= record.cv else true)
    val anchor = if (ANCHOR_EARLIEST and v.cAt < v.sAt) v.cAt else v.sAt
    val p = { seq: record.seq + 1, epoch: e, sv: v.sv, cv: v.cv,
              rights: v.sRights.intersect(v.cRights), validUntil: anchor + LEASE }
    all {
      fenced,
      record' = { epoch: e, seq: p.seq, sv: v.sv, cv: v.cv },
      pointer' = p,
      configRollback' = (configRollback or v.cv < pointer.cv),
      staleWin' = (staleWin or e < leaseEpoch),
    },
    regrantServed' = regrantServed, restored' = restored, compositeGap' = compositeGap, badServe' = badServe,
    widenedByRestore' = widenedByRestore,
    clock' = clock, views' = views, gws' = gws, keepSources, keepLeadership,
  }
  action poll(gateway: str): bool = all {
    val gw = gws.get(gateway)
    all {
      gw.connected,
      pointer.seq > gw.applied.seq,
      clock < pointer.validUntil,
      gws' = gws.put(gateway, { ...gw, applied: pointer }),
    },
    clock' = clock, views' = views, keepSources, keepLeadership, keepPublication, keepFlags,
  }

  action partition(gateway: str): bool = all {
    val gw = gws.get(gateway)
    all { gw.connected, gws' = gws.put(gateway, { ...gw, connected: false }) },
    clock' = clock, views' = views, keepSources, keepLeadership, keepPublication, keepFlags,
  }

  action heal(gateway: str): bool = all {
    val gw = gws.get(gateway)
    all { not(gw.connected), gws' = gws.put(gateway, { ...gw, connected: true }) },
    clock' = clock, views' = views, keepSources, keepLeadership, keepPublication, keepFlags,
  }

  action serve(gateway: str, right: str): bool = all {
    val a = gws.get(gateway).applied
    all {
      a.rights.contains(right),
      clock < a.validUntil,
      badServe' = (badServe or withheldLong(stateRights, stateGoneAt, right) or withheldLong(configRights, configGoneAt, right)),
      regrantServed' = (regrantServed or (inForce.contains(right) and everRemoved.contains(right))),
      compositeGap' = (compositeGap or withheldLong(inForce, forceGoneAt, right)),
    },
    restored' = restored, staleWin' = staleWin, configRollback' = configRollback,
    widenedByRestore' = widenedByRestore,
    clock' = clock, views' = views, gws' = gws, keepSources, keepLeadership, keepPublication,
  }

  action tick: bool = all {
    clock < MAX_TIME,
    clock' = clock + 1,
    views' = views, gws' = gws, keepSources, keepLeadership, keepPublication, keepFlags,
  }
  def withheldLong(granted: Set[str], gone: str -> int, right: str): bool =
    not(granted.contains(right)) and clock >= gone.get(right) + LEASE
  action step = {
    nondet replica = REPLICAS.oneOf()
    nondet gateway = GATEWAYS.oneOf()
    nondet right = RIGHTS.oneOf()
    nondet grant = Set(true, false).oneOf()
    nondet version = 0.to(stateVersion).oneOf()
    nondet rev = 0.to(configRevision).oneOf()
    any {
      stateChange(right, grant),
      configChange(right, grant),
      restore(version),
      readState(replica),
      listConfig(replica),
      watchSync(replica, rev),
      takeLease(replica),
      noticeLoss(replica),
      publish(replica),
      poll(gateway),
      partition(gateway),
      heal(gateway),
      serve(gateway, right),
      tick,
    }
  }
  // No right is served a full lease after either source withheld it.
  val restrictionsBounded = not(badServe)

  // Publications never move the config backwards.
  val noConfigRollback = not(configRollback)

  // A restore never grants what the current state does not.
  val restoreNeverWidens = not(widenedByRestore)

  val safe = restrictionsBounded and noConfigRollback and restoreNeverWidens
  val configRestrictionApplied = GATEWAYS.exists(g =>
    gws.get(g).applied.cv > 0 and RIGHTS.exists(r => not(configRights.contains(r)) and not(gws.get(g).applied.rights.contains(r))))
  val regrantedRightServed = regrantServed
  val staleLeaderPublished = staleWin
  val restoreHappened = restored and stateVersion > 1
  val compositeGapReached = compositeGap
  run compositeGapTest =
    initWith("r1")
      .then(configChange("a", false))
      .then(tick)
      .then(readState("r1"))
      .then(tick)
      .then(stateChange("a", false))
      .then(configChange("a", true))
      .then(listConfig("r1"))
      .then(publish("r1"))
      .then(poll("g1"))
      .then(tick)
      .then(serve("g1", "a"))
      .expect(compositeGapReached and restrictionsBounded)
  run staleCacheTest =
    initWith("r1")
      .then(configChange("a", false))
      .then(tick).then(tick).then(tick)
      .then(watchSync("r1", 0))
      .then(readState("r1"))
      .then(publish("r1"))
      .then(poll("g1"))
      .then(serve("g1", "a"))
      .expect(not(restrictionsBounded))
  run anchorTest =
    initWith("r1")
      .then(listConfig("r1"))
      .then(configChange("a", false))
      .then(tick).then(tick).then(tick)
      .then(readState("r1"))
      .then(publish("r1"))
      .then(poll("g1"))
      .then(serve("g1", "a"))
      .expect(not(restrictionsBounded))
  run configRollbackTest =
    initWith("r1")
      .then(listConfig("r2"))
      .then(configChange("a", false))
      .then(listConfig("r1"))
      .then(readState("r1"))
      .then(publish("r1"))
      .then(poll("g1"))
      .then(takeLease("r2"))
      .then(readState("r2"))
      .then(publish("r2"))
      .expect(not(noConfigRollback))
  run restoreWidensTest =
    initWith("r1")
      .then(stateChange("a", false))
      .then(restore(0))
      .expect(not(restoreNeverWidens))
}

module freshDesign {
  import freshness(REPLICAS = Set("r1", "r2"), GATEWAYS = Set("g1"), RIGHTS = Set("a", "b"),
    LEASE = 3, MAX_TIME = 7,
    CONFIG_FRESH = true, ANCHOR_EARLIEST = true, FENCE_BOTH = true, PRESERVE_ON_RESTORE = true).*
}

module staleCache {
  import freshness(REPLICAS = Set("r1", "r2"), GATEWAYS = Set("g1"), RIGHTS = Set("a", "b"),
    LEASE = 3, MAX_TIME = 7,
    CONFIG_FRESH = false, ANCHOR_EARLIEST = true, FENCE_BOTH = true, PRESERVE_ON_RESTORE = true).*
}

module anchorState {
  import freshness(REPLICAS = Set("r1", "r2"), GATEWAYS = Set("g1"), RIGHTS = Set("a", "b"),
    LEASE = 3, MAX_TIME = 7,
    CONFIG_FRESH = true, ANCHOR_EARLIEST = false, FENCE_BOTH = true, PRESERVE_ON_RESTORE = true).*
}

module fenceStateOnly {
  import freshness(REPLICAS = Set("r1", "r2"), GATEWAYS = Set("g1"), RIGHTS = Set("a", "b"),
    LEASE = 3, MAX_TIME = 7,
    CONFIG_FRESH = true, ANCHOR_EARLIEST = true, FENCE_BOTH = false, PRESERVE_ON_RESTORE = true).*
}

module naiveRestore {
  import freshness(REPLICAS = Set("r1", "r2"), GATEWAYS = Set("g1"), RIGHTS = Set("a", "b"),
    LEASE = 3, MAX_TIME = 7,
    CONFIG_FRESH = true, ANCHOR_EARLIEST = true, FENCE_BOTH = true, PRESERVE_ON_RESTORE = false).*
}

module small {
  import freshness(REPLICAS = Set("r1", "r2"), GATEWAYS = Set("g1"), RIGHTS = Set("a"), LEASE = 2, MAX_TIME = 4,
    CONFIG_FRESH = true, ANCHOR_EARLIEST = true, FENCE_BOTH = true, PRESERVE_ON_RESTORE = true).*
}
