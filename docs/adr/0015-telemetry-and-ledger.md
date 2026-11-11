# 0015: Telemetry is OTLP, routed by the collector; the ledger is ClickHouse

Status: Accepted (v1 baseline). From archive 0010.

## Decision

- golem emits only OTLP (traces, metrics, logs) to the enterprise
  collector, with the OTel GenAI and HTTP semantic conventions plus
  `golem.*` attributes (principal, scope, key id, funding, cost, price
  version, site, `usage_estimated`).
- The collector routes everything to ClickStack (HyperDX), and the access
  log records into ClickHouse, where a materialized view writes
  `golem.spend` (ReplacingMergeTree on request id, so at-least-once
  delivery does not double count), with a persistent queue in the
  collector. Audit events from the state document's outbox go the same
  way, to `golem.audit`.
- Captured content does not go through the collector (ADR 0016).
- Grafana dashboards and alerts live in `deploy/grafana`: spend, budgets,
  limits, latency, TTFT, snapshot age per gateway, the synthetic probe per
  pool, CAS retries, document size, and an alert on the absence of golem's
  own metrics.

## Alternatives

Direct ClickHouse inserts from gateways (a second write path in the hot
binary).
