# 0032: A smaller control plane after the move to object storage

Status: Accepted. Amends 0005, 0019, 0020, 0023, 0025.

## Context

ADRs 0028–0031 removed the database, the distribution API and the lease
protocol. Several mechanisms designed around them are now heavier than
they need to be.

## Decision

1. **One golem-control Deployment.** Every replica serves the API; the
   periodic jobs run on the replica holding the jobs lock, an object
   (`locks/jobs`) renewed every 10 s by `If-Match` and taken over when
   stale. The `api`/`worker` role split goes (amends 0025).
2. **No durable job table in M1** (amends 0020). Approvals are already state
   (requests with a state and expiry); the jobs are idempotent scans (expire
   keys, expire requests, roll periods, rebalance ceilings, delete expired
   captures). Notifications carry an idempotency key recorded in the state,
   so a scan repeated after a lock handover does not send twice. The
   `workflow` seam stays for Temporal later.
3. **Registration without heartbeats** (amends 0019). A model server's
   registration has a TTL (24 h) and is renewed by re-registering; whether
   it is up is the gateways' health probes' business, as for every other
   endpoint. No heartbeat store.
4. **CRD validation by schema, not webhook** (amends 0023). Structural and
   cross-field rules are CEL validation rules in the CRDs' OpenAPI schema
   (`x-kubernetes-validations`), enforced by the API server even when
   golem-control is down or not yet installed; semantic errors (a route
   naming a missing backend) are reported in status. No admission webhook,
   so no bootstrap ordering between CRDs and golem-control.
5. **Spend and pool views from ClickHouse only.** The control plane reads
   no site's Valkey; live pool usage is the ledger's (seconds behind).
6. **Valkey is required at every site** (amends 0005). The "no store,
   local shares" mode goes; development uses an in-memory limiter.
7. **Provider quotas split statically per site** in the `Backend` CRD
   (e.g. 70% us-east-2, 30% on-prem). Quotas are per-minute rates, which
   ceilings cannot track; dynamic quota sharing is Later. Within a site,
   gateways share the site's quota in its Valkey.

## Consequences

- One Deployment and one lock instead of two roles and a job table; one
  liveness mechanism for endpoints; CRDs usable before the control plane.

## Alternatives

- Keep the role split for independent scaling: nothing in the envelope
  needs it; it can return as a flag.
