# 0028: Control-plane state as one document in object storage; snapshots distributed through it

Status: Accepted. Supersedes [0027](0027-aurora-postgresql.md) for the entitlement store. Amended by [0029](0029-one-s3-store-for-every-site.md) (one S3 store at first), [0032](0032-control-plane-simplified.md) (audit outbox, a leader, cold standby) and [0033](0033-integrity-of-the-bucket.md) (signing, publish sequence, restore, locked writes).
entitlement store (PostgreSQL remains the fallback implementation of the
store interface). Amends 0009, 0012, 0016, 0026.

## Context

Within the design envelope (ADR 0026) the whole control-plane state
(scopes, memberships, role bindings, key hashes, grants, caps, requests,
overrides, cost centers, jobs) is a few thousand records: 1–2 MB, a few MB
at ten times the envelope. Writes are a few per minute. Object stores now
offer compare-and-swap: Amazon S3 supports `If-Match` and `If-None-Match`
on PutObject (a lost race gets 412); MinIO and Azure Blob support the
same. A database (ADR 0027) brings a cluster, capacity settings, upgrades,
IAM database authentication, connection pools, migrations and a SQL layer
for data that fits in memory.

The gateway's distribution stream (ADR 0012, 0026) needs a control plane
to be up for a gateway to start or to see a change.

## Decision

### The state document

- golem-control keeps **one state document** per installation: protobuf,
  zstd-compressed, encrypted with SSE-KMS, in a versioned bucket with
  public access blocked and access through a VPC endpoint. It carries a
  monotonic `state_version`.
- Each replica holds the decoded state in memory with its ETag. A write
  applies the change to a copy in Go, checks the org model's invariants,
  increments `state_version`, and does `PutObject` with `If-Match: <etag>`.
  On 412 it re-reads and retries the whole function (`store.Update(ctx,
  fn)`). Every write is serialized against every other: the strongest
  isolation there is, at golem's write rate.
- Reads are in memory: listings, lookups and checks are loops over structs.
- Replicas notice others' writes by a conditional GET (`If-None-Match`) on
  the document every 500 ms; S3 event notifications may shorten that.
- **Schema evolution**: the document carries a schema version; upgrade
  functions run on load, and a replica writes only the newest schema it
  knows. Rolling upgrades read n−1.
- **History and restore**: bucket versioning keeps every prior state;
  restore is copying an old version back (through `golemctl state
  restore`, audited). Old versions expire by lifecycle rule after the
  retention the platform sets.

### What does not go in it

- **Audit events**: OTel log records to ClickHouse, as the ledger
  (ADR 0010), and optionally immutable objects under S3 Object Lock where
  write-once retention is required.
- **High-churn data**: model-server registration heartbeats in Valkey with
  expiry (registrations themselves, which change rarely, are in the
  document); live counters in Valkey; spend in ClickHouse.

### Snapshot distribution through object storage

- After a successful write (or a CRD change), the replica that made it
  compiles each gateway pool's snapshot and writes it **content-addressed**:
  `snapshots/<pool>/<hash>.pb`, then the pool's pointer
  `snapshots/<pool>/current` holding `{state_version, crd_generation, hash}`.
- No compare-and-swap is needed for distribution: snapshot objects are
  immutable, and gateways ignore a pointer whose versions are older than
  the snapshot they hold, so a slow replica overwriting the pointer with an
  older one cannot roll a gateway back (the next write corrects the
  pointer). A store without conditional writes can therefore serve
  distribution.
- Gateways poll their pool's pointer with a conditional GET every second,
  fetch a new snapshot by hash, verify the hash, compile, swap, and keep a
  last-good copy on local disk.
- Gateways report the hash they applied (and compile errors) by writing
  `status/<pool>/<gateway-id>` (their only write permission) and in
  telemetry; golem-control reads the status objects for `Applied n/m` and
  `golemctl gateways`.
- The distribution API (Watch, Ack) goes. Budgets across sites reach
  gateways in their snapshots too (ADR 0031).

### Where it runs

In the initial phase every site shares one S3 bucket in us-east-2 (ADR
0029): EKS through Pod Identity, AKS through Entra-to-AWS federation,
on-prem through IAM Roles Anywhere or other credentials. Later options per
site:

| Target | State document (needs `If-Match`) | Snapshots (plain GET/PUT) |
|---|---|---|
| EKS | S3 in the region, via Pod Identity | the same bucket |
| AKS | Azure Blob, via workload identity | the same store |
| On-prem | Nutanix Objects, if it supports conditional PutObject | Nutanix Objects |

The store interface (`Get`, `PutIfMatch`, `Put`, `List`) is small enough
that each target is a thin adapter, all run against one conformance suite.

### Multi-region

A single writer bucket (us-east-2) with S3 Cross-Region Replication (with
Replication Time Control) to us-west-2. us-west-2's golem-control reads
the replica and writes to the writer bucket; us-west-2's gateways read
their snapshots from the replica. Failover, if us-east-2 is lost, is
promoting the replica bucket to writer (a runbook automated by the
operator); writes made in the replication lag before the loss may be lost.
Model traffic is unaffected throughout.

## Consequences

- No database to run: no cluster, capacity, upgrades, database auth,
  connection pools, migrations, migration Job or SQL. The store's tests
  are in-memory plus the blob conformance suite.
- Gateways depend on object storage, not on the control plane: they start
  and receive changes whenever the bucket is reachable, and a whole region's
  control plane can be down without delaying a revocation already written.
  Revocation latency: the write, plus at most a one-second poll.
- golem-control's replicas need no coordination beyond the CAS; all of
  them can serve reads.
- The ceiling: when the state approaches tens of MB or writes approach
  several per second, this design stops fitting; the dashboards watch
  document size and CAS retries, and the PostgreSQL implementation
  (ADR 0027) is the way out, behind the same interface.
- Ad hoc questions use `golemctl state dump` or DuckDB over an exported
  JSON copy, not SQL against a live database.

## Alternatives

- Aurora PostgreSQL (0027): managed failover and SQL, at the operating
  costs above.
- A Kubernetes object (ConfigMap, CRD) as the document: CAS by
  resourceVersion, but etcd's object size limit (about 1.5 MB) is inside
  the envelope's range.
- Arrow or Parquet: columnar formats for analytics, wrong for a small
  document that changes; right for archiving the ledger, which ClickHouse
  can export to S3.
