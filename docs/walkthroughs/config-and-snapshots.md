# Walkthrough: config, snapshots and their distribution

This is a guided reading of `lib/`, the code every golem binary shares: the
config CRDs, the compiler that turns them into a snapshot, the snapshot's
format, the signed pointer and checkpoint that publish it, the bucket it
travels through, the signing keys, API keys, budget periods, the contract
codes, and the two transports moved here from the gateway (Entra token
verification and OTLP export). It is for an engineer new to the
repository; read it with the code open. The design behind it is in
[ADR 0005](../adr/0005-config-crds-and-overrides.md) (config as CRDs),
[ADR 0006](../adr/0006-state-and-distribution-in-object-storage.md)
(state and snapshots in object storage), [ADR 0023](../adr/0023-standalone-gateway.md)
(the standalone gateway), [ADR 0030](../adr/0030-authorization-renewal-and-availability.md)
(the authorization lease), [ADR 0044](../adr/0044-budgets-are-controls-not-ceilings.md)
(why decoders refuse what they cannot interpret), [control-plane.md §4](../control-plane.md#4-distribution)
(distribution) and [contracts.md](../contracts.md) (the external contracts).

File:line references are to the code as of this walkthrough's last
update; references into `control/`, `gateway/` and `cmd/` name the
function too, since those move more often.

## The pieces at a glance

| Package | What it is | Who uses it |
|---|---|---|
| `lib/api/v1alpha1` | the config CRDs as Go types, and a strict YAML reader | golemctl, golem-control, the standalone gateway |
| `lib/compile` | config objects in, a validated `snapshot.Snapshot` out, or every problem with its field path | the same three |
| `lib/snapshot` | the snapshot format, its strict decoder, the signed `Pointer` and `Checkpoint` | publisher (control/publish), gateway (gateway/snapshot) |
| `lib/bucket` | the object store interface with S3's conditional semantics; `Dir`, a directory | everything that publishes or polls |
| `lib/bucket/s3bucket` | `Bucket` on Amazon S3, and `Open`, a bucket URL or a directory | golem-control, the pool gateway |
| `lib/bucket/buckettest` | the conformance suite every `Bucket` passes | the two above |
| `lib/sign`, `lib/sign/kmssign` | Signer and Verifier: Ed25519, ECDSA P-256 verification, `LoadKeys` (a directory of PEM public keys), a KMS signer | publisher signs, gateways verify |
| `lib/key` | golem's API key format | compile (LocalIdentity), control, gateway/auth |
| `lib/period` | budget period names ("2026-11") | gateway counters, ledger, control |
| `lib/contract` | refusal codes, origins and header names, checked against contracts.md | the gateway's every refusal |
| `lib/entra` | Entra ID v2 access-token verification with a cached key set | gateway/auth, the control API (control/api) |
| `lib/otlp` | OTLP/HTTP exporters, TLS unless the collector is a literal loopback address | gateway/telemetry, golem-control's metrics and audit |

## 1. Config CRDs (`lib/api/v1alpha1`)

`types.go` holds five kinds of group `golem.dev/v1alpha1`
(`APIVersion`, `types.go:9`):

- **Backend** (`types.go:38`): a provider or model server: `dialect` (one
  of `compile.Dialects`, `anthropic.messages` or `openai.chat`), `auth` (a
  header carrying either a Kubernetes `secretRef` or, standalone only, an
  `env` variable), `deployments` (one model at one URL each) and
  `network.allowPrivateAddresses`.
- **ModelRoute** (`types.go:96`): an `alias` callers send as their model
  name, a `pool` of backend deployments with weights, optional
  `timeouts`, `agent` and `approvedBetas`.
- **PriceList** (`types.go:204`): a currency and per-model rates in
  micro-units per million tokens ([contracts.md §4](../contracts.md#4-the-price-model-adr-0025);
  this version has the per-token rates only).
- **ScopePolicy**: a scope's policy, named by the scope's ID (ADR 0050):
  `models` (route aliases), `budgetMicros`, `defaultMemberCapMicros` and
  `budgetMode` (observe, the default, warn or enforce; set on every
  scope, as its caps follow it), `declaredWorstCaseCheck`, `inFlight`
  (`principal`, `scope`), `maxOutputTokens`, `maxInputBytes`,
  `providerStorage` and `features`. The scope itself exists in the state
  document (created through the control API); in standalone mode, where
  there is none, each ScopePolicy is a scope.
- **LocalIdentity**: standalone mode's stand-in for the state document's
  entitlements: dev keys, Entra users and services, members of the
  config's ScopePolicies. Refused outside standalone mode (ADR 0023).

The package has no Kubernetes client dependency: the gateway, golemctl and
golem-control all import it.

**Decoding** (`decode.go`). `DecodeFS` (`decode.go:38`) lists every
`.yaml`/`.yml` file (`yamlFiles`, `decode.go:58`), sorts the paths, and
splits each file into documents on `---` lines (`documents`,
`decode.go:121`). For each document (`Objects.decode`, `decode.go:73`):

1. it reads only `apiVersion` and `kind` (leniently);
2. an empty document, or one of another API group (a Kustomization, a
   Secret), is skipped (`decode.go:81`, `:83`);
3. a `golem.dev/` object of another version is an error naming the version
   this binary reads;
4. a golem kind is decoded **strictly** (`decodeInto`, `decode.go:111`,
   `yaml.UnmarshalStrict`): an unknown field is an error, so a misspelt
   field is never silently ignored; an unknown kind is an error too.

Every object keeps its origin as a `Source[T]` (file and 1-based document
number); `Source.Where` prints `golem.yaml#2`, which every compile problem
carries.

`ContentHash` (`decode.go:137`) hashes the YAML files' names and contents
in walk order: golem-control (`Controller.load`,
`control/controller/controller.go:552`) and the standalone gateway
(`Dir.Load`, `gateway/snapshot/file.go:40`) use it to skip decoding and
compiling a directory that has not changed.

## 2. The compiler (`lib/compile`)

One validation path (ADR 0005): `golemctl config validate` (`validate`,
`cmd/golemctl/main.go:136`) runs it offline, golem-control on the live
config (`Controller.Step`'s publication, `control/controller/controller.go:488`,
and `load`'s trial compile at `:570`), the standalone gateway on its
directory (`Dir.Load`, `gateway/snapshot/file.go:51`).

`Compile` (`compile.go:72`) reports **every** problem, not the first, as
`Problems`, each a `Problem{Object, Path, Msg}` such as

```
ModelRoute/chat (golem.yaml#2): spec.pool[0].deployment: Backend "provider" has no deployment "large"
```

The object is named by `objectName` (`compile.go:107`), the path is the
field's JSON path with indices. Every pass reports through one `adder`
(`compile.go:66`), so the problems come out in pass order. These messages
are user-facing and the tests match them (`TestProblems`,
`compile_test.go:86`; `TestEntitlementProblems`, `entitlements_test.go:73`):
change their wording deliberately or not at all.

The passes, in order:

1. **Options.** A published snapshot (not `Standalone`) needs a positive
   `Lease` (ADR 0030): the authorization lease becomes
   `Snapshot.ValidUntil`.
2. **Backends** (`compileBackends`, `compile.go:122`): a name, unique; a
   known dialect; auth by `backendAuth` (`compile.go:189`: a header and
   exactly one of `secretRef`/`env`, `env` standalone only, the secret
   compiled to a `"name/key"` reference, never a value); deployments by
   `deployments` (`compile.go:160`): at least one, each with a unique
   name, a model and a URL that `checkURL` (`compile.go:309`) accepts:
   https, or plain http only to a loopback IP or in standalone mode, and
   never credentials in the URL. Each backend with a usable name enters
   the `catalog` (`compile.go:114`, `catalog.add` at `:149`), problems or
   not, so that routes are checked against it rather than reported as
   dangling too: its dialect, and each deployment's provider model and
   `maxInFlight` and `maxInFlightPerPrincipal` (each gateway's ceilings on
   it, and on one principal's calls to it; 0 none, never negative), and
   its `healthCheck` (`healthCheck`: a path, its interval defaulted to
   10 s; none on a bedrock deployment).
3. **Routes** (`compileRoutes`, `compile.go:208`): a unique alias; a
   non-empty pool; timeouts by `timeouts` (`compile.go:334`), defaulting
   to `DefaultMaxDuration` (30 minutes) for the whole call, the whole call
   for headers, and `DefaultIdle` (5 minutes, capped by the whole call)
   for silence; attempts by `maxAttempts` (`spec.retries.maxAttempts`,
   `DefaultMaxAttempts` 2, 1 to 5); then the pool by `pool` (`compile.go:240`): members name a
   catalogued backend and deployment; weights not negative (zero means 1);
   **one dialect per pool**, the first member's (ADR 0033: conversion is
   explicit, never implied by a mixed pool).
4. **Scope policies** (`compilePolicies`, `policy.go`): each ScopePolicy
   needs a name, once; every alias it names must be routed; its policy is
   checked by `scopePolicy`, below.
5. **Entitlements** (`entitlements`, `entitlements.go`), below: the
   state's, joined with the policies, or a LocalIdentity's.
6. **Prices** (`priceList`, `entitlements.go`): a three-letter
   currency, one per installation; each model priced once; no negative
   rates.
7. **Budgets are chargeable** (`checkPriced`, `compile.go`): a
   ScopePolicy with a budget must reach only priced models, or its budget
   could never be spent (contracts.md §4). Both are config, so `golemctl
   config validate` refuses the mistake before it is merged.

With no problems, backends and routes are sorted, so equal configs encode
to equal bytes (`TestDeterministic`, `compile_test.go:72`), and
`Source.ConfigDigest` (`digest`, `compile.go:297`) is the SHA-256 of the
compiled backends and routes.

### Standalone versus published

| | Standalone (`Options.Standalone`) | Published (golem-control) |
|---|---|---|
| Lease | none: `ValidUntil` zero | required (ADR 0030) |
| `auth.env` | allowed | refused: use `secretRef` |
| plain http | to any host | loopback addresses only (ADR 0017) |
| LocalIdentity | at most one; its entitlements, its scopes the ScopePolicies | refused: entitlements come from the state |
| ScopePolicy | each is a scope; one with no LocalIdentity is refused (it would apply to no one) | joined by ID with the state's scopes |
| no identity at all | `Entitlements.Implicit`: one local principal, every route | empty, unless `Options.Entitlements` gives them |

### Entitlements

A **published** snapshot's entitlements come from the state document
(`control/state`): golem-control passes `Options.Entitlements`, a function
told the sorted route aliases that exist; golem-control's fills in each
scope's known spend (`Controller.entitlements`). Its scopes are IDs: the
state holds that a scope exists and its Entra group bindings, never its
policy. Compile **joins** them with the ScopePolicies by name (`join`,
`policy.go`): each of the state's scopes takes its policy's fields and
keeps its own known spend. A scope with no ScopePolicy is published as
`DefaultScope`: no models, so its members can call nothing (fail
closed: a policy is what grants anything). A ScopePolicy naming a scope
the state does not have is not published. Neither is fatal: each is told
to `Options.Unmatched` and the rest is published, since the state and the
config change apart (control-plane.md §4). Without `Options.Entitlements`
(`golemctl config validate`, which has no state) the policies are only
checked and the snapshot authorizes no one. Compile still runs
`entitlements` first, so that a LocalIdentity in a published config is
reported.

`scopePolicy` (`lib/compile/policy.go`) is the one check of a scope's
policy, a ScopePolicy's in either mode: a budget not negative and at most
`snapshot.MaxMicros`; `budgetMode` observe, warn or enforce, and only with
a budget; in-flight limits and ceilings not negative (the gateway applies
a limit only above zero, so a negative one would be none); `features`
restricted or compatible; `providerStorage` forbidden or allowed. Each
field fails at its own path (`spec.inFlight.scope`), every one reported.
It returns the snapshot's scope with its defaults explicit: an in-flight
limit of 0 or unset is `DefaultPrincipalInFlight` (4) or
`DefaultScopeInFlight` (16), and a budget's mode is `observe` unless set.

A **standalone** LocalIdentity's scopes are the ScopePolicies (there is
no state to say a scope exists), and it is compiled by two helpers:

- `localKeys` (`entitlements.go`): each key must parse (`key.Parse`)
  and be a `glm_dev_` key (a live key never belongs in a file); its scope
  must exist and its models only narrow the scope's. Only the key's SHA-256
  and its display prefix enter the snapshot (`TestEntitlements` checks the
  key itself does not). Each key's principal becomes a `KeyOwner`, a
  member of its keys' scopes: a key is honoured only for a listed owner.
- `localMembers` (`entitlements.go`): users and services share one
  namespace of Entra IDs, since gateway/auth finds both by it; each needs
  at least one scope, and a default among them (the first if unset).

**The budget bound.** `snapshot.MaxMicros` (`snapshot.go:28`) is 2^50
micro-units, about 1.1 billion units of currency. The site counter's
Valkey scripts sum in Lua numbers, doubles exact only below 2^53; under
this bound a period's sums stay exact there as they are in memory. The
compiler refuses a larger budget (`at most 1125899906842624 (2^50)`,
`entitlements_test.go:85`), and the gateway clamps the cost one call adds
to the same bound (`gateway/limit/limit.go:357`).

## 3. The snapshot format (`lib/snapshot/snapshot.go`)

A `Snapshot` (`snapshot.go:31`) is one pool's complete configuration at one
moment: `Schema` (`1`), `PublishSeq` (orders publications),
`IssuedAt`/`ValidUntil` (the lease), `Source` (the state version and config
digest it was built from), backends with secret *references*, routes,
entitlements, currency and prices. It is data only: the gateway compiles
it into lookup structures.

`Encode` (`snapshot.go:196`) is `encoding/json`'s `Marshal`, which writes
struct fields in declaration order: with the compiler's sorting, equal
snapshots give equal bytes, and `Hash` (`snapshot.go:230`), their hex
SHA-256, names the object in the bucket. So **reordering a struct's fields
changes every snapshot's hash.**

`Decode` (`snapshot.go:202`) reads in two steps:

1. **The schema first** (`snapshot.go:211`): only the `schema` member is
   read; any value other than 1 is a `SchemaError` telling the operator to
   upgrade gateways before the publisher. A newer schema's members are
   unknown here, so reading the schema first is what makes its refusal
   say "upgrade the gateway" rather than name a member.
2. **The whole snapshot, strictly** (`snapshot.go:215`):
   `encoding/json/v2` with `RejectUnknownMembers`, which also refuses
   duplicate names and names in another case (v2's defaults).

A snapshot is policy, and a reader that skipped a member it did not know
would apply weaker policy: a renamed check (`strict` became
`declaredWorstCaseCheck`) read as off. ADR 0044 (Consequences) requires
both decoders, this one and the CRD decoder, to fail closed instead.
`TestDecodeFailsClosed` (`snapshot_test.go:35`) covers each refusal,
including another schema with members of its own.

## 4. Pointer and checkpoint (`lib/snapshot/pointer.go`)

The bucket holds, per pool (`PointerKey`, `ObjectKey`, `CheckpointKey`,
`pointer.go:63`–`:70`):

- `snapshots/<pool>/<hash>.json`: the snapshot, content-addressed, so
  writing it twice is harmless;
- `snapshots/<pool>/current`: the signed **pointer** (`Pointer`,
  `pointer.go:16`), `{pool, publishSeq, epoch, source, issuedAt, expires,
  hash}`;
- `control/<pool>/checkpoint`: the signed **checkpoint** (`Checkpoint`,
  `pointer.go:37`), `{pool, publishSeq}`.

Both are stored as a `signed` envelope (`pointer.go:44`: payload, key ID,
signature), each kind in its own context: `seal` (`pointer.go:96`) signs
`"golem snapshot pointer v1\n"` or `"golem snapshot checkpoint v1\n"`
followed by the payload's bytes (`signedBytes`, `pointer.go:58`), so the
publishing key's signature over one kind means nothing as the other, or
elsewhere. `open` (`pointer.go:110`, behind `OpenPointer` and
`OpenCheckpoint`) verifies before it decodes, and decodes the verified
payload strictly. The snapshot needs no signature of its own: the gateway
checks its bytes against the pointer's hash.

The checkpoint gives a gateway that starts with an empty disk a floor, so
that a replayed old pointer (anyone who can write the pool's objects can
copy one back) is refused. It is signed and also under a prefix only
golem-control's role may write (control-plane.md §3): a bucket policy
mistake alone does not let anyone lower it. golem-control writes it after
the publication record and before the pointer. At start
(`Bucket.Start`, `gateway/snapshot/bucket.go:93`) a gateway takes the
higher of its local checkpoint and the pool's trusted one; a missing
checkpoint is floor zero only while the pool has no pointer either
(`trustedCheckpoint`, `:149`), and a published pool's missing or
unverifiable checkpoint stops a gateway with an empty disk from starting.

A pointer's `Expires` (`pointer.go:27`; the publisher sets it ten minutes
after issue unless `Publisher.PointerTTL` says otherwise, ten renewals at
the default one-minute renew) is TUF's timestamp role as a signal, not a
refusal: a gateway that sees nothing newer than an expired pointer reports
it overdue (`Bucket.Overdue`, logged once by `checkOverdue`,
`gateway/snapshot/bucket.go:273`) and goes on serving until its snapshot's
`ValidUntil`, which is what bounds a frozen or replayed bucket (ADR 0030).

## 5. The bucket (`lib/bucket`)

`Bucket` (`bucket.go:33`) is two calls with S3's conditional semantics:

- `Get(key, ifNoneMatch)`: the object and its entity tag, or
  `ErrNotModified` when the caller's tag is current (a poll costs one
  conditional GET), or `ErrNotFound`;
- `Put(key, body, ifMatch)`: unconditional with `""`, create-only with
  `Absent` (`*`), else only over the tag given; `ErrPrecondition` when the
  condition fails. The state document's compare-and-swap rests on this.

`Dir` (`bucket.go:46`) is a directory opened with `os.Root`, so a key
cannot leave it; `checkKey` (`bucket.go:74`) refuses unclean keys. Its
`Put` (`bucket.go:102`) checks the condition and writes under one mutex
(`check`, `:118`; `replace`, `:138`), writing to a temporary file and
renaming it over the object, so a reader never sees a torn object. It is
atomic within one process only: tests and development.

`s3bucket.Bucket` (`s3bucket.go:30`) passes S3's own conditions:
`If-None-Match` on GetObject, `If-Match` or `If-None-Match: *` on PutObject.
`mapErr` (`s3bucket.go:92`) maps 404, 304, 412 and 409 (S3's
ConditionalRequestConflict, the same outcome for a compare-and-swap) to
the bucket's errors, except that **`NoSuchBucket` is an error**, not
`ErrNotFound` (`s3bucket.go:96`): a missing bucket read as a missing
object would make the state store see no state yet, and a gateway a pool
never published. Reads are bounded by `MaxObject` (256 MiB default).

`s3bucket.Open` (`open.go:17`) is how both binaries name their bucket:
`s3://<bucket>/<prefix>` is an S3 bucket with the default AWS credential
chain (workload identity in the cluster), the prefix given a trailing `/`;
anything else is a directory (`bucket.OpenDir`).

`buckettest.Run` (`buckettest.go:16`) is the conformance suite: the
conditions one by one, and a race of 16 swaps from one tag of which
exactly one must win. It runs against `Dir` (`TestDir`, `bucket_test.go:10`)
and, through the real AWS SDK, against `fakeS3`, an in-process S3
stand-in (`TestS3Conformance`, `s3bucket_test.go:39`). `TestS3Publish`
(`:44`) runs a whole publication through it.

## 6. Signing (`lib/sign`)

`Signer` signs with one key and names it (`KeyID`); `Verifier` checks a
signature by key ID. Gateways hold only verifiers, keyed by ID, so a key is
rotated by publishing under a new ID while gateways trust both. Every
verifier refuses an ID it holds no key for with the same error
(`untrusted`, `sign.go:35`), wrapping `ErrBadSignature`.

- `Ed25519` / `Ed25519Keys` (`sign.go:40`, `:62`): tests, development,
  standalone publishing.
- `ECDSAKeys` (`ecdsa.go:15`): ECDSA P-256 over SHA-256, ASN.1 DER, as
  KMS's ECDSA_SHA_256 produces; `ParseECDSAPublicKey` (`ecdsa.go:32`)
  reads KMS's DER or PEM public key and refuses other curves.
- `Keys` and `LoadKeys` (`keys.go:17`, `:43`): what a pool gateway trusts
  (`-keys`, `cmd/golem-gateway/main.go:249`). Every `*.pub` file of the
  directory is a PEM `PUBLIC KEY`, Ed25519 or ECDSA P-256; its key ID is
  the PEM's `Key-ID` header if it has one (a KMS key's ARN), else the
  file's name without `.pub`. A key ID given by two files is refused,
  naming both (`keys.go:63`): one would replace the other, or, of two
  kinds, `Keys.Verify` would try only the Ed25519 one, so a misplaced
  file could shadow a trusted key. A directory with no keys is an error: a
  gateway trusts no snapshot without one. A key is rotated by adding the
  new file beside the old, then removing the old.
