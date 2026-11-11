# 0018: An edge gateway pool for internet-facing consumers

Amended where specified by [ADR 0029](0029-security-and-correctness-revision.md)
and its normative security and correctness requirements. Baseline text below
must be read with those amendments.

Status: Accepted (v1 baseline), M2. From archive 0022.

## Decision

A separate gateway pool, **edge**, exposed through the mesh's ingress on
its own hostname, serves external consumers: Copilot's enterprise custom
models (ADR 0020) and SaaS apps with configurable model endpoints. Each
consumer is an external Service owned by the scope that pays for it, with
static service keys (two active, for rotation), allowed source ranges
(GitHub's published ranges refreshed by a job), its own models, hard caps,
limits and capture settings (default off), and no pass-through or raw
routes. Its snapshot holds only its consumers' keys (ADR 0007). End-user
identifiers a consumer sends are attributes, not identity, unless signed by
the consumer's own issuer. The ingress adds TLS, WAF rules for the API paths
and first-line rate limits.

## Alternatives

Exposing the internal pool with stricter routes (one mistake from exposing
internal models).
