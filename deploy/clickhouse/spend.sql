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
    outcome               LowCardinality(String),
    error_origin          LowCardinality(String),
    error_code            LowCardinality(String),
    estimated_fraction    Float32,
    waived                Bool,
    counted               Bool
)
ENGINE = MergeTree
PARTITION BY period
ORDER BY (scope_id, principal_id, started_at, event_id);

-- Balances and rollups read this view, never the table: at-least-once
-- delivery writes some events twice, and background merges do not remove
-- them in time (ADR 0029). One row per event_id, whatever the merges.
CREATE VIEW IF NOT EXISTS golem.spend_dedup AS
SELECT * FROM golem.spend
ORDER BY event_id, ended_at
LIMIT 1 BY event_id;

-- Numbering records (ADR 0043): an event's number in its site's counter for
-- the scope and period, written when the limiter numbers it, apart from its
-- row (which never waits for it). golem-control's watermark joins them to
-- golem.spend by event_id. counter_seq 0: the event never reached the
-- counter. Deduplicate by (site, event_id) before use.
CREATE TABLE IF NOT EXISTS golem.spend_seq (
    schema_version        UInt16,
    event_id              UUID,
    site                  LowCardinality(String),
    scope_id              String,
    period                Date,
    counter_seq           UInt64
)
ENGINE = ReplacingMergeTree
PARTITION BY period
ORDER BY (site, scope_id, period, counter_seq, event_id);
