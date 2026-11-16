# Orientation: golem's code

A map of the code for an engineer new to it: what each package does and
where to start reading it, the request's path through the gateway, the
control plane's loop, the invariants (each with the function that enforces
it and the test that shows it), and where each model's connect test is.
Code is named by package and function, never by line: `grep -n 'func
(h \*Handler) prepare'` finds it. A name here that the code no longer has
is a bug in this page.

What golem should be is in the design documents: [gateway.md](../gateway.md)
(the data plane), [control-plane.md](../control-plane.md) (golem-control as
built), [contracts.md](../contracts.md) (codes, the ledger, headers), the
[ADRs](../adr/README.md); what M1 is building is
[m1-plan.md](../m1-plan.md). This page says what the code does now.

The longer walkthroughs this page replaces (request path, gateway edges,
accounting, control plane, control API, config and snapshots) are in git
history: `git log --diff-filter=D -- docs/walkthroughs/`.

## 1. The package map

Three binaries over shared libraries. `internal/archtest` keeps the layers
apart: the gateway imports nothing of `control/` (nor a Kubernetes,
PostgreSQL or ClickHouse client), `lib/` imports no component, golemctl
links `lib/` and the generated clients only, a dialect imports only
`canon` and `dialect/internal`, an envelope no dialect, and only
`cmd/golem-gateway` imports `gateway/proxy`.

### The gateway (`gateway/`, `cmd/golem-gateway`)

| Package | What it does | Start reading at |
|---|---|---|
| `cmd/golem-gateway` | flags, wiring, signals; standalone or pool mode; the release gate; the lifecycle's drain | `run`, `newMux`, `newHolder`, `newLimiter`, `releaseGate`, `lifecycle.shutdown` |
| `gateway/proxy` | the request path: the only package that knows the order of the steps | `Handler.ServeHTTP`, `Handler.prepare`, `callState.respond`, `Handler.relay` |
| `gateway/guard` | the floor before authentication: a source's requests and failed authentications, connections | `Guard.Admit`, `Guard.Failing`, `Guard.Failed`, `Listener.Accept` |
| `gateway/auth` | a credential to a `Principal` from the snapshot alone: golem keys by hash, Entra tokens by cached keys; the emergency denylist | `Keys.AuthenticateGrant`, `Keys.Honours`, `DenyFile.Load` |
| `gateway/dialect/...` | one package per API: the pass-through scan and splice, error shapes, usage from a response or stream | `messages.Request.Scan`, `chat.Request.Scan`, `responses.Request.Scan`; `jsonscan.Object` |
| `gateway/sse` | server-sent events split from any chunking; a filter that withholds some | `Parser`, `Filter` |
| `gateway/route` | alias to `Route`; a deployment's health: ceilings, ejection, probes, retry budgets, active checks | `Table.Lookup`, `Health.AcquireWhy`, `Lease.Commit`, `Lease.Fail`, `Checker.Run` |
| `gateway/admission` | the contract between the path and admission (a leaf) | `Admitter`, `Ticket`, `Refusal` |
| `gateway/limit` | admission's implementation: the snapshot's known spend against the scope's budget, in-flight slots per principal (per scope) and per scope, rate buckets; Valkey and in-memory stores; outages as waivers | `Limiter.Admit`, `admitScript`, `Ticket.Cancel`, `Limiter.Renew`, `Limiter.Recover`, `Limiter.Take` |
| `gateway/price` | integer micro-unit pricing, half-even, saturating | `Cost`, `Worst` |
| `gateway/canon` | the canonical model; only `Usage` so far | `Usage` |
| `gateway/envelope`, `envelope/bedrock` | how a host carries a dialect: Bedrock's InvokeModel, SigV4, its event stream as Messages SSE; the attempt's attribution | `bedrock.Transport.RoundTrip` |
| `gateway/snapshot` | the compiled snapshot behind an atomic pointer; the bucket source (floor, lease, last-good copy) and the directory source | `Holder.Apply`, `Holder.Current`, `Bucket.Start`, `Bucket.Poll`, `Dir.Load` |
| `gateway/telemetry` | spans, metrics, the ledger row and its queue | `Telemetry.Ended`, `Ledger.row`, `LedgerQueue` |

