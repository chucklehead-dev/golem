# 0030: Authorization renewal puts the control plane within five minutes of every request

Status: Accepted. Follows from [0029](0029-security-and-correctness-revision.md); amends the SLOs of PRD §9.4. Its accounting-spool part is superseded by [0031](0031-soft-budgets.md). Amended by [0032](0032-authorization-lease-length.md) (four-hour internal lease, SLO back to 99.9%).

## Context

ADR 0029 gives every snapshot a signed authorization lease (five minutes
by default) that only a renewal from fresh authoritative inputs extends.
That bounds revocation under partitions, and it also changes an
availability property the baseline relied on: gateways no longer serve
indefinitely from their last snapshot when the control plane or the bucket
is unavailable. Within five minutes of a control-plane or bucket outage,
every gateway refuses new requests.

## Decision

- **Renewal cadence**: the leader renews every pool's authorization every
  60 seconds from current state and CRDs (a fresh build, not a re-signature
  of old content), so a gateway always holds about four minutes of margin.
- **The lease is counted from the source read, not from signing.** A
  snapshot's `authorization-valid-until` is the time its source state was
  read plus the lease. Fencing cannot stop an old leader whose successor
  has not yet published, or a leader with a slightly stale view; counting
  from the read bounds what such a publication can do. Gateways must also
  check expiry: the publication model (`model/publication.md` §8) shows
  that dropping either half lets a stale publication serve a revoked key
  past the bound.
- **The control plane is on the availability path.** golem-control's SLO
  rises to match the gateway's: 99.95%, two replicas in different zones,
  leader failover within 30 seconds (Kubernetes Lease timings set for it),
  and an alert when any pool's authorization has less than three minutes
  left. The bucket and KMS (signing) are part of the same path and are
  monitored as such.
- **Exceptions**: an internal pool may carry a longer maximum
  authorization age only with recorded security approval (ADR 0029); the
  edge pool may not. The approval states the maximum and the reason
  (for example, an on-prem site with unreliable connectivity to AWS).
- **Gateway SLO** is stated as conditional: 99.95% while golem-control,
  the bucket and KMS are within their own SLOs; outages beyond the lease
  are counted as golem's (`error.origin = gateway`), not hidden.
- **Durable accounting spool** (superseded by ADR 0031: budgets are soft, and no journal is kept; kept here as the record of what was considered) (ADR 0029 gate): the Phase 0 spike compares
  a local write-ahead spool per gateway pod on a small persistent volume,
  shipped to ClickHouse and acknowledged by event ID; synchronous attempt
  records in the site's Valkey with append-only persistence and a replica;
  and batched objects in the bucket. The choice must survive a crash at
  every dispatch, settlement and export boundary.
  The accounting model (`model/accounting.md` §9) settles what any
  candidate must do: make the intent durable **before** dispatch, keep
  uncertain attempts until evidence resolves them, trim only acknowledged
  events, and balance by event identity. A local volume, or Valkey with
  asynchronous replication, can lose durable events with the medium, so it
  qualifies only with synchronous replication or a recorded acceptance
  that attempts in flight on a lost node are recovered only by provider
  reconciliation. The bucket qualifies, at a PUT per intent (or small
  batch) on the request path. The spike measures latency and cost among
  the candidates that qualify.

## Consequences

- Revocation is bounded even for partitioned gateways; the price is that
  golem-control and its dependencies are now as critical as the gateway.
- Remote sites with poor connectivity to us-east-2 are the likeliest
  exception requests; their connectivity (PRD §8) now matters for
  availability as well as residency.

## Alternatives

- Long default leases (hours): the baseline's availability, with
  revocation unbounded under partitions; rejected by 0029.
- A per-site renewal authority: availability without the cross-site
  dependency, but a second signer per site; revisit if a remote site's
  connectivity cannot meet the lease.
