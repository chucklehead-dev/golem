# 0053: One golem-control replica for M1

Status: Accepted. Supersedes the parts of
[0045](0045-leadership-in-the-state-document.md) and
[0049](0049-lease-object-and-fence.md) that elect a leader among replicas
(the lease, its renewals, its lapse by the taker's clock, `Release`), and
[0030](0030-authorization-renewal-and-availability.md)'s 30-second leader
failover target, for M1. Keeps everything else of 0049. Decided by the
product owner, who judged golem's high availability over-built for one
team's M1 rollout.

## Context

golem-control ran two replicas: one led (it published, shipped the audit
outbox, ran the group sync), the other served the control API and stood
by, taking over within about 40 seconds of a leader's death. Leadership
was two objects: a lease (`lead/lease.json`, renewed every 10 s, judged
lapsed by the taker's clock) and the fence in the state document, which
every leader-only write checks (`state.FencedUpdate`).

Each replica's pod has its own git-sync sidecar, so each has its own
checkout of the config repository, and they move independently. A replica
that takes over can be behind the one it replaces: its first step then
publishes an older config than the gateways hold (a route removed, a
ScopePolicy narrowed or a model taken away comes back) until its git-sync
catches up. Nothing in the publication path can tell an older checkout
from a revert, which is a legitimate change. M1 is one team, one site,
internal pools only; a control-plane outage of minutes costs it little,
while the lease's code, model and timing arguments are a large part of
what there is to review.

## Decision

- **One replica.** The chart runs `replicas: 1` with the `Recreate`
  strategy (the old pod is stopped before the new one starts) and no
  PodDisruptionBudget (with one pod there is no other to keep up).
- **The fence, claimed once.** golem-control claims the state document's
  fence as it starts, at one above its epoch (`state.Store.Claim`), and
  leads from then on. There is no lease object, no renewal and no release
  (`control/lead` is removed).
- **Every leader-only write stays fenced** (`state.FencedUpdate`):
  publication, the audit outbox's trim, the group sync. A predecessor
  still running (its pod paused, or on a node cut off and replaced) is
  refused at its next fenced write; the controller then finds the
  document's fence another's, stops (`controller.ErrDeposed`) and the
  process exits non-zero.
- Kept from 0049 unchanged: the fence in the document and its invariant
  (its epoch never goes back), one fenced publication write a cycle, the
  publication records and the checkpoint guard, lost replies recognised by
  write ID (`RecentWrites`), restore carrying the fence and records
  forward, `ErrMissing` for a deleted document.

## What is lost

- **Sub-minute failover of the control API and the jobs.** While the one
  pod restarts or is rescheduled (a rollout, a node drain, a node loss:
  seconds to a few minutes, longer if the node is lost and Kubernetes
  waits to evict), the control API refuses connections and nothing is
  published.
- Gateways ride on their authorization lease meanwhile: four hours for
  internal pools (ADR 0032), so an outage shorter than that is invisible
  to callers; revocations and config changes wait for golem-control to be
  back. **The edge pool's five-minute lease does not fit this**: before an
  edge pool is published, revisit this decision (two replicas with one
  checkout, or a longer edge lease).
- Every rollout of golem-control is a short control-API outage.
- The lease object was evidence, from a first start on, that golem-control
  had run on a bucket; now the evidence for `ErrMissing` is the pools'
  checkpoints and pointers, which exist from the first publication.

## What it removes

- **The hazard of two checkouts.** A takeover can no longer publish an
  older config checkout than its predecessor's: there is one checkout,
  which follows the branch, and a replacement pod clones the branch's head
  before it starts.
- The lease, its renewals (a bucket write every 10 s), its clock
  reasoning and its tests; the `lead/` prefix in the bucket policy.

## Consequences

- Each start claims a new epoch: the epoch counts golem-control's starts.
- `golem.control.leader` is 1 while the replica runs and has not been
  deposed; 0 from a running process means a successor claimed over it.
- The control API's own writes (the service layer's) were never fenced
  and are not now: they are ordinary compare-and-swap writes, safe from
  any number of writers, as they were with two replicas.
- model/publication.md's connect test drives the claim: `takeLease` is a
  replica's `Claim`, `noticeLoss` its reading another's fence. The
  model's lease-lapse reasoning (model/leadership.md) no longer describes
  the code; its fence placement, which the code keeps, does.
- Revisit for M2 or a second team: two replicas sharing one checkout (a
  config reader that publishes only a revision at least the last
  published one), or the CRD watch, which reads the cluster's one copy.

## Alternatives

- **Keep two replicas, refuse an older checkout**: record the published
  config revision and refuse to publish below it. Needs the revision to
  be ordered (git commit ancestry, not time), and a revert to be a new
  commit; more than M1 needs.
- **Keep two replicas, share one checkout** (a volume both pods mount):
  ReadWriteMany storage, and git-sync's writer is still one of them.
- **Keep the lease, drop git-sync on the standby**: the standby would
  need a checkout the moment it takes over, which is the same race.