Planned and not yet packages: hooks (ADR 0014, from M2) and translation
through `canon` (M1.5); upstream transports live in `gateway/snapshot`
(`upstreamTransport`), probes in `cmd/golem-gateway`.

### golem-control (`control/`, `cmd/golem-control`)

| Package | What it does | Start reading at |
|---|---|---|
| `cmd/golem-control` | flags and wiring; the `keygen`, `pubkey`, `restore` and `bootstrap` subcommands | `dispatch`, `run` |
| `control/state` | the state document in the bucket: in-memory reads, `Update` by compare-and-swap with invariants, the fence, the audit outbox, restore, bootstrap | `Store.Update`, `Store.FencedUpdate`, `Store.Claim`, `check`, `Restore`, `Store.Bootstrap` |
| `control/controller` | the loop: compile the config checkout, publish every pool when due, ship the audit, run the group sync | `Controller.Run`, `Controller.tick`, `Controller.Step`, `Controller.due`, `Checkout.resolve` |
| `control/publish` | one pool's publication: snapshot, record, checkpoint, pointer | `Publisher.Publish`, `PublishAll`, `Publisher.prepare` |
| `control/spend` | known spend per scope from ClickHouse, before a boundary | `Job.Step`, `ClickHouse.Spend`, `Job.Lags` |
| `control/audit` | the outbox shipped: archived, emitted, then trimmed | `Shipper.Step` |
| `control/entrasync` | bound Entra groups' members as synced memberships | `Sync.Step`, `reconcile` |
| `control/service` | the control API's rules: authorization, validation, one write with its audit event | `Service.apply`, `Service.MintKey`, `Service.AddMember` |
| `control/api` | transport: Connect services, Entra authentication, error mapping | `Server.Handler`, `EntraAuth.Authenticate`, `Server.connectError` |
| `control/metrics` | observable instruments over the controller's state | `Register` |

### Shared (`lib/`, `internal/`, `cmd/golemctl`, `deploy/`, `test/`)

| Package | What it does | Start reading at |
|---|---|---|
| `lib/api/v1alpha1` | the config kinds as Go types (Backend, ModelRoute, ScopePolicy, PriceList, LocalIdentity), read strictly from YAML | `DecodeFS` |
| `lib/compile` | the one validation path: config (and the state's entitlements) to a snapshot, or every problem with its object and field | `Compile`, `Renew`, `scopePolicy`, `join` |
| `lib/snapshot` | the snapshot format, its signed pointer and checkpoint | `Snapshot`, `Decode`, `SignPointer`, `OpenPointer` |
| `lib/bucket`, `s3bucket`, `buckettest` | the object store with S3's conditional semantics; the directory one; the conformance suite | `Bucket`, `Dir.Put` |
| `lib/sign`, `sign/kmssign` | Ed25519 and KMS ECDSA signers; verifiers by key ID | `LoadKeys`, `kmssign.PublicKey` |
| `lib/entra` | Entra token verification for one audience and scope | `Verifier.Verify`, `Verifier.Run` |
| `lib/key`, `lib/period`, `lib/contract`, `lib/otlp` | the key format; budget periods; codes and headers (checked against contracts.md); OTLP over TLS | `key.Parse`; `contract_test.go`; `otlp.PlaintextOK` |
| `internal/crdgen` | the CRDs, the chart's DDL and collector copies kept current | its tests |
| `cmd/golemctl` | `config validate`, `login`, `token`, the org and key commands | `main`, `api.go` |
| `deploy/` | the charts, Argo sample, `clickhouse/spend.sql`, `otel/collector.yaml` | deploy/charts/README.md |
| `test/compat`, `test/ledger` | real SDKs against a gateway; the ledger end to end | `TestSDKs`, `TestLedgerReachesClickHouse` |

## 2. A request's path through the gateway

`Handler.ServeHTTP`, top to bottom; each step refuses in the caller's
dialect with a contract code (`Handler.fail`, `appendError`). One `Handler`
per dialect, sharing one `InFlight`, `route.Health`, `guard.Guard` and
`Reading` (`newMux`).

1. **Accept.** `newServer`'s bounds (header size, read-header timeout,
   HTTP/2 streams); connections through `guard.Listener`. Standalone wraps
   every handler in `localOnly` (loopback host, same origin, JSON).
