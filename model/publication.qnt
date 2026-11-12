// Generated from publication.md by lmt. Edit the markdown, not this file.
module publication {
  const REPLICAS: Set[str]
  const GATEWAYS: Set[str]
  const KEYS: Set[str]
  const LEASE: int
  const MAX_TIME: int
  const FENCE: bool
  const LEASE_FROM_SOURCE: bool
  const TRUSTED_FLOOR: bool
  const PRESERVE_ON_RESTORE: bool
  const CHECK_EXPIRY: bool
  var clock: int
  var revoked: Set[str]
  var stateVersion: int
  var history: List[Set[str]]
  type View = { version: int, revoked: Set[str], loadedAt: int }

  var leaseHolder: str
  var leaseEpoch: int
  var believes: str -> int
  var views: str -> View
  type Pub = { seq: int, epoch: int, source: int, revoked: Set[str], validUntil: int }
  type PubRecord = { epoch: int, seq: int, source: int }

  var record: PubRecord
  var pointer: Pub
  var published: Set[Pub]
  type Gw = { applied: Pub, floor: int, connected: bool, checkpoint: int }

  var gws: str -> Gw
  var status: str -> int
  var revokedAt: str -> int
  var everRevoked: Set[str]
  var badServe: bool
  var expiredServe: bool
  var rollback: bool
  var belowCheckpoint: bool
  var outOfOrder: bool
  var restoredOnce: bool
  var staleWin: bool
  val genesis: Pub = { seq: 0, epoch: 1, source: 0, revoked: Set(), validUntil: LEASE }
  val blank: Pub = { seq: -1, epoch: 0, source: -1, revoked: Set(), validUntil: 0 }

  action initWith(first: str): bool = {
    all {
      clock' = 0,
      revoked' = Set(),
      stateVersion' = 0,
      history' = [Set()],
      leaseHolder' = first,
      leaseEpoch' = 1,
      believes' = REPLICAS.mapBy(r => if (r == first) 1 else 0),
      views' = REPLICAS.mapBy(r => { version: 0, revoked: Set(), loadedAt: 0 }),
      record' = { epoch: 1, seq: 0, source: 0 },
      pointer' = genesis,
      published' = Set(genesis),
      gws' = GATEWAYS.mapBy(g => { applied: genesis, floor: 0, connected: true, checkpoint: 0 }),
      status' = GATEWAYS.mapBy(g => 0),
      revokedAt' = Map(),
      everRevoked' = Set(),
      badServe' = false,
      expiredServe' = false,
      rollback' = false,
      belowCheckpoint' = false,
      outOfOrder' = false,
      restoredOnce' = false,
      staleWin' = false,
    }
  }

