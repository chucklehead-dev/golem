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
[control-plane.md §4](../control-plane.md#4-distribution) (distribution),
[model/publication.md](../../model/publication.md) (the publication
model the snapshot source is checked against),
[ADR 0042](../adr/0042-known-spend-by-counter-watermark.md) (known spend by
counter watermark), [ADR 0043](../adr/0043-ledger-durability.md) (ledger
durability lives in the collector), [ADR 0044](../adr/0044-budgets-are-controls-not-ceilings.md)
(budgets are controls) and the acceptance record
[m1-security-gates.md](../acceptance/m1-security-gates.md) (golem#4, golem#5).

## The pieces at a glance

| File | What it holds |
|---|---|
| `gateway/auth/auth.go` | `Compile` (a snapshot's entitlements indexed for lookup), `Keys.Authenticate`, `Principal`, `Refusal` |
| `lib/entra/entra.go` | `Verifier`: local verification of Entra v2 tokens over a cached key set (shared with the control API) |
| `gateway/snapshot/holder.go` | `Holder` (the current `Compiled` snapshot behind an atomic pointer, the floor), compilation, upstream transports and the dial check |
| `gateway/snapshot/file.go` | `Dir`, the standalone source: CRD YAML in a directory |
| `gateway/snapshot/bucket.go` | `Bucket`, the published source: a signed pointer in object storage |
| `gateway/snapshot/secrets.go` | `Secrets`, `EnvSecrets` (standalone), `FileSecrets` (mounted Secrets) |
| `gateway/snapshot/loop.go` | `repeat`, both sources' poll loop |
| `gateway/telemetry/telemetry.go` | `Telemetry`: spans, GenAI metrics, the refusal counter |
| `gateway/telemetry/ledger.go` | `Ledger`: golem.spend rows and golem.spend_seq numbering records |
| `gateway/telemetry/queue.go` | `LedgerQueue`: the ledger's own bounded, counted log processor |
| `gateway/telemetry/setup.go` | `OTLP`, `SDK`, `Stdout`, the plaintext rule |
| `cmd/golem-gateway/main.go` | flags, the release gate, wiring, the shutdown order |
| `cmd/golem-gateway/guard.go` | `localOnly`, standalone's browser guard |
| `cmd/golem-gateway/lifecycle.go` | readiness, drain and cut |

## 1. Authentication (`gateway/auth`)

Everything is decided from the snapshot, without a network call. When a
snapshot is applied, `Holder.compileAuth` (`gateway/snapshot/holder.go:243`)
calls `auth.Compile` (`gateway/auth/auth.go:89`), which builds a `Keys`:

- `compileScopes` (`auth.go:113`): each scope's `rights`, its allowed
  aliases and its `Limits` (budget, in-flight limits, request policy).
- `addMembers` (`auth.go:136`): users' and services' Entra identities,
  indexed by oid or app ID and then by scope (the empty scope is the
  member's default). Members are also recorded as key owners.
- `keyEntry` (`auth.go:154`): each key by its SHA-256 hash. A key whose
  scope no longer exists, whose owner is unlisted or of another kind, or
  whose owner left the scope gets a precomputed `refused`: it fails
  closed, never with a zero (unlimited) budget. A key's own model list
  narrows its scope's, never widens it.

`Keys.Authenticate` (`auth.go:198`) is called once per request by
`proxy.Handler.authenticate` (in `gateway/proxy/proxy.go`). Its order:

1. `credential` (`auth.go:280`) reads `Authorization: Bearer` and
   `x-api-key`. Two different credentials are refused rather than one
   silently preferred (golem#5).
2. The implicit principal. A standalone snapshot without a LocalIdentity
   is `Implicit` (`lib/compile/entitlements.go:30`), and `Compile` makes
   one local principal for it only when the installation is `key.Dev`
   (`auth.go:91`). It admits exactly one credential: the gateway's own
   local key, compared by hash in constant time (`auth.go:208`). Without
   `RequireLocalKey` it admits no one.
3. An Entra token, recognised by shape (`looksLikeJWS`, `entra.go:242`),
   goes to `Keys.token` (`auth.go:254`): `entra.Verifier.Verify` proves who is
   calling, the snapshot says in which scope (`golem-scope` header, else
   the default) and with which rights; a user's token cannot act as a
   service's principal or the reverse (`auth.go:271`).
4. A golem key is looked up by hash only if it carries this
   installation's prefix (`glm_live_` or `glm_dev_`): a publisher's
   mistake must not let another installation's key in. Parsing a key is
   needed only to explain why an unknown one is refused.

`Verifier.Verify` (lib/entra) takes RS256 only, refuses tokens once the
cached key set is older than `MaxKeyAge` (24 hours by default,
`entra.go:96`), checks issuer, audience, expiry (a minute's `Leeway`),
tenant and version, and then requires either the `golem.access` scope
with an oid (a user) or, with no scope at all, the `golem.service` role
with an azp (a service). An unknown key ID asks `Run` for a refresh
(`entra.go:101`), which `Run` grants at most once a minute; it also
refreshes hourly. The request path never fetches keys.

## 2. The snapshot holder and its sources (`gateway/snapshot`)

`Holder` keeps the current `Compiled` in an `atomic.Pointer`. A request
takes it once with `Current` (`holder.go:75`) and uses it to its end;
`Current` returns `ErrExpired` once the snapshot's authorization lease
(`ValidUntil`) has passed, and the proxy's `authenticate` then refuses
every new request. `Apply` (`holder.go:105`), under the holder's mutex,
refuses a snapshot that is below the floor (`holder.go:111`), not newer
than the one applied (`holder.go:114`), already expired (`holder.go:117`)
or that does not compile, keeping the previous one. On success it raises
the floor to the snapshot's sequence and swaps the pointer.

The floor is inclusive: it is the lowest publication the gateway may
apply. It starts at zero, is raised by `RaiseFloor` from a trusted
checkpoint before anything is applied, and follows each apply.

Compilation (`compile`, `holder.go:141`) resolves each backend's
credential through `Secrets`, makes a `route.Target` per deployment, and
builds the routes over them. Each deployment's `http.Transport` is kept
across snapshots by its `transportKey` (backend, deployment, scheme,
host, private grant), so connections survive a swap; transports that left
the config have their idle connections closed. `upstreamTransport`
(`holder.go:277`) sets every pool setting deliberately (no proxy from the
environment, per-host idle limits, HTTP/2 pings, no response-header
timeout) and checks every dial's resolved address in the dialer's
`ControlContext` (`holder.go:279`) with `checkDestination`
(`holder.go:308`): cloud metadata, link-local, unspecified and multicast
addresses are refused always; loopback, private and CGNAT addresses
unless the backend is granted `allowPrivateAddresses` (golem#5). Because
the check runs on the connection, a name that later re-resolves somewhere
forbidden is refused at its next dial.

### The standalone source: `Dir`

`Dir.Load` (`file.go:33`) opens the directory with `os.Root` (no symlink
leads outside it), hashes its contents, and when they changed decodes and
compiles them with `lib/compile` (standalone, the next local sequence),
then applies the result. A config that does not compile is reported and
the previous one served. `Dir.Run` reloads every second.

### The published source: `Bucket`

`Bucket` (`bucket.go:29`) reads a pool's signed pointer
(`snapshots/<pool>/current`) and the snapshot object it names.

- `Start` (`bucket.go:56`) raises the floor to the higher of the local
  checkpoint file (the last sequence this gateway applied) and the pool's
  trusted checkpoint in the bucket (`control/<pool>/checkpoint`, written
  only by golem-control). With neither readable it refuses to start: it
  could not tell a replayed pointer from the current one. This is the
  model's `TRUSTED_FLOOR` (model/publication.md §6.8); the status object
  is never trusted, since anyone with the pool's credentials can write it.
- `Poll` (`bucket.go:111`) makes a conditional get with the last judged
  entity tag, so an unchanged pointer costs one request.
- `read` (`bucket.go:136`) judges a changed pointer, as traced below.

Both sources loop through `repeat` (`loop.go:12`), which reports a failure
once until it changes or clears rather than every second. `Bucket.Run`
polls at once; `Dir.Run` waits an interval, because `main` has loaded the
directory already.

### Worked example: a publication arriving

golem-control publishes sequence 7 for pool `internal`: it writes the
snapshot object, the trusted checkpoint, then the signed pointer
(control-plane.md §4). On a gateway holding sequence 6:

1. `repeat` ticks and calls `Bucket.Poll` (`bucket.go:216`, `loop.go:21`).
2. `Poll` gets the pointer with its tag; the pointer changed, so the get
   returns the body and a new tag (`bucket.go:112`).
3. `read` opens it with `snapshot.OpenPointer` (`bucket.go:137`,
   `lib/snapshot/pointer.go:63`): the signature must verify against the
   pinned keys, or the pointer is refused. It must be this pool's
   (`bucket.go:141`), and at least the floor (`bucket.go:146`): an old
   signed pointer copied back stops here, before any fetch. A pointer for
   the publication already held returns at `bucket.go:149`.
4. The snapshot object is fetched by the pointer's hash
   (`bucket.go:152`). A failed fetch is a `fetchError`, the one failure
   for which `Poll` does not record the tag (`bucket.go:122`), so the
   next poll reads the pointer again. Every other outcome is final until
   the pointer changes.
5. The body must hash to the pointer's hash (`bucket.go:156`), decode
   strictly (`bucket.go:159`) and carry the pointer's sequence
   (`bucket.go:163`): a swapped object or a pointer naming another
   publication's snapshot is refused.
6. `Holder.Apply` (`bucket.go:166`, `holder.go:105`) rechecks the floor
   and the order under its mutex, refuses an expired lease, compiles
   (credentials, targets, routes, entitlements, prices), stores the new
   `Compiled`, sets the floor to 7 and calls `OnApply` (the limiter takes
   each scope's known spend and watermark, ADR 0042).
7. `saveCheckpoint` (`bucket.go:179`) writes "7" to a temporary file,
   syncs it and renames it over the checkpoint. A failure is logged, not
   returned: the snapshot is applied, and a restart's floor will be the
   older checkpoint (or the trusted one).
8. `writeStatus` (`bucket.go:203`) writes `status/internal/<gateway-id>`
   for golem-control's diagnostics. Requests that took sequence 6 before
   the swap finish on it; new ones take 7.

`publication_mbt_test.go` drives exactly this code (with the real
publisher, elector and a directory bucket) from model/publication.qnt's
traces, including replays and empty-disk restarts, and after each step
compares every gateway's applied publication and floor with the model's.

## 3. Telemetry (`gateway/telemetry`)

`Telemetry.Start` (`telemetry.go:170`) opens the request's server span;
`Ended` (`telemetry.go:185`) names it "chat <alias>", sets the GenAI and
golem attributes, records the token and duration histograms and writes
the ledger row; `Refused` (`telemetry.go:218`) marks the span with the
contract code and counts the refusal. Attribute sets are built once per
route, frontend and refusal code (`cached`, `telemetry.go:112`), so a
call allocates little and a 429 storm nothing for its counter. Every
method is a no-op on a nil `*Telemetry`.

The ledger (`ledger.go`) writes OTel log records whose attributes are
the ClickHouse tables' columns: a `golem.spend` row per upstream attempt
when the call ends (`Ledger.Ended`, `ledger.go:37`; 40 columns in the
DDL's order, checked by `TestRowMatchesLedgerDDL`), and, for a call the
limiter counted (`counted`), a separate `golem.spend_seq` numbering
record with its counter number (`Ledger.Numbered`, `ledger.go:45`). A row
is never held for its number (ADR 0043): a counter outage keeps no
billing rows in memory. A row's period is the limiter's when given, else
the call's start in the configured zone (`ledger.go:71`), so the counter
and the ledger agree on the month.

`LedgerQueue` (`queue.go:20`) is the ledger's log processor, because the
SDK's batch processor silently overwrites the oldest records when full.
Its guarantees:

- `OnEmit` never blocks: a row is queued or counted in `Dropped`
  (`queue.go:66`).
- A failed export is retried with doubling backoff, eight attempts, about
  four minutes (`queue.go:142`); a batch is resent whole and the ledger
  deduplicates by event. Rows given up are counted in `Failed`.
- `Shutdown` (`queue.go:180`) closes the queue under the same lock
  `OnEmit` holds shared, so every row accepted is queued before the
  worker's last drain and every later one is counted as dropped. It
  drains while its context lasts; the context's end cancels exports in
  flight, and whatever is left is counted as failed. So delivered +
  failed + dropped = emitted.
- `ForceFlush` never waits on a stopped worker.

Durability through a collector outage is the collector's (ADR 0043): the
queue (65,536 rows, `setup.go:88`) bridges a node-local agent's restart.

`OTLP` (`setup.go:36`) exports everything over OTLP/HTTP, with TLS unless
the endpoint is a literal loopback address (`plaintextOK`,
`setup.go:156`): `localhost` is a name and gets TLS. `SDK` puts the
ledger's queue under the logger provider and shuts down logs first,
traces next and metrics last, so the final drop and failure counts are
exported after the rows they count (`setup.go:81`). `Stdout` is
standalone's: no spans or metrics, ledger rows as JSON lines.

## 4. The binary's lifecycle (`cmd/golem-gateway`)

`main` (`main.go:39`) first applies `releaseGate` (`main.go:208`): until
M1's gates pass (golem#5, golem#7) it runs only with `--standalone` and
only on a loopback address. It then sets up telemetry, the in-memory
limiter (both in one period zone), the Entra verifier if a tenant is
given (its keys fetched before start), the local key (`loadLocalKey`,
`main.go:221`: a dev key in a 0600 file, kept across restarts), the
holder and the directory source, which must load once before the server
starts.

Every API handler is wrapped as `track(localOnly(...))`. `localOnly`
(`guard.go:24`) keeps the developer's browser out: a loopback `Host` only
(DNS rebinding), no cross-origin request by the browser's own headers,
and a JSON body only (no preflight-free page can send one). `track`
(`lifecycle.go:29`) counts calls in flight. `/readyz` fails while
draining and while the holder has no current snapshot.

### Worked example: shutdown with a stream in flight

SIGTERM ends `sig` (`main.go:94`). The shutdown goroutine
(`main.go:106`) runs `lifecycle.shutdown` (`lifecycle.go:54`):

1. `draining` is set, so `/readyz` answers 503, and the gateway sleeps
   `-unready` for the endpoints to stop sending.
2. `srv.Shutdown` closes the listeners (`ListenAndServe` returns at
   `main.go:116`) and waits up to `-drain` for calls to finish.
3. At the deadline `srv.Close` cuts the remaining connections; the
   stream's context ends and the proxy accounts it as a cancellation,
   under `context.WithoutCancel`, its row and counter add still written.
4. `shutdown` waits for `inflight` up to `cut` (10 seconds).

Only then does `main` stop the background loops (`main.go:122`; they ran
through the drain because the limiter's renewals keep calls' slots), let
the limiter finish (`main.go:125`), and shut telemetry down last
(`main.go:127`), delivering the rows the cut calls wrote.
`TestShutdownAccounts` checks this end to end: the cut stream leaves one
row with outcome `cancelled` and its numbering record.

## Invariants: where each is enforced and tested

| Invariant | Enforced | Tested |
|---|---|---|
| A snapshot below the floor, or not newer than the one held, is never applied | `Holder.Apply` (`holder.go:111`, `:114`); `Bucket.read` (`bucket.go:146`) | `TestHolder`, `TestRaiseFloor`, `TestBucketSource`, `TestPublicationModel` |
| A restart with an empty disk takes the trusted checkpoint as its floor; with no floor at all it does not start | `Bucket.Start` | `TestBucketSource` (restarts), `TestPublicationModel` (`restartEmpty`) |
| Only a correctly signed pointer of this pool, naming an object with its hash and sequence, is applied | `Bucket.read` | `TestBucketSource` |
| An expired lease serves nothing new | `Holder.Current`, `Holder.Apply` | `TestHolder`, `TestPublicationModel` (`serve`) |
| A config that does not compile keeps the previous one | `Holder.Apply`, `Dir.Load` | `TestDir` |
| Upstream dials to forbidden addresses are refused, at every connection | `checkDestination` in the dialer | `TestCheckDestination`, `TestDialRefused`, `TestDialReresolved` |
| A key is honoured only for a listed owner of its kind in its scope; a missing scope fails closed | `keyEntry` | `TestAuthenticate` |
| Two different credentials are refused | `credential` | `TestAuthenticate` |
| The implicit principal exists only for dev, and takes only the local key | `Compile`, `Authenticate` | `TestImplicit`, `TestLocalKey` |
| Entra tokens are refused once the key set is too old | `entra.Verifier.Verify` | `TestStaleKeys` |
| Every ledger row is delivered, dropped or failed, and counted | `LedgerQueue` | `TestLedgerQueue*` (shutdown races, emission during shutdown, flush during shutdown, retries, partial delivery) |
| OTLP uses TLS unless the collector is a literal loopback address | `plaintextOK` | `TestPlaintextOK` |
| A stream cut at shutdown still leaves its row and counter add | `lifecycle.shutdown`, the order in `main` | `TestShutdownAccounts` |
| Standalone answers only loopback, same-origin JSON requests | `localOnly` | `TestLocalOnly` |
| Production publication stays disabled | `releaseGate` | `TestReleaseGate` |
| A source reports a failure once, not every second | `repeat` | `TestRepeat` |

## Known gaps

- The binary has no bucket source yet: `Bucket` is built and tested, but
  `main` refuses to run without `--standalone` (the release gate), and
  `FileSecrets`, the production credential reader, is unused and
  untested. The on-disk last-good snapshot and the emergency denylist
  (gateway.md §6) are not built.
- A snapshot refused by `Holder.Apply` for a reason that may pass (a
  Secret not yet mounted) is still judged final for that pointer: the
  gateway retries only when the pointer changes, at the next publication
  or lease renewal.
- OTLP's TLS is tested at the decision, not against a collector; and the
  exporters still read the standard `OTEL_EXPORTER_OTLP_*INSECURE`
  environment variables, which can turn TLS off for a non-loopback
  collector.
- The checkpoint's rename is not followed by a sync of its directory: a
  power loss can bring back the previous sequence (the trusted checkpoint
  still raises the floor if the bucket is readable at start).
- Standalone's JSON-lines ledger ignores write errors on stdout.
- The m1 acceptance record's "Not yet shown" lists the rest: client
  certificates for OTLP, OpenAI's restricted mode, provider-account
  spending limits (ADR 0044).
