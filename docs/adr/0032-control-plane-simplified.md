# 0032: A smaller control plane after the move to object storage

Status: Accepted. Amends 0003, 0005, 0019, 0020, 0023, 0025, 0028.

## Context

ADRs 0028–0031 removed the database, the distribution API and the lease
protocol. Several mechanisms designed around them are now heavier than
they need to be.

## Decision

1. **One golem-control Deployment.** Every replica serves the API; one
   replica, the **leader**, chosen by a Kubernetes Lease (client-go leader
   election; golem-control already has a Kubernetes client), runs the
   periodic jobs and is the only publisher of snapshots (ADR 0033). The
   `api`/`worker` role split goes (amends 0025).
2. **No durable job table and no workflow seam in M1** (amends 0020).
   Approvals are already state (requests with a state and expiry); the jobs
   are idempotent scans (expire keys and requests, roll periods, rebalance
   ceilings, delete expired captures, ship audit). Notifications carry an
   idempotency key recorded in the state, so a scan repeated after a
   leader change does not send twice. Temporal (ADR 0020) arrives with its
   own seam when it is adopted.
3. **Registration without heartbeats** (amends 0019). A model server's
   registration has a TTL (24 h) and is renewed by re-registering; whether
   it is up is the gateways' health probes' business, as for every other
   endpoint. No heartbeat store.
4. **One validation path** (amends 0023). Structural and cross-field rules
   are CEL validation rules in the CRDs' OpenAPI schema
   (`x-kubernetes-validations`), enforced by the API server even when
   golem-control is down or not yet installed. Semantic validation (a route
   naming a missing backend) is the snapshot compiler, a library that
   `golemctl config validate` runs offline in CI on a directory of
   manifests, and golem-control runs on the live set, reporting in status.
   An invalid object does not block the rest: snapshots keep its last
   accepted generation, and its status says so. No admission webhook and no
   CI call to a live API.
5. **Spend and pool views from ClickHouse only.** The control plane reads
   no site's Valkey; live pool usage is the ledger's (seconds behind).
6. **Valkey only, required at every site** (amends 0005). Dragonfly and
   Redis are not supported targets and there is no cluster mode or hash
   tagging; the "no store, local shares" mode goes; development uses an
   in-memory limiter.
7. **Provider quotas split statically per site** in the `Backend` CRD
   (e.g. 70% us-east-2, 30% on-prem). Quotas are per-minute rates, which
   ceilings cannot track; dynamic quota sharing is Later. Within a site,
   gateways share the site's quota in its Valkey.

8. **Audit in the state document** (fixes 0028's "emitted after the
   write"). Each write appends its audit events to an outbox in the same
   document, atomically with the change; the leader ships new events to
   ClickHouse (as OTel logs) and trims them once shipped. No audit event
   can be lost between a write and its log.
9. **The second region is a cold standby** (amends 0028's multi-region).
   One active control plane; us-west-2 gets a replica of the bucket, its
   own gateways reading it, and a golem-control Deployment scaled to zero,
   started by the failover runbook within the 30-minute RTO. No second
   active writer.
10. **Agents use Entra tokens where they can** (amends 0003's login flow).
    `golemctl token` prints a fresh Entra access token (cached until near
    expiry) for agents with a credential helper (Claude Code's
    `apiKeyHelper`, others' equivalents), with the scope in a header; the
    gateway validates it locally. Agents and tools that take only a static
    key (Copilot BYOK, some IDEs) get a minted personal key. No control-plane
    write per login.
11. **One pause lever.** A freeze override pauses config delivery to a
    pool; `GatewayPool.spec.paused` goes.
12. **Lighter local GPU routing in M1** (amends 0019, 0026). Static
    concurrency ceilings per endpoint and a per-principal in-flight limit;
    adaptive concurrency, metrics polling and prefix affinity in M2.

## Consequences

- One Deployment and a leader instead of two roles and a job table; one
  liveness mechanism for endpoints; one validation library; CRDs usable
  before the control plane; audit atomic with the change; one active
  control plane anywhere.

## Alternatives

- Keep the role split for independent scaling: nothing in the envelope
  needs it; it can return as a flag.