2. **Prepare** (`Handler.prepare`, shared with `ServeCountTokens`):
   - `contract.CheckSingleHeaders`: a credential sent twice is refused.
   - `Handler.authenticate`: `Guard.Admit` (the source's request rate),
     then `Guard.Failing`: a failing source's golem key is still looked
     up, only its failures and its Entra tokens refused `limit.source`.
     `Keys.AuthenticateGrant` names the principal and its rights from the
     snapshot; `credentialsFault` decides whether a refusal counts against
     the source (`Guard.Failed`). An expired lease is
     `gateway.authorization_expired`.
   - Body bounds before reading: a `Content-Length` over the scope's
     `maxInputBytes` refused unread; `Reading.start` bounds the gateway's
     bytes being read and the principal's bodies (`limit.concurrency`);
     `Handler.readBody` caps the body (32 MiB by default).
   - `exchange.scan`: the dialect's scan (`jsonscan.Object`: the whole
     document, names unescaped, duplicates, trailing data and depth past
     128 refused); file and item references refused in both dialects.
   - `Handler.route`: the alias within the principal's models.
   - `Handler.features`: the route's feature mode and betas.
3. **Policy** (`Handler.policy`): `maxOutputTokens`, `maxInputBytes`,
   provider storage (`exchange.forbidStorage`). Server-side tools are
   admitted (ADR 0031).
4. **Betas** (`incapable`): no deployment that takes the request's betas,
   `route.no_capable_deployment`, before anything is counted.
5. **Rate limits** (`Handler.takeRate`): the member's and the scope's
   requests-a-minute buckets, once a call, failing open.
6. **First deployment** (`callState.next`, `pick`): the pool in order,
   each through `Health.AcquireWhy` (ceiling, per-principal ceiling,
   ejection, one probe). A principal at its own share is told
   `limit.concurrency` (`noDeployment`).
7. **Admission** (`Handler.admit`, `Limiter.Admit`): the scope's known
   spend from the snapshot against its budget, and the slots, in one store
   call (`admitScript`). Under `observe` and `warn` an over-budget call is
   admitted, its row marked (`Handler.overBudget`, `budgetWarning`); under
   `enforce`, `budget.pool_exhausted` (402). Only an admitted call credits
   the route's retry budget (`Health.Called`). From here `settlement` ends
   the ticket exactly once.
8. **Registration** (`InFlight.add`): a later snapshot or denylist entry
   that withdraws the grant ends the call (`InFlight.Sweep`).
