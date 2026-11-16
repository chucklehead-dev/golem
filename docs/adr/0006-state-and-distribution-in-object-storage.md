# 0006: Control-plane state as one document in object storage; snapshots distributed through it

Amended where specified by [ADR 0029](0029-security-and-correctness-revision.md)
and its normative security and correctness requirements. Baseline text below
must be read with those amendments.

Status: Accepted (v1 baseline). As built for M1 (control-plane.md §2): JSON, not yet protobuf and zstd, holding the fence, publication records, principals, scopes, keys, revocations, roles and the audit outbox; grants, caps, pools, site ceilings, overrides, registrations and notifications are not in it (from M2, if at all).

## Decision

### State

- One **state document** per installation: protobuf, zstd, SSE-KMS with
  Bucket Keys, in a versioned S3 bucket; a schema version and a monotonic
  `state_version`. It holds principals, scopes, memberships, role
  bindings, cost centers, keys (hashes), grants, caps, adjustment requests,
  pools, site ceilings, overrides, registrations, gateway pools,
  notification idempotency keys, an append-only revocation list and an
  audit outbox. About 1–2 MB within the design envelope (ADR 0021).
- **Writes**: `store.Update(ctx, fn)`: copy the in-memory state, apply
  `fn`, append audit events to the outbox, check invariants, increment
  `state_version`, `PutObject` with `If-Match`; on 412 re-read and retry.
  Every write is serialized. **Reads** are in memory, refreshed by a
  conditional GET every 500 ms.
- **Audit** is atomic with the change (the outbox); the leader ships it to
  ClickHouse and trims it.
- **Schema evolution**: additive protobuf changes need nothing; a breaking
  change takes two releases (readers first, then writers).
- **Fallback**: a PostgreSQL implementation of the same store interface
  (Aurora PostgreSQL on AWS) if the state outgrows the envelope.

### Distribution

- golem-control compiles each gateway pool's **whole snapshot** (the
  config, from a git checkout in M1: ADR 0054; the entitlements its
  audience allows; each scope's known spend) on any change, at the next
  one-second step; signs it; writes
  `snapshots/<pool>/<hash>.pb`; then the signed pointer
  `snapshots/<pool>/current` by CAS (ADR 0007).
- Gateways poll their pointer every second, verify, fetch, compile, swap,
  keep a last-good copy on disk, and write `status/<pool>/<gateway-id>`.
  golem-control reads the status objects for CRD status and
  `golemctl gateways`.
- Gateways never call the control plane: they start, serve and receive
  published changes whenever the bucket is reachable. Revocation takes
  the write plus a one-second poll.

## Consequences

No database, distribution server, change log or delta protocol. The
ceiling: tens of MB of state or several writes per second, watched by
dashboards (document size, CAS retries), with PostgreSQL behind the same
interface as the way out.

## Alternatives

Aurora PostgreSQL (managed failover and SQL, a database to run); Aurora
DSQL (active-active, a restrictive SQL subset); DynamoDB (no relational
queries, nothing equivalent off AWS); a streaming distribution API (the
control plane on every gateway's critical path).
