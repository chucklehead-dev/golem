# 0010: OTLP out, routed to ClickStack, the ClickHouse ledger and Langfuse

Status: Proposed

## Context

The enterprise runs ClickHouse, ClickStack (ClickHouse, HyperDX, its OTel
collector), Langfuse and Grafana. golem must feed operational telemetry,
an authoritative spend ledger, and LLM-specific tracing (sessions,
content, evaluations) without three bespoke exporters on the request path.

## Decision

- golem emits only OTLP (traces, metrics, logs) to the collector, using
  the GenAI and HTTP semantic conventions plus `golem.*` attributes
  (principal, scope, key id, funding, cost, price version).
- The collector routes:
  - everything to ClickStack (HyperDX search, incidents);
  - the access-log records into ClickHouse, where a materialized view
    extracts typed columns into `golem.spend` (ReplacingMergeTree on
    request id, so at-least-once delivery does not double count). The
    collector uses a persistent queue so a ClickHouse outage delays
    records rather than losing them;
  - GenAI spans to Langfuse's OTLP endpoint, with `langfuse.*` attributes
    (user, session, tags, metadata) and golem's computed cost, so Langfuse
    and the ledger agree. Prompt and completion content is attached only
    where the scope's policy allows, and only on the Langfuse route.
- Grafana dashboards and alert rules live in `deploy/grafana`, over the
  ClickHouse data source (spend, budgets, errors, latency, TTFT) and
  metrics.
- golem-control reads spend from ClickHouse; it never writes it.

## Consequences

- No database client in the data plane for telemetry; one exporter.
- The ledger depends on the collector pipeline: its persistent queue and
  ClickHouse's dedup are part of golem's correctness, and are tested.
- Content never lands in ClickStack, which has broader access than
  Langfuse.

## Alternatives

- Data plane inserts into ClickHouse directly (async inserts): fewer moving
  parts for the ledger, but a second write path and a ClickHouse client in
  the hot binary.
- Langfuse SDK-style ingestion API: richer objects, but a bespoke exporter;
  OTLP is supported by Langfuse and keeps golem vendor-neutral.