9. **Attempts.** `exchange.rewrite` splices the deployment's model;
   `upstreamRequest` copies the allowlisted headers (`Dialect.forwardRequest`;
   `OpenAI-Organization` is not among them); the deployment's transport
   sends it. An attempt the provider never reached (`reachedProvider`,
   Bedrock's `envelope.ErrNotSent` included), or refused (`retryable`), is
   retried on an untried deployment (`callState.retry`) within the route's
   attempts and retry budget. A stream commits at its first event
   (`firstEvent.await`); an error event first is a refusal, and a stream
   that fails or ends before any event is uncertain. A caller gone or
   a sweep's withdrawal releases the attempt's lease, not fails it.
10. **Respond** (`callState.respond`, `Handler.relay`): the bytes copied
    as they come, the same bytes through the dialect's usage reader; a
    stream whose provider sent an error event after output is `failed`; a
    cut stream gets the contract's error event and an abort.
11. **Account**: the row (`Telemetry.Ended`) before the ticket ends
    (`settlement.complete`, or `settlement.cancel` holding the slot for the
    provider's tail, at most to the deadline); unreported usage estimated
    high (`exchange.estimate`, `chargeUnanswered`).

`ServeCountTokens` takes steps 2, 4, 5 and the deployment's health, short
of admission: unbilled, no row. `ServeModels` lists the scope's aliases.

### A snapshot reaching the gateway

`Bucket.Start` raises the floor to the higher of the local and trusted
checkpoints (none: it does not start), else serves the last-good copy
(`Bucket.loadLastGood`). `Bucket.Poll` every second: `openPointer`
(signature, pool, sequence at least the floor, issue time within the skew),
`openSnapshot` (hash, sequence, `checkLease`, `checkZone`), then
`Holder.Apply` (newer than the one held, compiled off the request path).
`newHolder`'s hook hands the snapshot's known spend to the limiter
(`applyKnown`) and sweeps the calls it withdraws. `Holder.Current` refuses
once the lease has expired.

## 3. The control plane's loop

One replica (ADR 0053). `Controller.Run` claims the fence
(`Controller.Claim`, `Store.Claim`: one above the document's epoch), then
every second `Controller.tick`:

1. `Controller.Step`: `Store.Refresh`; `Controller.notice` (another's
   fence: `ErrDeposed`, the process exits); `Controller.load` compiles the
   checkout (`Checkout.resolve` through git-sync's link, the commit as
   `ConfigRevision`) with the state's entitlements, keeping the last good
   config on a failure (none since start: `loadPublished` renews the
   bucket's); `staleErr` once the checkout's last fetch is older than
   `-max-config-age`.
2. A pool is due (`Controller.due`) on a change of config, entitlements or
   epoch, at the renewal interval (60 s), or at its backoff after a
   failure; past the config age only a change, with the lease capped.
3. `Job.Step` (known spend) once a step with something due; then
   `Controller.publish` → `PublishAll`: every due pool's record in one
   fenced update, then each pool's snapshot, checkpoint and pointer.
4. `Shipper.Step` (the audit outbox), then `syncStep` (the group sync,
   every 5 minutes), both fenced.

The control API (`control/api`, `control/service`) runs beside the loop in
the same process; its writes are ordinary compare-and-swap updates.

## 4. Invariants

### The request path (`gateway/proxy`, `gateway/route`, `gateway/guard`)

| Invariant | Enforced by | Tested by |
|---|---|---|
| Every admitted ticket ends exactly once, a panic included | `settlement`, its deferred guard in `ServeHTTP`; `Ticket.Complete`/`Cancel` | `TestAdmitterContract`, `TestConcurrencyAndCancel`, `TestTicketEndsOnce` |
| The row is written before the ticket ends | `Telemetry.Ended` before `complete`/`cancel` in each end | `TestLedgerRows` |
| golem routes by exactly what the provider reads | `jsonscan.Object` | `TestObject`, `FuzzObject`, `TestRefusals`, `TestRequestBoundary` |
| File and item references are refused, escaped or nested, in both dialects | `jsonscan.Objects`, the scans' stateful checks, `exchange.forbidStorage` | `TestResponses`, `TestStoreOff`, the dialects' scan tests |
| Only a credential's fault counts against its source; a valid golem key from a failing source is served | `credentialsFault`, `Handler.authenticate`, `Guard.Failing` | `TestInvalidKeyFlood`, `TestEntraKeyTroubleIsNoFailure`, `TestFailuresRefuseTheSource` |
| A source's requests and the sources kept are bounded; a nil guard admits all | `Guard.Admit`, `Guard.source` | `TestRequestRate`, `TestSources`, `TestNilGuard` |
| Connections are bounded in all and per address | `guard.Listener` | `TestListener`, `TestListenerMax` |
| A body claiming more than the scope's ceiling is refused unread; bytes and bodies being read are bounded | `prepare`, `Reading.start`, `Reading.take` | `TestReadingClaimOverCeiling`, `TestReadingBounds`, `TestClaimedLengthNotTrusted`, `TestReadingFlags` |
| A retry is a failover, before the commit, of an attempt never reached or refused, within the route's attempts and retry budget | `pick`, `callState.retry`, `retryable`, `reachedProvider`, `firstEvent` | `TestAttemptsModel`, `TestFailover`, `TestFinalAttempts`, `TestAttemptBudgets`, `TestFailedBeforeCommit`, `TestNoFirstEvent`, `TestFailoverDrill`, `TestReachedProvider` |
| A cancelled call or a withdrawn one is no deployment failure | `callState.endUnanswered`, `callState.retry` (lease released) | `TestCallerGoneIsNoFailure` |
| Only an admitted call earns its route retries | `Health.Called` after `admit` | `TestRefusedCallsEarnNoRetries` |
| A principal at its own per-principal ceiling is told `limit.concurrency` | `Health.AcquireWhy`, `noDeployment` | `TestOwnShareIsTheCallersLimit`, `TestAcquireWhy` |
| A request's betas never fail over to a deployment that refuses them | `requestBetas`, `incapable`, `pick` | `TestBetaRouting` |
| A deployment's attempts stay within its ceiling and per-principal ceiling | `Health.Acquire` | `TestCeiling`, `TestPerPrincipalCeiling`, `TestCeilingFailsOver` |
| Failures eject; an ejected deployment takes one probe; a commit readmits | `Health.eject`, `Lease.Fail`, `Lease.Commit` | `TestEjection`, `TestProbeAbandoned`, `TestReadmissionEndsProbes` |
| A failed health check ejects a box; a pass readmits it once its ejection is over | `Checker`, `Health.Checked` | `TestCheckerEjectsAndReadmits` |
| A route's retries are a share of its calls | `Health.Called`, `Health.Retry` | `TestRetryBudget`, `TestRetryBudgetBoundsTheStorm` |
| A stream's provider error after output is a failed call | the usage readers' error flag, `callState.respond` | `TestStreamErrorAfterOutput`, `TestBedrockStreamErrorAfterOutput` |
| Work the provider may have done is never free; estimates err high | `chargeUnanswered`, `exchange.estimate` | `TestEstimates`, `TestUncertainUpstream` |
| An ambiguous end holds the slot for the provider's tail, at most to the deadline | `settlement.cancel` | `TestTailClampedToDeadline`, `TestConcurrencyAndCancel` |
| A cut response is never taken for whole | `streamError`, the abort in `respond` | `TestUpstreamCut` |
| A revoked or denied caller's calls in flight end within seconds; lease expiry ends none | `InFlight.Sweep`, `Keys.Honours`, `terminatedBy` | `TestTerminateStream`, `TestTerminateWhole`, `TestLeaseExpiryEndsNoCall`, `TestTerminateRace`, `TestSweepSparesANewerGrant`, `TestWiringEndsWithdrawnCalls` |
| Only allowlisted headers cross; golem's and `OpenAI-Organization` never reach the provider | `Dialect.forwardRequest`, `copyHeaders` | `TestHeaders`, `TestOpenAIHeaders` |
| The injected usage chunk never reaches the caller | `startStream`, `sse.Filter` | `TestInjectedUsageWithheld` |
| A slow client or a silent provider is bounded; a fired alarm cancels no later call | `exchange.send`'s write deadline, `bounds`, `alarm.disarm` | `TestStalledClient`, `TestSilentProviderKeepsClient`, `TestRouteBounds`, `TestBoundsReuse`, `TestBoundsRace` |
| Policy: ceilings, storage, feature modes | `Handler.policy`, `Handler.features` | `TestPolicy`, `TestStoreOff`, `TestPolicyUnits`, `TestFeatureModes` |
| `count_tokens` is held to a call's checks, short of admission | `ServeCountTokens` | `TestCountTokens`, `TestCountTokensChecks`, `TestCountTokensWiring` |
| A model list holds only the scope's aliases | `ServeModels` | `TestModels` |
| Prices are integers, rounded once, never wrap | `price.Cost`, `MaxCost` | `TestCost` |

### Admission (`gateway/limit`)

| Invariant | Enforced by | Tested by |
|---|---|---|
| The budget check and the slots are one atomic step | `admitScript`; `Mem.Admit` under its lock | `TestValkeyStore`, `TestMemStore`, `TestExposureModel`, `TestExposureModelValkey` |
| The two stores answer alike | one check (`overBudget`), mirrored in Lua | `TestStoresAgree`, `TestStoresAgreeVaried` |
| A principal's slots are counted per scope | `principalOf`, `Valkey.principalKey` | `TestPrincipalPerScope` |
| Only an enforced budget refuses; in-flight limits refuse in every mode | `refusing`, `Limiter.track` | `TestBudgetModes` (limit and proxy), `TestBudgetWarning` |
| A caller leaving during admission leaves no untracked slot | `Limiter.storeContext`, `Limiter.Admit` | `TestCallerLeavesDuringAdmission`, `TestCallerCancelIsNoOutage` |
| A cancelled call keeps its slot for its tail; a renewal never revives an ended call | `Ticket.Cancel`, `Ticket.EndAt`, `Limiter.reacquired` | `TestLimits`, `TestRenewalDoesNotRevive`, `TestRenewalStopsAtDeadline` |
| A gateway starts and serves with Valkey down; outage and grace admissions are waivers; running calls re-registered on recovery | `DialValkey`, `Limiter.MarkDown`, `admitLocally`, `Limiter.Recover` | `TestStartWithValkeyDown`, `TestOutage`, `TestOutageCounts`, `TestPoolModeValkeyDown` |
| A call is counted in the period it started | `Limiter.period`, `Ticket.Period` | `TestPeriodOfStart`, `TestPeriodRollover`, `TestPeriodIsTheStart` |
| A request is taken from every rate bucket or none; meters fail open | `Mem.Take`, `Valkey.Take`, `Limiter.Take` | `TestMetersFailOpen`, `TestRateLimits` |

### The gateway's edges (`gateway/snapshot`, `gateway/auth`, `gateway/telemetry`, the binary)

| Invariant | Enforced by | Tested by |
|---|---|---|
| A snapshot below the floor, or not newer than the one held, is never applied | `Holder.Apply`, `Bucket.openPointer` | `TestHolder`, `TestRaiseFloor`, `TestBucketSource`, `TestPublicationModel` |
| An empty disk takes the trusted checkpoint as its floor; with no floor it does not start | `Bucket.Start`, `trustedCheckpoint` | `TestBucketSource`, `TestPublicationModel` |
| A pointer from the future or a lease past `-max-lease` is refused; another period zone's snapshot is refused | `openPointer`, `checkLease`, `checkZone` | `TestClockBounds`, `TestPoolModeLeaseBound`, `TestPeriodZone` |
| A restart with the bucket unreachable serves the last publication, verified again | `loadLastGood`, `applyLastGood`, `verify` | `TestLastGood`, `TestLastGoodRetried` |
| An expired lease serves nothing new | `Holder.Current` | `TestHolder`, `TestLeaseExpired` |
| A config that does not compile keeps the previous one | `Holder.Apply`, `Dir.Load` | `TestDir`, `TestApplyRetried` |
| Upstream dials to forbidden addresses are refused at every connection | `checkDestination` | `TestCheckDestination`, `TestDialRefused`, `TestDialReresolved` |
| A key is honoured only for a listed owner of its kind in its scope | `keyEntry` | `TestAuthenticate`, `FuzzAuthenticate` |
| A user's token cannot act as a service, or the reverse; stale keys refuse tokens | `Keys.token`, `entra.Verifier.Verify` | `TestEntra`, `TestStaleKeys`, `TestRefresh` |
| Entra unreachable at start: keys served, tokens `gateway.not_ready` | `newEntra`, `Verifier.Run` | `TestPoolModeEntraDown`, `TestEntraKeysNotYetFetched` |
| The denylist refuses whatever the snapshot says; a bad edit keeps the list | `DenyFile.Load`, `Denylist.denies` | `TestDenylist` |
| Every ledger row is delivered, dropped or failed, and counted | `LedgerQueue` | `TestLedgerQueue*`, `TestStdoutCountsLostRows` |
| A row has `golem.spend`'s columns and types; attempt IDs are UUIDs | `Ledger.row`, `AttemptUUID` | `TestRowMatchesLedgerDDL`, `TestRowListAndMapTypes`, `TestAttemptIDs`, `TestLedgerReachesClickHouse` |
| OTLP is TLS unless to a literal loopback address | `otlp.PlaintextOK` | `TestPlaintextOK`, `TestOTLPIgnoresInsecureEnv` |
| A stream cut at shutdown still leaves its row; no call starts uncounted after the wait | `lifecycle.shutdown`, `lifecycle.begin` | `TestShutdownAccounts`, `TestTrackAfterWait` |
| Pool mode needs `-accept-unreleased` until golem#7 | `releaseGate` | `TestReleaseGate` |
| Bedrock: the body rewritten and signed with the attribution; a credentials refusal's text not forwarded; a request never sent is unreached | `bedrock.Transport` | `TestRewrite`, `TestRequest`, `TestMetadata`, `TestCredentialsRefused`, `TestNotSent`, `TestBedrockEndToEnd` |

### The control plane (`control/`)

| Invariant | Enforced by | Tested by |
|---|---|---|
| The fence's epoch never goes back; a leader-only write lands only under its fence | `checkFence`, `Store.FencedUpdate` | `TestFenceInvariant`, `TestClaim`, `TestFenced`, `TestSyncFenced`, `TestDeposedTrim`, `TestRunExitsWhenDeposed` |
| No pool's publication record is removed or goes back | `checkPublications` | `TestInvariants` |
| No pool is published from a record below its checkpoint or pointer; a bad checkpoint stops it | `Publisher.prepare`, `below` | `TestGuard` |
| A write whose reply was lost is applied once | `Store.Update`, `Document.wrote` | `TestLostReplyOvertaken`, `TestConcurrentStores` |
| A document that went back is refused; a missing one is not started over unless bootstrapped | `regressed`, `missing`, `Store.Bootstrap` | `TestRegressed`, `TestMissingDocument`, `TestBootstrapCommand` |
| Entitlements: owners exist and are members; scopes exist; one principal per Entra ID; no `entra:` IDs; revocations append-only | `checkPrincipals`, `checkKeys`, `checkRevocations`, `checkRoles` | `TestEntitlementInvariants`, `TestRolesAndAudit` |
| The outbox loses only its oldest events; past 80% only restrictive writes add any | `outboxFollows`, `checkOutbox`, `widens` | `TestOutboxBound`, `TestWidens`, `TestEveryActionClassified`, `TestOutboxBackpressure` |
| A restore never widens a right | `Restore` | `TestRestore`, `TestRestoreNeverWidens` |
| The audit is archived before it is trimmed | `Shipper.Step` | `TestShip`, `TestRetryAfterCrash`, `TestMismatch` |
| A broken config renews the last good; none since start renews the bucket's | `Controller.load`, `loadPublished` | `TestPublishes`, `TestRestartOverBrokenConfig`, `TestRenew` |
| A stale checkout renews no lease; a change is still published within the lease granted | `staleErr`, `Controller.due` | `TestCheckout`, `TestCheckoutAgeUnknown`, `TestConfigAgeMetric` |
| A scope with no ScopePolicy can call nothing; deleting a live scope's policy is refused | `join`, `DefaultScope`, `policiesRemoved` | `TestJoin`, `TestUnmatchedScopes`, `TestRemovedPolicyRefused` |
| A failing pool backs off while others publish | `Controller.due`, `backoff` | `TestFailingPoolBacksOff` |
| Known spend: a whole-minute boundary an allowance back; a failure keeps the last result, reported stale | `Job.Step`, `Job.Stale`, `Job.Lags` | `TestJob`, `TestJobReportsStaleResult`, `TestJobReportsIngestionLag`, `TestKnownSpend` |
| The group sync's removals of active users are guarded; offboarding is never held back | `Sync.Step`, `reconcile` | `TestSyncSafety`, `TestSyncOffboarding` |

### The control API (`control/service`, `control/api`, `cmd/golemctl`)

| Invariant | Enforced by | Tested by |
|---|---|---|
| Every call is authenticated before its body is read; a message is at most 1 MiB | `Server.Handler` | `TestLimits`, `TestAPI` |
| Only control API tokens are taken; a gateway token is not; Entra unreachable at start is `Unavailable` | `EntraAuth`, `entra.Verifier` | `TestEntraAuth`, `TestAPIStartsWithoutEntra` |
| Each method's callers are exactly org-model.md §2's; outsiders get `not found` | `manages`, `sees`, `visibleScope`, `mayMint`, `keyToChange` | `TestAuthorization`, `TestRevokeKey`, `TestReads` |
| A manager cannot adopt another team's service and mint as it | `principalToAdd`, `mayMint` | `TestServiceAdoption` |
| A manager cannot create a user or bind an Entra ID | `principalToAdd` | `TestIdentityBinding` |
| A key narrows its scope's models (from the config); expires within 90 days | `Service.MintKey`, `narrows`, `keyExpiry` | `TestKeyRules`, `TestKeyModelsFromConfig`, `TestScopeModels` |
| A change and its audit event are one write; a dry run writes nothing | `Service.apply` | `TestKeyRules`, `TestValidateOnly` |
| Internal errors' text never reaches the caller | `Server.connectError` | `TestLimits` |

### Config and snapshots (`lib/`)

| Invariant | Enforced by | Tested by |
|---|---|---|
| A misspelt field or unknown kind is refused, naming file and field | `v1alpha1.DecodeFS` | `TestDecodeStrict` |
| Every problem names object, field path and fix | `lib/compile`'s adder | `TestProblems`, `TestEntitlementProblems` |
| Equal configs give equal snapshot bytes | `Compile`'s sorting, `Snapshot.Encode` | `TestDeterministic` |
| No secret value reaches a snapshot | `backendAuth`, `localKeys` | `TestCompile`, `TestEntitlements` |
| A scope's policy is checked field by field; a budget over an unpriced model is a config problem | `scopePolicy`, `checkPriced` | `TestScopePolicyCheck`, `TestPolicyProblems` |
| A pool member's weight of 0 is refused | `pool` | `TestPoolWeight` |
| An unknown, duplicate or miscased snapshot member is refused | `snapshot.Decode` | `TestDecodeFailsClosed` |
| Only a trusted key's pointer or checkpoint opens | `OpenPointer`, `OpenCheckpoint`, `LoadKeys` | `TestPointer`, `TestCheckpoint`, `TestLoadKeys` |
| Of concurrent swaps from one tag exactly one wins | `Dir.Put`, S3's conditional writes | `buckettest` (on Dir and s3bucket) |
| A mistyped key is refused | `key.Parse` | `TestRefused` |
| Codes and headers match contracts.md | `lib/contract` | `contract_test.go` |
| The chart's CRDs, DDL and collector config are what the sources make | `internal/crdgen` | `TestCRDsCurrent`, `TestChartDDLCurrent`, `TestChartCollectorCurrent` |

## 5. The models and their connect tests

Three Quint models in [model/](../../model/README.md), each checked by
`make -C model check` and each driving the code through gotogether's
connect (the model's traces run against the real code):

| Model | What it checks | Connect test |
|---|---|---|
| publication.md | revocation within a lease; no lower publication applied; fencing, restore, lost replies | `TestPublicationModel`, gateway/snapshot/publication_mbt_test.go (golem-control's `Store.Claim`, `Publisher`, the gateway's `Bucket` source) |
| exposure.md | a scope within its stated bound (in-flight limits, snapshot lag, outages, waivers, cancelled calls' tails) | `TestExposureModel`, `TestExposureModelValkey`, gateway/limit/exposure_mbt_test.go (binding exposure.binding.yaml) |
| attempts.md | one answer per caller, each attempt charged once, retries within slots, an ejected deployment seen only by its probe | `TestAttemptsModel`, gateway/proxy/attempts_mbt_test.go |

CI runs them with `go test -run 'TestPublicationModel|TestExposureModel|TestAttemptsModel'`
after `make -C model verify-tangled check`. What testing exists beside
them: [testing.md](../testing.md).

## 6. Known gaps in the code

- The last-good copy's write order and syncs are untested.
- `MintKey`'s display-ID uniqueness check and the minted key's `no-store`
  header have no test.
