# 0026: Designed for the enterprise's size, with the envelope stated

Status: Proposed. Amends 0009, 0012, 0024, 0025. Amended by [0027](0027-aurora-postgresql.md) and [0028](0028-state-and-snapshots-in-object-storage.md): change detection and delivery through object storage.

## Context

The enterprise is small: under 2,000 users, with usage bi- or tri-modal:
most users light, under 100 heavy (coding agents most of the day), under 10
very heavy (parallel agents, automation). The organisation has fewer than
10 top-level teams, each with 3–10 sub-teams, and 4–10 projects per team.
Several mechanisms in the design were sized for a general product.

## The envelope

| Quantity | Estimate | Designed for (10× headroom) |
|---|---|---|
| Users | < 2,000 | 20,000 |
| Teams | ≤ 10 top-level, ≤ 100 sub-teams | 1,000 |
| Projects | ≤ 1,000 (10 per sub-team, worst case) | 10,000 |
| Keys | ~ 5,000 (personal and service) | 50,000 |
| Concurrent streams, peak | ~ 1,000 (100 heavy × 3, 10 very heavy × 50, the light majority ~100) | 10,000 |
| Requests per second, peak | ~ 50 | 500 |
| Ledger rows per day | ~ 1–2 million | 20 million |
| Snapshot size | < 2 MB uncompressed | 20 MB |

Golem's cost driver is the heavy tail, not the average: under 10 users can
be most of the spend and most of the local GPU demand.

## Decision

1. **Whole snapshots, no deltas.** golem-control rebuilds the whole
   snapshot when the store (ADR 0027: its version row) or a CRD changes (debounced to once per
   second) and streams it compressed (~ 0.5 MB) to each gateway, which
   swaps it in. No delta protocol, no idempotent upserts, no per-object
   versions; the distribution model to check is "the latest snapshot
   reaches every gateway". Revisit beyond the envelope's snapshot size.
2. **Fixed gateway replicas.** Three per region (one per zone) for
   availability, sized for the envelope with headroom; no HPA or KEDA. A
   gateway of 2 vCPU and 2 GiB holds thousands of streams.
3. **Small stores.** Valkey: one primary and one replica (ElastiCache,
   smallest memory-optimised node); no cluster mode. The store and ClickHouse
   are far inside their comfort zones.
4. **Sub-teams are scopes with a parent** (amends 0024): a team scope may
   have a parent team (depth at most 2: top-level team, sub-team). A parent
   grants to its sub-teams as a team grants to projects, without approval;
   managers of a parent are managers of its sub-teams unless set otherwise.
   Enforcement still charges one pool per request (0008): grants move money
   when they are made.
5. **Fair share on scarce capacity.** Local GPU machines are few and the
   very heavy users are many times the rest: on capped pools (ADR 0019)
   golem queues per principal and serves round-robin across principals, and
   each principal's in-flight share of a pool is limited (default: half of
   its capacity), so one user's agents cannot occupy every Spark.
6. **Capture cost is dominated by coding agents.** A heavy agent session
   sends 50–200k tokens of context per request; content capture at
   ~ 2,000 requests per heavy user per day is tens of GB per day before
   retention. The default for keys minted by `golemctl agent setup` is
   `metadata`, and the console shows the projected storage of a capture
   setting before it is saved.

## Consequences

- The distribution, autoscaling and store sections shrink; their
  complexity returns only if the envelope is exceeded, which the
  dashboards watch (snapshot size, streams, request rate).
- Fairness is part of M1, because the local machines are.

## Alternatives

- Keep the general design: correct at any size, paid for in M1.
