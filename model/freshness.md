# Two sources, one snapshot: the freshness model

A literate Quint specification, the companion of
[publication.md](publication.md). That model asks how long a revoked key
can be served, with one source of truth and an attacker who replays old
objects. This one asks the question [ADR 0041](../docs/adr/0041-security-boundaries-and-incident-controls.md)
§5 left open:

> A snapshot is built from **two** authorities: the state document (keys,
> memberships, roles, services, capture and storage switches) and the
> config CRDs (routes, scope policies). Any right can be taken away in
> either. Is every restriction, from either source, honoured by every
> gateway within one lease, even when a replica's watch cache is stale, a
> leader pauses between reading and publishing, a Git rollback re-grants a
> right, or an administrator restores old state?

The design's answer is yes, if four rules hold (ADR 0041 §5 and §7.2):

| Switch | When on (the design) | When off (the bug it prevents) |
|---|---|---|
| `CONFIG_FRESH` | Each renewal re-reads the config CRDs with a consistent LIST | A lagging watch cache is treated as fresh: a route narrowed in Git keeps being granted, with new validity |
| `ANCHOR_EARLIEST` | The lease is counted from the **earlier** of the two source reads | It is counted from the state read alone: an older config read gets the state read's freshness |
| `FENCE_BOTH` | A publisher must be at least as fresh as the last publication in **both** sources | Only the state source is fenced: a successor with an older config view rolls config back |
| `PRESERVE_ON_RESTORE` | A restore keeps every restriction made since the restored version | A restore silently re-grants removed rights |

