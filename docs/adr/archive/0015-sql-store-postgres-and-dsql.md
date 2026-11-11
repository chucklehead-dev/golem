# 0015: One SQL store implementation for Aurora DSQL and PostgreSQL

Status: Superseded by [0027](0027-aurora-postgresql.md): PostgreSQL everywhere, Aurora PostgreSQL on AWS.

## Context

The preferred store is Aurora DSQL, for active-active multi-region. golem
must also run on AKS or on-prem Kubernetes, where DSQL is not available.
DSQL is PostgreSQL-compatible with a reduced feature set: optimistic
concurrency only, repeatable read, transaction limits (3,000 rows, 10 MiB,
300 s), no triggers, PL/pgSQL, LISTEN/NOTIFY or temp tables, one DDL per
transaction, `CREATE INDEX ASYNC`, unindexable JSONB, IAM token auth.

## Decision

- One store implementation (`control/store/sql`, pgx) written to the
  intersection of DSQL and PostgreSQL 16+ (control-plane.md §3), with a
  dialect flag only where syntax must differ (async index creation, auth).
- All transactions run through a retrying wrapper on SQLSTATE 40001;
  conflict-forcing version rows protect read-then-write invariants.
- Ids are UUIDv7 made in Go; no ordering relies on sequences.
- No database feature does work the service layer should: audit and change
  log are written by the service in the transaction.
- CI runs the store suite on PostgreSQL per change and on DSQL nightly.

## Consequences

- Portable across EKS, AKS and on-prem with no second implementation.
- Some PostgreSQL conveniences (triggers, LISTEN/NOTIFY, advisory locks,
  `SELECT FOR UPDATE` that blocks) are off limits even on PostgreSQL.
- Bulk jobs are chunked.

## Alternatives

- PostgreSQL only, with logical replication for multi-region: an
  active-passive design and more operations.
- A store interface with separate DSQL and Postgres implementations: twice
  the SQL to keep correct.
- DynamoDB or CockroachDB: no PostgreSQL compatibility for on-prem
  (Cockroach would work everywhere but is another database to adopt).
