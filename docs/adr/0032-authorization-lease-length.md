# 0032: Internal pools carry a four-hour authorization lease; renewal stays every minute

Status: Accepted. Amends [0030](0030-authorization-renewal-and-availability.md) (lease length, control-plane SLO).

## Context

ADR 0030's five-minute lease put us-east-2's bucket, KMS, golem-control and
each remote site's federation trust (Entra-to-AWS, IAM Roles Anywhere) on
the availability path of every request: any of them failing for five
minutes stopped every site. Independent revocation is the per-site emergency denylist, subject to local
Kubernetes delivery; credential expiry applies at its encoded time (12 hours
for default agent keys). Entra disablement and secret-scanning revocation
require fresh snapshot publication and cannot be relied on during its outage.
ADR 0041 §1 defines denylist survival through Argo reconciliation. golem
will run on one site for a long time, with a second site as warm or cold
standby.

## Decision

- **Lease length**: internal pools 4 hours (configurable from 3 to 4 hours
  without an exception; longer needs ADR 0029's recorded approval); the edge
  pool keeps 5 minutes.
- **Renewal stays every 60 seconds**, from fresh inputs. The lease length
  only matters when renewal stops: it is the runway for an outage, not the
  normal staleness. Revocation latency on connected gateways is unchanged
  (about two seconds).
- **Fencing and the trusted floor stay** (ADR 0007). A longer lease makes a
  stale or replayed publication valid for longer, so the protections that
  stop rollbacks matter more, not less (`model/publication.md`).
- **golem-control's SLO returns to 99.9%.** An outage shorter than the
  runway is invisible to model traffic.
- **Alerts on the runway**: a pool whose authorization was renewed more
  than 2 minutes ago warns; more than 30 minutes pages; under 60 minutes
  of runway left is an incident. Every alert names the time at which
  gateways will start refusing.
- **The standby site** gets the same lease: a primary-site failure leaves
  hours to run the failover runbook (30-minute RTO) before standby
  gateways would stop.

### Periodicity and jitter

- **Polling**: gateways poll their pointer every second with ±20% jitter,
  and back off exponentially with full jitter on bucket errors, so a fleet
  restarted together, or recovering together from a bucket outage, does not
  poll or retry in step.
- **No jitter on expiry.** Expiry is a security bound, not a load event:
  spreading it would leave some gateways serving revoked rights past it.
  The cliff it creates (every gateway of a pool stops at the same moment
  if renewal has been down for the whole lease) is handled by the alerts
  above, which give hours of warning.
- **Renewal and publication** are a single leader's 60-second loop; leader
  election uses client-go's jittered Lease timings.
- **Other periodic work**: JWKS refresh, Entra sync, known-spend refresh
  and the daily jobs run with jitter of up to 10% of their period. Period
  rollover happens at a fixed instant by definition; refused agents that
  retry at that instant are a small burst at this scale, absorbed by rate
  and concurrency limits.

## Consequences

A bucket, KMS or federation outage of up to about four hours does not
interrupt model traffic. A partitioned internal gateway may serve a
revoked right for up to the lease, unless the emergency denylist is used;
that is the accepted risk.
