# 0049: The lease in its own object, the fence in the document

Status: Accepted. Amends [0045](0045-leadership-in-the-state-document.md)
(the lease in the state document) and
[0030](0030-authorization-renewal-and-availability.md) (failover timing).
Decided with model/leadership.md, which compared four placements by
exhaustive search, and two reviews.

## Context

ADR 0045 kept the lease in the state document, so the lease's epoch and the
publication record it fences were written by one compare-and-swap. The
cost: every renewal (every 5 s) and every pool's publication (every 60 s)
rewrote the whole document, which grows with the organisation, and each
version is kept and replicated under Object Lock: about 17,000 renewal
versions a day, plus 1,440 a pool. The reviews also found two gaps in the
code as it stood: the Entra group sync wrote the document unfenced, and
the publisher replaced a checkpoint above its record, so a rolled-back
record lowered the trusted floor.

## Decision

- **The lease** is an object of its own, `lead/lease.json` (holder,
  epoch, duration), written by compare-and-swap: 30 s, renewed every
  10 s, a failed try retried in 2 s with jitter. It holds no authority.
  A taker judges it lapsed by seeing it unchanged for its duration on its
  own clock (as client-go does); a holder stops believing when its own
  clock passes its last renewal plus the duration. `Release` on SIGTERM
  marks it released, so a rollout loses no time.
- **The fence** is the document's `fence` (holder, epoch). Taking
  leadership is two steps: acquire the lease at one above the higher of
  the lease's epoch and the fence's, then claim the fence, only if its
  epoch is below that. A replica leads only once it has claimed.
- **Every leader-only document write checks the fence in its own
  compare-and-swap** (`state.FencedUpdate`): publication, the group
  sync, and the period and expiry jobs as they come. Checking the lease
  object first and then writing the document is not enough (the model's
  `readLease` counterexample): no transaction spans two objects.
- **Publication** wins every pool's record in one fenced update per
  cycle: one document version a minute, whatever the number of pools.
- **The checkpoint guard**: a publication is numbered above the highest
  of the record, the verified checkpoint and the signed pointer; a
  checkpoint that does not verify, or is missing once one was written,
  stops publication (`ErrTampered`) for a person to look at.
- **Lost replies**: a write that lands but whose reply is lost is
  recognised by the write ID it carries (the store re-reads after a
  failed condition), kept among the document's last 32 write IDs so it is
  recognised even after other writers have written over it; a claim
  whose reply was lost is adopted at the next try.
- **Restore** (`golemctl state restore`, when built) carries the fence and
  the records forward, as it does revocations.

## What the model shows

Every run of the small instance to nine steps (eight with the checkpoint
attacker): the design holds under a rollback of either object, a forged,
replayed or deleted checkpoint, the restore procedure and lost replies;
each mechanism above is necessary (a counterexample without it). Two
things no placement prevents: a rollback of both objects at once (an epoch
is reused), and a record rolled back together with an older checkpoint and
pointer copied back (all three agree). The bucket policy prevents those,
and the document's Object Lock history is how they are detected and
recovered from. Running gateways refuse lower publications either way;
only one restarting with an empty disk would take the lower floor.

## Consequences

- Renewals rewrite a few hundred bytes, not the document; the document
  gains about 1,440 versions a day.
- Failover: a leader that dies is replaced within about the lease's
  duration once a taker has watched it (30 to 40 s), against ADR 0030's
  30 s target; with the edge pool's five-minute authorization lease that
  leaves over three minutes, above the alert.
- The bucket policy must let only golem-control's role write `state/`,
  `lead/`, `control/` and `snapshots/`; `lead/` needs a noncurrent-version
  expiry and is left out of the backup replication (it holds nothing to
  recover).
- Alarms: `ErrTampered`; in the document's history, a fence epoch or a
  record sequence that repeats or goes down.
- Revisit if the document passes about 10 MB: give the records an object
  of their own, also under Object Lock; not an unlocked one, which would
  lose the evidence the alarms read.

## Alternatives

- **Keep the lease in the document, renew less often** (60 s / 20 s):
  fewer versions, but still one per renewal, and the per-pool record
  writes untouched; and a 60 s lease is too slow for the edge pool.
- **The lease and the records in a lead object, an epoch copy in the
  document** (the model's `"object"`): a rolled-back lead object lets an
  epoch be claimed twice, and jobs must not start before the copy is
  bumped.
- **The records in the lead object, belief after the claim** (`"hybrid"`):
  as safe as this design in the model, with fewer document versions, but
  the records' history would live in an object without Object Lock, the
  one check left after a rollback the policy failed to prevent.
- **A Kubernetes Lease**: rejected in ADR 0045 (client-go, an API server
  in standalone and tests); the fence would still be needed.
