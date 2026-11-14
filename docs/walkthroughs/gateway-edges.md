# Walkthrough: the gateway's edges

The request path (`gateway/proxy`, `gateway/route`, `gateway/limit`, the
dialects) is surrounded by three edges: who is calling
(`gateway/auth`), what configuration the gateway serves
(`gateway/snapshot`), and what it reports afterwards
(`gateway/telemetry`). The binary, `cmd/golem-gateway`, wires them
together and owns their lifecycle. This is a guided reading of those four
packages for an engineer new to the repository; read it with the code
open. The design behind it is in [gateway.md](../gateway.md) (§3 the
request path, §6 the snapshot lifecycle, §6a standalone mode),
[control-plane.md §4](../control-plane.md#4-distribution) (distribution)
and §5 (the emergency denylist),
[model/publication.md](../../model/publication.md) (the publication
model the snapshot source is checked against),
[ADR 0048](../adr/0048-known-spend-by-time-boundary.md) (known spend by a
time boundary), [ADR 0043](../adr/0043-ledger-durability.md) (ledger
durability lives in the collector), [ADR 0044](../adr/0044-budgets-are-controls-not-ceilings.md)
(budgets are controls) and the acceptance record
[m1-security-gates.md](../acceptance/m1-security-gates.md) (golem#4, golem#5).

## The pieces at a glance

| File | What it holds |
|---|---|
| `gateway/auth/auth.go` | `Compile` (a snapshot's entitlements indexed for lookup), `Keys.Authenticate`, `Principal`, `Refusal` |
| `gateway/auth/deny.go` | `Denylist` (the site's emergency denylist, parsed), `DenyFile` (its mounted file, re-read as it changes) |
| `lib/entra/entra.go` | `Verifier`: local verification of Entra v2 tokens over a cached key set (shared with the control API) |
| `gateway/snapshot/holder.go` | `Holder` (the current `Compiled` snapshot behind an atomic pointer, the floor), compilation, upstream transports and the dial check |
| `gateway/snapshot/file.go` | `Dir`, the standalone source: CRD YAML in a directory |
| `gateway/snapshot/bucket.go` | `Bucket`, the published source: a signed pointer in object storage, the local checkpoint and the last-good copy on disk |
| `gateway/snapshot/secrets.go` | `Secrets`, `EnvSecrets` (standalone), `FileSecrets` (mounted Secrets) |
| `gateway/snapshot/loop.go` | `repeat`, both sources' poll loop |
| `gateway/telemetry/telemetry.go` | `Telemetry`: spans, GenAI metrics, the refusal counter |
| `gateway/telemetry/ledger.go` | `Ledger`: golem.spend rows |
| `gateway/telemetry/queue.go` | `LedgerQueue`: the ledger's own bounded, counted log processor |
| `gateway/telemetry/setup.go` | `OTLP`, `SDK`, `Stdout` |
| `gateway/telemetry/state.go` | `ObserveState`: the gateway's state gauges (snapshot age and lease, the pointer overdue, the limiter's outage), read at collection |
| `lib/otlp/otlp.go` | the OTLP/HTTP exporters and the transport rule, shared with golem-control's metrics and audit |
| `cmd/golem-gateway/main.go` | flags, the release gate, wiring (`run` and its steps), the shutdown order |
| `cmd/golem-gateway/guard.go` | `localOnly`, standalone's browser guard |
| `cmd/golem-gateway/lifecycle.go` | readiness, the count of calls in flight, drain and cut |

## 1. Authentication (`gateway/auth`)

Everything is decided from the snapshot, without a network call. When a
snapshot is applied, `Holder.compileAuth` (`gateway/snapshot/holder.go:253`)
calls `auth.Compile` (`gateway/auth/auth.go:93`), which builds a `Keys`:

- `compileScopes` (`auth.go:117`): each scope's `rights`, its allowed
  aliases and its `Limits` (budget, in-flight limits, request policy).
- `addMembers` (`auth.go:140`): users' and services' Entra identities,
  indexed by oid or app ID and then by scope (the empty scope is the
  member's default). Members are also recorded as key owners.
- `keyEntry` (`auth.go:158`): each key by its SHA-256 hash. A key whose
  scope no longer exists, whose owner is unlisted or of another kind, or
  whose owner left the scope gets a precomputed `refused`: it fails
  closed, never with a zero (unlimited) budget. A key's own model list
  narrows its scope's, never widens it.

`compileAuth` then attaches the site's denylist (`Keys.Deny`) and, standalone,
the local key (`RequireLocalKey`).

`Keys.Authenticate` (`auth.go:207`) is called once per request by
`proxy.Handler.authenticate` (`gateway/proxy/proxy.go:368`). Its order:

1. `credential` (`auth.go:313`) reads `Authorization: Bearer` and
   `x-api-key`. Two different credentials are refused rather than one
   silently preferred (`auth.go:209`, golem#5).
2. `principal` (`auth.go:231`) finds the snapshot's principal for the
   credential:
   - The implicit principal. A standalone snapshot without a LocalIdentity
     is `Implicit` (`lib/compile/entitlements.go:30`), and `Compile` makes
     one local principal for it only when the installation is `key.Dev`
     (`auth.go:95`). It admits exactly one credential: the gateway's own
     local key, compared by hash in constant time (`auth.go:237`). Without
     `RequireLocalKey` it admits no one.
   - An Entra token, recognised by shape (`looksLikeJWS`, `auth.go:331`),
     goes to `Keys.token` (`auth.go:283`): `entra.Verifier.Verify` proves who
     is calling, the snapshot says in which scope (`golem-scope` header,
     else the default) and with which rights; a user's token cannot act as
     a service's principal or the reverse (`auth.go:304`).
   - A golem key is looked up by hash only if it carries this
     installation's prefix (`glm_live_` or `glm_dev_`, `auth.go:254`): a
     publisher's mistake must not let another installation's key in.
     Parsing a key is needed only to explain why an unknown one is refused.
3. The denylist (`auth.go:213`): a principal the snapshot admits is still
   refused, with `auth.denied`, if the site's emergency denylist names its
   key, its principal or its Entra ID.

`Verifier.Verify` (`lib/entra/entra.go:105`) takes RS256 only, refuses
tokens once the cached key set is older than `MaxKeyAge` (24 hours by
default, `entra.go:118`), checks issuer, audience, expiry (a minute's
`Leeway`, `entra.go:62`), tenant and version, and then requires either the
`golem.access` scope with an oid (a user) or, with no scope at all, the
`golem.service` role with an azp (a service). An unknown key ID asks `Run`
for a refresh (`entra.go:123`), which `Run` (`entra.go:243`) grants at most
once a minute; it also refreshes hourly. The request path never fetches
keys.

### The emergency denylist (`deny.go`)

For a revocation that cannot wait for the control plane or the bucket, a
site admin edits a ConfigMap mounted as a file (`-denylist`). Its lines
are `key <hex SHA-256>`, `key-prefix <display prefix, 16+ characters>`,
`principal <id>` and `entra <oid or app ID>`, with `#` comments.
`ParseDenylist` (`deny.go:45`) fails the whole file on any line it cannot
read, so a typo is noticed, not skipped. `DenyFile.Load` (`deny.go:127`)
re-reads the file and swaps the parsed list in only when its content
changed and parses; an error leaves the previous list in force (a bad
edit must not clear it). `DenyFile.Run` (`deny.go:157`) reloads every five
seconds, logging a failure once until it changes. `Keys` holds the
`DenyFile`, not a list, so a reload applies to the next request without a
new snapshot. `denies` (`deny.go:94`) matches a key by hash or prefix (a
token is matched by its Entra ID only) and any caller by principal ID.

## 2. The snapshot holder and its sources (`gateway/snapshot`)

`Holder` keeps the current `Compiled` in an `atomic.Pointer`. A request
takes it once with `Current` (`holder.go:79`) and uses it to its end;
`Current` returns `ErrExpired` once the snapshot's authorization lease
(`ValidUntil`) has passed (`holder.go:84`), and the proxy's `authenticate`
then refuses every new request. `Apply` (`holder.go:113`), under the
holder's mutex, refuses a snapshot that is below the floor
(`holder.go:119`), not newer than the one applied (`holder.go:122`),
already expired (`holder.go:125`) or that does not compile
(`holder.go:128`), keeping the previous one. A compile failure is
returned as an `applyError` (`holder.go:132`): it may pass (a Secret
mounted later), and the bucket source retries it. On success it raises the
floor to the snapshot's sequence, swaps the pointer and calls `OnApply`.

The floor is inclusive: it is the lowest publication the gateway may
apply. It starts at zero, is raised by `RaiseFloor` (`holder.go:104`) from
the checkpoints before anything is applied, and follows each apply.

Compilation (`compile`, `holder.go:151`) resolves each backend's
credential through `Secrets`, makes a `route.Target` per deployment
(`targets`, `holder.go:195`), and builds the routes over them
(`compileRoute`, `holder.go:230`). Each deployment's `http.Transport` is
kept across snapshots by its `transportKey` (backend, deployment, scheme,
host, private grant), so connections survive a swap; transports that left
the config have their idle connections closed (`holder.go:167`).
`upstreamTransport` (`holder.go:288`) sets every pool setting deliberately
(no proxy from the environment, per-host idle limits, HTTP/2 pings, no
response-header timeout) and checks every dial's resolved address in the
dialer's `ControlContext` (`holder.go:290`) with `checkDestination`
(`holder.go:319`): cloud metadata, link-local, unspecified and multicast
addresses are refused always; loopback, private and CGNAT addresses
unless the backend is granted `allowPrivateAddresses` (golem#5). Because
the check runs on the connection, a name that later re-resolves somewhere
forbidden is refused at its next dial.

### The standalone source: `Dir`

`Dir.Load` (`file.go:33`) opens the directory with `os.Root` (no symlink
leads outside it), hashes its contents, and when they changed decodes and
compiles them with `lib/compile` (standalone, the next local sequence),
then applies the result. A config that does not compile is reported and
the previous one served. `Dir.Run` (`file.go:71`) reloads every second.

### The published source: `Bucket`

`Bucket` (`bucket.go:29`) reads a pool's signed pointer
(`snapshots/<pool>/current`) and the snapshot object it names.

- `Start` (`bucket.go:75`) raises the floor to the higher of the local
  checkpoint (`localCheckpoint`, `bucket.go:140`: the last sequence this
  gateway applied, in the `-checkpoint` file) and the pool's trusted
  checkpoint in the bucket (`trustedCheckpoint`, `bucket.go:160`:
  `control/<pool>/checkpoint`, signed, written only by golem-control).
  With neither readable it refuses to start: it could not tell a replayed
  pointer from the current one. A pool with no checkpoint and no pointer
  was never published (floor zero); one with a pointer and no checkpoint
  has lost its floor and is refused. This is the model's `TRUSTED_FLOOR`
  (model/publication.md §6.8); the status object is never trusted, since
  anyone with the pool's credentials can write it.
- Then `loadLastGood` (`bucket.go:103`) applies the last-good copy on disk,
  if there is one (`LastGood`, `last-good.json` beside the checkpoint): the
  signed pointer and snapshot exactly as the bucket held them, verified
  again as a fresh read would be (`applyLastGood`, `bucket.go:125`, through
  `verify`, `bucket.go:284`), so a gateway restarted while the bucket is
  unreachable serves until the lease ends. A copy that was altered, is
  below the floor, or is past its lease is logged and left; the bucket's
  next publication replaces it. Until a publication read from the bucket
  replaces it, `ServingLastGood` (`bucket.go:236`) reports that the copy
  from disk is what is served (`golem.snapshot.last_good`).
- `Poll` (`bucket.go:190`) makes a conditional get with the last judged
  entity tag, so an unchanged pointer costs one request.
- `read` (`bucket.go:295`) judges a changed pointer, as traced below.
- `checkOverdue` (`bucket.go:220`) logs, once, that the newest pointer
  seen is past its `Expires`: the publisher has stopped, or the bucket
  shows an old pointer. The gateway serves on until its snapshot's
  `ValidUntil`. The flag is an `atomic.Bool`, so `Overdue`
  (`bucket.go:231`) can be read from a metric's collection
  (`golem.snapshot.pointer_overdue`) while `Poll` writes it.

Both sources loop through `repeat` (`loop.go:12`), which reports a failure
once until it changes or clears rather than every second. `Bucket.Run`
(`bucket.go:396`) polls at once; `Dir.Run` waits an interval, because the
binary has loaded the directory already.

### Worked example: a publication arriving

golem-control publishes sequence 7 for pool `internal`: it writes the
snapshot object, the trusted checkpoint, then the signed pointer
(control-plane.md §4). On a gateway holding sequence 6:

1. `repeat` ticks and calls `Bucket.Poll` (`loop.go:21`, `bucket.go:397`).
2. `Poll` gets the pointer with its tag (`bucket.go:194`); the pointer
   changed, so the get returns the body and a new tag.
3. `read` opens it with `openPointer` (`bucket.go:296`, `bucket.go:252`):
   `snapshot.OpenPointer` (`lib/snapshot/pointer.go:77`) verifies the
   signature against the pinned keys, or the pointer is refused. It must
   be this pool's (`bucket.go:257`), and at least the floor
   (`bucket.go:262`): an old signed pointer copied back stops here, before
   any fetch. Its `Expires` is noted for `checkOverdue` (`bucket.go:300`).
   A pointer for the publication already held returns at
   `bucket.go:303`.
4. The snapshot object is fetched by the pointer's hash
   (`bucket.go:306`). A failed fetch is a `fetchError` (`bucket.go:308`),
   the one failure for which `Poll` does not record the tag
   (`bucket.go:207`), so the next poll reads the pointer again.
5. `openSnapshot` (`bucket.go:310`, `bucket.go:269`): the body must hash
   to the pointer's hash (`bucket.go:270`), decode strictly
   (`bucket.go:273`) and carry the pointer's sequence (`bucket.go:277`): a
   swapped object or a pointer naming another publication's snapshot is
   refused.
6. `Holder.Apply` (`bucket.go:314`, `holder.go:113`) rechecks the floor
   and the order under its mutex, refuses an expired lease, compiles
   (credentials, targets, routes, entitlements with the denylist, prices),
   stores the new `Compiled`, sets the floor to 7 and calls `OnApply` (the
   limiter takes each scope's known spend and boundary, ADR 0048). If the
   compile fails (a Secret not yet mounted), the `applyError` makes `Poll`
   set `retry` (`bucket.go:210`): after `ApplyRetry` (30 seconds by
   default) it forgets the tag (`bucket.go:191`) and reads the unchanged
   pointer again.
7. `saveLastGood` (`bucket.go:320`, `bucket.go:333`) writes the pointer
   and the snapshot to `last-good.json`, then `saveCheckpoint`
   (`bucket.go:323`, `bucket.go:345`) writes "7"; last-good first, so a
   crash between them leaves a copy at or above the floor, never one
   below it. Both go through `writeAtomic` (`bucket.go:353`): a temporary
   file, written and synced, renamed into place, then the directory
   synced, so a power loss cannot bring back the older checkpoint. A
   failure of either is logged, not returned: the snapshot is applied, and
   a restart's floor will be the older checkpoint (or the trusted one).
8. `writeStatus` (`bucket.go:327`, `bucket.go:384`) writes
   `status/internal/<gateway-id>` for golem-control's diagnostics.
   Requests that took sequence 6 before the swap finish on it; new ones
   take 7. Back in `Poll`, the tag is recorded (`bucket.go:208`): every
   outcome but a failed fetch or a failed apply is final until the
   pointer changes.

`publication_mbt_test.go` drives exactly this code (with the real
publisher, elector and a directory bucket) from model/publication.qnt's
traces, including replays and empty-disk restarts, and after each step
compares every gateway's applied publication and floor with the model's.

## 3. Telemetry (`gateway/telemetry`)

`Telemetry.Start` (`telemetry.go:168`) opens the request's server span;
`Ended` (`telemetry.go:183`) names it "chat <alias>", sets the GenAI and
golem attributes, records the token and duration histograms and writes
the ledger row; `Refused` (`telemetry.go:216`) marks the span with the
contract code and counts the refusal. Attribute sets are built once per
route, frontend and refusal code (`cached`, `telemetry.go:110`), so a
call allocates little and a 429 storm nothing for its counter. Every
method is a no-op on a nil `*Telemetry`.

The ledger (`ledger.go`) writes OTel log records whose attributes are
the ClickHouse table's columns: a `golem.spend` row per upstream attempt
when the call ends (`Ledger.Ended`, `ledger.go:36`; `row`, `ledger.go:55`:
39 columns in the DDL's order, built in an array on the stack and checked
by `TestRowMatchesLedgerDDL`), whatever the site counter's state (ADR
0043): a counter outage keeps no billing rows in memory. A row's period is
the limiter's when given, else the call's start in the configured zone
(`ledger.go:57`), so the counter and the ledger agree on the month.

`LedgerQueue` (`queue.go:20`) is the ledger's log processor, because the
SDK's batch processor silently overwrites the oldest records when full.
Its guarantees:

- `OnEmit` (`queue.go:66`) never blocks: a row is queued or counted in
  `Dropped`.
- A failed export is retried with doubling backoff, eight attempts, about
  four minutes (`export`, `queue.go:146`); a batch is resent whole and the
  ledger deduplicates by event. Rows given up are counted in `Failed`.
- `Shutdown` (`queue.go:184`) closes the queue under the same lock
  `OnEmit` holds shared, so every row accepted is queued before the
  worker's last drain and every later one is counted as dropped. It
  drains while its context lasts; the context's end cancels exports in
  flight, and whatever is left is counted as failed. So delivered +
  failed + dropped = emitted.
- `ForceFlush` (`queue.go:205`) never waits on a stopped worker.

Durability through a collector outage is the collector's (ADR 0043): the
queue (65,536 rows, `ledgerQueueSize`) bridges a node-local agent's restart.

`OTLP` (`setup.go:31`) exports everything over OTLP/HTTP through
`lib/otlp`, with TLS unless the endpoint is a literal loopback address
(`otlp.PlaintextOK`): `localhost` is a name and gets TLS; each exporter
gets its whole URL, so the `OTEL_EXPORTER_OTLP_*` variables cannot turn
TLS off. `SDK` (`setup.go:44`) puts the ledger's queue under the logger
provider and shuts down logs first, traces next and metrics last, so the
final drop and failure counts are exported after the rows they count.
`Stdout` (`setup.go:67`) is standalone's: no spans or metrics, ledger rows
as JSON lines. Standalone has no meter, so a row it cannot encode or write
is counted in `jsonLogger.failed` and logged at each power of two
(`setup.go:113`) rather than lost silently.

**State gauges.** `ObserveState` (`state.go:51`) registers observable
instruments that read the gateway's state when the meter collects (every
minute over OTLP), so nothing is added to a request:
`golem.snapshot.age` (now − the applied snapshot's `IssuedAt`, through
`Holder.Applied`, `holder.go:92`), `golem.snapshot.lease_remaining`
(`ValidUntil` − now, negative once run out), `golem.snapshot.seq`, the bucket
source's `golem.snapshot.pointer_overdue` and `golem.snapshot.last_good`
(0/1), the limiter's `golem.limit.down` (0/1), `golem.limit.waivers`
(`Limiter.Waived`, counted where a waived ticket is made, `limit.go:279`),
`golem.limit.held` (`Limiter.Held`, under the limiter's lock for a
`len`) and `golem.limit.dropped`, and `golem.denylist.entries`. They carry
no attributes of their own: the resource's site and pool name the
gateway's. The alerts over them are operations.md §2.2's.
`TestStateGauges` reads them through a manual reader with an injected
clock: a snapshot's age and lease, then the store down (a waiver, a held
increment, a lease run out).

## 4. The binary's lifecycle (`cmd/golem-gateway`)

`parse` (`main.go:64`) reads the flags and applies `releaseGate`
(`main.go:398`). The gateway has two modes:

- **standalone** (`-standalone <dir>`, ADR 0023): only on a loopback
  address; the in-memory limiter; dev keys, with the local key
  (`loadLocalKey`, `main.go:429`: a dev key in a 0600 file, kept across
  restarts); the directory source, which must load once before the server
  starts; provider credentials from the environment;
- **pool** (`-pool <name>`): `-bucket`, `-keys`, `-valkey` and `-site` are
  required, and until golem#7's deployment evidence is in, so is
  `-accept-unreleased`. `poolSource` (`main.go:283`) opens the bucket
  (`s3bucket.Open`; a directory for tests), loads the trusted publishing
  keys (`sign.LoadKeys`: a directory of PEM public keys, a KMS key's named
  by its `Key-ID` header), opens the Secrets mounted under `-secrets`
  (`FileSecrets`, live keys only), names the gateway (`-id`, else the host
  name) and starts the bucket source: `Start` raises the floor from the
  local checkpoint (`-checkpoint`) and the pool's signed trusted
  checkpoint, refusing to start with neither, and applies the last-good
  copy beside the checkpoint (`lastGoodPath`, `main.go:462`); then one
  `Poll`, whose failure is logged, not fatal. The limiter is over the
  site's Valkey (`limit.NewValkey`, `-valkey-replica-acks` for WAIT).

`run` (`main.go:99`) builds the parts in dependency order, each a named
step: telemetry (`setupTelemetry`, `main.go:363`, labelled with the site
and pool), the Entra verifier if a tenant is given (`newEntra`,
`main.go:318`), the limiter over its store (`newLimiter`, `main.go:200`),
the holder that hands each snapshot's known spend to the limiter
(`newHolder`, `main.go:220`), the denylist if one is named (loaded before
the source, `main.go:142`, so neither a first request nor the last-good
publication is ever compiled without it), the snapshot source
(`startSource`, `main.go:231`: standalone's directory with the dev key, or
`poolSource`), and the routes (`newMux`, `main.go:252`) on the server
(`newServer`, `main.go:337`). Every background loop (the source, the
denylist, the limiter, Entra's refresh) runs under `bg` in one
`WaitGroup`. Once the source has started, the state gauges are
registered over the holder, the bucket source, the limiter and the
denylist (`telemetry.ObserveState`, `main.go:162`; with any telemetry but
`off`, and exported only over OTLP). `announce` (`main.go:269`) says what
is served.

Standalone's API handlers are wrapped as `track(localOnly(...))`; pool
mode's as `track(...)`. `localOnly` (`guard.go:24`) keeps the developer's
browser out: a loopback `Host` only (DNS rebinding), no cross-origin
request by the browser's own headers, and a JSON body only (no
preflight-free page can send one). `track` (`lifecycle.go:36`) counts
calls in flight under a mutex (`begin`, `end`): not a `WaitGroup`, because
after `srv.Close` a handler whose connection was already accepted can
still start while the wait runs. Once `wait` (`lifecycle.go:69`) has begun
a call that starts is refused with a 503 rather than counted after the
count was taken. `/readyz` (`lifecycle.go:82`) fails while draining and
while the holder has no current snapshot or its lease has expired.
`TestPoolMode` runs pool mode end to end: golem-control's publisher writes
a snapshot to a bucket, and the gateway serves a key's call from it, with
the provider credential from a mounted Secret, its spend in a real Valkey
and its local checkpoint written.

### Worked example: shutdown with a stream in flight

SIGTERM ends `sig` (`main.go:128`). The shutdown goroutine
(`main.go:176`) runs `lifecycle.shutdown` (`lifecycle.go:98`):

1. `draining` is set, so `/readyz` answers 503, and the gateway sleeps
   `-unready` for the endpoints to stop sending.
2. `srv.Shutdown` closes the listeners (`ListenAndServe` returns at
   `main.go:183`) and waits up to `-drain` for calls to finish.
3. At the deadline `srv.Close` (`lifecycle.go:107`) cuts the remaining
   connections; the stream's context ends and the proxy accounts it as a
   cancellation, under `context.WithoutCancel`, its row and counter add
   still written.
4. `shutdown` waits for `lc.wait()` (`lifecycle.go:110`), the last
   counted call's end, up to `cut` (10 seconds); a call that starts now is
   refused.

Only then (`<-drained`, `main.go:188`) does `run` stop the background loops
(`stopBg`, `wg.Wait`, `main.go:189`; they ran through the drain because
the limiter's renewals keep calls' slots), let the limiter finish
(`main.go:192`), close the Valkey client (`closeStore`, deferred) and shut
telemetry down last (`shutdownTelemetry`, deferred at `main.go:119`),
delivering the rows the cut calls wrote. `TestShutdownAccounts` checks
this end to end: the cut stream leaves one row with outcome `cancelled`,
its cost in the counter. `TestTrackAfterWait` checks the count: a call
after the wait began is refused, and the wait ends with the last call
that started before it.

## Invariants: where each is enforced and tested

| Invariant | Enforced | Tested |
|---|---|---|
| A snapshot below the floor, or not newer than the one held, is never applied | `Holder.Apply` (`holder.go:119`, `:122`); `Bucket.openPointer` (`bucket.go:262`) | `TestHolder`, `TestRaiseFloor`, `TestBucketSource`, `TestPublicationModel` |
| A restart with an empty disk takes the trusted checkpoint as its floor; with no floor at all it does not start | `Bucket.Start`, `trustedCheckpoint` | `TestBucketSource` (restarts), `TestPublicationModel` (`restartEmpty`) |
| Only a correctly signed pointer of this pool, naming an object with its hash and sequence, is applied | `Bucket.openPointer`, `openSnapshot` | `TestBucketSource` |
| A restart while the bucket is unreachable serves the last publication applied, verified again; an altered copy, one below the floor or past its lease is not served | `Bucket.loadLastGood`, `applyLastGood`, `verify` | `TestLastGood` |
| The last-good copy is written before the checkpoint, and both durably (file and directory synced) | `Bucket.read`, `writeAtomic` | not directly: the order and the syncs are untested; `TestPoolMode` checks the checkpoint is written |
| A verified publication that failed to compile is retried while current; a refused one is not | `Holder.Apply` (`applyError`), `Bucket.Poll` | `TestApplyRetried` |
| A pointer past its `Expires` is reported once, and the gateway serves on | `Bucket.checkOverdue` | `TestOverdue` |
| The last-good copy served is reported until a bucket publication replaces it | `Bucket.ServingLastGood` | `TestLastGood` |
| The snapshot's age, lease remaining and the limiter's outage are reported at collection, with nothing on a request | `ObserveState` (observable instruments only) | `TestStateGauges`, `TestStateGaugesBeforeASnapshot`; `BenchmarkHandler*` unchanged |
| An expired lease serves nothing new | `Holder.Current`, `Holder.Apply` | `TestHolder`, `TestPublicationModel` (`serve`) |
| A config that does not compile keeps the previous one | `Holder.Apply`, `Dir.Load` | `TestDir` |
| Upstream dials to forbidden addresses are refused, at every connection | `checkDestination` in the dialer | `TestCheckDestination`, `TestDialRefused`, `TestDialReresolved` |
| The upstream pool's settings are chosen, not inherited | `upstreamTransport` | `TestUpstreamTransport` |
| A key is honoured only for a listed owner of its kind in its scope; a missing scope fails closed | `keyEntry` | `TestAuthenticate` |
| Two different credentials are refused; no malformed credential authenticates | `credential`, `Authenticate` | `TestAuthenticate`, `FuzzAuthenticate` |
| The implicit principal exists only for dev, and takes only the local key | `Compile`, `principal` | `TestImplicit`, `TestLocalKey` |
| A user's token cannot act as a service's principal, or the reverse | `Keys.token` | `TestEntra` |
| Entra tokens are refused once the key set is too old; a key fetched by a refresh is accepted | `entra.Verifier.Verify`, `Refresh` | `TestStaleKeys`, `TestRefresh` |
| A caller the denylist names is refused whatever the snapshot says; a bad edit keeps the list in force | `Authenticate`, `Denylist.denies`, `DenyFile.Load` | `TestDenylist` |
| Every ledger row is delivered, dropped or failed, and counted | `LedgerQueue` | `TestLedgerQueue*` (shutdown races, emission during shutdown, flush during shutdown, retries, partial delivery) |
| Standalone's ledger rows on stdout are written whole or counted as lost | `jsonLogger.Emit` | `TestStdoutCountsLostRows` |
| A row has golem.spend's columns in order, and the counter's period | `Ledger.row` | `TestRowMatchesLedgerDDL`, `TestRowPeriodIsTheCounters` |
| OTLP uses TLS unless the collector is a literal loopback address, whatever the environment says | `otlp.PlaintextOK`, `otlp.New` | `TestPlaintextOK`, `TestOTLPIgnoresInsecureEnv` (lib/otlp) |
| A stream cut at shutdown still leaves its row and counter add | `lifecycle.shutdown`, the order in `run` | `TestShutdownAccounts` |
| No call starts uncounted once the shutdown's wait has begun | `lifecycle.begin`, `wait` | `TestTrackAfterWait` |
| Standalone answers only loopback, same-origin JSON requests | `localOnly` | `TestLocalOnly` |
| Pool mode needs its inputs, and `-accept-unreleased` until golem#7 | `releaseGate` | `TestReleaseGate` |
| A pool's published snapshot is served, its credential from a Secret, its spend in Valkey | `poolSource`, `run` | `TestPoolMode` |
| A source reports a failure once, not every second | `repeat` | `TestRepeat` |

## Known gaps

- Pool mode runs only with `-accept-unreleased` until golem#7's evidence
  is in.
- A last-good copy that fails to apply at start (a Secret not yet
  mounted) is not retried from disk: the gateway waits for the bucket.
- An overdue pointer is a log line and a metric
  (`golem.snapshot.pointer_overdue`), not a readiness signal: a gateway
  serves its snapshot until the lease ends, by design.
- The state gauges carry no gateway identity of their own: the resource
  names the site and pool, and telling one pod's series from another's is
  left to the collector (its Kubernetes attributes), as for the
  gateway's other metrics. Standalone's stdout telemetry exports no
  metrics.
- A call refused by `track` once the shutdown's wait has begun gets a
  plain-text 503 ("shutting down"), not the dialect's error shape.
- OTLP's TLS is tested at the decision, not against a collector.
- Standalone's stdout ledger counts and logs a row it could not write, but
  does not retry it.
- The m1 acceptance record's "Not yet shown" lists the rest: client
  certificates for OTLP, OpenAI's restricted mode, provider-account
  spending limits (ADR 0044).