  action init = initWith("r1")
  action keepState = all {
    revoked' = revoked, stateVersion' = stateVersion, history' = history,
    revokedAt' = revokedAt, everRevoked' = everRevoked,
  }
  action keepLeadership = all {
    leaseHolder' = leaseHolder, leaseEpoch' = leaseEpoch, believes' = believes,
  }
  action keepPublication = all { record' = record, pointer' = pointer, published' = published }
  action keepGateways = all { gws' = gws, status' = status }
  action keepFlags = all {
    badServe' = badServe, expiredServe' = expiredServe, rollback' = rollback,
    belowCheckpoint' = belowCheckpoint, outOfOrder' = outOfOrder,
    restoredOnce' = restoredOnce, staleWin' = staleWin,
  }
  action revoke(key: str): bool = all {
    not(revoked.contains(key)),
    val nextRevoked = revoked.union(Set(key))
    all {
      revoked' = nextRevoked,
      stateVersion' = stateVersion + 1,
      history' = history.append(nextRevoked),
      revokedAt' = revokedAt.put(key, clock),
      everRevoked' = everRevoked.union(Set(key)),
    },
    clock' = clock, views' = views,
    keepLeadership, keepPublication, keepGateways, keepFlags,
  }
  action restore(version: int): bool = all {
    version >= 0,
    version < stateVersion,
    val old = history[version]
    val nextRevoked = if (PRESERVE_ON_RESTORE) old.union(revoked) else old
    all {
      revoked' = nextRevoked,
      stateVersion' = stateVersion + 1,
      history' = history.append(nextRevoked),
      revokedAt' = revokedAt,
      everRevoked' = everRevoked,
    },
    restoredOnce' = true, staleWin' = staleWin,
    badServe' = badServe, expiredServe' = expiredServe, rollback' = rollback,
    belowCheckpoint' = belowCheckpoint, outOfOrder' = outOfOrder,
    clock' = clock, views' = views,
    keepLeadership, keepPublication, keepGateways,
  }
  action loadState(replica: str): bool = all {
    views' = views.put(replica, { version: stateVersion, revoked: revoked, loadedAt: clock }),
    clock' = clock, keepState, keepLeadership, keepPublication, keepGateways, keepFlags,
  }
  action takeLease(replica: str): bool = all {
    replica != leaseHolder,
    leaseHolder' = replica,
    leaseEpoch' = leaseEpoch + 1,
    believes' = believes.put(replica, leaseEpoch + 1),
    clock' = clock, views' = views, keepState, keepPublication, keepGateways, keepFlags,
  }

  action noticeLoss(replica: str): bool = all {
    replica != leaseHolder,
    believes.get(replica) > 0,
    believes' = believes.put(replica, 0),
    leaseHolder' = leaseHolder, leaseEpoch' = leaseEpoch,
    clock' = clock, views' = views, keepState, keepPublication, keepGateways, keepFlags,
  }
  action publish(replica: str): bool = all {
    believes.get(replica) > 0,
    val e = believes.get(replica)
    val v = views.get(replica)
    val fenced = e >= record.epoch and v.version >= record.source
    val p = {
      seq: record.seq + 1,
      epoch: e,
      source: v.version,
      revoked: v.revoked,
      validUntil: if (LEASE_FROM_SOURCE) v.loadedAt + LEASE else clock + LEASE,
    }
    all {
      if (FENCE) fenced else true,
      record' = { epoch: e, seq: p.seq, source: v.version },
      pointer' = p,
      published' = published.union(Set(p)),
      outOfOrder' = (outOfOrder or v.version < pointer.source),
      staleWin' = (staleWin or e < leaseEpoch),
    },
    badServe' = badServe, expiredServe' = expiredServe, rollback' = rollback,
    belowCheckpoint' = belowCheckpoint, restoredOnce' = restoredOnce,
    clock' = clock, views' = views, keepState, keepLeadership, keepGateways,
  }
  action replayPointer(old: Pub): bool = all {
    published.contains(old),
    pointer' = old,
    record' = record, published' = published,
    clock' = clock, views' = views, keepState, keepLeadership, keepGateways, keepFlags,
  }

  action forgeStatus(gateway: str): bool = all {
    status' = status.put(gateway, 0),
    gws' = gws,
    clock' = clock, views' = views, keepState, keepLeadership, keepPublication, keepFlags,
  }
  action poll(gateway: str): bool = all {
    val gw = gws.get(gateway)
    val p = pointer
    all {
      gw.connected,
      p.seq >= gw.floor,
      p.seq > gw.applied.seq,
      if (CHECK_EXPIRY) clock < p.validUntil else true,
      gws' = gws.put(gateway, { ...gw, applied: p, floor: p.seq }),
      status' = status.put(gateway, p.seq),
      rollback' = (rollback or (gw.applied.seq >= 0 and p.source < gw.applied.source)),
      belowCheckpoint' = (belowCheckpoint or p.seq < gw.checkpoint),
    },
    badServe' = badServe, expiredServe' = expiredServe, outOfOrder' = outOfOrder,
    restoredOnce' = restoredOnce, staleWin' = staleWin,
    clock' = clock, views' = views, keepState, keepLeadership, keepPublication,
  }
  action restartEmpty(gateway: str): bool = all {
    val gw = gws.get(gateway)
    all {
      gw.connected,
      gws' = gws.put(gateway, {
        applied: blank,
        floor: if (TRUSTED_FLOOR) record.seq else status.get(gateway),
        connected: true,
        checkpoint: record.seq,
      }),
    },
    status' = status,
    clock' = clock, views' = views, keepState, keepLeadership, keepPublication, keepFlags,
  }
  action partition(gateway: str): bool = all {
    val gw = gws.get(gateway)
    all { gw.connected, gws' = gws.put(gateway, { ...gw, connected: false }) },
    status' = status,
    clock' = clock, views' = views, keepState, keepLeadership, keepPublication, keepFlags,
  }

  action heal(gateway: str): bool = all {
    val gw = gws.get(gateway)
    all { not(gw.connected), gws' = gws.put(gateway, { ...gw, connected: true }) },
    status' = status,
    clock' = clock, views' = views, keepState, keepLeadership, keepPublication, keepFlags,
  }
  action serve(gateway: str, key: str): bool = all {
    val a = gws.get(gateway).applied
    all {
      a.seq >= 0,
      not(a.revoked.contains(key)),
      if (CHECK_EXPIRY) clock < a.validUntil else true,
      badServe' = (badServe or (revokedAt.keys().contains(key) and clock >= revokedAt.get(key) + LEASE)),
      expiredServe' = (expiredServe or clock >= a.validUntil),
    },
    rollback' = rollback, belowCheckpoint' = belowCheckpoint, outOfOrder' = outOfOrder,
    restoredOnce' = restoredOnce, staleWin' = staleWin,
    clock' = clock, views' = views, keepState, keepLeadership, keepPublication, keepGateways,
  }
  action tick: bool = all {
    clock < MAX_TIME,
    clock' = clock + 1,
    views' = views, keepState, keepLeadership, keepPublication, keepGateways, keepFlags,
  }
  action step = {
    nondet replica = REPLICAS.oneOf()
    nondet gateway = GATEWAYS.oneOf()
    nondet key = KEYS.oneOf()
    nondet version = 0.to(stateVersion).oneOf()
    nondet old = published.oneOf()
    any {
      revoke(key),
      restore(version),
      loadState(replica),
      takeLease(replica),
      noticeLoss(replica),
      publish(replica),
      replayPointer(old),
      forgeStatus(gateway),
      poll(gateway),
      restartEmpty(gateway),
      partition(gateway),
      heal(gateway),
      serve(gateway, key),
      tick,
    }
  }
  val revocationBounded = not(badServe)
  val noExpiredServe = not(expiredServe)
  val noRollback = not(rollback)
  val noRollbackBelowCheckpoint = not(belowCheckpoint)
  val publicationsOrdered = not(outOfOrder)
  val revocationsSurviveRestore = everRevoked.subseteq(revoked)
  val safe = and {
    revocationBounded,
    noExpiredServe,
    noRollback,
    noRollbackBelowCheckpoint,
    publicationsOrdered,
    revocationsSurviveRestore,
  }
  val revocationReachesGateway = GATEWAYS.exists(g =>
    revoked.exists(k => gws.get(g).applied.revoked.contains(k)))

  val staleLeaderPublished = staleWin

  val partitionedAndExpired = GATEWAYS.exists(g =>
    not(gws.get(g).connected) and gws.get(g).applied.seq >= 0
      and clock >= gws.get(g).applied.validUntil)

  val restoredWithRevocations = restoredOnce and everRevoked.size() > 0
  run staleLeaderTest =
    initWith("r1")
      .then(loadState("r1"))
      .then(revoke("k1"))
      .then(takeLease("r2"))
      .then(tick).then(tick).then(tick)
      .then(publish("r1"))
      .then(poll("g1"))
      .then(serve("g1", "k1"))
      .expect(not(revocationBounded))
  run pausedLeaderTest =
    initWith("r1")
      .then(loadState("r1"))
      .then(revoke("k1"))
      .then(takeLease("r2"))
      .then(loadState("r2"))
      .then(publish("r2"))
      .then(poll("g1"))
      .then(publish("r1"))
      .then(poll("g1"))
      .expect(not(noRollback))
  run restoreTest =
    initWith("r1")
      .then(revoke("k1"))
      .then(restore(0))
      .then(tick).then(tick).then(tick)
      .then(loadState("r1"))
      .then(publish("r1"))
      .then(poll("g1"))
      .then(serve("g1", "k1"))
      .expect(not(revocationBounded))
}

module design {
  import publication(
    REPLICAS = Set("r1", "r2"), GATEWAYS = Set("g1", "g2"), KEYS = Set("k1", "k2"),
    LEASE = 3, MAX_TIME = 8,
    FENCE = true, LEASE_FROM_SOURCE = true, TRUSTED_FLOOR = true,
    PRESERVE_ON_RESTORE = true, CHECK_EXPIRY = true,
  ).*
}

module noFencing {
  import publication(
    REPLICAS = Set("r1", "r2"), GATEWAYS = Set("g1", "g2"), KEYS = Set("k1", "k2"),
    LEASE = 3, MAX_TIME = 8,
    FENCE = false, LEASE_FROM_SOURCE = true, TRUSTED_FLOOR = true,
    PRESERVE_ON_RESTORE = true, CHECK_EXPIRY = true,
  ).*
}

module leaseFromSigning {
  import publication(
    REPLICAS = Set("r1", "r2"), GATEWAYS = Set("g1", "g2"), KEYS = Set("k1", "k2"),
    LEASE = 3, MAX_TIME = 8,
    FENCE = true, LEASE_FROM_SOURCE = false, TRUSTED_FLOOR = true,
    PRESERVE_ON_RESTORE = true, CHECK_EXPIRY = true,
  ).*
}

module statusFloor {
  import publication(
    REPLICAS = Set("r1", "r2"), GATEWAYS = Set("g1", "g2"), KEYS = Set("k1", "k2"),
    LEASE = 3, MAX_TIME = 8,
    FENCE = true, LEASE_FROM_SOURCE = true, TRUSTED_FLOOR = false,
    PRESERVE_ON_RESTORE = true, CHECK_EXPIRY = true,
  ).*
}

module naiveRestore {
  import publication(
    REPLICAS = Set("r1", "r2"), GATEWAYS = Set("g1", "g2"), KEYS = Set("k1", "k2"),
    LEASE = 3, MAX_TIME = 8,
    FENCE = true, LEASE_FROM_SOURCE = true, TRUSTED_FLOOR = true,
    PRESERVE_ON_RESTORE = false, CHECK_EXPIRY = true,
  ).*
}

module noExpiry {
  import publication(
    REPLICAS = Set("r1", "r2"), GATEWAYS = Set("g1", "g2"), KEYS = Set("k1", "k2"),
    LEASE = 3, MAX_TIME = 8,
    FENCE = true, LEASE_FROM_SOURCE = true, TRUSTED_FLOOR = true,
    PRESERVE_ON_RESTORE = true, CHECK_EXPIRY = false,
  ).*
}
