# 0009: A control plane owns entitlements; the data plane holds a snapshot

Status: Proposed

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
  owns entitlements in a SQL store, **Aurora DSQL or PostgreSQL** (ADR
  0015), and serves the self-service and admin API (Entra-authenticated,
  roles per org model §2) and the distribution API.
- The gateway holds an immutable snapshot (entitlements and, since ADR
  0012, operator config) behind an atomic pointer, kept current by the
  distribution stream (control-plane.md §4: a polled change log, since
  DSQL has no LISTEN/NOTIFY). No store access from the gateway.
- Behind an interface (`entitle.Source`) so that an external key or
  identity system can be a source instead (PRD §8 Q1).
- golem-control being down stops changes, not traffic: the data plane
  serves from its last snapshot (persisted locally for restarts).

## Consequences

- Two more deployments and a store to run (DSQL managed, or Postgres via
  CloudNativePG or a managed service).
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
