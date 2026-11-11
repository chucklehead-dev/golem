# 0027: PostgreSQL everywhere: Aurora PostgreSQL on AWS, Global Database for the second region

Status: Accepted. Supersedes [0015](0015-sql-store-postgres-and-dsql.md).
Amends 0009, 0016, 0026.

## Context

ADR 0015 wrote one SQL implementation to the intersection of Aurora DSQL
and PostgreSQL, for DSQL's active-active multi-region writes. That subset
costs: optimistic concurrency with retries on every write and
conflict-forcing rows for pool arithmetic; one DDL statement per
transaction and asynchronous index builds (no standard migration tool);
chunked bulk work under the 3,000-row transaction limit; no triggers or
LISTEN/NOTIFY, hence a polled change log with a 300-second lookback;
unindexable JSONB; a nightly CI run against a real DSQL cluster because
Postgres in CI does not prove DSQL behaviour; and a migration, not an
upgrade, from one region to two.

What it buys matters little to golem. Gateways never touch the store and
serve from snapshots through control-plane outages (ADR 0009); writes are
rare (self-service, approvals, overrides: a few per minute, ADR 0026); and
a regional failure that pauses those writes for a failover's minutes
affects almost no one.

## Decision

- **The store is PostgreSQL**, the same engine everywhere: Aurora
  PostgreSQL Serverless v2 on AWS (us-east-2 first), Azure Database for
  PostgreSQL Flexible Server on AKS, CloudNativePG on-prem. CI's Postgres
  containers are representative; there is no second engine to test.
- **Transactions**: every service transaction runs at `SERIALIZABLE` in
  `store.InTx(ctx, fn)`, which retries the whole function on SQLSTATE 40001
  with jittered backoff. Invariants such as pool arithmetic are plain
  read-check-write; serializable isolation, not hand-placed locks or
  conflict rows, keeps them.
- **Migrations**: a standard tool (goose, migrations embedded in the
  binary), transactional DDL, run as a Job before golem-control rolls out.
- **Change detection** (amends 0026): every write transaction increments
  one `store_version` row. golem-control polls it every 500 ms (and, in
  the writer's region, also `LISTEN`s for a `NOTIFY` sent on commit) and
  rebuilds the whole snapshot when it moves. A rebuild every 60 seconds
  compares hashes as a backstop. The `change_log` table and its lookback
  window go. Concurrent writers serialise on that row, which is fine at
  golem's write rate; bulk jobs commit in batches so they do not hold it.
- **Auth**: IAM database authentication on Aurora (a pgx `BeforeConnect`
  hook mints tokens; `MaxConnLifetime` under the token's lifetime); Entra
  authentication on Azure; a Secret-held password on-prem.
- **Second region** (M3): Aurora Global Database, writer in us-east-2, a
  secondary cluster in us-west-2 (read replica, typically under a second
  behind). us-west-2's golem-control reads locally (snapshot builds,
  queries) and writes through the global writer endpoint (or Aurora's
  write forwarding; to confirm in the M3 spike). Failover is managed:
  minutes, during which control-plane writes wait and model traffic is
  unaffected. The witness region and peered clusters go.
- **Habits kept**, because they cost nothing and keep options open: UUIDv7
  ids generated in Go; one retrying transaction wrapper; audit rows
  written by the service in the transaction, not by triggers; anything
  queried is a column, not a JSONB path.

## Consequences

- The store section of the control plane, its migrations and its tests
  are ordinary Postgres; the nightly DSQL job and the DSQL simulation
  trial go.
- Some database operations return: Serverless v2 capacity bounds, minor
  version upgrades (managed), major version upgrades (planned, a few times
  per decade). Cost has a floor of the minimum capacity, small at this
  scale.
- Active-active writes across regions are not available. If they become a
  requirement, the kept habits make DSQL a smaller step than starting from
  plain Postgres would.

## Alternatives

- Aurora DSQL (0015): active-active and no operations, at the costs above.
- DynamoDB: no operations, Global Tables, transactions and Streams; but the
  data is relational (scopes, memberships, role bindings, grants, caps),
  every API listing would need a designed access pattern, and nothing
  equivalent runs on AKS or on-prem, so a second store implementation
  would be needed.
