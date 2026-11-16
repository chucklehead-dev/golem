# 0030: A signed authorization lease, renewed every minute; four hours for internal pools

Status: Accepted, consolidated 2026-11-16. Absorbs
[0032](0032-authorization-lease-length.md) (the four-hour internal lease,
jitter, the runway alerts), now a stub pointing here. Follows from
[0029](0029-security-and-correctness-revision.md); amends the SLOs of PRD
§9.4. Leadership (who renews) is [0053](0053-one-golem-control-replica-for-m1.md)'s:
one golem-control replica, so the 30-second leader failover this record
once set is gone for M1. The accounting spool it once proposed was
superseded by [0031](0031-soft-budgets.md). The full history is in git.

## Context

ADR 0029 gives every snapshot a signed authorization lease that only a
renewal from fresh authoritative inputs extends. That bounds how long a
partitioned gateway can serve a revoked right; it also means gateways stop
serving new requests once renewal has been down for a whole lease. A
five-minute lease put the bucket, KMS, golem-control and each remote site's
federation trust on the availability path of every request. Independent
revocation is the per-site emergency denylist (ADR 0041 §1); credential
expiry applies at its encoded time.

## Decision

- **Lease length**: internal pools 4 hours (3 to 4 without an exception;
  longer needs ADR 0029's recorded approval, stating the maximum and the
  reason); the edge pool 5 minutes. golem-control's `-lease` must be at
  most every gateway's `-max-lease`, which the charts set together.
- **Renewal every 60 seconds**, from fresh inputs: a fresh build from the
  current state and config, not a re-signature of old content. The lease
  is the runway for an outage, not the normal staleness; revocation on
  connected gateways takes about two seconds (the write, a step, the
  one-second poll).
- **Counted from the source read**, not from signing: a snapshot's
  `authorization-valid-until` is when its inputs were read plus the lease.
  For M1's git checkout the config's read time is its last fetch, and a
  checkout older than `-max-config-age` renews no lease (ADR 0054).
- **Gateways check expiry** as well as the publication sequence: dropping
  either lets a stale publication serve a revoked key past the bound
  (`model/publication.md`). An expired lease refuses new requests
  (`gateway.authorization_expired`); it ends no call in flight.
- **Fencing and the trusted floor stay** (ADR 0007, 0053): a longer lease
  makes a replayed publication valid for longer, so rollback protection
  matters more.
- **SLOs**: golem-control 99.9%: an outage shorter than the runway is
  invisible to model traffic. The gateway's 99.95% is conditional on
  golem-control, the bucket and KMS being within theirs; outages beyond the
  lease count as golem's (`error.origin = gateway`).
- **Alerts on the runway**: a pool renewed more than 2 minutes ago warns,
  more than 30 minutes pages, under 60 minutes of runway left is an
  incident; each alert names when gateways will start refusing.
- **Jitter**: gateways poll their pointer every second ±20%, backing off
  exponentially with full jitter on bucket errors; JWKS refresh, Entra
  sync and other periodic jobs jitter up to 10% of their period. **No
  jitter on expiry**: it is a security bound, and spreading it would leave
  some gateways serving revoked rights past it; the alerts give hours of
  warning of the cliff.
- **The standby site** (M3) gets the same lease.

## Consequences

- A bucket, KMS, federation or golem-control outage of up to about four
  hours does not interrupt internal model traffic; a partitioned internal
  gateway may serve a revoked right for up to the lease unless the
  emergency denylist is used: the accepted risk.
- The edge pool's five-minute lease does not fit M1's one golem-control
  replica: before an edge pool is published, ADR 0053 must be revisited.

## Alternatives

- Long leases with no renewal requirement: revocation unbounded under
  partitions; rejected by 0029.
- A per-site renewal authority: a second signer per site; revisit if a
  remote site's connectivity cannot meet the lease.
