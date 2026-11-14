# 0045: golem-control's leadership is a lease in the state document

Status: Amended by [0049](0049-lease-object-and-fence.md): the lease is now an
object of its own, and the document keeps a fence. Amends [0003](0003-components-and-layering.md),
[0019](0019-background-jobs.md) and [0030](0030-authorization-renewal-and-availability.md)
(a Kubernetes Lease) and model/publication.md §4.2's description.

## Context

golem-control's leader runs the jobs and publishes snapshots. Its epoch
fences publication: the publication record is won in the same write that
checks the lease's holder and epoch (model/publication.md's FENCE), so a
paused leader that publishes after losing leadership is refused, whether
or not its successor has published. The record is in the
state document (control/state). A Kubernetes Lease would put the epoch in
a second store, written by a second compare-and-swap, behind the API
server, and need client-go in golem-control and an API server in tests and
standalone runs.

## Decision

- The lease is the state document's `leader`: holder, epoch, when it was
  renewed and for how long. A replica takes it when it is free or has
  lapsed (by the taker's clock), incrementing the epoch; its holder renews
  it; the store refuses any write that moves the epoch back
  (`control/lead`).
- A replica believes it leads until it sees another holder, or until its
  own clock passes its last renewal plus the lease's duration, whichever
  is first: it stops before another replica may take the lease by its
  clock. Belief runs from the time it stamped before writing, so a slow
  write only shortens it; a store error within the lease keeps it.
- Defaults: a 15-second lease, renewed every 5 seconds; failover within
  ADR 0030's 30 seconds.
- Clock skew between replicas changes only when a lapsed lease may be
  taken. It cannot let a stale epoch publish over a newer one: the record
  refuses it.

## Consequences

- The publication model's connect test drives real electors: takeLease,
  noticeLoss and publish go through `control/lead` with a clock per
  replica, a paused replica's standing still.
- Leadership costs a state write every 5 seconds per leader, on the
  document's one compare-and-swap; non-leaders' tries are refused before
  writing.
- golem-control needs no Kubernetes client to lead.

## Alternatives

- A Kubernetes Lease (the previous text): the epoch apart from the record
  it fences, and an API server on the leadership path.
