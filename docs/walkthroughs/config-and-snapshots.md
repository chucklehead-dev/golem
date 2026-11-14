# Walkthrough: config, snapshots and their distribution

This is a guided reading of `lib/`, the code every golem binary shares: the
config CRDs, the compiler that turns them into a snapshot, the snapshot's
format, the signed pointer that publishes it, the bucket it travels
through, the signing keys, API keys, budget periods and the contract
codes. It is for an engineer new to the repository; read it with the code
open. The design behind it is in [ADR 0005](../adr/0005-config-crds-and-overrides.md)
(config as CRDs), [ADR 0006](../adr/0006-state-and-distribution-in-object-storage.md)
(state and snapshots in object storage), [ADR 0023](../adr/0023-standalone-gateway.md)
(the standalone gateway), [ADR 0044](../adr/0044-budgets-are-controls-not-ceilings.md)
(why decoders refuse what they cannot interpret), [control-plane.md §4](../control-plane.md#4-distribution)
(distribution) and [contracts.md](../contracts.md) (the external contracts).

## The pieces at a glance

| Package | What it is | Who uses it |
|---|---|---|
| `lib/api/v1alpha1` | the config CRDs as Go types, and a strict YAML reader | golemctl, golem-control, the standalone gateway |
| `lib/compile` | config objects in, a validated `snapshot.Snapshot` out, or every problem with its field path | the same three |
| `lib/snapshot` | the snapshot format, its strict decoder, the signed `Pointer` and the `Checkpoint` | publisher (control/publish), gateway (gateway/snapshot) |
| `lib/bucket` | the object store interface with S3's conditional semantics; `Dir`, a directory | everything that publishes or polls |
| `lib/bucket/s3bucket` | `Bucket` on Amazon S3 | production |
| `lib/bucket/buckettest` | the conformance suite every `Bucket` passes | the two above |
| `lib/sign`, `lib/sign/kmssign` | Signer and Verifier: Ed25519, ECDSA P-256 verification, a KMS signer | publisher signs, gateways verify |
| `lib/key` | golem's API key format | compile (LocalIdentity), control, gateway/auth |
| `lib/period` | budget period names ("2026-11") | gateway counters, ledger, control |
| `lib/contract` | refusal codes, origins and header names, checked against contracts.md | the gateway's every refusal |

## 1. Config CRDs (`lib/api/v1alpha1`)

`types.go` holds four kinds of group `golem.dev/v1alpha1`
(`APIVersion`, `types.go:8`):

- **Backend**: a provider or model server: `dialect` (one of
  `compile.Dialects`, `anthropic.messages` or `openai.chat`), `auth` (a
  header carrying either a Kubernetes `secretRef` or, standalone only, an
  `env` variable), `deployments` (one model at one URL each) and
  `network.allowPrivateAddresses`.
- **ModelRoute**: an `alias` callers send as their model name, a `pool` of
  backend deployments with weights, optional `timeouts`, `agent` and
  `approvedBetas`.
- **PriceList**: a currency and per-model rates in micro-units per million
  tokens ([contracts.md §4](../contracts.md#4-the-price-model-adr-0025);
  this version has the per-token rates only).
- **LocalIdentity**: standalone mode's stand-in for the state document's
  entitlements: scopes and their limits, dev keys, Entra users and
  services. Refused outside standalone mode (ADR 0023).

The package has no Kubernetes client dependency: the gateway, golemctl and
golem-control all import it.

**Decoding** (`decode.go`). `DecodeFS` (`decode.go:38`) lists every
`.yaml`/`.yml` file (`yamlFiles`, `decode.go:58`), sorts the names, and
splits each file into documents on `---` lines (`documents`,
`decode.go:120`). For each document (`Objects.decode`, `decode.go:72`):

1. it reads only `apiVersion` and `kind` (leniently);
2. an empty document, or one of another API group (a Kustomization, a
   Secret), is skipped;
3. a `golem.dev/` object of another version is an error naming the version
   this binary reads;
4. a golem kind is decoded **strictly** (`decodeInto`, `decode.go:110`,
   `yaml.UnmarshalStrict`): an unknown field is an error, so a misspelt
   field is never silently ignored; an unknown kind is an error too.

Every object keeps its origin as a `Source[T]` (file and 1-based document
number); `Source.Where` prints `golem.yaml#2`, which every compile problem
carries.

`ContentHash` (`decode.go:136`) hashes the YAML files' names and contents
in walk order: golem-control (`control/controller/controller.go:132`) and
the standalone gateway (`gateway/snapshot/file.go:40`) use it to skip
decoding and compiling a directory that has not changed.

## 2. The compiler (`lib/compile`)

One validation path (ADR 0005): `golemctl config validate`
(`cmd/golemctl/main.go:133`) runs it offline, golem-control on the live
config (`control/controller/controller.go:111` and its trial compile at
`:148`), the standalone gateway on its directory
(`gateway/snapshot/file.go:51`).

`Compile` (`compile.go:69`) reports **every** problem, not the first, as
`Problems`, each a `Problem{Object, Path, Msg}` such as

```
ModelRoute/claude (golem.yaml#2): spec.pool[0].deployment: Backend "anthropic" has no deployment "opus"
```

The object is named by `objectName` (`compile.go:104`), the path is the
field's JSON path with indices. These messages are user-facing and the
tests match them (`compile_test.go:86`, `entitlements_test.go:64`): change
their wording deliberately or not at all.

The passes, in order (the problems come out in this order):

1. **Options.** A published snapshot (not `Standalone`) needs a positive
   `Lease` (ADR 0030): the authorization lease becomes
   `Snapshot.ValidUntil`.
2. **Backends** (`compileBackends`, `compile.go:119`): a name, unique; a
   known dialect; auth by `backendAuth` (`compile.go:172`: a header and
   exactly one of `secretRef`/`env`, `env` standalone only, the secret
   compiled to a `"name/key"` reference, never a value); at least one
   deployment, each with a unique name, a model and a URL that `checkURL`
   (`compile.go:279`) accepts: https, or plain http only to a loopback IP
   or in standalone mode, and never credentials in the URL. Each backend
   enters a `catalog` (`compile.go:111`): its dialect, and each
   deployment's provider model.
3. **Routes** (`compileRoutes`, `compile.go:191`): a unique alias; a
   non-empty pool whose members name a catalogued backend and deployment;
   weights not negative (zero means 1); **one dialect per pool** (ADR 0033:
   conversion is explicit, never implied by a mixed pool); timeouts by
   `timeouts` (`compile.go:304`), defaulting to `DefaultMaxDuration` (30
   minutes) for the whole call, the whole call for headers, and
   `DefaultIdle` (5 minutes, capped by the whole call) for silence.
4. **Entitlements** (`entitlements.go:26`), below.
5. **Prices** (`priceList`, `entitlements.go:195`): a three-letter
   currency, one per installation; each model priced once; no negative
   rates.
6. **Budgets are chargeable** (`checkPriced`, `compile.go:242`): a scope
   with a budget must reach only priced models, or its budget could never
   be spent (contracts.md §4).

With no problems, backends and routes are sorted, so equal configs encode
to equal bytes (`TestDeterministic`, `compile_test.go:72`), and
`Source.ConfigDigest` is the SHA-256 of the compiled backends and routes.

### Standalone versus published

| | Standalone (`Options.Standalone`) | Published (golem-control) |
|---|---|---|
| Lease | none: `ValidUntil` zero | required (ADR 0030) |
| `auth.env` | allowed | refused: use `secretRef` |
| plain http | to any host | loopback addresses only (ADR 0017) |
| LocalIdentity | at most one; its entitlements | refused: entitlements come from the state |
| no identity at all | `Entitlements.Implicit`: one local principal, every route | empty, unless `Options.Entitlements` gives them |

### Entitlements

A **published** snapshot's entitlements come from the state document
(`control/state`): golem-control passes `Options.Entitlements`, a function
told the route aliases that exist (`compile.go:82`). Without one, the
snapshot authorizes no one. Compile still runs `entitlements` first, so
that a LocalIdentity in a published config is reported.

A **standalone** LocalIdentity is compiled by three helpers:

- `localScopes` (`entitlements.go:63`): unique names; non-negative budget,
  in-flight limits and ceilings; `features` restricted or compatible;
  `providerStorage` forbidden or allowed; every model alias routed.
  In-flight limits default to `DefaultPrincipalInFlight` (4) and
  `DefaultScopeInFlight` (16).
- `localKeys` (`entitlements.go:102`): each key must parse (`key.Parse`)
  and be a `glm_dev_` key (a live key never belongs in a file); its scope
  must exist and its models only narrow the scope's. Only the key's SHA-256
  and its display prefix enter the snapshot (`TestEntitlements` checks the
  key itself does not). Each key's principal becomes a `KeyOwner`, a
  member of its keys' scopes: a key is honoured only for a listed owner.
- `localMembers` (`entitlements.go:159`): users and services share one
  namespace of Entra IDs, since gateway/auth finds both by it; each needs
  at least one scope, and a default among them (the first if unset).

## 3. The snapshot format (`lib/snapshot/snapshot.go`)

A `Snapshot` (`snapshot.go:25`) is one pool's complete configuration at one
moment: `Schema` (`1`), `PublishSeq` (orders publications),
`IssuedAt`/`ValidUntil` (the lease), `Source` (the state version and config
digest it was built from), backends with secret *references*, routes,
entitlements, currency and prices. It is data only: the gateway compiles
it into lookup structures.

`Encode` (`snapshot.go:183`) is `encoding/json`'s `Marshal`, which writes
struct fields in declaration order: with the compiler's sorting, equal
snapshots give equal bytes, and `Hash` (`snapshot.go:209`), their hex
SHA-256, names the object in the bucket. So **reordering a struct's fields
changes every snapshot's hash.**

`Decode` (`snapshot.go:189`) is `encoding/json/v2` with
`RejectUnknownMembers`, which also refuses duplicate names and names in
another case (v2's defaults). A snapshot is policy, and a reader that
skipped a member it did not know would apply weaker policy: a renamed
check (`strict` became `declaredWorstCaseCheck`) read as off. ADR 0044
(Consequences) requires both decoders, this one and the CRD decoder, to
fail closed instead. A schema other than 1 is a `SchemaError` telling the
operator to upgrade gateways before the publisher. `TestDecodeFailsClosed`
(`snapshot_test.go:35`) covers each refusal.

## 4. Pointer and checkpoint (`lib/snapshot/pointer.go`)

The bucket holds, per pool (`PointerKey`, `ObjectKey`, `CheckpointKey`):

- `snapshots/<pool>/<hash>.json`: the snapshot, content-addressed, so
  writing it twice is harmless;
- `snapshots/<pool>/current`: the signed **pointer**, `{pool, publishSeq,
  epoch, source, issuedAt, expires, hash}`;
- `control/<pool>/checkpoint`: the signed **checkpoint**, `{pool,
  publishSeq}`.

Both are signed, each in its own context: `seal` signs
`"golem snapshot pointer v1\n"` or `"golem snapshot checkpoint v1\n"`
followed by the payload's bytes (`signedBytes`), so the publishing key's
signature over one kind means nothing as the other, or elsewhere. `open`
(behind `OpenPointer` and `OpenCheckpoint`) verifies before it decodes, and
decodes the verified payload strictly. The snapshot needs no signature of
its own: the gateway checks its bytes against the pointer's hash.

The checkpoint gives a gateway that starts with an empty disk a floor, so
that a replayed old pointer (anyone who can write the pool's objects can
copy one back) is refused. It is signed and also under a prefix only
golem-control's role may write (control-plane.md §3): a bucket policy
mistake alone does not let anyone lower it. A missing checkpoint is floor
zero only while the pool has no pointer either; a published pool's missing
checkpoint stops a gateway with an empty disk from starting.

A pointer's `expires` (ten minutes after issue by default, ten renewals)
is TUF's timestamp role as a signal: a gateway that sees nothing newer
than an expired pointer reports it overdue (`Bucket.Overdue`, logged once)
and goes on serving until its snapshot's `validUntil`, which is what bounds
a frozen or replayed bucket (ADR 0030).

## 5. The bucket (`lib/bucket`)

`Bucket` (`bucket.go:33`) is two calls with S3's conditional semantics:

- `Get(key, ifNoneMatch)`: the object and its entity tag, or
  `ErrNotModified` when the caller's tag is current (a poll costs one
  conditional GET), or `ErrNotFound`;
- `Put(key, body, ifMatch)`: unconditional with `""`, create-only with
  `Absent` (`*`), else only over the tag given; `ErrPrecondition` when the
  condition fails. The state document's compare-and-swap rests on this.

`Dir` (`bucket.go:46`) is a directory opened with `os.Root`, so a key
cannot leave it; `checkKey` refuses unclean keys. Its `Put` checks the
condition and writes under one mutex (`check`, `replace`), writing to a
temporary file and renaming it over the object, so a reader never sees a
torn object. It is atomic within one process only: tests and development.

`s3bucket.Bucket` (`s3bucket.go:29`) passes S3's own conditions:
`If-None-Match` on GetObject, `If-Match` or `If-None-Match: *` on PutObject.
`mapErr` (`s3bucket.go:88`) maps 404, 304, 412 and 409 (S3's
ConditionalRequestConflict, the same outcome for a compare-and-swap) to
the bucket's errors. Reads are bounded by `MaxObject` (256 MiB default).

`buckettest.Run` (`buckettest.go:15`) is the conformance suite: the
conditions one by one, and a race of 16 swaps from one tag of which
exactly one must win. It runs against `Dir` (`bucket_test.go:10`) and,
through the real AWS SDK, against `fakeS3`, an in-process S3 stand-in
(`s3bucket_test.go:36`). `TestS3Publish` runs a whole publication through
it.

## 6. Signing (`lib/sign`)

`Signer` signs with one key and names it (`KeyID`); `Verifier` checks a
signature by key ID. Gateways hold only verifiers, keyed by ID, so a key is
rotated by publishing under a new ID while gateways trust both.

- `Ed25519` / `Ed25519Keys` (`sign.go:35`, `:50`): tests, development,
  standalone publishing.
- `ECDSAKeys` (`ecdsa.go:15`): ECDSA P-256 over SHA-256, ASN.1 DER, as
  KMS's ECDSA_SHA_256 produces; `ParseECDSAPublicKey` reads KMS's DER or
  PEM public key and refuses other curves.
- `kmssign.Signer` (`kmssign.go:27`): the publishing key never leaves KMS.
  It sends SHA-256(msg) as a digest (KMS caps raw messages at 4 KB);
  `kmssign.PublicKey` fetches the public half at bootstrap and refuses a
  key that is not ECC_NIST_P256 for SIGN_VERIFY. The key ID is the key's
  ARN, so gateways must trust that exact string. Gateways never call KMS.

## 7. Keys and periods

`lib/key` ([contracts.md §5](../contracts.md#5-identifiers-and-headers-adr-0026)):
`glm_live_` or `glm_dev_`, then 49 base62 characters encoding 32 random
bytes and their CRC32 (`New`, `key.go:45`). `Parse` (`key.go:56`) checks the
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

## 9. Worked example: a directory to a verified snapshot

Take a directory `config/` holding one file, `golem.yaml`, with a Backend
`anthropic` (one deployment `sonnet`, `secretRef: {name: anthropic, key:
api-key}`) and a ModelRoute `claude` pooling it (this is `good` in
`compile_test.go:14`).

1. **Read.** golem-control's `load` (`control/controller/controller.go:132`)
   hashes the directory with `ContentHash`; it changed, so `DecodeFS`
   reads it (`decode.go:38`). Document 1 decodes into `objs.Backends`,
   document 2 into `objs.Routes`; a ConfigMap in document 3 is skipped
   (`decode.go:82`).
2. **Compile.** `Compile` (`compile.go:69`) with a lease and the state's
   entitlements. `compileBackends` accepts `anthropic` and catalogues
   `{anthropic, sonnet} → claude-sonnet-4-5`; its auth compiles to
   `secretRef: "anthropic/api-key"`. `compileRoutes` finds the pool member
   in the catalog, takes its dialect `anthropic.messages`, weight 1, and
   the default timeouts. `Options.Entitlements` supplies the state's
   scopes, told the alias `claude`. No problems: the snapshot gets
   `ValidUntil = Now + Lease` and its config digest.
3. **Encode and store.** `Publisher.Publish`
   (`control/publish/publish.go:63`) checks the record's epoch, numbers
   the publication `seq`, encodes (`snapshot.go:183`), hashes
   (`snapshot.go:209`) and puts `snapshots/<pool>/<hash>.json`
   (`publish.go:84`).
4. **Record, checkpoint, pointer.** It wins the pool's publication record
   in the state document by compare-and-swap (`publish.go:87`), writes the
   checkpoint (`publish.go:100`), then `SignPointer` (`pointer.go:49`) and
   the pointer (`publish.go:108`).
5. **Verify.** A gateway's `Poll` (`gateway/snapshot/bucket.go:111`) does a
   conditional GET of the pointer; it changed, so `read`
   (`gateway/snapshot/bucket.go:136`) calls `OpenPointer` with its
   `Ed25519Keys` or `ECDSAKeys` (`pointer.go:63`), refuses another pool's
   pointer or one below its floor, fetches the object, checks
   `snapshot.Hash(body) == ptr.Hash`, `Decode`s it strictly and checks the
   snapshot's `PublishSeq` against the pointer's before applying it and
   saving its local checkpoint.

An edit that misspells `alias` as `aliass` stops at step 1:
`x.yaml#2: ModelRoute: error unmarshaling JSON: ... unknown field "aliass"`
(`TestDecodeStrict`, `compile_test.go:154`); golem-control keeps
publishing the last good config.

## Invariants and where they are tested

| Invariant | Test |
|---|---|
| A misspelt CRD field or unknown kind is refused, naming file, document and field | `lib/compile/compile_test.go:154` |
| Every compile problem names object, field path and fix | `compile_test.go:86`, `entitlements_test.go:64` |
| Equal configs give equal snapshot bytes, whatever their file layout | `compile_test.go:72` |
| No secret value or API key reaches a snapshot | `compile_test.go:51`, `entitlements_test.go:35` |
| An unknown, duplicate or miscased snapshot member, or another schema, is refused | `lib/snapshot/snapshot_test.go:35` |
| Only a pointer signed by a trusted key, in the pointer context, opens | `lib/snapshot/pointer_test.go:37`, `lib/sign/kmssign/kmssign_test.go:48` |
| Of concurrent swaps from one tag exactly one wins, on Dir and S3 | `lib/bucket/buckettest/buckettest.go`, run by `bucket_test.go:10` and `s3bucket_test.go:36` |
| A mistyped or truncated key is refused | `lib/key/key_test.go:23` |
| Codes and headers match contracts.md | `lib/contract/contract_test.go:37`, `:73` |

## Known gaps

- **Snapshot encoding is JSON.** The design's protobuf encoding (ADR 0022)
  is to replace it behind the same `Encode` and `Decode`; control-plane.md
  §4 already speaks of `.pb` objects, which do not exist yet.
- **No real S3 or KMS in tests.** The S3 adapter is tested against an
  in-process stand-in through the real SDK, the KMS signer against a fake
  signer; an integration run in the target account, with the bucket policy
  that denies other writers, is open (control-plane.md §4).
- **A newer schema with new members** is refused as an unknown member
  before the schema is read, so its error does not say "upgrade the
  gateway" (see `Decode`, `snapshot.go:189`).
- The YAML splitter treats only a bare `---` line as a separator: `---`
  followed by content on the same line is not split.
- ScopePolicy and Hook, two of ADR 0005's kinds, are not in v1alpha1 yet.
