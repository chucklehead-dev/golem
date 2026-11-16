# 0053: One golem-control replica, fenced by the state document

Status: Accepted, consolidated 2026-11-16. This record is golem-control's
leadership as it stands for M1. It absorbs
[0045](0045-leadership-in-the-state-document.md) (leadership in the state
document) and [0049](0049-lease-object-and-fence.md) (the fence, the
checkpoint guard, lost replies), now stubs pointing here; it removes their
lease and [0030](0030-authorization-renewal-and-availability.md)'s
30-second leader failover. Decided by the product owner, who judged
golem's high availability over-built for one team's M1 rollout. Earlier
text is in git history.

## Context

golem-control's leader publishes snapshots, ships the audit outbox and runs
the group sync. Its epoch must fence those writes: a paused leader that
writes after another has taken over must be refused, whether or not the
successor has written yet. Earlier designs ran two replicas, with a lease
(first in the state document, then its own object renewed every 10 s,
judged lapsed by the taker's clock) beside the fence.

Each replica's pod had its own git-sync checkout of the config repository
(ADR 0054), and the two moved independently: a replica that took over
could be behind, and its first step published an older config than the
gateways held until its git-sync caught up. Nothing in the publication path
can tell an older checkout from a revert. M1 is one team, one site, internal
pools only: a control-plane outage of minutes costs it little, while the
lease's code, model and timing arguments were a large part of what there
was to review.

## Decision

- **One replica.** The chart runs `replicas: 1` with the `Recreate`
  strategy and no PodDisruptionBudget.
- **The fence, claimed once.** The state document's `fence` (holder,
  epoch). golem-control claims it as it starts, at one above its epoch
  (`state.Store.Claim`; a claim whose reply was lost is adopted at the next
  try), and leads from then on. No lease object, no renewal, no release.
  The epoch counts golem-control's starts; the store refuses any write that
  moves it back.
- **Every leader-only write is fenced in its own write**
  (`state.FencedUpdate`): publication, the audit outbox's trim, the group
  sync. Checking the fence and then writing would not do: no transaction
  spans two reads. A predecessor still running (its pod paused, or on a
  node cut off and replaced) is refused at its next fenced write; the
  controller then finds the fence another's, stops (`controller.ErrDeposed`)
  and the process exits non-zero.
- **One publication write a cycle**: every due pool's record is won in one
  fenced update, whatever the number of pools.
- **The checkpoint guard**: a publication is numbered above the highest of
  the record, the verified checkpoint and the signed pointer; a record
  below what the bucket proves is refused (`ErrRegressed`), and a checkpoint
  that does not verify, or is missing once written, stops that pool
  (`ErrTampered`) for a person to look at.
- **Lost replies**: a write that lands but whose reply is lost is
  recognised by the write ID it carries, among the document's last 32
  (`RecentWrites`), even after others have written over it.
- **Restore** (`golem-control restore`) carries the fence and the records
  forward, as it does revocations. A document deleted where the bucket
  shows golem-control has run (a pool's checkpoint or pointer) is
  `ErrMissing` until it is put back or `golem-control bootstrap` creates
  one deliberately.
- The control API's own writes are not fenced: they are ordinary
  compare-and-swap writes, safe from any number of writers.

## What is lost

- **Sub-minute failover of the control API and the jobs.** While the pod
  restarts or is rescheduled (seconds to minutes; longer if a node is lost
  and Kubernetes waits to evict), the control API refuses connections and
  nothing is published. Every rollout of golem-control is a short outage.
- Gateways ride on their authorization lease meanwhile: four hours for
  internal pools (ADR 0030), so callers see nothing; revocations and config
  changes wait. **The edge pool's five-minute lease does not fit this**:
  revisit before an edge pool is published.

## What it removes

- The hazard of two checkouts publishing an older config on a takeover.
- The lease, its renewals (a bucket write every 10 s), its clock
  reasoning, its model (model/leadership.md, retired) and its tests; the
  `lead/` prefix in the bucket policy.

## Consequences

- `golem.control.leader` is 1 while the replica runs and has not been
  deposed; 0 from a running process means a successor claimed over it.
- model/publication.md's connect test drives the claim: `takeLease` is a
  replica's `Claim`, `noticeLoss` its reading another's fence.
- The bucket policy lets only golem-control's role write `state/`,
  `control/` and `snapshots/`. Alarms: `ErrTampered`; a fence epoch or a
  record sequence that repeats or goes down in the document's history.
- Revisit for M2 or a second team: two replicas sharing one checkout (a
  config reader that publishes only a revision at least the last
  published one), or the CRD watch, which reads the cluster's one copy.

## Alternatives

- **Two replicas, refusing an older checkout**: needs revisions ordered by
  git ancestry and a revert to be a new commit; more than M1 needs.
- **Two replicas sharing one checkout**: ReadWriteMany storage, one
  git-sync writer.
- **A Kubernetes Lease**: client-go and an API server on the leadership
  path, in tests and standalone; the fence would still be needed.
