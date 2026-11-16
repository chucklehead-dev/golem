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

-- From the collector (deploy/otel, ADR 0043): the gateway sends each row as
-- an OTLP log record, event golem.spend, whose attributes are golem.spend's
-- columns (contracts.md §3.1). The collector's ClickHouse exporter
-- (opentelemetry-collector-contrib's clickhouseexporter, create_schema
-- off) inserts its own logs shape into golem.spend_otlp below, every
-- attribute as text in LogAttributes, and the views read the columns back
-- into their types. The landing tables are Null: nothing is kept in them,
-- the views see each inserted block.
--
-- golem.spend_otlp has the columns the exporter inserts: a collector
-- version that changes them fails its inserts, and the end-to-end test
-- (test/ledger), which runs the version deploy/otel pins.
CREATE TABLE IF NOT EXISTS golem.spend_otlp (
    Timestamp          DateTime64(9),
    TraceId            String,
    SpanId             String,
    TraceFlags         UInt8,
    SeverityText       LowCardinality(String),
    SeverityNumber     UInt8,
    ServiceName        LowCardinality(String),
    Body               String,
    ResourceSchemaUrl  LowCardinality(String),
    ResourceAttributes Map(LowCardinality(String), String),
    ScopeSchemaUrl     LowCardinality(String),
    ScopeName          String,
    ScopeVersion       LowCardinality(String),
    ScopeAttributes    Map(LowCardinality(String), String),
    LogAttributes      Map(LowCardinality(String), String),
    EventName          String
)
ENGINE = Null;

-- Each record checked: reason is empty for a row golem.spend can hold
-- exactly, else the first column that would not convert. Such a record
-- must not fail its insert: the collector would retry the whole batch,
-- every good row in it, for ever (its queue's retries have no end, ADR
-- 0043). It is kept whole in golem.spend_rejected instead, and alerted on
-- (operations.md, "The ledger's collector").
CREATE TABLE IF NOT EXISTS golem.spend_checked (
    reason        LowCardinality(String),
    Timestamp     DateTime64(9),
    EventName     String,
    LogAttributes Map(LowCardinality(String), String)
)
ENGINE = Null;

CREATE MATERIALIZED VIEW IF NOT EXISTS golem.spend_checked_mv TO golem.spend_checked AS
SELECT
    multiIf(
        EventName != 'golem.spend', 'event_name',
        toUInt16OrNull(LogAttributes['schema_version']) IS NULL, 'schema_version',
        LogAttributes['row_kind'] NOT IN ('attempt', 'adjustment'), 'row_kind',
        toUUIDOrNull(LogAttributes['request_id']) IS NULL, 'request_id',
        toUUIDOrNull(LogAttributes['attempt_id']) IS NULL, 'attempt_id',
        toUUIDOrNull(LogAttributes['event_id']) IS NULL, 'event_id',
        parseDateTime64BestEffortOrNull(LogAttributes['started_at'], 3, 'UTC') IS NULL, 'started_at',
        parseDateTime64BestEffortOrNull(LogAttributes['ended_at'], 3, 'UTC') IS NULL, 'ended_at',
        toDateOrNull(LogAttributes['period']) IS NULL, 'period',
        toInt64OrNull(LogAttributes['cost_micros']) IS NULL, 'cost_micros',
        length(LogAttributes['currency']) != 3, 'currency',
        '') AS reason,
    Timestamp, EventName, LogAttributes
FROM golem.spend_otlp;

-- The rows, each column from its attribute. No conversion here can fail
-- (the OrZero forms), and every row reaching them passed the checks above.
-- funding arrives as a JSON array of {"source", "share"} objects, units as
-- a JSON object of integers: the collector's text for an OTLP list and
-- map. An absent token count is zero.
CREATE MATERIALIZED VIEW IF NOT EXISTS golem.spend_otlp_mv TO golem.spend AS
SELECT
    toUInt16OrZero(a['schema_version'])                                     AS schema_version,
    CAST(if(a['row_kind'] = 'adjustment', 'adjustment', 'attempt') AS Enum8('attempt' = 1, 'adjustment' = 2)) AS row_kind,
    toUUIDOrZero(a['request_id'])                                           AS request_id,
    toUUIDOrZero(a['attempt_id'])                                           AS attempt_id,
    toUUIDOrZero(a['event_id'])                                             AS event_id,
    a['provider_request_id']                                                AS provider_request_id,
    parseDateTime64BestEffortOrZero(a['started_at'], 3, 'UTC')              AS started_at,
    parseDateTime64BestEffortOrZero(a['ended_at'], 3, 'UTC')                AS ended_at,
    toDateOrZero(a['period'])                                               AS period,
    a['site']                                                               AS site,
    a['gateway_pool']                                                       AS gateway_pool,
    a['principal_kind']                                                     AS principal_kind,
    a['principal_id']                                                       AS principal_id,
    a['key_id']                                                             AS key_id,
    a['scope_id']                                                           AS scope_id,
    a['scope_path']                                                         AS scope_path,
    JSONExtract(a['funding'], 'Array(Tuple(source String, share Float64))') AS funding,
    a['frontend_dialect']                                                   AS frontend_dialect,
    a['backend_dialect']                                                    AS backend_dialect,
    a['envelope']                                                           AS envelope,
    a['alias']                                                              AS alias,
    a['model']                                                              AS model,
    a['deployment']                                                         AS deployment,
    a['provider_account']                                                   AS provider_account,
    toUInt64OrZero(a['tokens_input'])                                       AS tokens_input,
    toUInt64OrZero(a['tokens_output'])                                      AS tokens_output,
    toUInt64OrZero(a['tokens_cache_read'])                                  AS tokens_cache_read,
    toUInt64OrZero(a['tokens_cache_write_5m'])                              AS tokens_cache_write_5m,
    toUInt64OrZero(a['tokens_cache_write_1h'])                              AS tokens_cache_write_1h,
    toUInt64OrZero(a['tokens_reasoning'])                                   AS tokens_reasoning,
    JSONExtract(a['units'], 'Map(String, UInt64)')                          AS units,
    toInt64OrZero(a['cost_micros'])                                         AS cost_micros,
    toFixedString(substring(a['currency'], 1, 3), 3)                        AS currency,
    a['price_version']                                                      AS price_version,
    a['outcome']                                                            AS outcome,
    a['error_origin']                                                       AS error_origin,
    a['error_code']                                                         AS error_code,
    toFloat32OrZero(a['estimated_fraction'])                                AS estimated_fraction,
    a['waived'] = 'true'                                                    AS waived,
    a['over_budget'] = 'true'                                               AS over_budget
FROM (SELECT LogAttributes AS a FROM golem.spend_checked WHERE reason = '');

-- Records golem.spend cannot hold, whole, with the first reason found.
-- Empty in a healthy installation: alert on any row, fix the sender, and
-- insert the repaired rows into golem.spend by hand.
CREATE TABLE IF NOT EXISTS golem.spend_rejected (
    received_at   DateTime64(3, 'UTC') DEFAULT now64(3),
    reason        LowCardinality(String),
    Timestamp     DateTime64(9),
    EventName     String,
    LogAttributes Map(LowCardinality(String), String)
)
ENGINE = MergeTree
ORDER BY received_at;

CREATE MATERIALIZED VIEW IF NOT EXISTS golem.spend_rejected_mv TO golem.spend_rejected AS
SELECT reason, Timestamp, EventName, LogAttributes FROM golem.spend_checked WHERE reason != '';
