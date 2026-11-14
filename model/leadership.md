# Where golem-control's lease lives: the leadership model

This is a literate Quint specification, like publication.md: the prose
explains, the `quint` blocks are the whole model, and `make` extracts them
into `leadership.qnt`. It answers one question that publication.md leaves
fixed: **where the leadership lease and the publication records live, and
what each placement needs to stay safe** when a leader pauses, a reply is
lost, or a stored object is rolled back (by an attacker who can write the
bucket, or an out-of-band restore).

publication.md checks what reaches gateways; this model abstracts that to
two numbers, the trusted checkpoint (a restarting gateway's floor) and the
last published sequence, and adds what publication.md does not have: the
leader-only jobs that write the state document (the Entra group sync is
the one modelled), and rollbacks of each stored object.

## 1. The designs

Three placements are compared (`DESIGN`):

- `"doc"`, today (ADR 0045): the lease and every pool's publication record
  are fields of the state document. Taking the lease and winning a record
  are each one compare-and-swap of the document.
- `"fence"`, the architect's: the lease moves to a small object of its own;
  the document keeps a *fence* (holder, epoch) and the records. Taking
  leadership is two steps: acquire the lease object at epoch
  E = maxOf(lease epoch, fence epoch) + 1, then *claim* the fence in the
  document, only if its epoch is below E. A replica believes it leads only
  after its claim. Publication and the jobs check the fence.
- `"object"`, the distributed-systems specialist's: the lease and the
  records move together into a lead object; the document keeps a copy of
  the epoch, bumped by a new leader before its first job, for the jobs to
  check. Publication checks the lead object.

And the mechanisms each may need, as switches:

| Switch | Meaning |
|---|---|
| `JOBS` | how a leader-only job's document write is fenced: `"none"` (today's group sync), `"doc"` (checked in the same write against the document's fence or epoch), `"readLease"` (the lease object read first, then the document written: two objects, no transaction) |
| `GUARD` | a trusted checkpoint above the record means the record went back: publish above the checkpoint, never replace it with a lower one (today's `advance` replaces it) |
| `CROSS_CHECK` | a new epoch is one more than the higher of the lease object's and the document's, not the lease object's alone |
| `JOBS_AFTER_CLAIM` | a leader's jobs start (read the directory) only once its epoch is in the document: after the claim in `"fence"`, the bump in `"object"` (in `"fence"` belief begins at the claim, so this holds by construction) |
| `ROLLBACK` | which stored object may be put back to an earlier version, once: `"none"`, `"doc"` or `"lead"` |

```quint leadership.qnt +=
// Generated from leadership.md by lmt. Edit the markdown, not this file.
module leadership {
  const REPLICAS: Set[str]
  const MAX_DIR: int
  const DESIGN: str
  const JOBS: str
  const GUARD: bool
  const CROSS_CHECK: bool
  const JOBS_AFTER_CLAIM: bool
  const ROLLBACK: str
```

## 2. State

The document holds the fence (in `"doc"` the lease itself; in `"object"`
the epoch copy), the record's sequence (unused in `"object"`), and the
group sync's last written value. The lease object holds holder and epoch,
and in `"object"` the record too. Each object's history is kept for the
rollback.

```quint leadership.qnt +=
  type Doc = { holder: str, epoch: int, seq: int, sync: int }
  type Lead = { holder: str, epoch: int, seq: int }

  var doc: Doc
  var lead: Lead
  var docHist: Set[Doc]
  var leadHist: Set[Lead]
  var rolled: bool

  var believes: str -> int   // the epoch a replica acts at; 0: none
  var pending: str -> int    // "fence": acquired, not yet claimed
  var leaseOk: str -> bool   // "readLease": what the replica last read
  var seen: str -> int       // the directory as the replica last read it
  var readAt: str -> int     // the epoch it read it at: a sync writes what it read as that leader
  var dir: int               // the directory (Entra), which only grows

  var checkpoint: int        // the trusted floor
  var pointer: int           // the last published sequence
```

The ghosts: whether the floor ever went down, whether a job ever wrote an
older directory than an accepted job already had (a removed member
re-granted), whether a publication ever went backwards (gateways refuse it:
availability, not safety), and which replica claimed each epoch.

```quint leadership.qnt +=
  var cpBack: bool
  var syncBack: bool
  var seqBack: bool
  var maxSynced: int
  var claims: Set[(int, str)]
```

## 3. Actions

Writes go through two helpers, which keep the history the rollback picks
from.

```quint leadership.qnt +=
  action writeDoc(d: Doc): bool = all { doc' = d, docHist' = docHist.union(Set(d)) }
  action writeLead(l: Lead): bool = all { lead' = l, leadHist' = leadHist.union(Set(l)) }
  action keepDoc = all { doc' = doc, docHist' = docHist }
  action keepLead = all { lead' = lead, leadHist' = leadHist }
  action keepJobs = all { readAt' = readAt, seen' = seen, dir' = dir, leaseOk' = leaseOk, maxSynced' = maxSynced, syncBack' = syncBack }
  action keepPub = all { checkpoint' = checkpoint, pointer' = pointer, cpBack' = cpBack, seqBack' = seqBack }
  action keepLeaders = all { believes' = believes, pending' = pending, claims' = claims }

  val nextEpoch = if (CROSS_CHECK) maxOf(lead.epoch, doc.epoch) + 1 else lead.epoch + 1
  pure def maxOf(a: int, b: int): int = if (a > b) a else b
```

**Taking leadership.** Any replica may take it at any time: when a lease
has lapsed is a matter of clocks, and the model allows the worst. In
`"doc"` it is one write of the document. In `"fence"` it is acquire, then
claim. In `"object"` acquiring is leading; the epoch copy is bumped
separately.

```quint leadership.qnt +=
  action takeDoc(r: str): bool = all {
    DESIGN == "doc",
    val e = doc.epoch + 1
    all {
      writeDoc(doc.with("holder", r).with("epoch", e)),
      believes' = believes.put(r, e), pending' = pending, claims' = claims.union(Set((e, r))),
    },
    keepLead, keepJobs, keepPub, rolled' = rolled,
  }

  action acquire(r: str): bool = all {
    DESIGN != "doc",
    val e = nextEpoch
    all {
      writeLead(lead.with("holder", r).with("epoch", e)),
      if (DESIGN == "fence") all { pending' = pending.put(r, e), believes' = believes, claims' = claims }
      else all { pending' = pending, believes' = believes.put(r, e), claims' = claims.union(Set((e, r))) },
    },
    keepDoc, keepJobs, keepPub, rolled' = rolled,
  }

  action claim(r: str): bool = all {
    DESIGN == "fence",
    pending.get(r) > doc.epoch,
    val e = pending.get(r)
    all {
      writeDoc(doc.with("holder", r).with("epoch", e)),
      believes' = believes.put(r, e), pending' = pending.put(r, 0), claims' = claims.union(Set((e, r))),
    },
    keepLead, keepJobs, keepPub, rolled' = rolled,
  }

  action bump(r: str): bool = all {
    DESIGN == "object",
    believes.get(r) > doc.epoch,
    writeDoc(doc.with("holder", r).with("epoch", believes.get(r))),
    keepLead, keepLeaders, keepJobs, keepPub, rolled' = rolled,
  }
```

**Publishing.** The record is won by a write that checks the replica's
belief against the object that holds authority; then the checkpoint and
pointer follow (one step here: publication.md shows the split safe). The
sequence is the record's plus one, or, under `GUARD`, above the checkpoint
too. Without `GUARD`, a checkpoint above the record is replaced, as
today's `advance` replaces one it takes for not golem-control's.

```quint leadership.qnt +=
  action publish(r: str): bool = all {
    val e = believes.get(r)
    val inDoc = DESIGN != "object"
    val fenced = if (inDoc) doc.holder == r and doc.epoch == e else lead.holder == r and lead.epoch == e
    val base = if (inDoc) doc.seq else lead.seq
    val s = if (GUARD) maxOf(base, checkpoint) + 1 else base + 1
    all {
      e > 0, fenced,
      if (inDoc) all { writeDoc(doc.with("seq", s)), keepLead }
      else all { writeLead(lead.with("seq", s)), keepDoc },
      checkpoint' = s, pointer' = s,
      cpBack' = (cpBack or s < checkpoint),
      seqBack' = (seqBack or s <= pointer),
    },
    keepLeaders, keepJobs, rolled' = rolled,
  }
```

**The group sync.** A replica reads the directory, and later writes what
it read. The directory only grows (a removal is a newer value). Fenced
`"doc"`, the write checks the document's fence in the same write; fenced
`"readLease"`, it checks what the replica read of the lease object earlier.

```quint leadership.qnt +=
  action dirChange: bool = all {
    dir < MAX_DIR, dir' = dir + 1,
    readAt' = readAt, seen' = seen, leaseOk' = leaseOk, maxSynced' = maxSynced, syncBack' = syncBack,
    keepDoc, keepLead, keepLeaders, keepPub, rolled' = rolled,
  }

  action readDir(r: str): bool = all {
    believes.get(r) > 0, not(JOBS_AFTER_CLAIM) or doc.epoch == believes.get(r),
    seen' = seen.put(r, dir), readAt' = readAt.put(r, believes.get(r)),
    dir' = dir, leaseOk' = leaseOk, maxSynced' = maxSynced, syncBack' = syncBack,
    keepDoc, keepLead, keepLeaders, keepPub, rolled' = rolled,
  }

  action readLease(r: str): bool = all {
    JOBS == "readLease",
    leaseOk' = leaseOk.put(r, (lead.holder == r and lead.epoch == believes.get(r))),
    readAt' = readAt, seen' = seen, dir' = dir, maxSynced' = maxSynced, syncBack' = syncBack,
    keepDoc, keepLead, keepLeaders, keepPub, rolled' = rolled,
  }

  action syncWrite(r: str): bool = all {
    val e = believes.get(r)
    val fenced = if (JOBS == "doc") doc.holder == r and doc.epoch == e
      else if (JOBS == "readLease") leaseOk.get(r)
      else true
    all {
      e > 0, fenced, readAt.get(r) == e, seen.get(r) != doc.sync,
      writeDoc(doc.with("sync", seen.get(r))),
      syncBack' = (syncBack or seen.get(r) < maxSynced),
      maxSynced' = maxOf(maxSynced, seen.get(r)),
      readAt' = readAt, seen' = seen, dir' = dir, leaseOk' = leaseOk,
    },
    keepLead, keepLeaders, keepPub, rolled' = rolled,
  }
```

**Rollbacks.** Once in a run, the object `ROLLBACK` names is put back to
any version it has had. A running store refuses to adopt an older version
it has seen, but a replica that starts after the rollback reads it as
current: the model takes the worst.

```quint leadership.qnt +=
  action rollbackDoc: bool = all {
    ROLLBACK == "doc", not(rolled),
    nondet d = docHist.oneOf()
    all { doc' = d, docHist' = docHist },
    rolled' = true, keepLead, keepLeaders, keepJobs, keepPub,
  }

  action rollbackLead: bool = all {
    ROLLBACK == "lead", not(rolled), DESIGN != "doc",
    nondet l = leadHist.oneOf()
    all { lead' = l, leadHist' = leadHist },
    rolled' = true, keepDoc, keepLeaders, keepJobs, keepPub,
  }
```

## 4. The initial state and the step

r1 leads at epoch 1; nothing is published yet.

```quint leadership.qnt +=
  action init = {
    val d = { holder: "r1", epoch: 1, seq: 0, sync: 0 }
    val l = { holder: "r1", epoch: 1, seq: 0 }
    all {
      doc' = d, lead' = l, docHist' = Set(d), leadHist' = Set(l), rolled' = false,
      believes' = REPLICAS.mapBy(r => if (r == "r1") 1 else 0),
      pending' = REPLICAS.mapBy(_ => 0), leaseOk' = REPLICAS.mapBy(_ => false),
      seen' = REPLICAS.mapBy(_ => 0), readAt' = REPLICAS.mapBy(_ => 0), dir' = 0,
      checkpoint' = 0, pointer' = 0,
      cpBack' = false, syncBack' = false, seqBack' = false, maxSynced' = 0,
      claims' = Set((1, "r1")),
    }
  }

  action step = any {
    nondet r = REPLICAS.oneOf()
    any { takeDoc(r), acquire(r), claim(r), bump(r), publish(r), readDir(r), readLease(r), syncWrite(r) },
    dirChange, rollbackDoc, rollbackLead,
  }
```

## 5. Properties

- `floorNeverDrops`: the trusted checkpoint never goes down. If it did, a
  gateway restarting with an empty disk would take the lower floor and
  could accept a replayed old pointer (publication.md's replayPointer).
- `noStaleSync`: no group-sync write lands with an older directory than an
  accepted one: a deposed leader's sync never re-grants a removed member.
  Not judged once the document has been rolled back: that rollback reverts
  the memberships by itself, and its fence with them (what a restore must
  preserve is publication.md's and freshness.md's PRESERVE_ON_RESTORE), so
  under a document rollback this model checks the floor and the epochs.
- `epochsUnique`: no epoch is claimed by two replicas.
- `safe`: all three.
- `forward` (availability, reported, not required): no publication at or
  below the last, which gateways would refuse until a later one.

```quint leadership.qnt +=
  val floorNeverDrops = not(cpBack)
  val noStaleSync = not(syncBack) or (ROLLBACK == "doc" and rolled)
  val epochsUnique = claims.forall(a => claims.forall(b => a._1 == b._1 implies a._2 == b._2))
  val safe = floorNeverDrops and noStaleSync and epochsUnique
  val forward = not(seqBack)
```

Possibility: leadership changes hands, a sync lands, a publication lands
after a takeover.

```quint leadership.qnt +=
  val takenOver = claims.exists(c => c._2 == "r2")
  val synced = maxSynced > 0
  val publishedAfterTakeover = takenOver and pointer > 0
}
```

## 6. What the search shows

`check-leadership.sh` explores every run of the small instance to depth 10
(two replicas, a directory of three values, one rollback), for each design
and each rollback, and shows each mechanism necessary by turning it off:

| Design | Holds, with | Breaks without |
|---|---|---|
| today (`"doc"`) | jobs fenced in the document's write; the checkpoint guard | an unfenced sync (`noStaleSync`, the gap in today's group sync); without the guard, a rolled-back document lowers the floor (`floorNeverDrops`, today's `advance`); a rolled-back document lets an epoch be claimed twice, which a single object cannot prevent |
| the architect's (`"fence"`) | jobs fenced in the document's write; the guard | an unfenced sync, or one fenced by reading the lease object first (`"readLease"`: check then act across two objects); without the guard, a rolled-back document lowers the floor. Without the cross-check nothing breaks: a rolled-back lease object only delays a takeover, since the claim still needs a higher epoch than the fence |
| the specialist's (`"object"`) | jobs fenced by the epoch copy; jobs start only after the bump; the guard | jobs started before the bump let a deposed leader's sync land after the new leader read (`noStaleSync`); a rolled-back lead object lets an epoch be claimed twice even with the cross-check, when the new leader has not bumped the copy yet (`epochsUnique`); without the guard a rolled-back lead object lowers the floor |

## 7. The instances

`small` is the default: the `"fence"` design with every mechanism on and no
rollback. The check script varies the design, the rollback and one switch
at a time with `-const`.

```quint leadership.qnt +=

module small {
  import leadership(REPLICAS = Set("r1", "r2"), MAX_DIR = 2,
    DESIGN = "fence", JOBS = "doc", GUARD = true, CROSS_CHECK = true, JOBS_AFTER_CLAIM = true,
    ROLLBACK = "none").*
}
```
