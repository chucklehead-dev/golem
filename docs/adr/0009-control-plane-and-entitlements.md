# 0009: A control plane owns entitlements; the data plane holds a snapshot

Status: Proposed. Amended by [0028](0028-state-and-snapshots-in-object-storage.md): the state is one document in object storage, and gateways poll their snapshots there.

## Context

Entitlements (users, teams, projects, services, keys, grants, allocations,
approvals) change through self-service, need transactions (an approval and
the grant it creates; a key and its audit event), and must reach every data
plane instance quickly (revocation). CRDs are the wrong store: users would
need RBAC to create Kubernetes objects, etcd is not a business database,
and an approval workflow on CRDs is contortion. ClickHouse is available but
is not a transactional store.

## Decision

- A control plane, **golem-control** (APIs) with **golem-worker** (jobs),
  owns entitlements (since ADR 0028, as one document in object storage)
  and serves the self-service and admin API (Entra-authenticated, roles
  per org model §2).
- The gateway holds an immutable snapshot (entitlements and, since ADR
  0012, operator config) behind an atomic pointer, kept current by polling
  the snapshots golem-control publishes (control-plane.md §4). The gateway
  never writes the state and never calls the control plane.
- Behind an interface (`entitle.Source`) so that an external key or
  identity system can be a source instead (PRD §8 Q1).
- golem-control being down stops changes, not traffic: the data plane
  serves from its last snapshot (persisted locally for restarts).

## Consequences

- More deployments to run, and a bucket (ADR 0028).
- Revocation latency is the feed's (about a second), not a cache TTL.
- The snapshot's size grows with keys and members; at tens of thousands
  of keys it is still a few MB per instance.

## Alternatives

- Data plane queries Postgres with a cache: simpler, but revocation is a
  TTL and Postgres is on the failure path of every cache miss.
- CRDs for everything: GitOps-friendly for services, unworkable for
  self-service.
- Store entitlements in the limit store (Dragonfly): no transactions
  across entities, no durable audit.
