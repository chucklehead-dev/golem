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
  var cpBack: bool
  var cpMax: int             // the highest checkpoint golem-control signed
  var cpInflated: bool       // a publication numbered above an unsigned checkpoint
  var syncBack: bool
  var seqBack: bool
  var maxSynced: int
  var claims: Set[(int, str)]
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
  val floorNeverDrops = not(cpBack)
  val noStaleSync = not(syncBack) or rolledDoc or (restored and RESTORE == "naive")
  val epochsUnique = claims.forall(a => claims.forall(b => a._1 == b._1 implies a._2 == b._2))
  val noInflation = not(cpInflated)
  val safe = floorNeverDrops and noStaleSync and epochsUnique and noInflation
  val forward = not(seqBack)
  val takenOver = claims.exists(c => c._2 == "r2")
  val synced = maxSynced > 0
  val publishedAfterTakeover = takenOver and pointer > 0
}

module small {
  import leadership(REPLICAS = Set("r1", "r2"), MAX_DIR = 2,
    DESIGN = "fence", JOBS = "doc", GUARD = true, CROSS_CHECK = true, JOBS_AFTER_CLAIM = true,
    ROLLBACK = "none", VERIFY = true, CP_ATTACK = false, RESTORE = "none", LOST_REPLY = false).*
}