Fencing by leadership epoch, the trusted floor and expiry checking (the
publication model's mechanisms) are always on here.

## 1. Reading and running

`make` extracts this file into `freshness.qnt` and runs
`check-freshness.sh`: the design holds, every scenario is reachable, every
broken design breaks its property, every story behaves as §8 says.

```quint freshness.qnt +=
// Generated from freshness.md by lmt. Edit the markdown, not this file.
module freshness {
```

## 2. The world

```quint freshness.qnt +=
  const REPLICAS: Set[str]
  const GATEWAYS: Set[str]
  const RIGHTS: Set[str]
  const LEASE: int
  const MAX_TIME: int

  const CONFIG_FRESH: bool
  const ANCHOR_EARLIEST: bool
  const FENCE_BOTH: bool
  const PRESERVE_ON_RESTORE: bool
```

A **right** stands for anything a request needs: a key being valid, a
membership, a role, a service existing, capture or provider storage being
allowed, a route being permitted. A right is in force only if **both**
sources grant it, because authorization is an intersection (org-model §3):
the state must say the key and membership exist, and the config must
permit the route. Taking it away in either source is a restriction.

## 3. State

### 3.1 The two sources

Each source has its current grants, a version that rises on every change,
and its history: the bucket keeps every state version; Git keeps every
config revision (and a rollback is just a new revision with old content).

```quint freshness.qnt +=
  var clock: int
  var stateRights: Set[str]
  var stateVersion: int
  var stateHistory: List[Set[str]]
  var configRights: Set[str]
  var configRevision: int
  var configHistory: List[Set[str]]

  def inForce: Set[str] = stateRights.intersect(configRights)
```

### 3.2 Replicas and their views

Leadership as in the publication model: a Lease with an epoch, and what
each replica *believes* its epoch is. Each replica's view holds what it
last read from each source, and **when**.

```quint freshness.qnt +=
  type View = { sv: int, sRights: Set[str], sAt: int, cv: int, cRights: Set[str], cAt: int }

  var leaseHolder: str
  var leaseEpoch: int
  var believes: str -> int
  var views: str -> View
```

### 3.3 Publication and gateways

A publication now records both source versions it was built from. The
record is the authoritative "latest publication", won by compare-and-swap.

```quint freshness.qnt +=
  type Pub = { seq: int, epoch: int, sv: int, cv: int, rights: Set[str], validUntil: int }
  type Gw = { applied: Pub, connected: bool }

  var record: { epoch: int, seq: int, sv: int, cv: int }
  var pointer: Pub
  var gws: str -> Gw
```

### 3.4 Ghosts

- `stateGoneAt`, `configGoneAt`: for each right a source does not grant,
  when that source last stopped granting it (−1 while it grants it). The
  guarantee is per source: once a source withholds a right, no gateway
  serves it a full lease later. A right re-granted (by a Git rollback, say)
  may be served again at once.
- `forceGoneAt`: the same for the intersection, the right being in force.
  It is not the guarantee (§6.2 shows why) but a scenario.
- `everRemoved`, `regrantServed`, `restored`, `staleWin`, `compositeGap`:
  what the possibility scenarios need.
- `badServe`, `configRollback`, `widenedByRestore`: the bugs.

```quint freshness.qnt +=
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
```

## 4. The initial state

Everything granted; one replica leads and has published.

```quint freshness.qnt +=
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
```

Helpers: the ghost bookkeeping when a set of grants changes (a right
that stops being granted is stamped with the time; one granted again is
cleared), and "the rest stays as it is" for each group of variables.

```quint freshness.qnt +=
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
```

## 5. Actions

### 5.1 The state changes

A grant or a restriction in the state document: a key minted or revoked, a
member added or removed, a role, a service, a capture or storage switch.

```quint freshness.qnt +=
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
```

### 5.2 The config changes

A commit in the GitOps repository that Argo CD applies: a route narrowed,
a scope policy forbidding capture, or a rollback to an older revision that
re-grants something. Every accepted change, deletions included, raises the
publisher-managed config revision (ADR 0041 §5: Kubernetes'
`resourceVersion` cannot serve, being opaque across objects).

```quint freshness.qnt +=
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
```

### 5.3 Restore

`golemctl state restore` writes an old state as a new version. With
`PRESERVE_ON_RESTORE` it keeps every restriction made since: the result is
the old grants **intersected** with the current ones, so nothing removed
since comes back (and anything granted since is dropped too, which is what
going back means). Without it, the old grants return as they were.

```quint freshness.qnt +=
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
```

### 5.4 Reading the sources

A replica reads the state document (a GET: authoritative) and, in the
design, the config CRDs with a consistent LIST (authoritative too). The two
reads happen at different moments, and a replica can be paused between
reading and publishing; each read keeps its own time.

```quint freshness.qnt +=
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
```

Without `CONFIG_FRESH`, the config view comes from the replica's watch
cache, which delivers revisions in order but may lag arbitrarily behind
Git (a slow watch, a missed event before a relist). The bug is treating
whatever the cache holds as read *now*.

```quint freshness.qnt +=
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
```

### 5.5 Leadership

```quint freshness.qnt +=
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
```

### 5.6 Publishing

The snapshot grants what **both** views grant. The two rules of this
model meet here:

- **Fencing on both sources** (`FENCE_BOTH`): the publisher's epoch and both
  of its source versions must be at least the record's. A successor whose
  config view is older than what its predecessor already published cannot
  roll the config back.
- **The earliest anchor** (`ANCHOR_EARLIEST`): the authorization lasts one
  lease from the **earlier** of the two reads. Each part of the snapshot is
  only as fresh as the read it came from; a fresh state read must not
  license a stale config read (ADR 0041 §5).

```quint freshness.qnt +=
  action publish(replica: str): bool = all {
    believes.get(replica) > 0,
    val e = believes.get(replica)
    val v = views.get(replica)
    val fenced = replica == leaseHolder and e == leaseEpoch
      and e >= record.epoch and v.sv >= record.sv and (if (FENCE_BOTH) v.cv >= record.cv else true)
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
```

### 5.7 Gateways

Polling, partitions and serving as in the publication model. Serving a
right that either source has withheld for a full lease sets `badServe`;
serving a right that was removed and has since been re-granted is fine and
is recorded as a scenario, as is serving one out of force for a full lease
although neither source has withheld it that long (§6.2).

```quint freshness.qnt +=
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
```

where a right is withheld long by a set of grants if the set has not held
it for a full lease:

```quint freshness.qnt +=
  def withheldLong(granted: Set[str], gone: str -> int, right: str): bool =
    not(granted.contains(right)) and clock >= gone.get(right) + LEASE
```

### 5.8 The step

```quint freshness.qnt +=
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
```

## 6. Properties

### 6.1 Safety

```quint freshness.qnt +=
  // No right is served a full lease after either source withheld it.
  val restrictionsBounded = not(badServe)

  // Publications never move the config backwards.
  val noConfigRollback = not(configRollback)

  // A restore never grants what the current state does not.
  val restoreNeverWidens = not(widenedByRestore)

  val safe = restrictionsBounded and noConfigRollback and restoreNeverWidens
```

### 6.2 Possibility

The design must be able to: carry a config restriction to a gateway; serve
a right that a Git rollback re-granted; and restore. (A stale leader
cannot publish: the record's write checks the Lease, publication.md §6.5.)

It also can, and this is worth knowing, serve a right that has been out of
force for longer than a lease (`compositeGapReached`). The two reads
happen at different moments, so a snapshot can combine a state that granted
a right while the config forbade it with a later config that allows it
again after the state withdrew it: a pairing that never existed at any one
instant. In terms of the org model: a member's route is narrowed, then the
member is removed, then the route is widened back; a snapshot built from
the state read before the removal and the config read after the widening
grants the member the route. The design does not prevent this, because
each restriction is still honoured within one lease of **its** source's
change: the member's removal stops the grant one lease after it was made.
Preventing it would need a read that is atomic across S3 and the API
server, which does not exist; the per-source bound is the guarantee ADR
0041 states, and this scenario is why it is stated per source.

```quint freshness.qnt +=
  val configRestrictionApplied = GATEWAYS.exists(g =>
    gws.get(g).applied.cv > 0 and RIGHTS.exists(r => not(configRights.contains(r)) and not(gws.get(g).applied.rights.contains(r))))
  val regrantedRightServed = regrantServed
  val staleLeaderPublished = staleWin
  val restoreHappened = restored and stateVersion > 1
  val compositeGapReached = compositeGap
```

A story makes the gap concrete; it passes in the design (and is the
scenario's witness: five steps in one order are too rare for random
simulation to find). Route `a` is
narrowed at time 0 (`a` leaves force); r1 reads the state at 1, which
still has the member; at 2 the member is removed and the route widened
back; r1 lists the config and publishes, the lease anchored at 1. At 3, `a`
has been out of force for a full lease and g1 serves it, yet the state
withdrew it only at 2 and the config grants it: no restriction is late.

```quint freshness.qnt +=
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
```

## 7. The stories

**A stale watch cache re-grants a narrowed route** (`staleCache`). Route
`a` is narrowed in Git at time 0. Three ticks later, replica r1's watch
cache still holds revision 0; it reads the state and publishes, believing
its config view is fresh, so the snapshot grants `a` with a new lease, and
g1 serves it a full lease after the restriction. In the design there is no
watch-fed view to publish from: every renewal lists the config.

```quint freshness.qnt +=
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
```

**A fresh state read licenses an old config read** (`anchorState`). r1
lists the config at time 0; route `a` is narrowed straight after; three
ticks later r1 reads the state and publishes. Anchored on the state read,
the snapshot is valid until 6 and carries the old config, so g1 serves
`a` at 3. Anchored on the earlier read, it expired at 3 and no gateway
takes it.

```quint freshness.qnt +=
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
```

**A successor rolls the config back** (`fenceStateOnly`). r2 lists the
config before route `a` is narrowed; r1, the leader, lists it afterwards
and publishes the narrowing. Then r2 takes the Lease and publishes from its
older config view: its epoch is newer and its state view is as fresh as
the record's, so only fencing on the config source stops it.

```quint freshness.qnt +=
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
```

**A restore re-grants a removed right** (`naiveRestore`). A membership `a`
is removed; an administrator restores version 0, from before the removal.

```quint freshness.qnt +=
  run restoreWidensTest =
    initWith("r1")
      .then(stateChange("a", false))
      .then(restore(0))
      .expect(not(restoreNeverWidens))
```

The model ends here:

```quint freshness.qnt +=
}
```

## 8. The designs to check

| Instance | Switch off | Should break |
|---|---|---|
| `freshDesign` | none | nothing (`safe` holds) |
| `staleCache` | `CONFIG_FRESH` | `restrictionsBounded` |
| `anchorState` | `ANCHOR_EARLIEST` | `restrictionsBounded` |
| `fenceStateOnly` | `FENCE_BOTH` | `noConfigRollback` |
| `naiveRestore` | `PRESERVE_ON_RESTORE` | `restoreNeverWidens` |

Each story passes in the design it names and fails in `freshDesign`;
`compositeGapTest` passes in `freshDesign`:

| Story | Passes in |
|---|---|
| `staleCacheTest` | `staleCache` |
| `anchorTest` | `anchorState` |
| `configRollbackTest` | `fenceStateOnly` |
| `restoreWidensTest` | `naiveRestore` |
| `compositeGapTest` | `freshDesign` (a scenario, not a bug) |

```quint freshness.qnt +=

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
```

## 9. What this model leaves out

- **Replays and forged status objects**: the publication model's subject;
  here the publication record and the floor are assumed to work as that
  model shows.
- **Several pools and audiences**: one pool; audience filtering is a
  property of snapshot building.
- **Clock skew**: one clock.
- **The emergency denylist**: it only removes rights, so it only shortens
  the bound; its own delivery (ADR 0041 §1) is an operational property, not
  this protocol's.
- **Object identity**: rights stand for (UID, generation)-identified
  objects; a deleted-and-recreated object is a removal and a new grant.

## The small instance

The instance the exhaustive search explores (freshness.small.yaml, run by
check-freshness.sh where it has one): the design at sizes small enough to
search every run to a depth. The binding names this module rather than
repeating its constants, and the check scripts vary one switch at a time
with `-const`.

```quint freshness.qnt +=

module small {
  import freshness(REPLICAS = Set("r1", "r2"), GATEWAYS = Set("g1"), RIGHTS = Set("a"), LEASE = 2, MAX_TIME = 4,
    CONFIG_FRESH = true, ANCHOR_EARLIEST = true, FENCE_BOTH = true, PRESERVE_ON_RESTORE = true).*
}
```
