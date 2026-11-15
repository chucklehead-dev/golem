# golem: operations

Status: draft. How golem is installed and run day to day, without an
operator (ADR 0036), on the platform's own tools (ADR 0040): Helm charts
delivered by Argo CD, Argo Rollouts for gateway canaries, Upbound's managed
Crossplane for cloud resources, and a few golem-control jobs.

## 1. What is installed, and by what

| Piece | Delivered as | Notes |
|---|---|---|
| Config CRDs (`Backend`, `ModelRoute`, `ScopePolicy`, `Hook`, `PriceList`) | CRD definitions in the chart (first sync wave); objects in the GitOps repository (CODEOWNERS: platform team), synced by Argo CD | Validated by schema CEL rules at `kubectl apply` and by `golemctl config validate` in CI; status written by golem-control (ADR 0005) |
| golem-control | Helm chart: Deployment (two replicas), Service, PDB, ServiceAccount with workload identity, RBAC to read config CRDs, write their status, hold the leader Lease and update the collector's Langfuse Secret | One installation, on EKS (ADR 0008) |
| Gateway pools | Helm chart per pool: Argo Rollout (three replicas across zones), Service, PDB, ServiceAccount, a mount/reference to the separately managed emergency-denylist ConfigMap, provider Secrets mounted for the pool's backends, egress ServiceEntries, the `AuthorizationPolicy` limiting who may reach it | Internal pools and the edge pool (its ingress, WAF and source ranges) |
| Valkey | ElastiCache, composed by Crossplane per site | In-transit encryption and RBAC: the gateways run with `-valkey-tls` (the system's roots, or `-valkey-ca`) and `-valkey-user`, the password in `$VALKEY_PASSWORD` from a Secret, never a flag |
| Bucket, KMS keys, IAM roles, SCPs, Entra registrations | Crossplane claims: `GolemInstallation` once, `GolemSite` per remote site (ADR 0040) | Includes the bypass SCP of ADR 0039 and remote sites' Roles Anywhere or federation |
| Argo CD Applications | An ApplicationSet over the site list; sync waves: CRDs, then golem-control, then gateway pools | |
| ClickHouse DDL, Grafana dashboards and alerts | Chart hooks (a Job for DDL; ConfigMaps for Grafana's sidecar) | |
| Capture collector | A golem-owned OpenTelemetry collector Deployment: Langfuse exporters only, accepts only gateway pool identities, holds the capture credentials Secret (ADR 0041 §7.4) | The enterprise collector keeps operational telemetry and no Langfuse credentials |

## 2. Day-2 operations

| Operation | How |
|---|---|
| **Upgrade** | A chart version bump in the GitOps repository; Argo CD syncs in waves: golem-control first (rolling; reads the state document's n−1 schema), then gateway pools. Stored formats change by the two-release rule (PRD §9.14). |
| **Gateway rollout and canary** | Argo Rollouts: a canary step (one replica or a traffic share) with an analysis on golem's `error.origin = gateway` rate and latency, promoted when they hold, aborted otherwise. New pods become ready only with a valid snapshot. |
| **Drain** | `preStop` turns readiness off and waits for in-flight streams up to the drain grace (default 10 minutes for agent streams), then ends them with a dialect error event; a call arriving once the drain's wait has begun is refused in its dialect (`gateway.not_ready`, 503 with Retry-After), so the SDK retries it on another gateway; `terminationGracePeriodSeconds` follows it. |
| **Provider credential rotation** | Update the Secret; kubelet refreshes the mounted file; the gateway re-reads it. Workload identity needs none. |
| **Backend maintenance** | Planned: `Backend.spec.disabled` or `weight: 0` in GitOps. Unplanned: an expiring override (`golemctl overrides create --disable deployment/... --for 2h --reason ...`). |
| **Incident freeze** | A freeze override stops publication to a pool; gateways keep their snapshot until the lease runs out (ADR 0032). |
| **Leadership and tampering** (ADR 0049) | The bucket policy lets only golem-control's role write `state/`, `lead/`, `control/` and `snapshots/`: it is the only defence against a rollback of both the lease object and the document, or of a record with an older checkpoint and pointer. `lead/lease.json` needs a noncurrent-version expiry and is left out of the backup replication (it holds no authority). A `tampered` publication failure pages (§2.2: a pool's checkpoint does not verify or is missing, `ErrTampered`; that pool stops publishing until restored from the bucket's versions), and so does a pool's publication sequence or the leadership epoch going down (§2.2, *Sequence or epoch went back*); the document's Object Lock history then shows when and by whom. |
| **Config and prices** | golem-control trial-compiles each edit of the config directory with the state's scopes. An edit that does not compile (a ScopePolicy whose budget reaches a model no PriceList prices among its problems; `golemctl config validate` in CI catches it first) is refused (`config not applied, the last good one is published`) and the last good config keeps being renewed: fix the edit. A golem-control that starts (or takes over) while the directory does not compile renews the publication already in the bucket with the current entitlements and logs `renewing the published config: the config directory does not compile` every step: the gateways stay authorized, but no config change reaches them until the directory is fixed. A scope of the state's with no ScopePolicy is published with no models (its members can call nothing), and a ScopePolicy naming no scope of the state's is not published; both are logged (`scopes and ScopePolicies do not match`, naming each) every step until they match: merge the scope's ScopePolicy, or create the scope (`golemctl scopes create <id>`), or correct the name. Every other scope and pool is unaffected. |
| **Changing a scope's policy** (its models, budget, budget mode, limits) | A pull request to the scope's `ScopePolicy` (its name is the scope's ID) in the GitOps repository, reviewed by the platform team (CODEOWNERS), checked by `golemctl config validate` in CI, synced by Argo CD; the next publication carries it (within a minute of golem-control reading it). The control API no longer sets policy: `golemctl scopes create <id>` creates the scope, which can call nothing until its ScopePolicy is merged. A new scope's budget starts in `budgetMode: observe` (the default): watch its ledger rows' `over_budget` for a period, then move it to `warn` or `enforce` by another pull request (PRD §9.1; an M1 exit criterion). A budget raised mid-month takes effect at the next publication; an emergency stop for one scope is the same pull request (`models: []`), or a key revocation for one caller. History and review are the repository's (ADR 0050). |
| **Publishing key rotation** | See [§2.1](#21-rotating-the-publishing-key). |
| **A deleted or rolled-back state document** | A `regressed` publication failure pages (§2.2: `ErrRegressed` from the publisher, a pool's publication record below its verified checkpoint or pointer, so an older document was put back and that pool is not published; or `state.ErrRegressed` from the store). `ErrMissing` stops golem-control at start (`state/document.json` is missing though the lease object or a pool's checkpoint or pointer exists): it shows as golem-control's metrics absent and its pods crash-looping with that message (§2.2, *golem-control silent*). Either way, stop every golem-control replica, put the newest version of `state/document.json` back from the bucket's versions (or its Object Lock replica), and start them. Only if no newer version can be recovered: `golem-control bootstrap -bucket <url> -keys <the gateways' -keys directory> -pools <every pool> -actor <who>`: with no document it creates an empty one (every caller refused until entitlements are created again); with `-adopt` it accepts the older document that is there: before running it, redo through the API the revocations made since that version (the audit archive, `audit/<seq>.json`, lists them), since publication resumes from it. Each pool's record is set to what the bucket proves was published, so publication goes on above what gateways trust. Audited as `state.bootstrap` or `state.adopt`. |
| **State restore** | Fetch the wanted older version of `state/document.json` from the bucket's history (or its Object Lock replica), then `golem-control restore -bucket <url> -from old.json -actor <who>`: a dry run listing what the backup holds that would be **withheld** (rights removed since: re-grant each as its own change) and what would be **rolled back** (rights added since; keys minted since are revoked). Add `-apply` to write it, audited as `state.restore`. The fence, the publication records, the revocations and the audit outbox are always the current document's; a restore never widens a right. |
| **Snapshot objects** | Every publication writes a new content-addressed snapshot object (`snapshots/<pool>/<hash>.json`; a renewal's differs in its lease), about 1,440 a pool a day, and none is deleted by golem. The bucket's lifecycle rule expires objects under `snapshots/<pool>/` other than `current` two days after creation: longer than any authorization lease (four hours), so no object a gateway could still apply is removed, and a publisher down for days has let its lease lapse long before. The Crossplane composition sets the rule; a hand-built bucket needs it. |
| **Emergency revocation** | Site responders edit a separately bootstrapped denylist (`golem-gateway -denylist`; format in control-plane.md §5) outside Argo ownership/pruning; confirm projected-volume propagation (gateways re-read it every five seconds and log each list applied, or why an edit was not). It refuses new requests; calls already in flight finish (no cancellation in flight, ADR 0041). No seconds guarantee until measured; local Kubernetes delivery must work. Keep denials until fresh durable revocation is confirmed (ADR 0041 §1). |
| **Authorization runway** | The alerts of §2.2 over `golem.snapshot.lease_remaining`, `golem.snapshot.age` and `golem.control.publish.time`, with ADR 0032's thresholds. Edge pools warn after one missed 60-second renewal and page after two, before their five-minute expiry; include bounded clock skew and name the actual expiry time (`golem.control.publish.valid_until`). |
| **Langfuse projects** | A golem-control job creates a project when a scope first needs its own and adds its credentials to the capture collector's Secret (ADRs 0035, 0041). |
| **Second site** | The bucket replicated to the standby region; its gateways read the replica; a golem-control there scaled to zero; the failover runbook promotes the replica and scales up (ADR 0008). |
| **Support** | `golemctl support-bundle`: CRD statuses, gateway status objects, recent errors, a flight-recorder trace from a chosen gateway. |

### 2.1 Rotating the publishing key

golem-control signs each pool's pointer and trusted checkpoint with
`-signing-key`; gateways verify them with the public keys in their
`-keys` directory, by key ID; golem-control verifies the checkpoint and
pointer already in the bucket before it numbers past them, with the
signing key's own public half and any in `-trusted-keys`. A checkpoint it
cannot verify stops that pool's publication (`ErrTampered`), so the
signer is never switched alone. In order:

1. **Make the new key under a new key ID**: a new KMS key (its ARN is its
   ID), or for a file key a new file name (`publishing-2.key`: a file
   key's ID is its name). Two keys under one ID cannot be told apart by
   any verifier. Print its public half: `golem-control pubkey <new key>
   > new.pub`.
2. **Gateways trust the new key first**: add `new.pub` beside the old key
   in every pool's `-keys` directory and roll the pools out. Until a
   gateway trusts the new key it refuses every pointer signed with it
   (and keeps serving its snapshot until its lease ends), so every pool
   must be done before step 3.
3. **Switch golem-control's signer, the old key still trusted**: put the
   old key's public half (`golem-control pubkey <old key>`) in a
   directory given as `-trusted-keys`, set `-signing-key` to the new key,
   and roll golem-control out. Its first publication (within a minute)
   verifies the old-key checkpoint and pointer under `-trusted-keys`, and
   writes each pool's checkpoint and pointer signed with the new key.
   Check every pool logs `published` and none `ErrTampered`.
4. **After a full authorization lease** (four hours for internal pools;
   the longest lease of any pool), retire the old key: remove it from
   every gateway's `-keys` and from golem-control's `-trusted-keys`
   (drop the flag), then disable the old key (KMS: schedule its
   deletion). By then nothing a gateway holds or the bucket keeps as
   current was signed with it.

If golem-control was switched without `-trusted-keys` and stopped with
`ErrTampered` naming a checkpoint that does not verify, step 3 is the
fix: add the old key's public half there and restart it. Restoring the
checkpoint from the bucket's versions does not help: those were signed
with the old key too.

### 2.2 Alerts

golem-control exports its metrics over OTLP with `-telemetry <collector>`
(each replica its own, its resource naming it as `service.instance.id`);
gateways with an OTLP `-telemetry` export theirs, under their resource's
`golem.site` and `golem.pool`. Every instrument below is observable: read
from the component's state when the SDK collects (every minute), never on
a request. Times are Unix seconds, so "seconds since" is the rule's
`now() - metric`. A replica that does not lead keeps reporting its own
last publication, so rules over golem-control take the newest across
replicas (`max by (pool)`).

| Instrument | Kind, unit | Attributes | Meaning |
|---|---|---|---|
| `golem.control.publish.time` | gauge, s | `pool` | When this replica last published the pool successfully |
| `golem.control.publish.seq` | gauge, {count} | `pool` | That publication's sequence |
| `golem.control.publish.valid_until` | gauge, s | `pool` | The lease it issued (the snapshot's `ValidUntil`): minus `publish.time`, the lease at publication; minus now, the runway left if nothing is published again |
| `golem.control.publish.failures` | counter, {count} | `pool`, `reason` | Publication attempts that failed or were degraded; `reason` is `tampered`, `regressed`, `fenced`, `conflict`, `scope_policy_unmatched`, `config_renewing` or `other` (every pool and reason is reported, from zero). A failing pool is retried with backoff (two seconds, doubling to the one-minute renewal interval) and at once on a change, so it counts at least once a minute while it fails, and the healthy pools publish at their own cadence |
| `golem.control.leader`, `golem.control.leader.epoch` | gauge, 1 / {count} | | 1 while this replica leads; the epoch it leads at (0 when not) |
| `golem.control.scope_policy.unmatched` | gauge, {count} | | Scopes of the state's with no ScopePolicy (published able to call nothing), and ScopePolicies of no scope, at the last publication |
| `golem.control.config.renewing` | gauge, 1 | | 1 while the config directory is not what is published (it does not compile, or an edit was refused) |
| `golem.control.spend.age`, `golem.control.spend.stale` | gauge, s / 1 | `period` | Known spend's boundary age (`spend.Job.Age`) and whether it is stale (`Job.Stale`); with `-clickhouse` only |
| `golem.control.spend.ingestion_lag`, `golem.control.spend.ingestion_behind` | gauge, s / 1 | `site` | How far each site's ledger rows are behind now, and whether past the allowance (or gone) (`spend.Job.Lags`); with `-clickhouse` only |
| `golem.control.entra_sync.time` | gauge, s | | When the group sync last succeeded on this replica; with `-entra-sync-client-id` only |
| `golem.control.entra_sync.guard_trips` | counter, {count} | | Syncs refused by the removal guard (`ErrTooManyRemovals`): over `-entra-sync-max-removals` memberships of still-active users would end; the offboarding of disabled or deleted users in the same step is written regardless |
| `golem.control.audit.outbox` | gauge, {count} | | Audit events in the document's outbox |
| `golem.control.audit.shipped.time` | gauge, s | | When an audit shipping step last succeeded on this replica; with `-audit-archive` only |
| `golem.snapshot.age` | gauge, s | | Now − the applied snapshot's `IssuedAt` (PRD §9.13) |
| `golem.snapshot.lease_remaining` | gauge, s | | The applied snapshot's `ValidUntil` − now; negative once it has run out |
| `golem.snapshot.seq` | gauge, {count} | | The applied snapshot's publication sequence |
| `golem.snapshot.pointer_overdue` | gauge, 1 | | 1 while the newest pointer seen is past its `Expires` (pool mode) |
| `golem.snapshot.last_good` | gauge, 1 | | 1 while the snapshot served is the last-good copy from disk, the bucket not read since a start (pool mode) |
| `golem.limit.down` | gauge, 1 | | 1 while the limiter believes the site's Valkey unreachable |
| `golem.limit.waivers` | counter, {count} | | Calls admitted as waivers (outage, or the grace after it) |
| `golem.limit.held`, `golem.limit.dropped` | gauge / counter, {count} | | Spend increments held for replay; held ones dropped (the counter lacks them) |
| `golem.denylist.entries` | gauge, {count} | | Entries in the emergency denylist in force (with `-denylist`) |

The alerts, in words over these metrics (Grafana's alert rules, delivered
with the chart, §1). Every alert on the authorization names the time
gateways start refusing: the pool's lowest `golem.snapshot.lease_remaining`
added to now, or `max by (pool) golem.control.publish.valid_until`.

| Alert | Expression | Severity | Runbook |
|---|---|---|---|
| **Authorization runway short** | any gateway's `golem.snapshot.lease_remaining` < 1 h (edge pools: < 3 min) | incident | Nothing has been published for hours. Work the publication alerts below from the top; if golem-control cannot be fixed before the expiry, decide the incident freeze (§2) or the standby site's failover (§2, *Second site*). |
| **Authorization lapsed** | any gateway's `golem.snapshot.lease_remaining` ≤ 0 | page | That gateway refuses every request and fails `/readyz`. As above; a gateway serves again with the next publication it reads. |
| **Pool not published** | for a pool, `now() - max(golem.control.publish.time)` over replicas > 3 min (three renewal intervals: ADR 0032's two minutes and a collection); pages > 30 min. Edge pools: warn > 2 min, page > 3 min | warning, then page | Look at the leader's log (`golem-control`'s `err`, once until it changes) and the failure counter's `reason`; the rows below name each. No leader: see *No leader*. |
| **Snapshot old at a gateway** | `golem.snapshot.age` > 3 min (PRD §9.13), or `golem.snapshot.pointer_overdue` = 1, for 5 min; while the pool *is* published | warning | The gateway does not see what is published: its bucket access (credentials, network, the replica it reads) or a publication it refuses (its log names it: below the floor, a signature its `-keys` do not trust after a key rotation, §2.1; a pointer "in this gateway's future" or a lease "longer than the longest lease": the publisher's or the gateway's clock is off, fix NTP, or raise `-max-lease` to a pool's approved longer lease). |
| **Serving the last-good copy** | `golem.snapshot.last_good` = 1 for 5 min | warning | The gateway started while the bucket was unreachable and has not read it since: as *Snapshot old*. It serves until the copy's lease ends. |
| **Checkpoint tampered** | `increase(golem.control.publish.failures{reason="tampered"})` > 0 | page | That pool is not published. If the publishing key was just rotated, add the old key to `-trusted-keys` (§2.1); otherwise restore `control/<pool>/checkpoint` from the bucket's versions and find who wrote it (CloudTrail). |
| **State document went back** | `increase(golem.control.publish.failures{reason="regressed"})` > 0 | page | *A deleted or rolled-back state document* (§2). |
| **Sequence or epoch went back** | for a pool, `max(golem.control.publish.seq)` below its maximum over the last day; or `max(golem.control.leader.epoch)` below its maximum over the last day | page | A rollback that bypassed the publisher's guard: the bucket policy's writers (*Leadership and tampering*, §2), the document's Object Lock history. |
| **Publication failing** | `increase(golem.control.publish.failures{reason=~"other\|conflict"}[2m])` > 0 for 5 min (a window over the backoff's one-minute retries) | warning (page via *Pool not published*) | The leader's log names it: KMS refusing to sign (the key policy, the role), the bucket refusing a write (the bucket policy), a build error. `conflict` alone, briefly, is a concurrent writer. |
| **Fenced repeatedly** | `increase(golem.control.publish.failures{reason="fenced"}[2m])` > 0 for 5 min | warning | A replica believes it leads but the document's fence names another: clock trouble or a partition between the replicas' views of the lease. Check both replicas' `golem.control.leader`; restart the one that is fenced. |
| **No leader** | `max(golem.control.leader)` over replicas = 0 for 2 min | page | Both replicas fail to take the lease: their logs (`lead:`), the bucket's `lead/lease.json` and its policy. |
| **Config not applied** | `golem.control.config.renewing` = 1 for 10 min | warning | *Config and prices* (§2): fix the edit the log names; gateways stay authorized, but no config change reaches them. |
| **Scope policy unmatched** | `golem.control.scope_policy.unmatched` > 0 for 10 min | warning | *Config and prices* (§2): the leader's log names each scope with no ScopePolicy (merge one: its members can call nothing until then) and each ScopePolicy of no scope (create the scope, or correct the name). |
| **Known spend stale** | `golem.control.spend.stale` = 1 (on the leader) | warning; page after 1 h | ClickHouse is unreachable or the query fails (the leader's `known spend` error): gateways count their own site's spend and the stale result, and miss other sites' since its boundary (ADR 0048). |
| **Ledger ingestion behind** | `golem.control.spend.ingestion_behind` = 1 for a site for 10 min, while that site's gateways serve traffic | warning | The site's rows reach ClickHouse later than `-spend-allowance`: known spend's boundary misses them (other sites' budgets undercount that site's spend). Check the site's node-local collector and its queue; a quiet site also reads as behind (the ledger cannot tell quiet from late), hence the traffic condition. |
| **Group sync not succeeding** | on the leader, `now() - golem.control.entra_sync.time` > 1 h | warning | The sync's error in the leader's log: Graph or the federated token. Memberships stop following Entra (removals included) until it succeeds. |
| **Group sync guard tripped** | `increase(golem.control.entra_sync.guard_trips)` > 0 | warning | A sync would end more memberships of still-active users than `-entra-sync-max-removals` (a group re-bound or emptied, a directory answering empty), and wrote none of them; users Entra reports disabled or deleted were offboarded anyway (the log's `entra sync` line, `offboarded`). If the removals are intended, raise the flag for one run; otherwise investigate the directory change. |
| **Audit outbox not draining** | on the leader, `now() - golem.control.audit.shipped.time` > 15 min, or `golem.control.audit.outbox` > 1,000 and growing for 30 min | warning; page at 10,000 | The leader's `audit:` error: the archive refusing a write (its role, Object Lock), or `ErrMismatch` (an archived event differs: a person looks before anything is trimmed). Without `-audit-archive` the outbox only grows. |
| **Limiter down** | `golem.limit.down` = 1 for 5 min | warning; page after 30 min | The site's Valkey is unreachable (a gateway that started meanwhile starts down, ready, retrying the connection): admission is on local counts and every admission is a waiver (`golem.limit.waivers` rising), held increments (`golem.limit.held`) wait for its return. Fix Valkey (ElastiCache, the network, the gateways' credentials). |
| **Held increments dropped** | `increase(golem.limit.dropped)` > 0 | warning | An outage outlasted the hold's cap, or a gateway left with Valkey down: the counter lacks that spend until known spend's boundary passes it (the ledger has the rows). |
| **Denylist differs across a site** | `golem.denylist.entries` not equal across a site's gateways for 5 min | warning | The projected volume has not reached every gateway, or an edit was refused (the gateway logs why and keeps the previous list). |
| **golem-control silent** | `absent(golem.control.leader)` for 5 min | page | No replica is exporting: pods down or crash-looping (`ErrMissing` at start, a broken flag), or the collector path. |
| **Gateway silent** | `absent(golem.snapshot.age)` for a pool for 5 min | page | No gateway of the pool exports: the pool or the collector path is down (PRD §9.13). |

## 3. Sites

Each cluster's charts are deployed by Argo CD's ApplicationSet, which
already knows every cluster; nothing in one cluster reaches another's API server
(ADR 0008). Remote sites run gateway pools and a Valkey; their config CRDs
live in the EKS cluster, selected by a `site` label.

## Spending: what golem controls, and what the provider account must

Golem's budgets are admission and monitoring controls, not spending
ceilings (ADR 0044). They stop new calls once known spend reaches a
budget, within about a minute on every site; calls in flight, estimates
that come out low and work a provider bills that golem never saw can go
past it. The financial boundary is the provider account's:

- **Required before production traffic:** every provider account golem
  routes to (ADR 0038's dedicated accounts) has a spending limit or a
  budget alarm with an enforced action at the provider (an Anthropic
  workspace spend limit, an OpenAI project budget, an AWS Budgets action
  that denies Bedrock invocation), sized to what finance accepts losing
  in the worst case. Golem cannot check it; the release checklist records
  who set it and at what level.
- **Reconciliation is the record:** the monthly job books the provider's
  bill against the ledger (ADR 0038). Budget reports are corrected by it.
  A large unattributed or estimated share is investigated, not tuned away.
- **Expected exposure, not a guarantee:** per principal, roughly its
  in-flight limit times its largest declared call, plus a refresh lag's
  spend across sites (ADR 0031). The declared-worst-case check (`declaredWorstCaseCheck`)
  narrows that by a call and refuses calls that declare no worst case; it
  is not a ceiling either.

## Telemetry transport: the trusted hop and its owner

Ledger rows, traces and metrics leave the gateway over OTLP/HTTP (golem-control's metrics and audit records take the same transport, `lib/otlp`). What
golem's code does, and what it leaves to the deployment:

- **Gateway to a collector in its own pod** (ADR 0043's sidecar
  variant): a literal loopback address, plaintext. The hop stays inside
  the pod's network namespace, which only the pod's own containers
  share. Golem refuses plaintext to any other address, the name
  `localhost` included.
- **Gateway to a DaemonSet collector** (ADR 0043's other variant): the
  node's address, which is not loopback from the pod, so the hop is over
  TLS, and the collector must present a certificate for that address.
  Golem has no CA option of its own, so a private CA is trusted through
  the container's system roots (`SSL_CERT_FILE` or `SSL_CERT_DIR`).
- **Gateway to any other collector:** server-authenticated TLS. The
  gateway verifies the collector's certificate against the system roots
  and **presents no client certificate**.
- **Collector onward (to the central collector and ClickHouse):** the
  collector's own configuration. Golem neither sets nor checks it.

**Who authenticates the client is not yet decided** (golem#7). If
server-authenticated TLS satisfies policy for a hop, the release
checklist says so. If policy requires client certificates, the
deployment names who presents them on each non-loopback hop (the
gateway, the node-local collector or a mesh sidecar) and demonstrates
it. Until then, mTLS is an assumption about the deployment, not a
property golem provides.

## Incident and delivery requirements (ADR 0041)

[ADR 0041](adr/0041-security-boundaries-and-incident-controls.md) specifies the denylist lifecycle, reserved restrictive-action
audit capacity, dedicated capture pipeline, inherited-project readers, and
least-privileged Argo/Crossplane deployment. Bootstrap must establish the denylist
before gateway readiness; charts never render, own or prune its contents.
The control-plane provisioning job may update only the capture collector's Secret (§7.4).
