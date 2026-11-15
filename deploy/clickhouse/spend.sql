-- The ledger: golem.spend, the chargeback contract (contracts.md §3,
-- ADRs 0025, 0029, 0038). Rows are immutable events; a row is never edited.
-- lib/contract's test checks these columns against contracts.md §3.

CREATE DATABASE IF NOT EXISTS golem;

CREATE TABLE IF NOT EXISTS golem.spend (
    schema_version        UInt16,
    row_kind              Enum8('attempt' = 1, 'adjustment' = 2),
    request_id            UUID,
    attempt_id            UUID,
    event_id              UUID,
    provider_request_id   String,
    started_at            DateTime64(3, 'UTC'),
    ended_at              DateTime64(3, 'UTC'),
    period                Date,
    site                  LowCardinality(String),
    gateway_pool          LowCardinality(String),
    principal_kind        LowCardinality(String),
    principal_id          String,
    key_id                String,
    scope_id              String,
    scope_path            String,
    funding               Array(Tuple(source String, share Float64)),
    frontend_dialect      LowCardinality(String),
    backend_dialect       LowCardinality(String),
    envelope              LowCardinality(String),
    alias                 LowCardinality(String),
    model                 LowCardinality(String),
    deployment            LowCardinality(String),
    provider_account      LowCardinality(String),
    tokens_input          UInt64,
    tokens_output         UInt64,
    tokens_cache_read     UInt64,
    tokens_cache_write_5m UInt64,
    tokens_cache_write_1h UInt64,
    tokens_reasoning      UInt64,
    units                 Map(String, UInt64),
    cost_micros           Int64,
    currency              FixedString(3),
    price_version         String,
    -- completed, cancelled, terminated, failed or uncertain (contracts.md §3)
    outcome               LowCardinality(String),
    error_origin          LowCardinality(String),
    error_code            LowCardinality(String),
    estimated_fraction    Float32,
    waived                Bool,
    over_budget           Bool
)
ENGINE = MergeTree
PARTITION BY period
ORDER BY (scope_id, principal_id, started_at, event_id);

-- Balances and rollups read this view, never the table: at-least-once
-- delivery writes some events twice, and background merges do not remove
-- them in time (ADR 0029). One row per event_id, whatever the merges. A
-- snapshot's known spend is the same sum, read from golem.spend_by_event
-- below, which is cheaper to read every minute (ADR 0048).
CREATE VIEW IF NOT EXISTS golem.spend_dedup AS
SELECT * FROM golem.spend
ORDER BY event_id, ended_at
LIMIT 1 BY event_id;

-- Known spend's input (ADR 0048): one narrow row per event, filled from
-- golem.spend by the materialized view below as rows arrive. The known-
-- spend job reads it every minute; golem.spend_dedup would sort the whole
-- wide table each time.
--
-- Exact: a duplicate of an event (a retried batch, at-least-once delivery)
-- has the same scope, event_id and period, so the same sorting key in the
-- same partition, and ReplacingMergeTree keeps one. Merges do that
-- eventually; FINAL does it at read time, within the one partition the
-- query names, so a read never counts a duplicate whether or not it has
-- been merged yet. The cost is a read of five narrow columns of one month.
-- Measured on ClickHouse 24.8 (one node, Docker), 5 million rows over 200
-- scopes with 1% delivered twice and unmerged: this table's query 170 ms
-- and 86 MiB, golem.spend_dedup's 1.96 s and 769 MiB, identical results.
-- If that grows too large (hundreds of millions of calls a month), the next
-- step is a per-minute rollup of the minutes old enough to be merged, with
-- FINAL only over the recent ones.
CREATE TABLE IF NOT EXISTS golem.spend_by_event (
    period      Date,
    scope_id    String,
    event_id    UUID,
    ended_at    DateTime64(3, 'UTC'),
    cost_micros Int64
)
ENGINE = ReplacingMergeTree
PARTITION BY period
ORDER BY (scope_id, event_id);

-- Rows inserted before this view existed are not in the table: on a ledger
-- that already has rows, backfill once with
--   INSERT INTO golem.spend_by_event SELECT period, scope_id, event_id, ended_at, cost_micros FROM golem.spend;
CREATE MATERIALIZED VIEW IF NOT EXISTS golem.spend_by_event_mv TO golem.spend_by_event AS
SELECT period, scope_id, event_id, ended_at, cost_micros FROM golem.spend;

-- Ingestion lag's input (ADR 0048): each site's latest ended_at, filled
-- from golem.spend by the materialized view below as rows arrive. Known
-- spend's boundary assumes every call that ended before it is in the
-- ledger; the known-spend job reads this every minute and reports a site
-- whose latest row is more than the allowance old (spend.Job.Lags), whose
-- later rows the boundary would miss. One row per site once merged; the
-- query takes max over the parts not yet merged, so it is exact at read
-- time. The read is of a few rows, whatever the ledger's size.
CREATE TABLE IF NOT EXISTS golem.ingest_by_site (
    site   LowCardinality(String),
    latest SimpleAggregateFunction(max, DateTime64(3, 'UTC'))
)
ENGINE = AggregatingMergeTree
ORDER BY site;

-- Rows inserted before this view existed are not in the table: on a ledger
-- that already has rows, backfill once with
--   INSERT INTO golem.ingest_by_site SELECT site, max(ended_at) FROM golem.spend GROUP BY site;
CREATE MATERIALIZED VIEW IF NOT EXISTS golem.ingest_by_site_mv TO golem.ingest_by_site AS
SELECT site, max(ended_at) AS latest FROM golem.spend GROUP BY site;
