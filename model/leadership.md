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

Four placements are compared (`DESIGN`):

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
- `"hybrid"`, the specialist's revision: the records stay in the lead
  object, but taking leadership is the architect's two steps, the claim in
  the document, and a replica believes only after it.

And the mechanisms each may need, as switches:

| Switch | Meaning |
|---|---|
| `JOBS` | how a leader-only job's document write is fenced: `"none"` (today's group sync), `"doc"` (checked in the same write against the document's fence or epoch), `"readLease"` (the lease object read first, then the document written: two objects, no transaction) |
| `GUARD` | a trusted checkpoint above the record means the record went back: publish above the checkpoint, never replace it with a lower one (today's `advance` replaces it) |
| `CROSS_CHECK` | a new epoch is one more than the higher of the lease object's and the document's, not the lease object's alone |
| `JOBS_AFTER_CLAIM` | a leader's jobs start (read the directory) only once its epoch is in the document: after the claim in `"fence"`, the bump in `"object"` (in `"fence"` belief begins at the claim, so this holds by construction) |
| `ROLLBACK` | which stored object may be put back to an earlier version, once each: `"none"`, `"doc"`, `"lead"` or `"both"` |
| `VERIFY` | the guard counts the stored checkpoint only if its signature verifies, refuses to publish (and alarms) when it is there unsigned or gone though a record or pointer says something was published, and numbers above the signed pointer too |
| `CP_ATTACK` | someone who can write the bucket but not sign forges an unsigned checkpoint, copies an older signed one back, or deletes it |
| `RESTORE` | the restore procedure: `"preserve"` writes an older document's contents above the current version keeping the fence and records, `"naive"` puts the old document back whole |
| `LOST_REPLY` | a claim, or a record's write, lands but its writer is not told: the claimer re-reads the document (`adopt`); the publisher, seeing a conflict, writes neither checkpoint nor pointer |

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
  const VERIFY: bool
  const CP_ATTACK: bool
  const RESTORE: str
  const LOST_REPLY: bool
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
  var rolledDoc: bool
  var rolledLead: bool
  var restored: bool

  var believes: str -> int   // the epoch a replica acts at; 0: none
  var pending: str -> int    // "fence": acquired, not yet claimed
  var leaseOk: str -> bool   // "readLease": what the replica last read
  var seen: str -> int       // the directory as the replica last read it
  var readAt: str -> int     // the epoch it read it at: a sync writes what it read as that leader
  var dir: int               // the directory (Entra), which only grows

  var checkpoint: int        // the stored checkpoint's sequence
  var cpSigned: bool         // whether it is golem-control's signed one
  var cpHist: Set[int]       // every signed checkpoint written: what a replay copies back
  var pointer: int           // the last published sequence
```

The ghosts: whether the floor ever went down, whether a job ever wrote an
older directory than an accepted job already had (a removed member
re-granted), whether a publication ever went backwards (gateways refuse it:
availability, not safety), and which replica claimed each epoch.

```quint leadership.qnt +=
  var cpBack: bool
  var cpMax: int             // the highest checkpoint golem-control signed
  var cpInflated: bool       // a publication numbered above an unsigned checkpoint
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
  action keepPub = all { checkpoint' = checkpoint, cpSigned' = cpSigned, cpHist' = cpHist, pointer' = pointer,
    cpBack' = cpBack, cpMax' = cpMax, cpInflated' = cpInflated, seqBack' = seqBack }
  action keepRolled = all { rolledDoc' = rolledDoc, rolledLead' = rolledLead, restored' = restored }
  val recordsInDoc = DESIGN == "doc" or DESIGN == "fence"
  val twoStep = DESIGN == "fence" or DESIGN == "hybrid"
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
    keepLead, keepJobs, keepPub, keepRolled,
  }

  action acquire(r: str): bool = all {
    DESIGN != "doc",
    val e = nextEpoch
    all {
      writeLead(lead.with("holder", r).with("epoch", e)),
      if (twoStep) all { pending' = pending.put(r, e), believes' = believes, claims' = claims }
      else all { pending' = pending, believes' = believes.put(r, e), claims' = claims.union(Set((e, r))) },
    },
    keepDoc, keepJobs, keepPub, keepRolled,
  }

  action claim(r: str): bool = all {
    twoStep,
    pending.get(r) > doc.epoch,
    val e = pending.get(r)
    all {
      writeDoc(doc.with("holder", r).with("epoch", e)),
      believes' = believes.put(r, e), pending' = pending.put(r, 0), claims' = claims.union(Set((e, r))),
    },
    keepLead, keepJobs, keepPub, keepRolled,
  }

  // A claim that lands but whose reply is lost: the replica does not know
  // it leads until it reads the document again (adopt).
  action claimLost(r: str): bool = all {
    twoStep, LOST_REPLY,
    pending.get(r) > doc.epoch,
    val e = pending.get(r)
    all {
      writeDoc(doc.with("holder", r).with("epoch", e)),
      believes' = believes, pending' = pending, claims' = claims.union(Set((e, r))),
    },
    keepLead, keepJobs, keepPub, keepRolled,
  }

  action adopt(r: str): bool = all {
    twoStep, LOST_REPLY,
    pending.get(r) > 0, doc.holder == r, doc.epoch == pending.get(r),
    believes' = believes.put(r, pending.get(r)), pending' = pending.put(r, 0), claims' = claims,
    keepDoc, keepLead, keepJobs, keepPub, keepRolled,
  }

  action bump(r: str): bool = all {
    DESIGN == "object",
    believes.get(r) > doc.epoch,
    writeDoc(doc.with("holder", r).with("epoch", believes.get(r))),
    keepLead, keepLeaders, keepJobs, keepPub, keepRolled,
  }
```

**Publishing.** The record is won by a write that checks the replica's
belief against the object that holds authority; then the checkpoint and
pointer follow (one step here: publication.md shows the split safe). The
sequence is the record's plus one, or, under `GUARD`, above the checkpoint
too. Without `GUARD`, a checkpoint above the record is replaced, as
today's `advance` replaces one it takes for not golem-control's.

```quint leadership.qnt +=
  // A publication: the record won under the fence, then the checkpoint and
  // pointer. With lost, the record's write landed but its reply was lost,
  // and the publisher, seeing a conflict, wrote neither.
  action publishAs(r: str, lost: bool): bool = all {
    val e = believes.get(r)
    val fenced = if (recordsInDoc) doc.holder == r and doc.epoch == e else lead.holder == r and lead.epoch == e
    val base = if (recordsInDoc) doc.seq else lead.seq
    // GUARD: number above the stored checkpoint too, counting it only if
    // it verifies (VERIFY); an unverified one is not golem-control's.
    val counted = if (GUARD and (cpSigned or not(VERIFY))) checkpoint else 0
    // ...and above the current signed pointer: an older signed checkpoint
    // copied back beside a rolled-back record is a consistent old pair, and
    // only the pointer still carries the newer number. (An attacker who also
    // copies an old pointer back makes all three agree: no design can tell,
    // and the bucket policy is what stops it; see §6.)
    val s = maxOf(base, maxOf(counted, if (GUARD and VERIFY) pointer else 0)) + 1
    // VERIFY also refuses to publish, and alarms, when the checkpoint is
    // there but does not verify, or is gone though the record or a pointer
    // says a publication was made (the gateway's own rule for a missing
    // checkpoint): either means someone other than golem-control wrote the
    // bucket. Deleting the pointer too is beyond this model's attacker.
    val tampered = VERIFY and (not(cpSigned) and (checkpoint > 0 or base > 0 or pointer > 0))
    all {
      e > 0, fenced, not(lost) or LOST_REPLY, not(tampered),
      if (recordsInDoc) all { writeDoc(doc.with("seq", s)), keepLead }
      else all { writeLead(lead.with("seq", s)), keepDoc },
      if (lost) all { checkpoint' = checkpoint, cpSigned' = cpSigned, cpHist' = cpHist, pointer' = pointer,
                      cpBack' = cpBack, cpMax' = cpMax, seqBack' = seqBack }
      else all {
        checkpoint' = s, cpSigned' = true, cpHist' = cpHist.union(Set(s)), pointer' = s,
        cpBack' = (cpBack or s < cpMax), cpMax' = maxOf(cpMax, s),
        seqBack' = (seqBack or s <= pointer),
      },
      cpInflated' = (cpInflated or (not(cpSigned) and counted > base)),
    },
    keepLeaders, keepJobs, keepRolled,
  }

  action publish(r: str): bool = publishAs(r, false)
  action publishLost(r: str): bool = publishAs(r, true)
```

**The group sync.** A replica reads the directory, and later writes what
it read. The directory only grows (a removal is a newer value). Fenced
`"doc"`, the write checks the document's fence in the same write; fenced
`"readLease"`, it checks what the replica read of the lease object earlier.

```quint leadership.qnt +=
  action dirChange: bool = all {
    dir < MAX_DIR, dir' = dir + 1,
    readAt' = readAt, seen' = seen, leaseOk' = leaseOk, maxSynced' = maxSynced, syncBack' = syncBack,
    keepDoc, keepLead, keepLeaders, keepPub, keepRolled,
  }

  action readDir(r: str): bool = all {
    believes.get(r) > 0, not(JOBS_AFTER_CLAIM) or doc.epoch == believes.get(r),
    seen' = seen.put(r, dir), readAt' = readAt.put(r, believes.get(r)),
    dir' = dir, leaseOk' = leaseOk, maxSynced' = maxSynced, syncBack' = syncBack,
    keepDoc, keepLead, keepLeaders, keepPub, keepRolled,
  }

  action readLease(r: str): bool = all {
    JOBS == "readLease",
    leaseOk' = leaseOk.put(r, (lead.holder == r and lead.epoch == believes.get(r))),
    readAt' = readAt, seen' = seen, dir' = dir, maxSynced' = maxSynced, syncBack' = syncBack,
    keepDoc, keepLead, keepLeaders, keepPub, keepRolled,
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
    keepLead, keepLeaders, keepPub, keepRolled,
  }
```

**Rollbacks.** Once in a run, the object `ROLLBACK` names is put back to
any version it has had. A running store refuses to adopt an older version
it has seen, but a replica that starts after the rollback reads it as
current: the model takes the worst.

```quint leadership.qnt +=
  action rollbackDoc: bool = all {
    ROLLBACK == "doc" or ROLLBACK == "both", not(rolledDoc),
    nondet d = docHist.oneOf()
    all { doc' = d, docHist' = docHist },
    rolledDoc' = true, rolledLead' = rolledLead, restored' = restored, keepLead, keepLeaders, keepJobs, keepPub,
  }

  action rollbackLead: bool = all {
    ROLLBACK == "lead" or ROLLBACK == "both", not(rolledLead), DESIGN != "doc",
    nondet l = leadHist.oneOf()
    all { lead' = l, leadHist' = leadHist },
    rolledLead' = true, rolledDoc' = rolledDoc, restored' = restored, keepDoc, keepLeaders, keepJobs, keepPub,
  }

  // The restore procedure (golemctl state restore): an older document's
  // contents written above the current version, keeping what may not go
  // back (here the fence and the records). "naive" puts the old document
  // back whole, as a rollback does.
  action restore: bool = all {
    RESTORE != "none", not(restored),
    nondet d = docHist.oneOf()
    val back = if (RESTORE == "preserve") d.with("holder", doc.holder).with("epoch", doc.epoch).with("seq", doc.seq) else d
    writeDoc(back),
    restored' = true, rolledDoc' = rolledDoc, rolledLead' = rolledLead, keepLead, keepLeaders, keepJobs, keepPub,
  }

  // The attacker on the checkpoint (an identity that can write the
  // bucket but not sign): an unsigned forgery above the record, an older
  // signed one copied back, or none.
  action forgeCheckpoint: bool = all {
    CP_ATTACK, checkpoint' = pointer + 2, cpSigned' = false,
    cpHist' = cpHist, pointer' = pointer, cpBack' = cpBack, cpMax' = cpMax, cpInflated' = cpInflated, seqBack' = seqBack,
    keepDoc, keepLead, keepLeaders, keepJobs, keepRolled,
  }

  action replayCheckpoint: bool = all {
    CP_ATTACK, cpHist.size() > 0,
    nondet v = cpHist.oneOf()
    all { checkpoint' = v, cpSigned' = true },
    cpHist' = cpHist, pointer' = pointer, cpBack' = cpBack, cpMax' = cpMax, cpInflated' = cpInflated, seqBack' = seqBack,
    keepDoc, keepLead, keepLeaders, keepJobs, keepRolled,
  }

  action deleteCheckpoint: bool = all {
    CP_ATTACK, checkpoint' = 0, cpSigned' = false,
    cpHist' = cpHist, pointer' = pointer, cpBack' = cpBack, cpMax' = cpMax, cpInflated' = cpInflated, seqBack' = seqBack,
    keepDoc, keepLead, keepLeaders, keepJobs, keepRolled,
  }
```

## 4. The initial state and the step

r1 leads at epoch 1; nothing is published yet.

```quint leadership.qnt +=
  action init = {
    val d = { holder: "r1", epoch: 1, seq: 0, sync: 0 }
    val l = { holder: "r1", epoch: 1, seq: 0 }
    all {
      doc' = d, lead' = l, docHist' = Set(d), leadHist' = Set(l),
      rolledDoc' = false, rolledLead' = false, restored' = false,
      believes' = REPLICAS.mapBy(r => if (r == "r1") 1 else 0),
      pending' = REPLICAS.mapBy(_ => 0), leaseOk' = REPLICAS.mapBy(_ => false),
      seen' = REPLICAS.mapBy(_ => 0), readAt' = REPLICAS.mapBy(_ => 0), dir' = 0,
      checkpoint' = 0, cpSigned' = false, cpHist' = Set(), pointer' = 0,
      cpMax' = 0, cpInflated' = false, cpBack' = false, syncBack' = false, seqBack' = false, maxSynced' = 0,
      claims' = Set((1, "r1")),
    }
  }

  action step = any {
    nondet r = REPLICAS.oneOf()
    any { takeDoc(r), acquire(r), claim(r), claimLost(r), adopt(r), bump(r), publish(r), publishLost(r),
          readDir(r), readLease(r), syncWrite(r) },
    dirChange, rollbackDoc, rollbackLead, restore, forgeCheckpoint, replayCheckpoint, deleteCheckpoint,
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
  val noStaleSync = not(syncBack) or rolledDoc or (restored and RESTORE == "naive")
  val epochsUnique = claims.forall(a => claims.forall(b => a._1 == b._1 implies a._2 == b._2))
  val noInflation = not(cpInflated)
  val safe = floorNeverDrops and noStaleSync and epochsUnique and noInflation
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

`check-leadership.sh` explores every run of the small instance (two
replicas, a directory of three values). Each design with its mechanisms is
shown holding to a depth (nine steps, eight with the checkpoint attacker,
whose runs are longer; ten would need more than 4 GiB of stored graph), and
each mechanism necessary by a counterexample found when it is off.

| Design | Holds, with | Breaks without |
|---|---|---|
| today (`"doc"`) | jobs fenced in the document's write; the guard | an unfenced sync (`noStaleSync`: today's group sync); without the guard, a rolled-back document lowers the floor (`floorNeverDrops`: today's `advance`); a rolled-back document lets an epoch be claimed twice, which one object cannot prevent |
| the architect's (`"fence"`) | jobs fenced in the document's write; the verifying guard; under each single rollback, the checkpoint attacker, the restore procedure and lost replies | an unfenced sync, or one fenced by reading the lease object first (check then act across two objects); without the guard, a rolled-back document lowers the floor; a guard that numbers above an unsigned checkpoint is inflated by a forgery (`noInflation`); one that ignores a missing or unsigned checkpoint, or counts only the checkpoint, lowers the floor when a rolled-back record meets a deleted, forged or replayed checkpoint. The cross-check is liveness only: a rolled-back lease object delays a takeover, since the claim still needs an epoch above the fence |
| the specialist's first (`"object"`) | jobs fenced by the epoch copy; jobs only after the bump; the guard; no lead-object rollback | jobs started before the bump let a deposed leader's sync land after the new leader read; a rolled-back lead object lets an epoch be claimed twice even with the cross-check, the copy lagging the acquire |
| the hybrid (`"hybrid"`) | as the architect's, under each single rollback, the checkpoint attacker and lost replies | as the architect's: an unfenced sync, no guard, an unverifying guard |

Both remaining designs reuse an epoch when **both** objects are rolled
back in one run (`ROLLBACK="both"`): with every stored copy of the epochs
put back, nothing durable remains to number above. That, and an attacker
who rolls the record back and copies back an older checkpoint *and* pointer
so all three agree, are beyond any placement: the bucket policy (only
golem-control's role writes `state/`, `control/` and the lease object) is
what prevents them, and the document's versions, replicated under Object
Lock, are what detects and recovers them. Running gateways refuse the
lower publications either way (availability, not safety); only one
restarting with an empty disk would take the lower floor.

In code (control/publish's `floor`), a checkpoint missing beside record 1
and no signed pointer is not tampering: it is a publisher that died between
the first record and the first checkpoint, writes this model makes one
step. From the first checkpoint on, a missing one is refused.

The code's guard is stricter than this model's under a document rollback.
Here `publishAs` numbers past a record below the verified checkpoint or
pointer, which keeps the floor. In code (`prepare` and `below`) such a
record halts the pool's publication (`ErrRegressed`) until a person puts
the newest document back or adopts the older one (`golem-control
bootstrap -adopt`): a record is written before its checkpoint and pointer
and a restore keeps the current records, so a record below them means the
document went back, and numbering past it would publish the older
document's entitlements, revoked keys revived, which this model does not
track (publication.md's PRESERVE_ON_RESTORE is that property). The code's
behaviours are a subset of the model's (a publication the model allows is
refused, none is added), so every property checked here still holds of it;
what is given up is availability after a document rollback, which the
model reports (`forward`) and does not require.

What the model leaves out: write volume, timing, and the lease object's
lapse by clocks (any replica may acquire at any time, which only widens the
behaviours checked).

## 7. The instances

`small` is the default: the `"fence"` design with every mechanism on and no
rollback. The check script varies the design, the rollback and one switch
at a time with `-const`.

```quint leadership.qnt +=

module small {
  import leadership(REPLICAS = Set("r1", "r2"), MAX_DIR = 2,
    DESIGN = "fence", JOBS = "doc", GUARD = true, CROSS_CHECK = true, JOBS_AFTER_CLAIM = true,
    ROLLBACK = "none", VERIFY = true, CP_ATTACK = false, RESTORE = "none", LOST_REPLY = false).*
}
```
