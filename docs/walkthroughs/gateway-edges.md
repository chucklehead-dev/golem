# Walkthrough: the gateway's edges

The request path (`gateway/proxy`, `gateway/route`, `gateway/limit`, the
dialects) is surrounded by three edges: who is calling
(`gateway/auth`, with `gateway/guard`'s floor beneath it), what
configuration the gateway serves (`gateway/snapshot`, down to each
deployment's `route.Target`, its health and its envelope), and what it
reports afterwards (`gateway/telemetry`). The binary, `cmd/golem-gateway`,
wires them together and owns their lifecycle. This is a guided reading of
those packages for an engineer new to the repository; read it with the code
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
| `gateway/guard/guard.go` | `Guard`: each source's requests and failed authentications before authentication |
| `gateway/guard/listener.go` | `Listener`: connections open at once, in all and from one address |
| `lib/entra/entra.go` | `Verifier`: local verification of Entra v2 tokens over a cached key set (shared with the control API) |
| `gateway/snapshot/holder.go` | `Holder` (the current `Compiled` snapshot behind an atomic pointer, the floor), compilation, upstream transports and the dial check |
| `gateway/snapshot/file.go` | `Dir`, the standalone source: CRD YAML in a directory |
| `gateway/snapshot/bucket.go` | `Bucket`, the published source: a signed pointer in object storage, the local checkpoint and the last-good copy on disk |
| `gateway/snapshot/secrets.go` | `Secrets`, `EnvSecrets` (standalone), `FileSecrets` (mounted Secrets) |
| `gateway/snapshot/loop.go` | `repeat`, both sources' poll loop |
| `gateway/route/route.go` | `Target` (one deployment as the request path uses it), `Route`, `Table` |
| `gateway/route/health.go` | `Health`: each deployment's ceilings (in all and per principal), ejection and probe, each route's retry budget |
| `gateway/route/check.go` | `Checker`: the active health checks of deployments that have one |
| `gateway/envelope/envelope.go` | `Attribution`: an attempt's IDs and caller, for an envelope to pass on |
| `gateway/envelope/bedrock/bedrock.go` | `Transport`: the Bedrock envelope, an `http.RoundTripper` |
| `gateway/telemetry/telemetry.go` | `Telemetry`: spans, GenAI metrics, the refusal counter |
| `gateway/telemetry/ledger.go` | `Ledger`: golem.spend rows |
| `gateway/telemetry/queue.go` | `LedgerQueue`: the ledger's own bounded, counted log processor |
| `gateway/telemetry/setup.go` | `OTLP`, `SDK`, `Stdout` |
| `gateway/telemetry/state.go` | `ObserveState`: the gateway's state gauges (snapshot age and lease, the pointer overdue, the limiter's outage), read at collection |
| `lib/otlp/otlp.go` | the OTLP/HTTP exporters and the transport rule, shared with golem-control's metrics and audit |
| `cmd/golem-gateway/main.go` | flags, the release gate, wiring (`run` and its steps), the shutdown order |
| `cmd/golem-gateway/guard.go` | `localOnly` and `loopbackOnly`, standalone's browser guard |
| `cmd/golem-gateway/lifecycle.go` | readiness, the count of calls in flight, drain and cut |

## 1. Authentication (`gateway/auth`)

Everything is decided from the snapshot, without a network call. When a
snapshot is applied, `Holder.compileAuth` (`gateway/snapshot/holder.go:274`)
calls `auth.Compile` (`gateway/auth/auth.go:112`), which builds a `Keys`:

- `compileScopes` (`auth.go:136`): each scope's `rights`, its allowed
  aliases and its `Limits` (budget, in-flight limits, request policy).
- `addMembers` (`auth.go:169`): users' and services' Entra identities,
  indexed by oid or app ID and then by scope (the empty scope is the
  member's default). Members are also recorded as key owners.
- `keyEntry` (`auth.go:187`): each key by its SHA-256 hash. A key whose
  scope no longer exists, whose owner is unlisted or of another kind, or
  whose owner left the scope gets a precomputed `refused`: it fails
  closed, never with a zero (unlimited) budget. A key's own model list
  narrows its scope's, never widens it.

`compileAuth` then attaches the site's denylist (`Keys.Deny`) and, standalone,
the local key (`RequireLocalKey`).

`Keys.Authenticate` (`auth.go:237`) is called once per request by
`proxy.Handler.authenticate` (`gateway/proxy/proxy.go:609`). Its order:

1. `credential` (`auth.go:433`) reads `Authorization: Bearer` and
   `x-api-key`. Two different credentials are refused rather than one
   silently preferred (`auth.go:249`, golem#5).
2. `principal` (`auth.go:343`) finds the snapshot's principal for the
   credential:
   - The implicit principal. A standalone snapshot without a LocalIdentity
     is `Implicit` (`lib/compile/entitlements.go:36`), and `Compile` makes
     one local principal for it only when the installation is `key.Dev`
     (`auth.go:114`). It admits exactly one credential: the gateway's own
     local key, compared by hash in constant time (`auth.go:349`). Without
     `RequireLocalKey` it admits no one.
   - An Entra token, recognised by shape (`looksLikeJWS`, `auth.go:451`),
     goes to `Keys.token` (`auth.go:399`): `entra.Verifier.Verify` proves who
     is calling, the snapshot says in which scope (`golem-scope` header,
     else the default) and with which rights; a user's token cannot act as
     a service's principal or the reverse (`auth.go:424`).
   - A golem key is looked up by hash only if it carries this
     installation's prefix (`glm_live_` or `glm_dev_`, `auth.go:368`): a
     publisher's mistake must not let another installation's key in.
     Parsing a key is needed only to explain why an unknown one is refused.
3. The denylist (`auth.go:257`): a principal the snapshot admits is still
   refused, with `auth.denied`, if the site's emergency denylist names its
   key, its principal or its Entra ID.

`Verifier.Verify` (`lib/entra/entra.go:115`) takes RS256 only, refuses
every token as `Unavailable` while no key set has ever been fetched
(`entra.go:128`: "Entra's signing keys not yet fetched"; `Keys.token`
answers it `gateway.not_ready`, a 503 with Retry-After, since it is the
gateway's state and not the token's fault), refuses tokens once the cached
key set is older than `MaxKeyAge` (24 hours by default, `entra.go:132`),
checks issuer, audience, expiry (a minute's `Leeway`, `entra.go:62`),
tenant and version, and then requires either the `golem.access` scope with
an oid (a user) or, with no scope at all, the `golem.service` role with an
azp (a service). An unknown key ID asks `Run` for a refresh
(`entra.go:137`). `Run` (`entra.go:274`) fetches at once if it has no key
set, hourly after a fetch, and when asked, at most once a minute: a request
within the minute after the last refresh is deferred to the minute's end,
not dropped, so a key Entra rotated in just after a refresh is fetched a
minute later rather than at the next hour. A failed fetch is retried from
a second, doubling to five minutes. The request path never fetches keys.

### The emergency denylist (`deny.go`)

For a revocation that cannot wait for the control plane or the bucket, a
site admin edits a ConfigMap mounted as a file (`-denylist`). Its lines
are `key <hex SHA-256>`, `key-prefix <display prefix, 16+ characters>`,
`principal <id>` and `entra <oid or app ID>`, with `#` comments.
`ParseDenylist` (`deny.go:45`) fails the whole file on any line it cannot
read, so a typo is noticed, not skipped. `DenyFile.Load` (`deny.go:130`)
re-reads the file and swaps the parsed list in only when its content
changed and parses; an error leaves the previous list in force (a bad
edit must not clear it). `DenyFile.Run` (`deny.go:163`) reloads every five
seconds, logging a failure once until it changes. `Keys` holds the
`DenyFile`, not a list, so a reload applies to the next request without a
new snapshot. `denies` (`deny.go:94`) matches a key by hash or prefix (a
token is matched by its Entra ID only) and any caller by principal ID.
`DenyFile.OnApply` is told each changed list once it is in force:
golem-gateway sweeps its calls in flight with it (below).

### Ending withdrawn calls in flight (`Grant`, `Honours`)

`AuthenticateGrant` is `Authenticate` that also fills a caller-held
`Grant`: what was honoured (a key's hash and text, a token's principal
with its Entra ID and scope, the implicit principal), no allocation. A
later snapshot's `Keys.Honours(grant)` answers whether it still would, by
the same rules: a key while listed and not revoked, its scope there and
its principal a listed member of it (else the refusal a new request gets:
`auth.key_revoked`, `authz.scope_not_member`); a token while its principal
is a member of the scope it acted in; neither while the denylist in force
names it (`auth.denied`). Expiry is not checked: a key's, a token's or the
lease's passing ends no call that began in time.

The request path (`gateway/proxy/inflight.go`) registers each admitted call
in the shared `InFlight`, with its grant and its upstream context's cancel,
from just after admission until its upstream context is released: an
intrusive list in one of 16 mutex-guarded shards (picked by the request
ID's random byte), the entry in the pooled exchange, so a call costs two
uncontended lock round trips and no allocation (`BenchmarkInFlight`, about
60 ns). `InFlight.Sweep`, run by golem-gateway's `newHolder` after each
snapshot is applied and by `newDenyFile`'s hook after each list, asks the
current keys (`InFlight.Keys`: the applied snapshot's, lease expired or
not) about every call in flight and cancels each one refused, with the
refusal as the cause. A sweep counts a generation first; a call that
authenticated before a sweep began and registered after it passed its
shard sees the changed generation and checks itself (`add`), so no call
slips between. The cancelled upstream request then ends as a cut the
gateway made (`terminatedBy`): see request-path.md, step 8.

### The floor before authentication (`gateway/guard`)

The limiter's budgets and rates are a principal's, so a flood that never
authenticates (invalid keys, forged tokens) is not theirs to stop
(security-and-correctness.md §8). `proxy.Handler.authenticate`
(`proxy.go:609`) asks the shared `Guard` before it reads the snapshot or
the credential, and tells it of every refused credential:

- `Source` (`gateway/guard/guard.go:87`) names the caller: with
  `-source-header`, the header's last value, its last comma-separated part
  (the one the proxy in front appended), cut to 64 bytes; else the
  connection's host. Behind
  a sidecar every connection is the sidecar's, so without the header the
  budgets are the pod's.
- `Admit` (`guard.go:117`) checks two token buckets per source. The
  failures bucket (`-auth-failures`, 20 a second, `-auth-failure-burst`
  200) first: a source that has emptied it has every request refused until
  it refills, valid ones included, since its flood cannot be told from its
  calls and each failure (a token's signature checked) costs more than a
  refusal. Then the requests bucket (`-source-rate`, off by default;
  `-source-burst`, default the rate) takes one. A refusal is
  `limit.source` with a Retry-After of the bucket's refill.
- `Failed` (`guard.go:142`) takes a token from the failures bucket, never
  below empty, when `AuthenticateGrant` refuses.
- `source` (`guard.go:162`) keeps at most `MaxSources` (65,536): when
  full, at most once a second, it forgets the sources whose buckets have
  refilled (`bucket.full`, `guard.go:190`: a forgotten one starts full, as
  it would be); if none were, the new source shares one overflow pair, so
  a spray of addresses is limited as one.

`newGuard` (`main.go:533`) returns nil, which admits everything, when both
rates are off. Connections are bounded beneath it by `Listener`
(`listener.go:13`), which `serve` (`main.go:523`) puts under the server:
`-max-conns` (8192) slots, an `Accept` waiting for one before it accepts
(the rest wait in the kernel's backlog), and `-max-conns-per-source` (off)
from one remote address, a connection past it closed as it is accepted.
`Close` ends an `Accept` waiting for a slot, so a shutdown does not wait
for a connection to close first.

## 2. The snapshot holder and its sources (`gateway/snapshot`)

`Holder` keeps the current `Compiled` in an `atomic.Pointer`. A request
takes it once with `Current` (`holder.go:86`) and uses it to its end;
`Current` returns `ErrExpired` once the snapshot's authorization lease
(`ValidUntil`) has passed (`holder.go:91`), and the proxy's `authenticate`
then refuses every new request. `Apply` (`holder.go:120`), under the
holder's mutex, refuses a snapshot that is below the floor
(`holder.go:126`), not newer than the one applied (`holder.go:129`),
already expired (`holder.go:132`) or that does not compile
(`holder.go:135`), keeping the previous one. A compile failure is
returned as an `applyError` (`holder.go:139`): it may pass (a Secret
mounted later), and the bucket source retries it. On success it raises the
floor to the snapshot's sequence, swaps the pointer and calls `OnApply`.

The floor is inclusive: it is the lowest publication the gateway may
apply. It starts at zero, is raised by `RaiseFloor` (`holder.go:111`) from
the checkpoints before anything is applied, and follows each apply.

Compilation (`compile`, `holder.go:158`) resolves each backend's
credential through `Secrets`, makes a `route.Target` per deployment
(`targets`, `holder.go:202`), and builds the routes over them
(`compileRoute`, `holder.go:251`). Each deployment's `http.Transport` is
kept across snapshots by its `transportKey` (backend, deployment, scheme,
host, private grant), so connections survive a swap; transports that left
the config have their idle connections closed (`holder.go:176`).
`upstreamTransport` (`holder.go:309`) sets every pool setting deliberately
(no proxy from the environment, per-host idle limits, HTTP/2 pings, no
response-header timeout) and checks every dial's resolved address in the
dialer's `ControlContext` (`holder.go:311`) with `checkDestination`
(`holder.go:340`): cloud metadata, link-local, unspecified and multicast
addresses are refused always; loopback, private and CGNAT addresses
unless the backend is granted `allowPrivateAddresses` (golem#5). Because
the check runs on the connection, a name that later re-resolves somewhere
forbidden is refused at its next dial.

### Deployments: `route.Target`, their health and their ceilings

`targets` makes each deployment's `route.Target` (`route.go:18`,
`holder.go:240`): its URL, transport and credential header, and what the
request path decides by: `MaxInFlight` and `MaxPerPrincipal` (a
Deployment's `maxInFlight` and `maxInFlightPerPrincipal`), the health
check's path and interval, `QuotaRPM` (the site's share of the
provider's quota, taken per attempt by the limiter's meters), `Betas`
(the `anthropic-beta` values it accepts, nil for any: `Accepts`,
`route.go:48`, narrows a call's pool) and `Envelope`. Its `ID`,
"backend/deployment", is set here so the request path never builds it;
`Key` (`route.go:62`) is what health is kept by across snapshots.

golem-gateway has one `route.Health` for every dialect's calls
(`newHealth`, `main.go:304`), since they share deployments. Each attempt
takes a lease from it (`Acquire`, `health.go:95`, from the proxy's
`pick`): refused while the deployment is ejected, while its one probe is
in flight, at its `MaxInFlight` (`health.go:109`), or, on a shared box
(ADR 0017), when the call's principal holds `MaxPerPrincipal` of its
leases already (`health.go:113`): one principal must not fill it. A
refused lease names when to try again; a ceiling's is unknown (zero), as
it frees when an attempt ends. Failures in a row (three) or a failed
probe eject it (`eject`, `health.go:155`: ten seconds, doubling each time
in a row up to five minutes, or the provider's Retry-After if longer); a
commit readmits it (`readmit`, `health.go:146`).

A deployment with a `healthCheck` (a local box; not for bedrock) is also
checked by `route.Checker` (`check.go:18`), run by `newHealth`'s loop over
the applied snapshot's targets (`Table.Targets`, each deployment once).
`Run` (`check.go:40`) ticks every second; `Check` (`check.go:55`) starts
the checks that are due, in parallel, one in flight per deployment; `check` (`check.go:84`)
GETs the path on the deployment's host with its credential and its
transport, bounded by five seconds or the interval, a 2xx passing. Each
result goes to `Health.Checked` (`health.go:217`): a failure ejects the
box (unless its ejection is still running), so its routes fail over
before a call finds it; a pass readmits it once its ejection is over, but
neither cuts an ejection short nor forgets a call's failures, which a
liveness check cannot speak for. A check cut short by the shutdown
reports nothing.

### The Bedrock envelope (`gateway/envelope`)

A backend with `envelope: bedrock` has no credential header: `targets`
gives each of its deployments a `bedrock.Transport` (`holder.go:238`,
`bedrock.go:43`) over the deployment's kept transport, its endpoint the
deployment's URL or the region's runtime (`EndpointFor`,
`holder.go:222`), signing with `Holder.AWS`: golem-gateway's
`awsCredentials` (`main.go:366`), the AWS SDK's default chain (the pod's
role through IRSA or Pod Identity), nothing fetched until a bedrock call.
The request path sees an Anthropic provider. `RoundTrip`
(`bedrock.go:95`):

1. `rewrite` (`bedrock.go:278`) takes `model` and `stream` out of the
   Messages body (the model is the path's, streaming the action's), adds
   `anthropic_version` if absent, and moves the `anthropic-beta` header's
   values into `anthropic_beta`, after the body's own. A token count
   becomes CountTokens' body (`countBody`, `bedrock.go:188`: the body
   base64 in `input.invokeModel.body`, with a `max_tokens` added).
2. The path is `/model/<id>/<action>` (`invoke`,
   `invoke-with-response-stream` or `count-tokens`), the model one escaped
   segment as the AWS SDK escapes it (an inference profile's ARN).
3. The attempt's attribution (`envelope.Attribution`, set by the proxy
   for an enveloped target only, `proxy.go:357`: the request and attempt
   IDs, principal, scope and key's display ID) becomes Bedrock's
   request-metadata header (`metadata`, `bedrock.go:71`), set before
   signing, so Bedrock's invocation logs join the ledger's rows.
4. `sign` (`bedrock.go:164`) signs with SigV4; with no credentials the
   attempt fails saying what to configure.
5. An error status comes back in Anthropic's shape, its status kept
   (`anthropicError`, `bedrock.go:376`; `errorType`, `bedrock.go:357`:
   throttling a `rate_limit_error`, unavailable an `overloaded_error`), a
   count's answer as `{"input_tokens": n}` (`countAnswer`,
   `bedrock.go:220`), and a stream's AWS event stream as the Messages SSE
   (`reframer`, `bedrock.go:430`): each chunk's base64 event as
   `event:`/`data:`, an exception as the dialect's error event and the
   end, a failed CRC or a cut as a read error. So the commit point refuses
   an exception before any output, and the attempt loop fails over (ADR
   0051).

### The standalone source: `Dir`

`Dir.Load` (`file.go:33`) opens the directory with `os.Root` (no symlink
leads outside it), hashes its contents, and when they changed decodes and
compiles them with `lib/compile` (standalone, the next local sequence),
then applies the result. A config that does not compile is reported and
the previous one served. `Dir.Run` (`file.go:71`) reloads every second.

### The published source: `Bucket`

`Bucket` (`bucket.go:29`) reads a pool's signed pointer
(`snapshots/<pool>/current`) and the snapshot object it names.

- `Start` (`bucket.go:93`) raises the floor to the higher of the local
  checkpoint (`localCheckpoint`, `bucket.go:192`: the last sequence this
  gateway applied, in the `-checkpoint` file) and the pool's trusted
  checkpoint in the bucket (`trustedCheckpoint`, `bucket.go:212`:
  `control/<pool>/checkpoint`, signed, written only by golem-control).
  With neither readable it refuses to start: it could not tell a replayed
  pointer from the current one. A pool with no checkpoint and no pointer
  was never published (floor zero); one with a pointer and no checkpoint
  has lost its floor and is refused. This is the model's `TRUSTED_FLOOR`
  (model/publication.md §6.8); the status object is never trusted, since
  anyone with the pool's credentials can write it.
- Then `loadLastGood` (`bucket.go:122`) applies the last-good copy on disk,
  if there is one (`LastGood`, `last-good.json` beside the checkpoint): the
  signed pointer and snapshot exactly as the bucket held them, verified
  again as a fresh read would be (`applyLastGood`, `bucket.go:177`, through
  `verify`, `bucket.go:357`), so a gateway restarted while the bucket is
  unreachable serves until the lease ends. A copy that was altered, is
  below the floor, or is past its lease is logged and left; the bucket's
  next publication replaces it. A copy that verifies but does not compile
  yet (an `applyError`: a Secret not yet mounted) is kept by
  `tryLastGood` (`bucket.go:140`) and tried again from disk by
  `retryLastGood` (`bucket.go:161`) at the start of each `Poll`, every
  `ApplyRetry`, whether or not the bucket answers, until it applies, fails
  for good, or a publication has applied (`read` drops it then). Until a publication read from the bucket
  replaces it, `ServingLastGood` (`bucket.go:289`) reports that the copy
  from disk is what is served (`golem.snapshot.last_good`).
- `Poll` (`bucket.go:242`) makes a conditional get with the last judged
  entity tag, so an unchanged pointer costs one request.
- `read` (`bucket.go:371`) judges a changed pointer, as traced below.
- `checkOverdue` (`bucket.go:273`) logs, once, that the newest pointer
  seen is past its `Expires`: the publisher has stopped, or the bucket
  shows an old pointer. The gateway serves on until its snapshot's
  `ValidUntil`. The flag is an `atomic.Bool`, so `Overdue`
  (`bucket.go:284`) can be read from a metric's collection
  (`golem.snapshot.pointer_overdue`) while `Poll` writes it.

Both sources loop through `repeat` (`loop.go:12`), which reports a failure
once until it changes or clears rather than every second. `Bucket.Run`
(`bucket.go:476`) polls at once; `Dir.Run` waits an interval, because the
binary has loaded the directory already.

### Worked example: a publication arriving

golem-control publishes sequence 7 for pool `internal`: it writes the
snapshot object, the trusted checkpoint, then the signed pointer
(control-plane.md §4). On a gateway holding sequence 6:

1. `repeat` ticks and calls `Bucket.Poll` (`loop.go:21`, `bucket.go:477`).
2. `Poll` gets the pointer with its tag (`bucket.go:247`); the pointer
   changed, so the get returns the body and a new tag.
3. `read` opens it with `openPointer` (`bucket.go:372`, `bucket.go:305`):
   `snapshot.OpenPointer` (`lib/snapshot/pointer.go:77`) verifies the
   signature against the pinned keys, or the pointer is refused. It must
   be this pool's (`bucket.go:310`), and at least the floor
   (`bucket.go:315`): an old signed pointer copied back stops here, before
   any fetch. A pointer issued more than `MaxSkew` (`-clock-skew`, two
   minutes) in the gateway's future is refused too (`bucket.go:318`):
   a publisher whose clock runs ahead, caught before its snapshot. Its `Expires` is noted for `checkOverdue` (`bucket.go:376`).
   A pointer for the publication already held returns at
   `bucket.go:379`.
4. The snapshot object is fetched by the pointer's hash
   (`bucket.go:382`). A failed fetch is a `fetchError` (`bucket.go:384`),
   the one failure for which `Poll` does not record the tag
   (`bucket.go:260`), so the next poll reads the pointer again.
5. `openSnapshot` (`bucket.go:386`, `bucket.go:342`): the body must hash
   to the pointer's hash (`bucket.go:343`), decode strictly
   (`bucket.go:346`) and carry the pointer's sequence (`bucket.go:350`): a
   swapped object or a pointer naming another publication's snapshot is
   refused. Then `checkLease` (`bucket.go:390`, `bucket.go:327`) refuses a
   snapshot whose `ValidUntil` is more than `MaxLease` (`-max-lease`, four
   hours: golem-control's `-lease` default and an internal pool's maximum,
   ADR 0032) plus the skew ahead of the gateway's clock. Refused, not
   clamped: a lease the gateway cannot trust is not one to shorten and
   serve, so it fails closed, keeping what it holds until that expires.
   The last-good copy is held to both bounds too (`verify`).
6. `Holder.Apply` (`bucket.go:393`, `holder.go:120`) rechecks the floor
   and the order under its mutex, refuses an expired lease, compiles
   (credentials, targets, routes, entitlements with the denylist, prices),
   stores the new `Compiled`, sets the floor to 7 and calls `OnApply` (the
   limiter takes each scope's known spend and boundary, ADR 0048). If the
   compile fails (a Secret not yet mounted), the `applyError` makes `Poll`
   set `retry` (`bucket.go:263`): after `ApplyRetry` (30 seconds by
   default) it forgets the tag (`bucket.go:244`) and reads the unchanged
   pointer again.
7. `saveLastGood` (`bucket.go:400`, `bucket.go:413`) writes the pointer
   and the snapshot to `last-good.json`, then `saveCheckpoint`
   (`bucket.go:403`, `bucket.go:425`) writes "7"; last-good first, so a
   crash between them leaves a copy at or above the floor, never one
   below it. Both go through `writeAtomic` (`bucket.go:433`): a temporary
   file, written and synced, renamed into place, then the directory
   synced, so a power loss cannot bring back the older checkpoint. A
   failure of either is logged, not returned: the snapshot is applied, and
   a restart's floor will be the older checkpoint (or the trusted one).
8. `writeStatus` (`bucket.go:407`, `bucket.go:464`) writes
   `status/internal/<gateway-id>` for golem-control's diagnostics.
   Requests that took sequence 6 before the swap finish on it; new ones
   take 7. Back in `Poll`, the tag is recorded (`bucket.go:261`): every
   outcome but a failed fetch or a failed apply is final until the
   pointer changes.

`publication_mbt_test.go` drives exactly this code (with the real
publisher, elector and a directory bucket) from model/publication.qnt's
traces, including replays and empty-disk restarts, and after each step
compares every gateway's applied publication and floor with the model's.

## 3. Telemetry (`gateway/telemetry`)

`Telemetry.Start` (`telemetry.go:186`) opens the request's server span;
`Ended` (`telemetry.go:201`) names it "chat <alias>", sets the GenAI and
golem attributes, records the token and duration histograms and writes
the ledger row; `Refused` (`telemetry.go:248`) marks the span with the
contract code and counts the refusal. Attribute sets are built once per
route, frontend and refusal code (`cached`, `telemetry.go:128`), so a
call allocates little and a 429 storm nothing for its counter. Every
method is a no-op on a nil `*Telemetry`.

The ledger (`ledger.go`) writes OTel log records whose attributes are
the ClickHouse table's columns: a `golem.spend` row per upstream attempt
when the call ends (`Ledger.Ended`, `ledger.go:36`; `row`, `ledger.go:55`:
39 columns in the DDL's order, built in an array on the stack and checked
by `TestRowMatchesLedgerDDL`), whatever the site counter's state (ADR
0043): a counter outage keeps no billing rows in memory. A row's period is
the limiter's when given, else the call's start in the configured zone
(`ledger.go:58`), so the counter and the ledger agree on the month.

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
(`setup.go:114`) rather than lost silently.

**State gauges.** `ObserveState` (`state.go:51`) registers observable
instruments that read the gateway's state when the meter collects (every
minute over OTLP), so nothing is added to a request:
`golem.snapshot.age` (now − the applied snapshot's `IssuedAt`, through
`Holder.Applied`, `holder.go:99`), `golem.snapshot.lease_remaining`
(`ValidUntil` − now, negative once run out), `golem.snapshot.seq`, the bucket
source's `golem.snapshot.pointer_overdue` and `golem.snapshot.last_good`
(0/1), the limiter's `golem.limit.down` (0/1), `golem.limit.waivers`
(`Limiter.Waived`, counted where a waived ticket is made, `limit.go:387`),
`golem.limit.held` (`Limiter.Held`, under the limiter's lock for a
`len`) and `golem.limit.dropped`, and `golem.denylist.entries`. They carry
no attributes of their own: the resource's site and pool name the
gateway's. The alerts over them are operations.md §2.2's.
`TestStateGauges` reads them through a manual reader with an injected
clock: a snapshot's age and lease, then the store down (a waiver, a held
increment, a lease run out).

## 4. The binary's lifecycle (`cmd/golem-gateway`)

`parse` (`main.go:83`) reads the flags, grouped as `options` holds them
(general, Entra, pool mode, the site's Valkey, the floor before
authentication), and applies `releaseGate` (`main.go:604`). The gateway has two modes:

- **standalone** (`-standalone <dir>`, ADR 0023): only on a loopback
  address; the in-memory limiter; dev keys, with the local key
  (`loadLocalKey`, `main.go:638`: a dev key in a 0600 file, kept across
  restarts); the directory source, which must load once before the server
  starts; provider credentials from the environment;
- **pool** (`-pool <name>`): `-bucket`, `-keys`, `-valkey` and `-site` are
  required, and until golem#7's deployment evidence is in, so is
  `-accept-unreleased`. `poolSource` (`main.go:467`) opens the bucket
  (`s3bucket.Open`; a directory for tests), loads the trusted publishing
  keys (`sign.LoadKeys`: a directory of PEM public keys, a KMS key's named
  by its `Key-ID` header), opens the Secrets mounted under `-secrets`
  (`FileSecrets`, live keys only), names the gateway (`-id`, else the host
  name) and starts the bucket source: `Start` raises the floor from the
  local checkpoint (`-checkpoint`) and the pool's signed trusted
  checkpoint, refusing to start with neither, and applies the last-good
  copy beside the checkpoint (`lastGoodPath`, `main.go:671`); then one
  `Poll`, whose failure is logged, not fatal. The limiter is over the
  site's Valkey (`limit.DialValkey`, `-valkey-replica-acks` for WAIT), its
  client's options from `valkeyOption` (`main.go:271`): `-valkey-tls` for TLS
  (ElastiCache's in-transit encryption), verified against the system's
  roots or `-valkey-ca`'s, each dial's certificate checked against that
  dial's host (no fixed `ServerName`, so a cluster's nodes are each
  verified by their own name); `-valkey-user` for an ACL user's AUTH, its
  password from `$VALKEY_PASSWORD` only (a flag would show in the process
  list and the pod spec). A half-given setting (a CA without TLS, a user
  without a password, a CA file with no PEM certificate) refuses to start,
  saying what to add. A
  Valkey unreachable at the start is not fatal either: the limiter starts
  in its outage mode (`Limiter.MarkDown`: waivers, increments held) and
  `Valkey.Connect` retries the dial in the background, backing off to 30
  seconds; the limiter's `Run` then recovers it (accounting.md §3.4).

`run` (`main.go:135`) builds the parts in dependency order, each a named
step: telemetry (`setupTelemetry`, `main.go:571`, labelled with the site
and pool), the Entra verifier if a tenant is given (`newEntra`,
`main.go:504`: its key set, from `-entra-jwks` or Entra's public cloud,
fetched once; a failure is logged, not fatal, and the verifier's `Run`
retries, golem keys served meanwhile), the limiter over its store (`newLimiter`, `main.go:242`),
the holder that hands each snapshot's known spend to the limiter and then
sweeps the calls in flight every dialect's handlers share (`newHolder`,
`main.go:346`, `proxy.InFlight`), the denylist if one is named (`newDenyFile`, its each
list sweeping them too; loaded before
the source, `main.go:179`, so neither a first request nor the last-good
publication is ever compiled without it), the snapshot source
(`startSource`, `main.go:393`: standalone's directory with the dev key, or
`poolSource`), the deployments' health and its checks (`newHealth`,
`main.go:304`, above), and the routes (`newMux`, `main.go:415`: the three
dialects' APIs, the models list, Anthropic's token count, the refusal of
stored responses' paths, the probes; the handlers share one `guard.Guard`
from `newGuard`) on the server (`newServer`, `main.go:543`), served by
`serve` (`main.go:523`) over a `guard.Listener`. Every background loop
(the source, the denylist, the limiter, Valkey's connection retries,
Entra's refresh, the health checks) runs under `bg` in one `WaitGroup`. Once the source has started, the state gauges are
registered over the holder, the bucket source, the limiter and the
denylist (`telemetry.ObserveState`, `main.go:199`; with any telemetry but
`off`, and exported only over OTLP). `announce` (`main.go:453`) says what
is served.

Standalone's API handlers are wrapped as `track(localOnly(...))`, and the
models list (a GET, no body) as `track(loopbackOnly(...))`; pool mode's as
`track(...)` (`newMux`'s `handle`). `localOnly` (`guard.go:25`) keeps the
developer's browser out: no cross-origin request by the browser's own
headers, a loopback `Host` only (`loopbackOnly`, `guard.go:44`: DNS
rebinding), and a JSON body only (no preflight-free page can send one). `track` (`lifecycle.go:42`) counts
calls in flight under a mutex (`begin`, `end`): not a `WaitGroup`, because
after `srv.Close` a handler whose connection was already accepted can
still start while the wait runs. Once `wait` (`lifecycle.go:75`) has begun
a call that starts is refused rather than counted after the count was
taken, in its route's dialect (`track` is given it, as `localOnly` is):
`proxy.Refuse` with `gateway.not_ready`, a 503 with Retry-After, so an SDK
parses it and retries, reaching another of the pool's gateways. `/readyz` (`lifecycle.go:88`) fails while draining and
while the holder has no current snapshot or its lease has expired. It
does not look at the site's Valkey: the limiter fails open by design, so a
Valkey outage must not take the pool's endpoints away (`golem.limit.down`
is the signal, operations.md §2.2).
`TestPoolMode` runs pool mode end to end: golem-control's publisher writes
a snapshot to a bucket, and the gateway serves a key's call from it, with
the provider credential from a mounted Secret, its spend in a real Valkey
and its local checkpoint written. `TestPoolModeValkeyDown` starts the
same gateway with nothing listening at `-valkey`: it becomes ready and
serves the call as a waiver.

### Worked example: shutdown with a stream in flight

SIGTERM ends `sig` (`main.go:161`). The shutdown goroutine
(`main.go:215`) runs `lifecycle.shutdown` (`lifecycle.go:104`):

1. `draining` is set, so `/readyz` answers 503, and the gateway sleeps
   `-unready` for the endpoints to stop sending.
2. `srv.Shutdown` closes the listeners (`serve` returns at
   `main.go:222`) and waits up to `-drain` for calls to finish.
3. At the deadline `srv.Close` (`lifecycle.go:113`) cuts the remaining
   connections; the stream's context ends and the proxy accounts it as a
   cancellation, under `context.WithoutCancel`, its row and counter add
   still written.
4. `shutdown` waits for `lc.wait()` (`lifecycle.go:116`), the last
   counted call's end, up to `cut` (10 seconds); a call that starts now is
   refused (`gateway.not_ready`, in its dialect).

Only then (`<-drained`, `main.go:227`) does `run` stop the background loops
(`stopBg`, `wg.Wait`, `main.go:228`; they ran through the drain because
the limiter's renewals keep calls' slots), let the limiter finish
(`main.go:231`), close the Valkey client (`closeStore`, deferred) and shut
telemetry down last (`shutdownTelemetry`, deferred at `main.go:155`),
delivering the rows the cut calls wrote. `TestShutdownAccounts` checks
this end to end: the cut stream leaves one row with outcome `cancelled`,
its cost in the counter. `TestTrackAfterWait` checks the count: a call
after the wait began is refused, and the wait ends with the last call
that started before it.

## Invariants: where each is enforced and tested

| Invariant | Enforced | Tested |
|---|---|---|
| A snapshot below the floor, or not newer than the one held, is never applied | `Holder.Apply` (`holder.go:126`, `:129`); `Bucket.openPointer` (`bucket.go:315`) | `TestHolder`, `TestRaiseFloor`, `TestBucketSource`, `TestPublicationModel` |
| A restart with an empty disk takes the trusted checkpoint as its floor; with no floor at all it does not start | `Bucket.Start`, `trustedCheckpoint` | `TestBucketSource` (restarts), `TestPublicationModel` (`restartEmpty`) |
| Only a correctly signed pointer of this pool, naming an object with its hash and sequence, is applied | `Bucket.openPointer`, `openSnapshot` | `TestBucketSource` |
| A pointer issued more than the skew in the gateway's future, or a snapshot whose lease runs past the longest lease plus the skew, is refused, from the bucket or from disk | `Bucket.openPointer`, `checkLease` | `TestClockBounds`; `TestPoolModeLeaseBound` (`-max-lease`) |
| A restart while the bucket is unreachable serves the last publication applied, verified again; an altered copy, one below the floor or past its lease is not served | `Bucket.loadLastGood`, `applyLastGood`, `verify` | `TestLastGood` |
| The last-good copy is written before the checkpoint, and both durably (file and directory synced) | `Bucket.read`, `writeAtomic` | not directly: the order and the syncs are untested; `TestPoolMode` checks the checkpoint is written |
| A verified publication that failed to compile is retried while current; a refused one is not | `Holder.Apply` (`applyError`), `Bucket.Poll` | `TestApplyRetried` |
| A pointer past its `Expires` is reported once, and the gateway serves on | `Bucket.checkOverdue` | `TestOverdue` |
| The last-good copy served is reported until a bucket publication replaces it | `Bucket.ServingLastGood` | `TestLastGood` |
| A last-good copy that failed to compile at start is retried from disk every `ApplyRetry`, the bucket unreachable, until something applies | `Bucket.tryLastGood`, `retryLastGood` | `TestLastGoodRetried` |
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
| A gateway starts while Entra is unreachable: golem keys served, Entra tokens refused as `gateway.not_ready` until a retried fetch succeeds | `newEntra`, `Verify` (`errNoKeys`), `Run`'s backoff, `Keys.token` | `TestPoolModeEntraDown`, `TestStartWithoutKeys` (lib/entra), `TestEntraKeysNotYetFetched` |
| A refresh asked for within a minute of the last is deferred to the minute's end, not dropped | `Verifier.Run` | `TestRefreshDeferred` (lib/entra, `testing/synctest`) |
| A source past its failed authentications has every request refused, before its credential is checked or its body read; each refused credential is counted | `Guard.Admit`, `Guard.Failed`, `proxy.Handler.authenticate` | `TestFailuresRefuseTheSource`, `TestInvalidKeyFlood` (gateway/proxy) |
| A source's requests before authentication are bounded; the sources kept are bounded, a spray of new ones limited as one; a nil guard admits all | `Guard.Admit`, `Guard.source` | `TestRequestRate`, `TestSources`, `TestNilGuard` |
| Connections are bounded in all and per remote address; closing the listener ends an `Accept` waiting for a slot | `guard.Listener` | `TestListener`, `TestListenerMax` |
| A caller the denylist names is refused whatever the snapshot says; a bad edit keeps the list in force | `Authenticate`, `Denylist.denies`, `DenyFile.Load` | `TestDenylist` |
| A grant is honoured by a later snapshot by the rules a new request is refused by (revoked, removed, its principal gone from its scope or from golem, denied); expiry, the key's or the lease's, ends none, nor a scope losing a model (by decision: they stop new requests only) | `Keys.AuthenticateGrant`, `Keys.Honours` | `TestHonours` |
| A snapshot revoking a key, or a denylist entry, ends that caller's calls in flight within seconds (both dialects: a stream's error event, a waiting call's error, a begun whole response cut); other calls run on; the lease's expiry ends none | `InFlight.Sweep`, `add`, `newHolder`, `newDenyFile` | `TestTerminateStream`, `TestTerminateWhole`, `TestLeaseExpiryEndsNoCall`, `TestTerminateRace` (gateway/proxy), `TestWiringEndsWithdrawnCalls` |
| Every ledger row is delivered, dropped or failed, and counted | `LedgerQueue` | `TestLedgerQueue*` (shutdown races, emission during shutdown, flush during shutdown, retries, partial delivery) |
| Standalone's ledger rows on stdout are written whole or counted as lost | `jsonLogger.Emit` | `TestStdoutCountsLostRows` |
| A row has golem.spend's columns in order, and the counter's period | `Ledger.row` | `TestRowMatchesLedgerDDL`, `TestRowPeriodIsTheCounters` |
| OTLP uses TLS unless the collector is a literal loopback address, whatever the environment says | `otlp.PlaintextOK`, `otlp.New` | `TestPlaintextOK`, `TestOTLPIgnoresInsecureEnv` (lib/otlp) |
| A stream cut at shutdown still leaves its row and counter add | `lifecycle.shutdown`, the order in `run` | `TestShutdownAccounts` |
| No call starts uncounted once the shutdown's wait has begun; one refused then gets its dialect's `gateway.not_ready` with Retry-After | `lifecycle.begin`, `wait`, `track` | `TestTrackAfterWait` (both dialects) |
| Standalone answers only loopback, same-origin JSON requests | `localOnly`, `loopbackOnly` | `TestLocalOnly` (the models list's `loopbackOnly` alone is not tested directly) |
| Pool mode needs its inputs, and `-accept-unreleased` until golem#7 | `releaseGate` | `TestReleaseGate` |
| A pool's published snapshot is served, its credential from a Secret, its spend in Valkey | `poolSource`, `run` | `TestPoolMode` |
| Valkey is reached over TLS with AUTH when configured; the password never comes from a flag | `valkeyOption` | `TestValkeyOption`, `TestPoolModeValkeyTLS` (a TLS-only server with an ACL user) |
| A gateway starts, is ready and serves while its Valkey is unreachable | `newLimiter` (`limit.DialValkey`, `MarkDown`), `/readyz` | `TestPoolModeValkeyDown`, `TestStartWithValkeyDown` (gateway/limit) |
| A source reports a failure once, not every second | `repeat` | `TestRepeat` |
| A deployment's attempts stay within its ceiling, and one principal's within its per-principal ceiling | `Health.Acquire` | `TestCeiling`, `TestPerPrincipalCeiling` |
| Failures in a row or a failed probe eject a deployment; an ejected one takes one probe at a time; a commit readmits it, and a probe sent before the readmission is no probe | `Health.Acquire`, `Lease.Fail`, `Lease.Commit`, `Lease.Release` | `TestEjection`, `TestProbeAbandoned`, `TestReadmissionEndsProbes` |
| A box whose health check fails is ejected before a call finds it, and one that passes is readmitted once its ejection is over | `route.Checker`, `Health.Checked` | `TestCheckerEjectsAndReadmits` |
| A route's retries are a share of its calls | `Health.Called`, `Health.Retry` | `TestRetryBudget`, `TestRetryBudgetBoundsTheStorm` |
| A bedrock deployment gets a Messages body rewritten for Bedrock, signed with the attribution header under the signature; its exceptions come back in Anthropic's shape and its stream as Messages SSE | `bedrock.Transport` (`rewrite`, `sign`, `metadata`, `anthropicError`, `reframer`) | `TestRewrite`, `TestRequest`, `TestMetadata`, `TestNoCredentials`, `TestReframe`, `TestErrorResponse`, `TestCountTokens`; `TestBedrockEndToEnd` (gateway/proxy) |

## Known gaps

- Pool mode runs only with `-accept-unreleased` until golem#7's evidence
  is in.
- An overdue pointer is a log line and a metric
  (`golem.snapshot.pointer_overdue`), not a readiness signal: a gateway
  serves its snapshot until the lease ends, by design.
- The state gauges carry no gateway identity of their own: the resource
  names the site and pool, and telling one pod's series from another's is
  left to the collector (its Kubernetes attributes), as for the
  gateway's other metrics. Standalone's stdout telemetry exports no
  metrics.
- A gateway still without Entra's signing keys reports it in its log only
  (each failed fetch, and `gateway.not_ready` refusals counted with the
  others): there is no state gauge for it yet.
- OTLP's TLS is tested at the decision, not against a collector.
- Standalone's stdout ledger counts and logs a row it could not write, but
  does not retry it.
- Termination's seconds bound runs from the change being in force at the
  gateway (a snapshot applied, a denylist read): the denylist's five-second
  re-read and the ConfigMap's projection come before it, and are measured
  per site, not promised. A sweep ends only what the keys can see: a
  route or model taken from a scope does not end calls already on it, nor
  does a key's or a token's expiry.
- The floor before authentication is each gateway's own, in memory: a
  source's budgets are per gateway, not the pool's, and behind a sidecar
  without `-source-header` every caller shares the pod's. Health, ejection
  and the health checks are per gateway too (model/attempts.md §9).
- The m1 acceptance record's "Not yet shown" lists the rest: client
  certificates for OTLP, OpenAI's restricted mode, provider-account
  spending limits (ADR 0044).
