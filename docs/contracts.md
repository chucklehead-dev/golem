# golem: contracts

Review amendment: [ADR 0029](adr/0029-security-and-correctness-revision.md)
and [security-and-correctness.md](security-and-correctness.md) govern the
affected guarantees and release gates. What is built: the code's
[orientation](walkthroughs/README.md) and the "Built" notes; release
evidence is tracked in golem#7.

Status: v1 baseline. The interfaces other people and systems will depend
on, written down before the first caller does: refusals and their status
codes, error origin, the ledger, the price model, identifiers and headers,
and provider-side retention. Decisions: ADRs 0024–0027.

## 1. Refusals and status codes (ADR 0024)

Every SDK golem serves (OpenAI's and Anthropic's, and the agents built on
them) retries 429, 408, 409 and 5xx automatically with backoff. A refusal
that retrying cannot fix must therefore not use those codes, or clients
retry pointlessly and users wait through backoff before seeing why.

| Situation | Status | Golem code (examples) | Retryable |
|---|---|---|---|
| Rate or concurrency limit; provider quota share full | 429 + `Retry-After` | `limit.rate`, `limit.concurrency`, `limit.provider_quota` | yes, after the delay |
| Too many requests, or failed authentications, from one address (before authentication) | 429 + `Retry-After` | `limit.source` | yes, after the delay (a failed key or token: fix it first) |
| Budget or cap reached by known spend (in `enforce` mode) | 402 | `budget.pool_exhausted`, `budget.member_cap`, `budget.key_cap` | no |
| Not authenticated (missing, unknown, expired, revoked key or token; a key of the other installation) | 401 | `auth.missing`, `auth.key_unknown`, `auth.key_expired`, `auth.key_revoked`, `auth.key_wrong_installation`, `auth.token_invalid`, `auth.token_expired` | no |
| Authenticated, not allowed (model, scope, dialect, capture policy, provider storage); a standalone gateway reached from a web page or by a non-loopback host name | 403 | `authz.model_not_allowed`, `authz.scope_not_member`, `authz.provider_storage_forbidden`, `authz.origin_forbidden`, `authz.feature_forbidden` | no |
| A credential or caller on the site's emergency denylist | 403 | `auth.denied` | no |
| A call in flight whose authorization is withdrawn (a snapshot applied revokes its key, or removes its principal from the key's scope or from golem; the denylist gains its key or principal): terminated within seconds. Not the authorization lease's expiry, which stops new requests only | 401 or 403 while the provider's headers are awaited; once streaming, an error event in the stream; a whole response already begun is cut | `auth.key_revoked`, `authz.scope_not_member`, `auth.denied` (the refusal a new request would get) | no |
| A malformed request (not one JSON object, duplicate members, a routing or policy field of the wrong type, such as a `store` that is not a boolean, unreadable) or one over the size limit; one over its scope's ceilings (output tokens asked for, input size) | 400 or 413 | `request.invalid`, `request.unreadable`, `request.too_large`, `request.over_ceiling` | no |
| Request golem cannot route (no deployment with the capabilities used, unknown alias, frontend that cannot reach the model) | 400 or 404 | `route.unknown_model`, `route.no_capable_deployment`, `route.wrong_frontend` | no |
| A feature the translation cannot express | 400 | `translate.unsupported_feature` | no |
| Every deployment failed before the first byte (a stream: before its first event, an error as that event included) | 503 + `Retry-After` | `upstream.unavailable` | yes |
| Upstream failure after the first byte | an error event in the stream | `upstream.stream_failed` | the client decides |
| The gateway's authorization lease has expired (ADR 0030) | 503 + `Retry-After` | `gateway.authorization_expired` | yes, once renewed |
| The gateway has no configuration yet, cannot check Entra tokens yet (their signing keys not yet fetched; golem keys are served), or is shutting down (another of the pool's gateways takes the retry) | 503 + `Retry-After` | `gateway.not_ready` | yes |
| Golem's own fault | 500 | `gateway.internal` | yes |

- The body is the caller's dialect's error shape (OpenAI's `error` object,
  Anthropic's `type: error`), with golem's `code`, a message that says what
  to do next and who can change it (the scope's managers, a budget
  manager), and the request ID.
- A scope's budget mode is its ScopePolicy's `budgetMode` (PRD §9.1, ADR
  0050), `observe` unless set. `warn` lets a request over the budget
  through and adds `golem-warning: budget.pool_exhausted 104%` (the code
  `enforce` would have refused with, and the share of the budget known
  spend has reached, rounded down); `observe` lets it through and adds
  nothing to the response. Both mark its ledger row `over_budget` (§3).
  In-flight limits (`limit.concurrency`) refuse in every mode.
- The codes are a stable, documented list: this table, defined as code in
  `lib/contract` (the gateway can send no other), and checked against the
  table by a test in both directions. The ledger's DDL
  (`deploy/clickhouse/spend.sql`) is checked against §3 the same way.
- The SDKs' retry rules are pinned by the SDK compatibility tests (§7):
  `test/compat` checks every status above against the official Python and
  TypeScript SDKs in CI (docs/spikes/sdk-compat.md).
- A terminated stream surfaces in every SDK as an error raised after the
  text that came before it, carrying golem's code (Python's
  `anthropic.APIStatusError` and `openai.APIError`, TypeScript's
  `APIError` for both), never as a short whole answer
  (`TestSDKsSeeTermination`).

## 2. Error origin (ADR 0024)

Every failed or degraded request carries `golem.error.origin`:

| Origin | Meaning |
|---|---|
| `client` | The caller's request or credentials (4xx golem returned for the caller's reasons) |
| `policy` | golem refused by rule (budget, limit, authorization) |
| `provider` | The provider or model server failed or refused (its 4xx/5xx, timeouts, cut streams) |
| `gateway` | golem itself failed |

It is an attribute on the span, the access log and the ledger row, and a
label on the error metrics. SLOs (PRD §9.4) count `gateway` errors against
golem and `provider` errors against the provider; ejection and failover
act only on `provider`; dashboards and support start from it.

## 3. The ledger (ADR 0025)

`golem.spend` in ClickHouse is the chargeback contract. Immutable events per upstream attempt, with distinct estimate, settlement
and adjustment identities (ADR 0029); rows are never edited. Deduplicate
by event identity before sums and rollups; background merges alone do not
prevent double counting.

| Column | Type | Notes |
|---|---|---|
| `schema_version` | UInt16 | this table's contract version |
| `row_kind` | Enum: `attempt`, `adjustment` | attempt: one per upstream attempt, at its end (`estimated_fraction` > 0 when a cut stream was counted from deltas); adjustment: from provider reconciliation (ADR 0031) |
| `request_id`, `attempt_id`, `event_id` | UUID | logical request, upstream attempt, stable accounting-event identity (ADR 0029). All three are UUIDs in canonical lowercase text. A call's first attempt's ID is its request ID (a UUIDv7, §5); attempt n > 1's is the name-based UUIDv5 (RFC 9562 §5.5) whose namespace is the request ID and whose name is n in decimal (`uuid5(request_id, "2")`), derivable again from the request ID. An attempt row's `event_id` is its `attempt_id`: each attempt its own event, so deduplication never merges a call's attempts (model/attempts.md) |
| `provider_request_id` | String | the provider's own ID, for disputes |
| `started_at`, `ended_at` | DateTime64(3, UTC) | |
| `period` | Date | the **start** time's period in the period time zone: a stream crossing a period boundary is charged to the period it started in |
| `site`, `gateway_pool` | LowCardinality(String) | |
| `principal_kind`, `principal_id`, `key_id` | | user or service; key prefix and id, never the key |
| `scope_id`, `scope_path` | | `scope_path` is the team/sub-team/project chain at request time |
| `funding` | Array(Tuple(source, share)) | the scope's funding sources and shares at request time; on the wire (§3.1) a list of `{source, share}` maps. Empty until scopes carry funding |
| `frontend_dialect`, `backend_dialect`, `envelope` | LowCardinality(String) | |
| `alias`, `model`, `deployment`, `provider_account` | | `model` as the provider names it (inference profile ID on Bedrock); `provider_account` the Backend's `spec.providerAccount` (the provider's organisation, workspace, project or AWS account ID the call is billed to), the Backend's name if it sets none |
| `tokens_input`, `tokens_output`, `tokens_cache_read`, `tokens_cache_write_5m`, `tokens_cache_write_1h`, `tokens_reasoning` | UInt64 | cache writes by lifetime: Anthropic's `cache_creation` breakdown (`ephemeral_1h_input_tokens`; the rest of `cache_creation_input_tokens` five-minute writes); a provider without one reports five-minute writes only |
| `units` | Map(String, UInt64) | other billed units: `web_search`, `code_execution_seconds`, `images`; on the wire a map of integers. Empty until a dialect reads them |
| `cost_micros` | Int64 | in the configured currency's millionths; computed by golem |
| `currency` | FixedString(3) | |
| `price_version` | String | the `PriceList` version used: `<name>/<spec.version>`, or `<name>@sha256:<16 hex digits of its spec's digest>` for a list without one (§4) |
| `outcome`, `error_origin`, `error_code` | | `completed`, `cancelled` (the caller went away), `terminated` (the gateway ended the call in flight, its authorization withdrawn, §1: charged as a cancelled call is, what was produced, its slot held for the provider's tail; the code is the refusal's), `failed`, or `uncertain` (the call failed after the provider may have received it: charged its worst case as an estimate until reconciliation, golem#4); the failure's §2 origin and §1 code |
| `estimated_fraction` | Float32 | the share of `cost_micros` estimated rather than reported: 0 for actual rows. When a provider's report is missing (a stream cut, a response without usage; never for an error response, which no provider bills), each side is charged by rules chosen to err high, not an average: output every byte of the response received a token (whatever its fields or framing), at most the request's `max_tokens × n`; input, if not reported, every byte of the request a token (as the declared worst case; an estimate, not a bound on real spend: ADR 0044). Reconciliation corrects it (ADR 0031) |
| `waived` | Bool | admitted outside the budget's bound: while the site store was unreachable, or in the grace after a recovery (ADR 0041 §7.3); the exposure the bound does not cover |
| `over_budget` | Bool | admitted over its scope's budget because the scope's budget mode is `observe` or `warn` (§1, ADR 0050): a call `enforce` would have refused |

- Known spend (ADR 0048): a snapshot carries, per scope and period, a
  boundary B (a minute, an ingestion allowance before the snapshot's
  time) and the sum of `cost_micros` over `golem.spend_dedup`'s rows of
  that scope and period with `ended_at` before B, deduplicated by
  `event_id`. The site counter holds the same calls' costs by the minute
  of their end, so a call is counted on one side of B only. A row that
  arrives later than the allowance is counted from the next snapshot
  whose boundary passes it, and by reconciliation; golem-control reports
  each site whose latest ingested `ended_at` is more than the allowance
  old (`golem.ingest_by_site`, `spend.Job.Lags`).
- The period time zone: a snapshot states the publisher's as
  `periodZone` (an IANA name, golem-control's `-period-zone`); its known
  spend's period is a calendar month there. A gateway whose own
  `-period-zone` differs refuses the snapshot, naming both, so that its
  admission and its rows' `period` agree with known spend's. Empty (a
  standalone snapshot, an older publisher): not checked.
- Rounding: per row, cost is computed in micro-units from integer token
  counts and prices in micro-units per million tokens, rounded half-even
  once, at the end.
- Adjustment rows (from provider reconciliation, ADR 0012) carry the
  period, account and model they correct and a reason.
- Adjustments are attributed where the provider's usage data allows, else
  booked to an `unattributed` principal under the provider account, so
  totals match invoices even when a crash lost a few attempts' rows
  (ADR 0031).
- Balances and rollups are computed from a view deduplicated by
  `event_id`; no query sums the raw table (ADR 0029).
- Schema changes are additive within a version; a breaking change is a new
  table version with a view keeping the old shape for one release.

### 3.1 From the gateway to the table

- **The record.** Each row leaves the gateway as one OTLP log record
  whose event name (and body) is `golem.spend`, whose timestamp is
  `ended_at`, and whose attributes are the table's columns, named as
  they are, in the DDL's order (`gateway/telemetry/ledger.go`, checked
  against `deploy/clickhouse/spend.sql`). Integers, floats and booleans
  are OTLP's; IDs, times (RFC 3339, UTC), the period's first day and
  enums are strings; `funding` is a list of `{source, share}` maps and
  `units` a map of integers. Standalone, the same attributes are a JSON
  line on stdout.
- **The collector** (`deploy/otel/collector.yaml`, operations.md, "The
  ledger's collector") keeps `golem.spend` records in a pipeline of their
  own, queues them on node-local disk (ADR 0043) and writes them with the
  contrib ClickHouse exporter into `golem.spend_otlp`, a Null table of the
  exporter's shape (every attribute as text).
- **The views** (`spend.sql`) read each record's columns back into their
  types and insert the row into `golem.spend`, whose own views then fill
  `golem.spend_by_event` and `golem.ingest_by_site`. A record that would
  not convert (an ID that is not a UUID, an unparseable time, a missing
  cost, a currency not of three letters, another event) is not inserted:
  it is kept whole in `golem.spend_rejected` with the first column at
  fault, so it never fails the batch of good rows it arrived with.
- **Delivery is at least once**: the collector's retries and the
  gateway's re-exports can insert a row twice. The second copy has the
  same `event_id`, and every reader deduplicates by it (above).

## 4. The price model (ADR 0025)

A `PriceList` entry prices a model on a provider account (or a whole
provider), from an effective date, in micro-units per million tokens or
per unit:

- input; output; reasoning (if billed separately);
- cache read; cache write by TTL (5 minutes, 1 hour: `cacheWritePerMTok`, and `cacheWrite1hPerMTok`, which defaults to the five-minute rate);
- tiers by prompt length (for example above 200k input tokens), each tier
  with its own rates;
- batch discount (a factor or explicit rates);
- regional or inference-profile premiums (Bedrock cross-region profiles,
  regional endpoints);
- per-call units for server-side tools (web search, code execution) and
  per-image inputs where priced per image;
- for self-hosted models, internal rates set by the platform team.

Versions are immutable once used by a ledger row; a change is a new
version with a new effective date. Built: a PriceList's version is its
`spec.version` with its name (`default/2026-11`), recorded as each row's
`price_version`; a list without one is versioned by its spec's digest,
which any change moves. The snapshot that carried the prices names the
config's git commit they were read from (`source.configRevision`, a
commit hash string; `""` when the config is not a checkout, and an older
publisher's number reads as `""`), so a version can be traced to the
commit that defined it. Only the per-token rates are built (no tiers,
batch, premiums or units yet: ADR 0044's "good enough"). The vendored list is refreshed by a
scheduled PR (PRD §9.7); enterprise rates override it.

## 5. Identifiers and headers (ADR 0026)

- **Keys**: `glm_live_…` (production installation) and `glm_dev_…`
  (standalone and `make dev-up`), then 32 random bytes and a CRC32 checksum
  in base62. A production gateway refuses `glm_dev_` keys and a standalone
  one refuses `glm_live_` keys. Both prefixes are registered for secret
  scanning.
- **Repeated headers**: a header golem reads as one value
  (`Authorization`, `x-api-key`, `content-type`, `golem-scope`,
  `golem-session`, `golem-end-user`, `anthropic-version`,
  `openai-organization`) sent more than once is refused
  (`request.invalid`, naming it), before authentication: golem would read
  the first copy, a proxy in front or the provider behind perhaps the
  last. Those whose values never hold a comma (`x-api-key`, `golem-scope`,
  `anthropic-version`, `openai-organization`) are refused holding one too,
  as an intermediary may join two copies into one line. `anthropic-beta`
  and `golem-tags` are lists and may repeat. The lists are
  `lib/contract.SingleHeaders` and `ListHeaders`; a header the code reads
  must be in one (`TestHeadersDeclared`). The control API takes exactly
  one `Authorization` header.
- **Request IDs**: golem's request ID (UUIDv7) is returned on every
  response as `golem-request-id`, and in error bodies; an incoming
  `x-request-id` is recorded as the caller's. The provider's request ID
  (`request-id`, `x-request-id`, `x-amzn-requestid`) is recorded in the
  span and the ledger.
- **Sessions**: one session concept drives prompt-cache affinity and
  Langfuse sessions. golem takes, in order: `golem-session`, then what each
  agent already sends (Claude Code's session identifier in
  `metadata.user_id` or its session header, Codex's, OpenAI's `user` with a
  conversation prefix hash as a last resort). The mapping per agent is
  checked by the SDK compatibility tests.
- **Scope selection** with Entra tokens: `golem-scope: team/platform`.
- **Tags and end users**: `golem-tags`, `golem-end-user` (attributes, never
  identity).
- Response headers golem adds: `golem-request-id`, `golem-warning`,
  `golem-route` (alias and deployment, when the scope allows it),
  `golem-translated` (when the canonical model was used), and provider-style
  `x-ratelimit-*`.
- Header names have no `x-` prefix (RFC 6648), and golem never forwards its
  own headers upstream.

## 6. Provider-side retention (ADR 0027)

Some provider APIs keep data on the provider's side: OpenAI's Responses
API stores responses by default (`store: true`), and file and batch APIs
keep uploads. Data stored there escapes golem's capture and retention
rules.

- A `ScopePolicy` sets `providerStorage: forbidden | allowed`, default
  `forbidden`.
- When forbidden, golem sets `store: false` on Responses requests (and
  equivalents) and refuses file and batch uploads with
  `authz.provider_storage_forbidden`; stateful continuations
  (`previous_response_id`) are therefore unavailable in such scopes.
- M1 remains stateless even when allowed is configured: stored operations,
  continuations, background work and managed-object references are refused
  pending an ownership registry. Later, each object operation checks sharing
  rights and records provider storage (ADR 0029).

## 7. SDK and API-version compatibility

- A matrix of supported `anthropic-version` values, Anthropic beta
  features, OpenAI API features, and SDK and agent versions (OpenAI and
  Anthropic SDKs for Python, TypeScript, Go; Claude Code, Codex, Copilot
  BYOK clients), each tested nightly against the gateway with recorded and
  live providers.
- The tests pin each SDK's retry behaviour (§1) and each agent's session
  and header behaviour (§5). Built for the SDKs (`test/compat`); the agents
  are next.
- golem never changes a stream's shape for a caller: where it asks a
  provider for more than the caller did (OpenAI's `include_usage`), it
  withholds the extra from the caller.
- When a provider ships a breaking change, the pass-through path requires
  policy compatibility review before enabling new provider features; the adapters and envelopes get a fix and a matrix
  row, and the release notes name it.

## 8. Policy-relevant request fields (ADR 0037)

ADR 0041 §3 and §7.1 add feature modes. Restricted (unapproved API
versions, fields and betas rejected) is the default for the edge pool,
non-agent applications and sensitive or residency-bound scopes; compatible
(unknown features forwarded, exposure accepted until remediation) is the
default for coding-agent routes under one standing approval. Names are bounded/sanitized or keyed-hashed before logging;
values are never logged. Nightly diffs do not bound remediation time. These are enforced
wherever they appear, and the list grows with the nightly review. The
mode is the scope's `features` (unset: restricted, but compatible on routes
marked `agent`); restricted refuses unknown request members, unapproved
`anthropic-beta` values (the route's `approvedBetas`) and unapproved
`anthropic-version`s (`authz.feature_forbidden`; approved: `2023-06-01` and
`2026-10-21`, the SDKs' 2026-10 majors' default, reviewed against this table
and golem's usage and stream parsing):

| Field or family | Policy |
|---|---|
| `store`, `previous_response_id`, `background`, conversation identifiers | provider-side storage (ADR 0027; M1 refuses stateful use). Enforced: where a scope forbids storage (the default), `store: true` reaches the provider as `false` (a `store` that is not a boolean or null is refused, `request.invalid`), and an Anthropic `container` is refused (`authz.provider_storage_forbidden`) |
| file, container and upload identifiers | provider-side storage |
| `mcp_servers`, server-side tools with network access (web search, fetch, code execution) | egress and data movement; billing units. Enforced: refused in scopes with the declared-worst-case check, since such a request declares no worst case (`authz.feature_forbidden`, ADR 0044) |
| `n`, `max_tokens` above the scope's ceiling, input above the scope's size ceiling | cost (ADR 0038). Enforced: `max_tokens` × `n` over the scope's `maxOutputTokens`, a missing `max_tokens` under it, or a body over `maxInputBytes` is refused (`request.over_ceiling`); the declared worst case counts `n` completions |
| `metadata.user_id`, `user`, session fields | attribution only, never identity |

## 9. Reconciliation (ADR 0038)

golem's providers run on dedicated accounts or projects. Each month the
ledger is compared with provider usage and cost per account and model;
differences within 3–5% are booked as adjustments, larger ones
investigated. Chargeback uses the provider's cost per account and model,
split across scopes in proportion to golem's per-request attribution.

