# 0018: Content capture per scope, with override control and per-record retention

Status: Proposed. Amended by [0025](0025-smaller-surfaces.md): no sampling at first.

## Context

Projects (and teams) must be able to turn capture on or off, set
retention, and allow or forbid their keys and services to override.
Users may set it on their own keys where allowed. Retention must be
configurable at key or project level. Langfuse sets retention per Langfuse
project (minimum 3 days) and offers trace deletion by id (batch delete,
not instant) and an organisation API for creating projects.

## Decision

- **Settings** at scope policy (ceiling), team or project (capture: off,
  metadata, content; retention class: 7, 30, 90 or 365 days, 3, 5 or 7
  years; sample; override: locked, narrow, open) and key or service (inherit or override), as in org-model §3.1.
  The gateway computes the effective setting per request from its snapshot.
- **One Langfuse project per capturing scope** (team or project), created
  when a scope first sets capture to metadata or content. Langfuse's own
  access control then matches the scope: its members can see its traces,
  and nobody else's.
- **Retention** is enforced per record: each captured trace carries
  `golem.capture.expires_at`. golem-control's jobs deletes expired traces through
  Langfuse's batch delete API, driven by a ClickHouse query on the ledger
  (which records trace id and expiry). The Langfuse project's own retention
  is set to the scope's maximum allowed retention, as a backstop if the
  worker falls behind.
- **Provisioning** (Langfuse project, its retention, its API keys) is done
  by golem-operator with an organisation-scoped Langfuse key; per-project
  credentials go into one Kubernetes Secret mounted into gateways and the
  worker (keys added in place, no restarts). A scope's capture takes effect
  only once its destination is reported ready.
- **Export**: the gateway sends captured traces (content or metadata) to
  the scope's Langfuse project with a dedicated OTLP/HTTP exporter, since
  the collector cannot pick per-project credentials by attribute. All other
  telemetry still goes through the collector (ADR 0010).
- Redaction hooks the scope policy requires run before export; export
  failure drops the capture (counted), never the request.

- The global ceiling is 7 years; classes beyond one year must be enabled
  in a scope's policy by platform admins.

## Consequences

- Retention can differ per key within a project, and changes to
  retention apply to new records; existing records keep the expiry they
  were written with (stated in the UI).
- Deletion is eventually consistent (Langfuse's batch deletion is not
  instant); the backstop bounds the worst case.
- A Langfuse project per capturing scope: dozens to hundreds of projects,
  which the operator manages; Langfuse membership sync from golem scopes
  is M2.

- Multi-year content in Langfuse means its ClickHouse and blob storage
  grow for years: storage tiering (Langfuse's object storage lifecycle) is
  part of the M2 operations work.

## To verify before M1

Langfuse's organisation API (project creation, retention field and its
entitlement), the batch trace deletion API's limits, and per-project
OTLP ingestion with basic auth, against the Langfuse version in use.

## Alternatives

- One Langfuse project per retention class: Langfuse enforces retention
  with no deletion job, but every team's content shares projects and
  Langfuse access control cannot separate them.
- Content in ClickStack: broader access than content warrants.
