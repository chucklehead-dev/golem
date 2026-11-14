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
  var cpBack: bool
  var syncBack: bool
  var seqBack: bool
  var maxSynced: int
  var claims: Set[(int, str)]
  action writeDoc(d: Doc): bool = all { doc' = d, docHist' = docHist.union(Set(d)) }
  action writeLead(l: Lead): bool = all { lead' = l, leadHist' = leadHist.union(Set(l)) }
  action keepDoc = all { doc' = doc, docHist' = docHist }
  action keepLead = all { lead' = lead, leadHist' = leadHist }
  action keepJobs = all { readAt' = readAt, seen' = seen, dir' = dir, leaseOk' = leaseOk, maxSynced' = maxSynced, syncBack' = syncBack }
  action keepPub = all { checkpoint' = checkpoint, pointer' = pointer, cpBack' = cpBack, seqBack' = seqBack }
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
  val floorNeverDrops = not(cpBack)
  val noStaleSync = not(syncBack) or (ROLLBACK == "doc" and rolled)
  val epochsUnique = claims.forall(a => claims.forall(b => a._1 == b._1 implies a._2 == b._2))
  val safe = floorNeverDrops and noStaleSync and epochsUnique
  val forward = not(seqBack)
  val takenOver = claims.exists(c => c._2 == "r2")
  val synced = maxSynced > 0
  val publishedAfterTakeover = takenOver and pointer > 0
}

module small {
  import leadership(REPLICAS = Set("r1", "r2"), MAX_DIR = 2,
    DESIGN = "fence", JOBS = "doc", GUARD = true, CROSS_CHECK = true, JOBS_AFTER_CLAIM = true,
    ROLLBACK = "none").*
}
