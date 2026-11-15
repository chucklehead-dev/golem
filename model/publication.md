# How a revocation reaches every gateway: the publication model

This is a literate Quint specification. The prose explains the design; the
`quint` blocks, read top to bottom, are the whole model. `make` (in this
directory) extracts them into `publication.qnt` with
[lmt](https://github.com/driusan/lmt) and checks them. You can read it
without running anything, and you should be able to understand golem's
publication design from it in one sitting.

It models the decisions in [ADR 0006](../docs/adr/0006-state-and-distribution-in-object-storage.md)
(state and snapshots in object storage), [ADR 0007](../docs/adr/0007-bucket-root-of-trust.md)
(the bucket as the root of trust), and their revision in
[ADR 0029](../docs/adr/0029-security-and-correctness-revision.md) and
[ADR 0030](../docs/adr/0030-authorization-renewal-and-availability.md).

## 1. The question

A user's key is revoked. golem-control writes the revocation into the
state document, and every gateway must stop accepting that key. Nothing
calls the gateways: they poll a signed pointer in the bucket every second
and apply whatever snapshot it names. The question this model answers is:

> After a revocation is acknowledged, how long can any gateway, anywhere,
> keep serving the revoked key, and what has to be true for that bound to
> hold?

The answer the design gives, and the model checks, is: **less than one
authorization lease (`LEASE`) after the revocation**, whatever happens to
leaders, networks, gateway disks or the bucket's contents, provided five
mechanisms are in place. Each mechanism is a switch in the model (§3), so
you can turn one off and watch the bound break.

## 2. Reading and running

- `make` extracts the model and runs `check.sh`: a type check, then a
  random simulation of the correct design against every property (which
  must find nothing), then a simulation of each broken design against the
  property it should break (which must find a counterexample).
- `quint run --main=design --invariant=safe publication.qnt` runs the
  correct design by hand; `--main=noFencing` and the other instances in §10
  run the broken ones; `quint test --main=leaseFromSigning publication.qnt`
  replays a bug's story (§8).
- Simulation samples behaviours; it is not a proof. The sizes are small
  (two replicas, two gateways, two keys, a short clock) because the
  interesting interleavings are already there. Each model also has a
  smaller instance (`<model>.small.yaml`) that the check script explores
  exhaustively with gotogether's state-graph search: every run up to a
  depth, the design holding in all of them and each broken design's
  shortest counterexample found. That is a proof for that instance and
  depth, and still not for every size. `check.sh` uses a fixed
  seed so CI is deterministic; `SEED=random SAMPLES=50000 ./check.sh`
  searches wider. It uses Quint's TypeScript backend by default
  (`QUINT_BACKEND=rust` for the faster one, where it is installed).

The model starts here:

```quint publication.qnt +=
// Generated from publication.md by lmt. Edit the markdown, not this file.
module publication {
```

## 3. The world, and the design switches

There are **replicas** of golem-control (one of which leads at a time),
**gateways** (in one pool, for simplicity), and **keys**. Time is a
counter; `LEASE` is the authorization lease (in production four hours for
internal pools and five minutes for the edge pool, ADR 0032; a few ticks
here) and `MAX_TIME` bounds a run. The length of the lease does not change
which mechanisms are needed, only how long a stale publication could do
harm without them, which is why a longer lease keeps every one of them.

```quint publication.qnt +=
  const REPLICAS: Set[str]
  const GATEWAYS: Set[str]
  const KEYS: Set[str]
  const LEASE: int
  const MAX_TIME: int
```

The five mechanisms the bound depends on, each a switch so that the broken
design can be run too:

| Switch | When on (the design) | When off (the bug it prevents) |
|---|---|---|
| `FENCE` | A publisher may publish only if its leadership epoch is not older than the publication record's, and its view of the state is not older than the record's source | A paused old leader wakes up and publishes stale content over newer content |
| `LEASE_FROM_SOURCE` | A snapshot's authorization lasts `LEASE` from when its **source state was read** | The lease runs from when the snapshot was **signed**, so stale content gets fresh validity |
| `TRUSTED_FLOOR` | A gateway restarting with an empty disk takes its rollback floor from the authoritative publication record | It trusts its own status object, which anyone holding the pool's credentials can overwrite |
| `PRESERVE_ON_RESTORE` | Restoring an old state keeps every revocation made since | A restore brings revoked keys back |
| `CHECK_EXPIRY` | A gateway refuses requests once its snapshot's authorization has expired | A partitioned gateway serves its last snapshot forever |

```quint publication.qnt +=
  const FENCE: bool
  const LEASE_FROM_SOURCE: bool
  const TRUSTED_FLOOR: bool
  const PRESERVE_ON_RESTORE: bool
  const CHECK_EXPIRY: bool
```

## 4. State

### 4.1 The authoritative state

The state document is reduced to what this question needs: the set of
revoked keys and a version that increases on every write. `history` keeps
every version's revoked set, as bucket versioning does, so that a restore
has something to restore.

```quint publication.qnt +=
  var clock: int
  var revoked: Set[str]
  var stateVersion: int
  var history: List[Set[str]]
```

### 4.2 Leadership

Here the Lease is one atomic value: who leads, at which epoch. In golem it
is the state document's fence, claimed by a replica that has first
acquired the lease object (ADR 0049); how the lease object, the claim and
rollbacks of either fit together is model/leadership.md's question, and
this model takes its answer as given.

Leadership is one holder and an epoch that increases
when it changes hands. The crucial detail is `believes`: what each replica
*thinks* its epoch is. A replica that was paused (a long GC, a frozen node)
still believes it leads after the Lease has moved on. It finds out only
when it next looks. Every stale-leader bug lives in that gap.

Each replica also has a `view`: its in-memory copy of the state, with the
time it was read.

```quint publication.qnt +=
  type View = { version: int, revoked: Set[str], loadedAt: int }

  var leaseHolder: str
  var leaseEpoch: int
  var believes: str -> int
  var views: str -> View
```

### 4.3 Publication

A **publication** is a signed pointer to a snapshot. In the bucket the
pointer and the snapshot are two objects bound by a hash; here they are one
record. It carries a publication sequence number, the publisher's epoch,
the state version it was built from (`source`), the snapshot's content
(the revoked keys) and the end of its authorization (`validUntil`).

The **publication record** is the authoritative "latest publication",
updated by compare-and-swap; a publisher must win it to publish.
`published` is every pointer ever signed: the bucket keeps old versions, and
anyone who can write the bucket (but not sign) can copy one back.

```quint publication.qnt +=
  type Pub = { seq: int, epoch: int, source: int, revoked: Set[str], validUntil: int }
  type PubRecord = { epoch: int, seq: int, source: int }

  var record: PubRecord
  var pointer: Pub
  var published: Set[Pub]
```

### 4.4 Gateways

Each gateway has the publication it **applied**, a **floor** (it never
applies a sequence below it), whether it can reach the bucket, and a ghost
`checkpoint` (§4.5). `status` is each gateway's status object: diagnostic
output that the gateway writes, and that anyone with the pool's credentials
can overwrite.

```quint publication.qnt +=
  type Gw = { applied: Pub, floor: int, connected: bool, checkpoint: int }

  var gws: str -> Gw
  var status: str -> int
```

### 4.5 Ghosts: what the properties remember

Some properties are about history ("a key revoked at time t is never
served after t + LEASE"), so the model keeps a little memory that no
implementation would: when each key was revoked, which keys were ever
revoked, the authoritative record's sequence at a gateway's last restart
(`checkpoint`), whether a restore has happened, whether a replica published
while an older epoch than the Lease's (`staleWin`), and flags set the moment
something bad happens. Checking
on arrival keeps the properties cheap: no property re-scans history.

```quint publication.qnt +=
  var revokedAt: str -> int
  var everRevoked: Set[str]
  var badServe: bool
  var expiredServe: bool
  var rollback: bool
  var belowCheckpoint: bool
  var outOfOrder: bool
  var restoredOnce: bool
  var staleWin: bool
```

## 5. The initial state

One replica holds the Lease at epoch 1 and has published the genesis
snapshot (nothing revoked, valid for one lease). Every gateway has applied
it. `blank` is what a gateway holds after an empty-disk restart: nothing
it may serve from.

```quint publication.qnt +=
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
```

The first leader is `r1`. The replicas are interchangeable, so choosing
it loses no behaviour (every run that starts with `r2` is the same run with
the names swapped), and it means a recorded run of the real gateway
replays from `init` without a search for which replica led first
(gateway/snapshot's Hegel test, publication_hegel_test.go).

Every action below changes a few variables and leaves the rest alone.
These helpers say "the rest stays as it is" for each group, so each action
shows only what it changes.

```quint publication.qnt +=
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
```

## 6. Actions

### 6.1 Revoking a key

An administrator, a manager or the secret-scanning receiver revokes a key.
golem-control writes it into the state document (by compare-and-swap,
modelled in the state-document model, not here); the write is
acknowledged; from this moment the clock on the bound starts.

```quint publication.qnt +=
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
```

### 6.2 Restoring an old state

`golemctl state restore` writes an old version's content as a **new**
version. With `PRESERVE_ON_RESTORE` it keeps every revocation the current
state has; without it, the old content comes back as it was, revoked keys
and all.

```quint publication.qnt +=
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
```

### 6.3 A replica reads the state

Each replica refreshes its in-memory view with a conditional GET. Between
two reads its view is stale; how stale it may be when it publishes is
exactly what `LEASE_FROM_SOURCE` limits.

```quint publication.qnt +=
  action loadState(replica: str): bool = all {
    views' = views.put(replica, { version: stateVersion, revoked: revoked, loadedAt: clock }),
    clock' = clock, keepState, keepLeadership, keepPublication, keepGateways, keepFlags,
  }
```

### 6.4 Leadership changes hands

Another replica takes the Lease, with the next epoch. The old leader is not
told: it still believes it leads until it notices (`noticeLoss`). In
between it may still try to publish, and §6.5 is what stops it.

```quint publication.qnt +=
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
```

### 6.5 Publishing: the heart of the design

A replica that believes it leads builds a snapshot from its view and tries
to win the publication record. Two rules make this safe:

- **Fencing** (`FENCE`): the record is won in the same write that checks
  the Lease (ADR 0045: the Lease and the record are one document), so only
  the replica holding the Lease, at its current epoch, can win it, and the
  record remembers the epoch and source of the last publication, so a
  view older than the record's source cannot win it either. A paused old
  leader is refused whether or not its successor has published. Pointer
  compare-and-swap alone is not enough: a stale leader that re-reads the
  pointer's ETag wins it happily, with stale content.
- **Lease from source** (`LEASE_FROM_SOURCE`): the snapshot's authorization
  ends `LEASE` after its **source** was read, not after it was signed.
  Fencing cannot stop a leader whose own view is stale (it read the state,
  then a revocation was written, then it published); this rule bounds what
  such a publication can do: whatever it carries, it stops counting `LEASE`
  after the state it reflects.

Publishing is also renewal: the leader publishes every 60 seconds (ADR
0030) even when nothing changed, which extends the lease from a fresh read.
In the bucket, winning the record and writing the pointer are two writes;
here they are one step, because the record is the linearization point and
a pointer left behind by a crash is replaced by the next publication.

```quint publication.qnt +=
  action publish(replica: str): bool = all {
    believes.get(replica) > 0,
    val e = believes.get(replica)
    val v = views.get(replica)
    val fenced = replica == leaseHolder and e == leaseEpoch
      and e >= record.epoch and v.version >= record.source
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
```

### 6.6 The attacker

Two kinds of attacker are in scope. Someone who can **write the bucket but
not sign** (a mistaken role, a compromised operator laptop) can copy any
old signed pointer back over the current one. Someone holding **one pool's
gateway credentials** can overwrite that pool's status objects. Neither
can produce a new signature: the signing key never leaves KMS.

```quint publication.qnt +=
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
```

### 6.7 A gateway polls

A connected gateway reads the pointer and applies it only if the signature
verifies (always, here: forged signatures are out of scope), its sequence
is at or above the gateway's floor and above what it holds, and, with
`CHECK_EXPIRY`, its authorization has not ended. It then raises its floor
and writes its status object.

The two flags record the bugs this guards against the moment they happen:
`rollback` (a gateway moving to older content without a restart) and
`belowCheckpoint` (a restarted gateway accepting a publication older than
the authoritative record was when it restarted).

```quint publication.qnt +=
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
```

### 6.8 A gateway restarts with an empty disk

The pod is replaced and its local last-good copy is gone. It must start
from nothing it can serve (`blank`) and with a floor. With `TRUSTED_FLOOR`
the floor is the authoritative record's sequence; without it, the gateway
reads its own status object, which the attacker of §6.6 may have reset to
zero, opening the door to replayed old pointers. The ghost `checkpoint`
remembers what the trusted floor was, so §7 can tell the difference.

```quint publication.qnt +=
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
```

### 6.9 Partitions

A gateway loses or regains its path to the bucket. While cut off it keeps
what it applied, and can serve from it only while the authorization lasts.

```quint publication.qnt +=
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
```

### 6.10 Serving a request

A gateway serves a request with a key if the key is not revoked in the
snapshot it holds, it holds one, and (with `CHECK_EXPIRY`) the snapshot's
authorization has not ended. This is where the bound is checked: serving a
key at or after its revocation time plus `LEASE` sets `badServe`.

```quint publication.qnt +=
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
```

### 6.11 Time passes

```quint publication.qnt +=
  action tick: bool = all {
    clock < MAX_TIME,
    clock' = clock + 1,
    views' = views, keepState, keepLeadership, keepPublication, keepGateways, keepFlags,
  }
```

### 6.12 The step

Any of the above, with every parameter picked under its own name (the
convention that lets gotogether later drive or judge the real code with
this model).

```quint publication.qnt +=
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
```

## 7. Properties

### 7.1 Safety: what must never happen

**The bound.** No gateway serves a key at or after its revocation time plus
one lease. This is the property the whole design exists for.

```quint publication.qnt +=
  val revocationBounded = not(badServe)
```

**No serving on expired authorization.** A gateway never serves from a
snapshot whose authorization has ended, connected or not.

```quint publication.qnt +=
  val noExpiredServe = not(expiredServe)
```

**No rollback without a restart.** A gateway that has not restarted never
moves to a publication built from older state than the one it holds.

```quint publication.qnt +=
  val noRollback = not(rollback)
```

**No rollback below the trusted checkpoint.** A gateway that restarted
with an empty disk never accepts a publication older than the authoritative
record was at its restart.

```quint publication.qnt +=
  val noRollbackBelowCheckpoint = not(belowCheckpoint)
```

**Publications are ordered.** A publication with a higher sequence never
carries older state than the one before it.

```quint publication.qnt +=
  val publicationsOrdered = not(outOfOrder)
```

**A restore never revives a key.** Every key ever revoked is still
revoked.

```quint publication.qnt +=
  val revocationsSurviveRestore = everRevoked.subseteq(revoked)
```

All of them together, for the correct design:

```quint publication.qnt +=
  val safe = and {
    revocationBounded,
    noExpiredServe,
    noRollback,
    noRollbackBelowCheckpoint,
    publicationsOrdered,
    revocationsSurviveRestore,
  }
```

### 7.2 Possibility: what the model must be able to do

A model that forbids too much proves nothing: if revocations never reached
gateways, the bound would hold vacuously. Each scenario below is a state
the correct design must be able to reach; `check.sh` asks the simulator to
find one (by checking the negation as an invariant, which must fail).

- A revocation reaches a gateway: some gateway holds a snapshot in which a
  revoked key is revoked.
- A partitioned gateway refuses requests because its authorization has
  ended.
- A restore happens and the revoked keys stay revoked.

```quint publication.qnt +=
  val revocationReachesGateway = GATEWAYS.exists(g =>
    revoked.exists(k => gws.get(g).applied.revoked.contains(k)))

  // Never reached in the design (the record's write checks the Lease);
  // reached without FENCE.
  val staleLeaderPublished = staleWin

  val partitionedAndExpired = GATEWAYS.exists(g =>
    not(gws.get(g).connected) and gws.get(g).applied.seq >= 0
      and clock >= gws.get(g).applied.validUntil)

  val restoredWithRevocations = restoredOnce and everRevoked.size() > 0
```


## 8. The bugs, as stories

The simulator finds each broken design's counterexample, but a random
trace is hard to read. Here each bug is told as a short scenario, step by
step. Each story **passes** in its broken design (it reaches the bad state)
and **fails** in the correct one (one of its steps is impossible), and
`check.sh` checks both. Which story passes in which design:

| Story | design | noFencing | leaseFromSigning | statusFloor | naiveRestore | noExpiry |
|---|---|---|---|---|---|---|
| a stale view revives a key | impossible | impossible | **passes** | impossible | impossible | **passes** |
| paused leader rolls back | impossible | **passes** | impossible | impossible | impossible | impossible |
| restore brings a key back | impossible | impossible | impossible | impossible | **passes** | impossible |

The stale-view story passes in two broken designs, and that is the
lesson in it: the bound needs *both* halves of the lease, its end counted
from the source read and the gateway checking it. Either one missing lets a
stale publication through. (The status-floor bug has no story here: its
counterexample needs an empty-disk restart, a forged status object and a
replayed pointer, which the simulator finds in §10.) Reading them is the quickest way to see why each
mechanism is there.

**A stale view revives a revoked key** (`leaseFromSigning`). The leader
r1 reads the state, then k1 is revoked. Three ticks later r1, which still
holds the Lease, publishes its old view (its refresh failed, or it was
slow). Fencing lets it through: it leads, and no newer publication exists,
so everything rests on the lease: in the correct design the snapshot's
authorization ended at r1's read time plus `LEASE`, so no gateway accepts
it; with the lease counted from signing, g1 accepts it and serves k1 a
full lease after the revocation.

```quint publication.qnt +=
  run staleViewTest =
    initWith("r1")
      .then(loadState("r1"))
      .then(revoke("k1"))
      .then(tick).then(tick).then(tick)
      .then(publish("r1"))
      .then(poll("g1"))
      .then(serve("g1", "k1"))
      .expect(not(revocationBounded))
```

**A paused leader rolls a gateway back** (`noFencing`). k1 is revoked; the
new leader r2 publishes it and g1 applies it. Then the old leader r1 wakes
with its pre-revocation view and publishes. With fencing, the record's
newer epoch stops it; without, it wins the next sequence number and g1
moves backwards to content in which k1 is valid again. (In the design r1
no longer holds the Lease, and the record's write refuses it.) (Here the lease
still bounds the damage: the rollback is what fencing prevents.)

```quint publication.qnt +=
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
```

**A restore brings a key back** (`naiveRestore`). k1 is revoked, then an
administrator restores version 0, from before the revocation. Without
preservation k1 is valid in the restored state, and every renewal keeps it
valid: three ticks later, long past the bound, g1 serves it.

```quint publication.qnt +=
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
```

The model ends here:

```quint publication.qnt +=
}
```

## 9. What the model leaves out, deliberately

- **Clock skew.** One clock for everyone. In production the bound is
  `LEASE` plus the allowed skew; the gateway also rejects pointers issued
  more than the skew in its future (`-clock-skew`, two minutes) and
  snapshots whose `validUntil` is further ahead than the longest lease plus
  the skew (`-max-lease`), so a publisher's clock running ahead cannot
  stretch the lease (`Bucket.openPointer`, `checkLease`;
  `TestClockBounds`).
- **Forged signatures.** Out of scope: the signing key never leaves KMS.
  The attacker can only replay what was genuinely signed.
- **Old publication records.** Replaying an old *record* lowers a
  restarting gateway's floor, but anything it then accepts is still bounded
  by `validUntil`, so the bound holds; the model keeps the record out of the
  attacker's reach to keep the state small.
- **The state document's own races.** Concurrent writers and
  compare-and-swap on the state document are another model's subject. Here
  a revocation is one atomic, acknowledged step.
- **The emergency denylist.** It can only remove rights, so it can only
  shorten the bound.
- **Several pools and audiences.** One pool is enough for this question;
  the edge pool's audience filter is a property of snapshot building.

## 10. The designs to check

The correct design and one broken design per switch. Each broken design
should violate the property next to it, and only because of its switch:

| Instance | Switch off | Should break |
|---|---|---|
| `design` | none | nothing (`safe` holds) |
| `noFencing` | `FENCE` | `publicationsOrdered` and `noRollback` |
| `leaseFromSigning` | `LEASE_FROM_SOURCE` | `revocationBounded` |
| `statusFloor` | `TRUSTED_FLOOR` | `noRollbackBelowCheckpoint` |
| `naiveRestore` | `PRESERVE_ON_RESTORE` | `revocationsSurviveRestore` and `revocationBounded` |
| `noExpiry` | `CHECK_EXPIRY` | `noExpiredServe` and `revocationBounded` |

```quint publication.qnt +=

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
```

## 11. From model to code

When the publisher and the gateway's poller exist, this model becomes their
judge rather than only their design (testing.md §3): the actions are named
and their parameters picked under their own names, so gotogether can drive
the real code with the model's traces, run it in simulation with the same
faults (paused leaders, partitions, empty disks, replayed objects), and
validate recorded runs against `safe`. The broken designs become planted
mutants of the code, which the checks must catch.

## The small instance

The instance the exhaustive search explores (publication.small.yaml, run by
check-publication.sh where it has one): the design at sizes small enough to
search every run to a depth. The binding names this module rather than
repeating its constants, and the check scripts vary one switch at a time
with `-const`.

```quint publication.qnt +=

module small {
  import publication(REPLICAS = Set("r1", "r2"), GATEWAYS = Set("g1"), KEYS = Set("k1"), LEASE = 2, MAX_TIME = 4,
    FENCE = true, LEASE_FROM_SOURCE = true, TRUSTED_FLOOR = true, PRESERVE_ON_RESTORE = true, CHECK_EXPIRY = true).*
}
```