- `kmssign.Signer` (`kmssign.go:27`): the publishing key never leaves KMS.
  It sends SHA-256(msg) as a digest (KMS caps raw messages at 4 KB);
  `kmssign.PublicKey` (`kmssign.go:59`) fetches the public half at
  bootstrap and refuses a key that is not ECC_NIST_P256 for SIGN_VERIFY.
  The key ID is the key's ARN, so gateways must trust that exact string.
  Gateways never call KMS.

## 7. Keys and periods

`lib/key` ([contracts.md §5](../contracts.md#5-identifiers-and-headers-adr-0026)):
`glm_live_` or `glm_dev_`, then 49 base62 characters encoding 32 random
bytes and their CRC32 (`New`, `key.go:46`). `Parse` (`key.go:57`) checks the
prefix, the length, the alphabet and the checksum, so a mistyped key is
refused before any lookup. golem stores only `Hash` (SHA-256) and shows
only `Display`, the first 16 characters.

`lib/period`: `Of(t, zone)` is the calendar month a call started in, in
the installation's period zone ("2026-11"); `FirstDay` is the ledger's
date for it. Everything that names a period goes through here, so a call
near midnight belongs to one month for everyone.

## 8. Contract codes (`lib/contract`)

`codes.go` lists every refusal the gateway sends (`Code{Name, Status,
Origin}`), the origins, and golem's header names. `contract_test.go` reads
contracts.md and fails if the document and the code disagree
(`TestCodesMatchContracts`, `TestHeadersMatchContracts`): the document is
the source; add a code to both.

## 9. Entra tokens and OTLP (`lib/entra`, `lib/otlp`)

Both moved here from the gateway when golem-control came to need them too.

**`entra.Verifier`** (`entra.go:31`, ADR 0009, [entra.md](../entra.md))
verifies Entra ID v2 access tokens locally, for one API: the gateway's
(golem API's registration, scope `golem.access`) or the control API's
(its own registration, scope `golem.manage`, ADR 0046), so a token for one
is never accepted by the other. `Verify` (`entra.go:115`) accepts RS256
only (`:114`), a key from the cached set, an expiry, this tenant's v2
issuer and one of the `Audiences`, then names a user (the scope and an
`oid`, not an app-only token) or a service (no scope, `idtyp` "app", the
`golem.service` role, an `azp`); `golem.admin` marks either an admin. A
refusal is an `*entra.Error`, `Expired` set for an expired token so the
caller can say to get a new one. The key set is fetched by `Refresh` and
kept fresh by `Run` (hourly, and at most once a minute when a token names
an unknown key, `requestRefresh`, `:126`): the request path makes no
network call. A key set older than `MaxKeyAge` (24 hours by default,
`:121`) while refreshes fail refuses every token, so a key Entra has
revoked is not trusted for ever. Its tests are its callers':
`gateway/auth/entra_test.go` and `control/api/entra_test.go`.

**`lib/otlp`** is the OTLP/HTTP transport: `New` (`otlp.go:40`) the
gateway's three exporters, `LoggerProvider` (`otlp.go:93`) golem-control's
audit logs. Telemetry carries billing and audit data, so `Base`
(`otlp.go:103`) gives https unless `PlaintextOK` (`otlp.go:116`): the host is
a literal loopback address (a name is resolved when dialed, so is not held
to this machine; ADR 0043). Each exporter is given its whole URL, which
overrides the SDK's `OTEL_EXPORTER_OTLP_*` endpoint and insecure
variables, so none can turn TLS off or send the data elsewhere
(`TestOTLPIgnoresInsecureEnv`, `otlp_test.go:41`).

## 10. Worked example: a directory to a verified snapshot

Take a directory `config/` holding one file, `golem.yaml`: the `good`
fixture of `compile_test.go:14`, a Backend `anthropic` with one deployment
(`secretRef: {name: anthropic, key: api-key}`), a ModelRoute pooling that
deployment under one alias, and a ConfigMap.

1. **Read.** golem-control's `Controller.Step` calls `load`
   (`control/controller/controller.go:552`), which hashes the directory
   with `ContentHash`; it changed, so `DecodeFS` reads it (`decode.go:38`).
   Document 1 decodes into `objs.Backends`, document 2 into
   `objs.Routes`; the ConfigMap in document 3 is skipped (`decode.go:83`).
   A trial compile with a lease and no entitlements (`controller.go:570`)
   passes, and the objects become the good config.
2. **Compile.** `PublishAll`'s build function calls `Compile`
   (`compile.go:72`) with the publication's sequence, a lease and the
   state's entitlements (`controller.go:488`). `compileBackends`
   (`compile.go:122`) accepts `anthropic`; `backendAuth` compiles its auth
   to `secretRef: "anthropic/api-key"`; `deployments` checks the one
   deployment's https URL; `catalog.add` records the backend's dialect and
   the deployment's provider model. `compileRoutes` (`compile.go:208`)
   takes the default timeouts, and `pool` finds the member in the
   catalog, takes its dialect `anthropic.messages` as the route's and
   weight 1. `Options.Entitlements` supplies the state's scopes, told the
   route's alias, and `join` gives each its ScopePolicy. No problems: the snapshot gets
   `ValidUntil = Now + Lease` and its config digest.
3. **Encode and store.** `PublishAll` (`control/publish/publish.go:107`)
   checks the lease; `prepare` (`publish.go:176`) numbers the publication
   above the record and the bucket's verified floor, encodes
   (`snapshot.go:196`), hashes (`snapshot.go:230`) and puts
   `snapshots/<pool>/<hash>.json` (`publish.go:201`).
4. **Record, checkpoint, pointer.** It wins every pool's publication
   record in one fenced update of the state document (`publish.go:136`,
   ADR 0049); then `finish` (`publish.go:208`) signs and writes the
   checkpoint (`SignCheckpoint`, `pointer.go:85`), then signs the pointer
   with its `Expires` (`SignPointer`, `pointer.go:73`) and writes it.
5. **Verify.** A gateway's `Poll` (`gateway/snapshot/bucket.go:242`) does
   a conditional GET of the pointer; it changed, so `read` (`:371`) calls
   `openPointer` (`:305`), which calls `OpenPointer` with its `sign.Keys`
   (`pointer.go:79`) and refuses another pool's pointer or one below its
   floor. It notes the pointer's `Expires`, fetches the object, and
   `openSnapshot` (`:342`) checks `snapshot.Hash(body) == ptr.Hash`,
   `Decode`s it strictly and checks the snapshot's `PublishSeq` against
   the pointer's. The holder applies it, and the gateway saves its
   last-good copy and its local checkpoint.

An edit that misspells `alias` as `aliass` stops at step 1:
`x.yaml#2: ModelRoute: error unmarshaling JSON: ... unknown field "aliass"`
(`TestDecodeStrict`, `compile_test.go:154`); golem-control keeps
publishing the last good config.

## Invariants and where they are tested

| Invariant | Where enforced | Test |
|---|---|---|
| A misspelt CRD field or unknown kind is refused, naming file, document and field | `decodeInto`, `decode.go:111`; `Objects.decode`, `:73` | `TestDecodeStrict`, `lib/compile/compile_test.go:154` |
| Every compile problem names object, field path and fix | `adder` and the passes, `lib/compile` | `TestProblems`, `compile_test.go:86`; `TestEntitlementProblems`, `entitlements_test.go:73` |
| Equal configs give equal snapshot bytes, whatever their file layout | sorting in `Compile`, `compile.go:94`, and the passes; `Encode` | `TestDeterministic`, `compile_test.go:72` |
| No secret value or API key reaches a snapshot | `backendAuth`, `compile.go:189`; `localKeys`, `entitlements.go:96` | `TestCompile`, `compile_test.go:51`; `TestEntitlements`, `entitlements_test.go:35` |
| A scope's policy is refused field by field: a budget above 2^50 micro-units or negative, an unknown budget mode or one without a budget, a negative in-flight limit or ceiling, an unknown `features` or `providerStorage` | `scopePolicy`, `policy.go` | `TestScopePolicyCheck`, `policy_test.go`; `TestEntitlementProblems`, `entitlements_test.go` |
| A ScopePolicy names routed aliases, once per scope, and a budget over an unpriced model is the config's problem | `compilePolicies`, `checkPriced` | `TestPolicyProblems`, `policy_test.go` |
| A state scope with no ScopePolicy can call nothing; a ScopePolicy of no scope is not published; both are reported, neither stops the publication | `join`, `DefaultScope`, `policy.go` | `TestJoin`, `policy_test.go`; `TestRenew`; control/controller's `TestUnmatchedScopes` |
| An unknown, duplicate or miscased snapshot member, or another schema, is refused; another schema says to upgrade the gateway | `Decode`, `snapshot.go:202` | `TestDecodeFailsClosed`, `lib/snapshot/snapshot_test.go:35` |
| Only a pointer or checkpoint signed by a trusted key, in its own context, opens | `open`, `pointer.go:110` | `TestPointer`, `lib/snapshot/pointer_test.go:37`; `TestCheckpoint`, `:67`; `TestKMSPointer`, `lib/sign/kmssign/kmssign_test.go:48` |
| A gateway trusts the keys of its key directory, by Key-ID header or file name, and refuses an empty one, or one key ID given by two files | `LoadKeys`, `lib/sign/keys.go:43` | `TestLoadKeys`, `lib/sign/keys_test.go:53`; `TestLoadKeysRefusesARepeatedID`, `:21` |
| Of concurrent swaps from one tag exactly one wins, on Dir and S3 | `Dir.Put`, `bucket.go:102`; S3's conditional writes | `lib/bucket/buckettest/buckettest.go:56`, run by `bucket_test.go:10` and `s3bucket_test.go:39` |
| A missing S3 bucket is an error, never a missing object | `mapErr`, `s3bucket.go:96` | `TestNoSuchBucket`, `s3bucket_test.go:94` |
| An S3 object over `MaxObject` is refused, not buffered | `Bucket.Get`, `s3bucket.go:45` | `TestMaxObject`, `s3bucket_test.go:76` |
| A mistyped or truncated key is refused | `Parse`, `lib/key/key.go:57` | `TestRefused`, `lib/key/key_test.go:23` |
| Codes and headers match contracts.md | `lib/contract/codes.go` | `lib/contract/contract_test.go:37`, `:73` |
| Telemetry goes over TLS unless to a literal loopback address, whatever the environment says | `Base`, `lib/otlp/otlp.go:103` | `TestPlaintextOK`, `otlp_test.go:26`; `TestOTLPIgnoresInsecureEnv`, `:41` |
| An Entra token is accepted only RS256, for its API's audience and scope, with keys not too old | `Verifier.Verify`, `lib/entra/entra.go:115` | `gateway/auth/entra_test.go` (`TestEntra`, `TestStaleKeys`), `control/api/entra_test.go` |

## Known gaps

- **Snapshot encoding is JSON.** The design's protobuf encoding (ADR 0022)
  is to replace it behind the same `Encode` and `Decode`; control-plane.md
  §4 already speaks of `.pb` objects, which do not exist yet.
- **No real S3 or KMS in tests.** The S3 adapter is tested against an
  in-process stand-in through the real SDK, the KMS signer against a fake
  signer; an integration run in the target account, with the bucket policy
  that denies other writers, is open (control-plane.md §4).
- **Untested in `lib/`.** `s3bucket.Open`'s URL parsing has no test of its
  own, `lib/entra` is tested only through its callers, and
  `TestPointer` round-trips a pointer with a zero `Expires` (the
  gateway's `TestOverdue`, `gateway/snapshot/bucket_test.go:241`, covers
  an expired one).
- The YAML splitter treats only a bare `---` line as a separator: `---`
  followed by content on the same line is not split.
- Hook, one of ADR 0005's kinds, is not in v1alpha1 yet. ScopePolicy has
  the policy fields above; ADR 0005's capture ceiling and residency join
  it with capture.
- The CRDs' CEL rules (ADR 0005) are not written yet: the compiler is the
  only check of a ScopePolicy.
