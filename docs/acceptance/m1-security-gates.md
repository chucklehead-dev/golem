# Acceptance: M1 security gates (golem#4, golem#5)

Each invariant the issues list, the negative test that shows it holds, and
the commit. "Ablated" means the test was run with the fix removed and
failed. Tests run under `go test -race ./...`.

## golem#4: uncertain upstream work and ledger delivery

| Invariant | Test | Ablated | Commit |
|---|---|---|---|
| A round trip the provider cannot have seen (DNS, dial, TLS handshake) is free and frees its slot | `gateway/proxy` `TestUncertainUpstream` (the dial failed; the name did not resolve) | — | ee14c74 |
| Any other failure before the headers (timeout, reset after the request went out) is `uncertain`, charged its worst case as an estimate, never zero | `TestUncertainUpstream` (reset after the request went out; the headers timed out, the provider silent) | yes (the timeout taken as unreached) | ee14c74; timeout case this commit |
| A caller gone before the headers is `cancelled`, charged its input | `TestUncertainUpstream` (the caller left before the headers) | — | ee14c74 |
| After an ambiguous end the slot is held for the provider's tail, then freed | `TestUncertainUpstream`; `TestConcurrencyAndCancel` (slot held, then free) | — | ee14c74 |
| A failed ledger batch is retried with bounded backoff, not dropped (collector restart) | `gateway/telemetry` `TestLedgerQueueRetries` | — | ee14c74 |
| A batch the collector took but did not acknowledge is resent whole, each row keeping its event_id; deduplication by event counts it once | `TestLedgerQueuePartialDelivery`; `golem.spend_dedup` (one row per event_id; the snapshot's spend is summed from it, ADR 0048) | — | edfadd1 |
| Shutdown racing emission, retries or an export in flight keeps its deadline and accounts for every row (delivered + failed + dropped = emitted) | `TestLedgerQueueShutdownRaces` (three cases, -race; a stress test, which a logical race between atomics can pass) | yes | edfadd1 |
| A row past the closed check when Shutdown begins is queued before the worker's last drain, so delivered or counted: emission and shutdown share a lock | `TestLedgerQueueEmitDuringShutdown` (the emission held between its check and its enqueue) | yes | 2c0d42e |
| A ForceFlush racing Shutdown returns once the worker has stopped, never waiting on it forever | `TestLedgerQueueFlushDuringShutdown` (the flush held until the shutdown finished) | yes (hangs) | 54a912b |
| Rows emitted after shutdown are counted, never lost silently | `TestLedgerQueueShutdown` | — | 3d20567 |

A missing report is charged an upper bound, never an average (golem#3,
PR #6, on its branch): output every byte of the response received a
token, at most the request's `max_tokens × n`; input, if unreported,
every request byte, the caller-cancelled case included. Nothing is
recognised, so token-dense output, unknown fields and malformed events
are not undercounted: `gateway/proxy` `TestEstimates`. Ablated: yes (the
ceiling). f394c10 on security/request-accounting-hardening; it replaces
53090b7's content scan.

## golem#5: release gates

| Invariant | Test | Ablated | Commit |
|---|---|---|---|
| A key is honoured only for a listed owner of its kind, a member of its scope | `gateway/auth` `TestAuthenticate` (its owner is not listed; of another kind; left the scope) | — | c74e1c1 |
| Entra tokens are refused once the cached signing keys are past MaxKeyAge | `TestStaleKeys` | — | c74e1c1 |
| Different credentials in Authorization and x-api-key are refused | `TestAuthenticate` (two different credentials; the same twice is fine) | — | c74e1c1 |
| The implicit principal exists only for a dev installation, and takes only standalone's generated local key | `TestImplicit`; `cmd/golem-gateway` `TestLocalKey` (0600, kept across restarts, a bad file refused); by hand with the binary (no key 401, the local key 200) | — | c74e1c1 |
| Restricted feature mode (the default) refuses unknown members, unapproved betas and API versions; compatible and agent routes forward them | `gateway/proxy` `TestFeatureModes` | — | 8165990 |
| A request body must arrive within BodyTimeout | `TestRequestBoundary` (a trickling body) | yes (hangs) | 8165990 |
| A response past MaxResponse is cut | `TestRequestBoundary` (past the ceiling) | — | 8165990 |
| Upstream connections to metadata, link-local, unspecified or multicast addresses are refused always; to loopback or private ones unless granted | `gateway/snapshot` `TestCheckDestination` (IPv4-mapped and CGNAT included), `TestDialRefused` (on a real dial); `TestDialReresolved` (a name re-resolved to metadata, then link-local, between connections: refused at the next dial) | yes (the dial check removed: it connects) | 79e0d0a; re-resolution this commit |
| OTLP export uses TLS unless the collector is a literal loopback address (a name, localhost included, gets TLS) | `gateway/telemetry` `TestPlaintextOK` (the decision; no live collector) | — | 79e0d0a; names 2c0d42e |
| A snapshot the reader cannot interpret is refused, never read with weaker policy: unknown members (`strict` included), duplicate names, names in another case | `lib/snapshot` `TestDecodeFailsClosed` | yes | this commit |
| Production publication stays disabled: the gateway runs standalone only, on loopback | `cmd/golem-gateway` `TestReleaseGate` | — | 434c819 |

## Not yet shown

- OTLP's TLS is tested at the decision, not against a collector.
- Restricted mode's approved lists cover Anthropic's beta header and API
  version; OpenAI has no equivalent header to approve.
- OTLP export requires server-authenticated TLS to any non-loopback
  collector, and golem presents no client certificate. Whether that
  satisfies policy, and who presents client certificates if it does not,
  is undecided: an assumption, not proof (operations.md, "Telemetry
  transport"; golem#7).
- The provider-account spending limits (ADR 0044, operations.md) are a
  deployment requirement golem cannot verify: evidence is the release
  checklist's record of each account's limit, not a test.

Deployment evidence that tests cannot give (provider spending limits,
OTLP TLS against the real collector, the mTLS owner of each hop) is tracked in golem#7, a prerequisite
for lifting `releaseGate`.

## Validation of this record

`go test ./...`, `go vet ./...`, race tests of the gateway, telemetry and
JSON scanning, and the models' sampled checks (`model/check-*.sh`) pass.
The accounting model's exhaustive search (`check-accounting.sh` with the
gotogether CLI) explores every run up to 12 steps of the small instance:
it holds for the soft and declared-worst-case designs and finds each
broken design's counterexample; it is complete to that depth, not
beyond.
