# golem: operations

Status: draft. How golem is installed and run day to day, without an
operator (ADR 0036), on the platform's own tools (ADR 0040): Helm charts
delivered by Argo CD, Argo Rollouts for gateway canaries, Upbound's managed
Crossplane for cloud resources, and a few golem-control jobs.

**M1 is less than this page's design.** What M1 has: the two charts
(deploy/charts), the Argo sample (deploy/argocd), the config from a git
checkout (ADR 0054), one golem-control (ADR 0053). Rows marked *Not built
in M1* name the M1 lever to use instead. The M1 levers are: a pull request
to a `ModelRoute` or `Backend` (or a scope's `ScopePolicy`) in the config
repository, published within a minute of its merge; the gateways' own
ejection of a failing deployment (three consecutive failures eject it for
10 s, doubling to 5 min, probed back one call at a time, per gateway);
and the site's emergency denylist (*Emergency revocation*).

## 1. What is installed, and by what

| Piece | Delivered as | Notes |
|---|---|---|
| Config (`Backend`, `ModelRoute`, `ScopePolicy`, `PriceList`; `Hook` not built) | YAML in the GitOps repository (CODEOWNERS: platform team), checked out by golem-control's git-sync sidecar and compiled from the checkout (ADR 0054). **Not synced into the cluster**: no Argo Application for the config repository, and the chart's CRDs are not installed (`skipCrds`; kept generated for a later CRD watch) | Validated by `golemctl config validate` in CI (the one compiler; the CRDs carry no CEL rules), and by golem-control at load; no status objects: its log and `golem.control.config.renewing` say whether an edit applied, each snapshot's `configRevision` which commit is published |
| golem-control | Helm chart: Deployment (one replica, Recreate, ADR 0053), Service, ServiceAccount with workload identity (no Kubernetes RBAC: it reads no cluster objects), a git-sync sidecar with the config repository's deploy key | One installation, on EKS (ADR 0008). Not built in M1: the job updating the capture collector's Langfuse Secret |
| Gateway pools | Helm chart per pool: a Deployment (three replicas across zones; an Argo Rollout is *not built in M1*), each pod with its OpenTelemetry collector as a sidecar (ADR 0043), Service, PDB, ServiceAccount, a mount/reference to the separately managed emergency-denylist ConfigMap, provider Secrets mounted for the pool's backends, the `AuthorizationPolicy` limiting who may reach it (egress ServiceEntries *not built in M1*) | Internal pools; the edge pool (its ingress, WAF and source ranges) is M2 |
| Valkey | ElastiCache, composed by Crossplane per site | In-transit encryption and RBAC: the gateways run with `-valkey-tls` (the system's roots, or `-valkey-ca`) and `-valkey-user`, the password in `$VALKEY_PASSWORD` from a Secret, never a flag |
| Bucket, KMS keys, IAM roles, SCPs, Entra registrations | Crossplane claims: `GolemInstallation` once, `GolemSite` per remote site (ADR 0040) | Includes the bypass SCP of ADR 0039 and remote sites' Roles Anywhere or federation |
| Argo CD Applications | An ApplicationSet over the site list; sync waves: gateway pools (wave 0), then golem-control (wave 1), deploy/argocd/golem.yaml | Upgrades go gateways first (§2, *Upgrade*); the first install is the other way round (§4) |
| ClickHouse DDL, Grafana dashboards and alerts | Chart hooks: a Job for DDL (golem-control's `clickhouse.ddl`). Grafana dashboards and alert-rule ConfigMaps are *not built in M1*: write §2.2's alerts in the site's own alerting by hand | |
| Capture collector | A golem-owned OpenTelemetry collector Deployment: Langfuse exporters only, accepts only gateway pool identities, holds the capture credentials Secret (ADR 0041 §7.4) | The enterprise collector keeps operational telemetry and no Langfuse credentials |

## 2. Day-2 operations

| Operation | How |
|---|---|
| **Upgrade** | A chart version bump in the GitOps repository; Argo CD syncs in waves: **every gateway pool first, then golem-control**. A gateway decodes snapshots strictly (a member it does not know is refused, and the gateway keeps serving what it holds until its lease runs out), so a publisher that adds a member, such as the snapshot's `periodZone`, must find every gateway already able to read it; a new gateway reads the older publisher's snapshots. Wait for every pool's rollout to finish (all replicas ready on the new version, no `refused` in their logs) before golem-control syncs: the waves order the syncs, not the rollouts' completion. golem-control is then replaced (Recreate, one replica, ADR 0053: a short control-API outage; it reads the state document's n−1 schema). Stored formats change by the two-release rule (PRD §9.14). A rollback goes the other way: golem-control first, then the gateways. |
| **Gateway rollout and canary** | Design: Argo Rollouts, a canary step (one replica or a traffic share) with an analysis on golem's `error.origin = gateway` rate and latency, promoted when they hold, aborted otherwise. *Not built in M1*: the chart is a Deployment rolling one pod at a time (`maxSurge: 1`, `maxUnavailable: 0`); new pods become ready only with a valid snapshot, so a version that cannot apply the pool's publication stops the rollout itself. Canary by hand: sync one pool (or one site's) first and watch its `error.origin = gateway` rate and latency before the rest; roll back by reverting the chart version. |
| **Drain** | `preStop` turns readiness off and waits for in-flight streams up to the drain grace (default 10 minutes for agent streams), then ends them with a dialect error event; a call arriving once the drain's wait has begun is refused in its dialect (`gateway.not_ready`, 503 with Retry-After), so the SDK retries it on another gateway; `terminationGracePeriodSeconds` follows it. |
| **Provider credential rotation** | Update the Secret; kubelet refreshes the mounted file; the gateway re-reads it. Workload identity needs none. |
| **Backend maintenance** | Planned: a pull request removing the deployment from each `ModelRoute`'s `spec.pool` (and restoring it after). `Backend.spec.disabled` is *not built in M1*, and `weight: 0` is refused by the compiler (weights are not used in M1: it would not take the deployment out). Unplanned: the gateways eject a deployment that fails on their own (three failures in a row, probed back); for a deployment answering wrongly but not failing, the same pull request. Expiring overrides (`golemctl overrides ...`) are *not built in M1*. |
| **Incident freeze** | *Not built in M1* (a freeze override). To stop new config reaching a pool, do not merge to the config repository's branch (git-sync follows it; each merge is published within a minute). To stop callers, the emergency denylist (*Emergency revocation*). Stopping golem-control is not a freeze: gateways keep their snapshot until the lease runs out (ADR 0032), and then refuse everything. |
| **Leadership and tampering** (ADR 0049) | The bucket policy lets only golem-control's role write `state/`, `lead/`, `control/` and `snapshots/`: it is the only defence against a rollback of both the lease object and the document, or of a record with an older checkpoint and pointer. `lead/lease.json` needs a noncurrent-version expiry and is left out of the backup replication (it holds no authority). A `tampered` publication failure pages (§2.2: a pool's checkpoint does not verify or is missing, `ErrTampered`; that pool stops publishing until restored from the bucket's versions), and so does a pool's publication sequence or the leadership epoch going down (§2.2, *Sequence or epoch went back*); the document's Object Lock history then shows when and by whom. |
| **Config and prices** | golem-control trial-compiles each edit of the config directory with the state's scopes. An edit that does not compile (a ScopePolicy whose budget reaches a model no PriceList prices among its problems; `golemctl config validate` in CI catches it first) is refused (`config not applied, the last good one is published`) and the last good config keeps being renewed: fix the edit. A golem-control that starts (or takes over) while the directory does not compile renews the publication already in the bucket with the current entitlements and logs `renewing the published config: the config directory does not compile` every step: the gateways stay authorized, but no config change reaches them until the directory is fixed. An edit that deletes the ScopePolicy of a scope the state has is refused (`the ScopePolicy of live scope … removed`): restore it, or set its `models: []` to stop the scope. A scope of the state's with no ScopePolicy is published with no models (its members can call nothing), and a ScopePolicy naming no scope of the state's is not published; both are logged (`scopes and ScopePolicies do not match`, naming each) every step until they match: merge the scope's ScopePolicy, or create the scope (`golemctl scopes create <id>`), or correct the name. Every other scope and pool is unaffected. |
| **Changing a scope's policy** (its models, budget, budget mode, limits) | A pull request to the scope's `ScopePolicy` (its name is the scope's ID) in the GitOps repository, reviewed by the platform team (CODEOWNERS), checked by `golemctl config validate` in CI, synced by Argo CD; the next publication carries it (within a minute of golem-control reading it). The control API no longer sets policy: `golemctl scopes create <id>` creates the scope, which can call nothing until its ScopePolicy is merged. A new scope's budget starts in `budgetMode: observe` (the default): watch its ledger rows' `over_budget` for a period, then move it to `warn` or `enforce` by another pull request (PRD §9.1; an M1 exit criterion). A budget raised mid-month takes effect at the next publication; an emergency stop for one scope is the same pull request (`models: []`), or a key revocation for one caller. History and review are the repository's (ADR 0050). |
| **Publishing key rotation** | See [§2.1](#21-rotating-the-publishing-key). |
| **A deleted or rolled-back state document** | A `regressed` publication failure pages (§2.2: `ErrRegressed` from the publisher, a pool's publication record below its verified checkpoint or pointer, so an older document was put back and that pool is not published; or `state.ErrRegressed` from the store). `ErrMissing` stops golem-control at start (`state/document.json` is missing though the lease object or a pool's checkpoint or pointer exists): it shows as golem-control's metrics absent and its pods crash-looping with that message (§2.2, *golem-control silent*). Either way, stop every golem-control replica, put the newest version of `state/document.json` back from the bucket's versions (or its Object Lock replica), and start them. Only if no newer version can be recovered: `golem-control bootstrap -bucket <url> -keys <the gateways' -keys directory> -pools <every pool> -actor <who>`: with no document it creates an empty one (every caller refused until entitlements are created again); with `-adopt` it accepts the older document that is there: before running it, redo through the API the revocations made since that version (the audit archive, `audit/<seq>.json`, lists them), since publication resumes from it. Each pool's record is set to what the bucket proves was published, so publication goes on above what gateways trust. Audited as `state.bootstrap` or `state.adopt`. |
| **State history** | The state bucket keeps every version of `state/document.json` for 30 days (a noncurrent-version expiry on `state/`); every renewal writes one, about 1,440 a day, so do not keep them for ever. A daily job copies the current document to the backup account's Object Lock bucket, kept a year. Recovery point: any write in the last 30 days; older, the day's copy. `snapshots/` and `control/` follow their own rules (*Snapshot objects*; checkpoints and pointers are small and overwritten in place, their versions kept 30 days too). The bucket's lifecycle rules and the backup job are part of the Crossplane composition (not built yet: until then, configure them on the bucket by hand). |
| **State restore** | Fetch the wanted older version of `state/document.json` from the bucket's history (or its Object Lock replica), then `golem-control restore -bucket <url> -from old.json -actor <who>`: a dry run listing what the backup holds that would be **withheld** (rights removed since: re-grant each as its own change) and what would be **rolled back** (rights added since; keys minted since are revoked). Add `-apply` to write it, audited as `state.restore`. The fence, the publication records, the revocations and the audit outbox are always the current document's; a restore never widens a right. |
| **Snapshot objects** | Every publication writes a new content-addressed snapshot object (`snapshots/<pool>/<hash>.json`; a renewal's differs in its lease), about 1,440 a pool a day, and none is deleted by golem. The bucket's lifecycle rule expires objects under `snapshots/<pool>/` other than `current` two days after creation: longer than any authorization lease (four hours), so no object a gateway could still apply is removed, and a publisher down for days has let its lease lapse long before. The Crossplane composition sets the rule; a hand-built bucket needs it. |
| **Emergency revocation** | Site responders edit a separately bootstrapped denylist (`golem-gateway -denylist`; format in control-plane.md §5) outside Argo ownership/pruning; confirm projected-volume propagation (gateways re-read it every five seconds and log each list applied, or why an edit was not). It refuses new requests, and each list applied terminates the matching calls in flight within seconds (a stream ends with its dialect's error event `auth.denied`; the ledger row's outcome is `terminated`; logged as `ended calls in flight whose authorization was withdrawn`). A snapshot that revokes a key, or removes its principal, does the same at each gateway as it is applied. Lease expiry stops new requests only. No seconds guarantee from the edit to the file until measured (projection is the site's); local Kubernetes delivery must work. Keep denials until fresh durable revocation is confirmed (ADR 0041 §1). |
| **Authorization runway** | The alerts of §2.2 over `golem.snapshot.lease_remaining`, `golem.snapshot.age` and `golem.control.publish.time`, with ADR 0032's thresholds. Edge pools warn after one missed 60-second renewal and page after two, before their five-minute expiry; include bounded clock skew and name the actual expiry time (`golem.control.publish.valid_until`). |
| **Langfuse projects** | Design: a golem-control job creates a project when a scope first needs its own and adds its credentials to the capture collector's Secret (ADRs 0035, 0041). *Not built in M1* (capture is M2). |
| **Second site** | Design: the bucket replicated to the standby region; its gateways read the replica; a golem-control there scaled to zero; the failover runbook promotes the replica and scales up (ADR 0008). *Not built in M1* (one site). |
| **Support** | `golemctl support-bundle` and `golemctl gateways` are *not built in M1*. By hand: golem-control's log (`published`, with each pool's sequence and `configRevision`; the config's errors), each gateway's log and its status object in the bucket (`status/<pool>/<gateway>`: what it applied; diagnostic only), §2.2's metrics, and `golemctl config validate` on the commit published. |

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
`golem.site` and `golem.pool`. Every instrument below but the gateway's
request-path ones (`golem.ttft`, `golem.calls`, `golem.attempts`,
`golem.refusals`, recorded as each call or attempt ends) is observable:
read from the component's state when the SDK collects (every minute),
never on a request. No instrument is attributed by principal, scope or
request: their attributes are the snapshot's routes and deployments and
the contract's fixed values. Times are Unix seconds, so "seconds since" is the rule's
`now() - metric`. A replica that does not lead keeps reporting its own
last publication, so rules over golem-control take the newest across
replicas (`max by (pool)`).

| Instrument | Kind, unit | Attributes | Meaning |
|---|---|---|---|
| `golem.control.publish.time` | gauge, s | `pool` | When this replica last published the pool successfully |
| `golem.control.publish.seq` | gauge, {count} | `pool` | That publication's sequence |
| `golem.control.publish.valid_until` | gauge, s | `pool` | The lease it issued (the snapshot's `ValidUntil`): minus `publish.time`, the lease at publication; minus now, the runway left if nothing is published again |
| `golem.control.publish.failures` | counter, {count} | `pool`, `reason` | Publication attempts that failed or were degraded; `reason` is `tampered`, `regressed`, `fenced`, `conflict`, `scope_policy_unmatched`, `config_renewing`, `config_stale` or `other` (every pool and reason is reported, from zero). A failing pool is retried with backoff (two seconds, doubling to the one-minute renewal interval) and at once on a change, so it counts at least once a minute while it fails, and the healthy pools publish at their own cadence |
| `golem.control.leader`, `golem.control.leader.epoch` | gauge, 1 / {count} | | 1 while this replica leads; the epoch it leads at (0 when not) |
| `golem.control.scope_policy.unmatched` | gauge, {count} | | Scopes of the state's with no ScopePolicy (published able to call nothing), and ScopePolicies of no scope, at the last publication |
| `golem.control.config.renewing` | gauge, 1 | | 1 while the config directory is not what is published (it does not compile, or an edit was refused) |
| `golem.control.config.age` | gauge, s | | How long ago git-sync last fetched the config checkout (`-config-fetched`, its `FETCH_HEAD`; ADR 0054); absent while unknown. Past `-max-config-age` (30 minutes by default) golem-control renews no lease |
| `golem.control.spend.age`, `golem.control.spend.stale` | gauge, s / 1 | `period` | Known spend's boundary age (`spend.Job.Age`) and whether it is stale (`Job.Stale`); with `-clickhouse` only |
| `golem.control.spend.ingestion_lag`, `golem.control.spend.ingestion_behind` | gauge, s / 1 | `site` | How far each site's ledger rows are behind now, and whether past the allowance (or gone) (`spend.Job.Lags`); with `-clickhouse` only |
| `golem.control.entra_sync.time` | gauge, s | | When the group sync last succeeded on this replica; with `-entra-sync-client-id` only |
| `golem.control.entra_sync.guard_trips` | counter, {count} | | Syncs refused by the removal guard (`ErrTooManyRemovals`): over `-entra-sync-max-removals` memberships of still-active users would end; the offboarding of disabled or deleted users in the same step is written regardless |
| `golem.control.audit.outbox` | gauge, {count} | | Audit events in the document's outbox |
| `golem.control.audit.outbox_max` | gauge, {count} | | The outbox's bound (`-audit-outbox-max`, default 10,000): past 80% of it changes that grant access are refused, at it every audited change |
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
| `golem.deployment.in_flight` | gauge, {attempt} | `golem.deployment` | Each of the applied snapshot's deployments' attempts holding a place under its ceiling (`route.Health`) |
| `golem.deployment.ejected` | gauge, 1 | `golem.deployment` | 1 while that deployment is ejected (consecutive failures, a failed probe or check), until a commit or a passing check readmits it; per gateway (model/attempts.md §9) |
| `golem.ttft` | histogram, s | `gen_ai.request.model` (the alias), `gen_ai.response.model` (the model), `golem.deployment` | Time to first token: from the request's arrival to its commit point, a stream's first event relayed or a whole response's headers, retries and failovers included (exit 3: p95 under 2 s; 2 s is a bucket boundary). A call answered with an error, or never committed, records none |
| `golem.calls` | counter, {call} | `gen_ai.request.model`, `golem.deployment` (its last attempt's), `golem.outcome`, `golem.error.origin` | Admitted calls, as they end: `outcome` is the ledger's (`completed`, `cancelled`, `terminated`, `failed`, `uncertain`) and `error.origin` contracts.md §2's (`client`, `policy`, `provider`, `gateway`, or empty). A call is counted once, however many attempts it made |
| `golem.attempts` | counter, {attempt} | `golem.deployment`, `golem.attempt.result` | Attempts, by how each ended (model/attempts.md): `committed`, `refused` (429, 503, 529, or an error as a stream's first event), `unreached` (DNS, dial, TLS), `uncertain` (sent, then no answer). More attempts than calls is retries and failovers |
| `golem.refusals` | counter, {count} | `golem.error.code`, `golem.error.origin` | Requests refused before any attempt (authentication, policy, limits, budgets): not in `golem.calls`, so an error-origin rate over every request adds the two |

The alerts, in words over these metrics (Grafana's alert rules, delivered
with the chart, §1: *not built in M1*, so the site writes them in its own
alerting). Every alert on the authorization names the time
gateways start refusing: the pool's lowest `golem.snapshot.lease_remaining`
added to now, or `max by (pool) golem.control.publish.valid_until`.

| Alert | Expression | Severity | Runbook |
|---|---|---|---|
| **Authorization runway short** | any gateway's `golem.snapshot.lease_remaining` < 1 h (edge pools: < 3 min) | incident | Nothing has been published for hours. Work the publication alerts below from the top (a stale config checkout, *Config checkout old*, stops renewals on purpose); if golem-control cannot be fixed before the expiry, plan for the gateways refusing every call (the incident freeze and the standby site's failover are *not built in M1*). |
| **Authorization lapsed** | any gateway's `golem.snapshot.lease_remaining` ≤ 0 | page | That gateway refuses every request and fails `/readyz`. As above; a gateway serves again with the next publication it reads. |
| **Pool not published** | for a pool, `now() - max(golem.control.publish.time)` over replicas > 3 min (three renewal intervals: ADR 0032's two minutes and a collection); pages > 30 min. Edge pools: warn > 2 min, page > 3 min | warning, then page | Look at the leader's log (`golem-control`'s `err`, once until it changes) and the failure counter's `reason`; the rows below name each. No leader: see *No leader*. |
| **Snapshot old at a gateway** | `golem.snapshot.age` > 3 min (PRD §9.13), or `golem.snapshot.pointer_overdue` = 1, for 5 min; while the pool *is* published | warning | The gateway does not see what is published: its bucket access (credentials, network, the replica it reads) or a publication it refuses (its log names it: below the floor, a signature its `-keys` do not trust after a key rotation, §2.1; a pointer "in this gateway's future" or a lease "longer than the longest lease": the publisher's or the gateway's clock is off, fix NTP, or raise `-max-lease` to a pool's approved longer lease). |
| **Serving the last-good copy** | `golem.snapshot.last_good` = 1 for 5 min | warning | The gateway started while the bucket was unreachable and has not read it since: as *Snapshot old*. It serves until the copy's lease ends. |
| **Checkpoint tampered** | `increase(golem.control.publish.failures{reason="tampered"})` > 0 | page | That pool is not published. If the publishing key was just rotated, add the old key to `-trusted-keys` (§2.1); otherwise restore `control/<pool>/checkpoint` from the bucket's versions and find who wrote it (CloudTrail). |
| **State document went back** | `increase(golem.control.publish.failures{reason="regressed"})` > 0 | page | *A deleted or rolled-back state document* (§2). |
| **Sequence or epoch went back** | for a pool, `max(golem.control.publish.seq)` below its maximum over the last day; or `max(golem.control.leader.epoch)` below its maximum over the last day | page | A rollback that bypassed the publisher's guard: the bucket policy's writers (*Leadership and tampering*, §2), the document's Object Lock history. |
| **Publication failing** | `increase(golem.control.publish.failures{reason=~"other\|conflict"}[2m])` > 0 for 5 min (a window over the backoff's one-minute retries) | warning (page via *Pool not published*) | The leader's log names it: KMS refusing to sign (the key policy, the role), the bucket refusing a write (the bucket policy), a build error. `conflict` alone, briefly, is a concurrent writer. |
| **Fenced repeatedly** | `increase(golem.control.publish.failures{reason="fenced"}[2m])` > 0 for 5 min | warning | Two golem-control processes ran (ADR 0053: one replica, which claims the fence as it starts): a predecessor still running is refused at its next fenced write and exits (`controller.ErrDeposed`). If one keeps reporting it, delete that pod (a paused or cut-off node's). |
| **No leader** | `max(golem.control.leader)` = 0 for 2 min | page | The running golem-control was deposed (a successor claimed the fence; it should have exited), or it cannot claim the fence at start (its log: the state document, the bucket's policy). Make sure exactly one golem-control pod runs. |
| **Config checkout old** | `golem.control.config.age` > 5 min (ten sync periods) | warning; page at 20 min (before `-max-config-age`'s 30) | git-sync is not fetching: its container's log (the deploy key, `known_hosts`, the repository host, the network). At `-max-config-age` golem-control stops renewing leases (log: `the config checkout is stale`; `publish.failures{reason="config_stale"}` on a change it could not publish): the gateways serve until their last lease ends (four hours, *Authorization runway*), then refuse everything, rather than serve a config that may miss a restriction merged since. Changes to the state (revocations) are still published within the lease already granted. A fetch resumes the renewals at once. `absent(golem.control.config.age)` with golem-control exporting means `-config-fetched` is unreadable, which counts as stale. |
| **Config not applied** | `golem.control.config.renewing` = 1 for 10 min | warning | *Config and prices* (§2): fix the edit the log names; gateways stay authorized, but no config change reaches them. |
| **Scope policy unmatched** | `golem.control.scope_policy.unmatched` > 0 for 10 min | warning | *Config and prices* (§2): the leader's log names each scope with no ScopePolicy (merge one: its members can call nothing until then) and each ScopePolicy of no scope (create the scope, or correct the name). |
| **Known spend stale** | `golem.control.spend.stale` = 1 (on the leader) | warning; page after 1 h | ClickHouse is unreachable or the query fails (the leader's `known spend` error): gateways count their own site's spend and the stale result, and miss other sites' since its boundary (ADR 0048). |
| **Ledger ingestion behind** | `golem.control.spend.ingestion_behind` = 1 for a site for 10 min, while that site's gateways serve traffic | warning | The site's rows reach ClickHouse later than `-spend-allowance`: known spend's boundary misses them (other sites' budgets undercount that site's spend). Check the site's node-local collector and its queue; a quiet site also reads as behind (the ledger cannot tell quiet from late), hence the traffic condition. |
| **Group sync not succeeding** | on the leader, `now() - golem.control.entra_sync.time` > 1 h | warning | The sync's error in the leader's log: Graph or the federated token. Memberships stop following Entra (removals included) until it succeeds. |
| **Group sync guard tripped** | `increase(golem.control.entra_sync.guard_trips)` > 0 | warning | A sync would end more memberships of still-active users than `-entra-sync-max-removals` (a group re-bound or emptied, a directory answering empty), and wrote none of them; users Entra reports disabled or deleted were offboarded anyway (the log's `entra sync` line, `offboarded`). If the removals are intended, raise the flag for one run; otherwise investigate the directory change. |
| **Audit outbox not draining** | on the leader, `now() - golem.control.audit.shipped.time` > 15 min, or `golem.control.audit.outbox / golem.control.audit.outbox_max` > 0.5; pages at ≥ 0.8, where grants are refused | warning; page at 80% | The leader's `audit:` error: the archive refusing a write (its role, Object Lock, the network to the separate account), or `ErrMismatch` (an archived event differs: a person looks before anything is trimmed). Without `-audit-archive` nothing ships and the outbox only fills: set it. Past 80% of the bound the control API answers every change that grants or widens access (minting a key, adding a member, creating a scope, binding a role or group, the group sync's creations and additions) `resource_exhausted`, "the audit outbox is full for changes that grant access"; revocations, removals, the sync's offboarding and a restore still land. At 100% every audited change is refused, revocations too: revoke through the emergency denylist (§2, *Emergency revocation*) until the outbox drains. Once the archive takes writes again each step ships and trims 100 events, and grants land as soon as the outbox is back under 80%. Raising `-audit-outbox-max` buys room, not a fix. |
| **Limiter down** | `golem.limit.down` = 1 for 5 min | warning; page after 30 min | The site's Valkey is unreachable (a gateway that started meanwhile starts down, ready, retrying the connection): admission is on local counts and every admission is a waiver (`golem.limit.waivers` rising), held increments (`golem.limit.held`) wait for its return. Fix Valkey (ElastiCache, the network, the gateways' credentials). |
| **Held increments dropped** | `increase(golem.limit.dropped)` > 0 | warning | An outage outlasted the hold's cap, or a gateway left with Valkey down: the counter lacks that spend until known spend's boundary passes it (the ledger has the rows). |
| **Denylist differs across a site** | `golem.denylist.entries` not equal across a site's gateways for 5 min | warning | The projected volume has not reached every gateway, or an edit was refused (the gateway logs why and keeps the previous list). |
| **Time to first token slow** | for a route (`gen_ai.request.model`), p95 of `golem.ttft` over 5 min > 2 s, for 10 min | warning | Exit 3's bound. By `golem.deployment`: one deployment slow is its provider (its region's quota, a cold local box): `golem.attempts{golem.attempt.result="refused"}` rising there means failovers add to the wait. All slow at once with few attempts per call is the gateway's (its CPU, the admission's Valkey round trip). |
| **Provider failures high** | `sum(rate(golem.calls{golem.error.origin="provider"}[5m])) / sum(rate(golem.calls[5m]))` > 5%, for 5 min | page | Failover is not hiding a provider outage: by `golem.deployment`, the deployments ending calls in failure, and by `golem.outcome` whether they are refusals relayed (every deployment of the pool refusing: quota, an SCP), cut streams (`failed`) or `uncertain` (timeouts; charged their worst case until reconciliation). Check `golem.deployment.ejected` and the route's retry budget. |
| **Deployment ejected** | `golem.deployment.ejected` = 1 for a deployment for 10 min | warning | Its calls fail over to the pool's next deployment (or fail, in a pool of one). The gateways' logs and the ledger's rows for that deployment name the failures; a provider-side outage readmits it on its own at the first probe that commits. An ejection lasting past the provider's recovery is a broken probe path (credentials, network, a stale endpoint in the config). |
| **golem-control silent** | `absent(golem.control.leader)` for 5 min | page | No replica is exporting: pods down or crash-looping (`ErrMissing` at start, a broken flag), or the collector path. |
| **Gateway silent** | `absent(golem.snapshot.age)` for a pool for 5 min | page | No gateway of the pool exports: the pool or the collector path is down (PRD §9.13). |

## 3. Sites

Each cluster's charts are deployed by Argo CD's ApplicationSet, which
already knows every cluster; nothing in one cluster reaches another's API server
(ADR 0008). Remote sites run gateway pools and a Valkey; their config is
the same GitOps repository golem-control publishes from (ADR 0054; per-site
selection by label is not built in M1: every pool gets the same snapshot).

## 4. First install

What the charts assume exists before Argo CD syncs them, in the order to
make it. The `GolemInstallation` Crossplane claim (ADR 0040) is to make
steps 1 to 3; until it is built, make them by hand. Names below are the
charts' defaults (deploy/charts/*/values.yaml); the namespace is `golem`.

1. **The state bucket and its policy.** S3, versioned, in golem's
   account. Its policy lets only golem-control's role write `state/`,
   `control/` and `snapshots/` (*Leadership and tampering*, §2), lets each
   gateway pool's role read `snapshots/<pool>/` and `control/<pool>/` and
   write `status/<pool>/`, and nothing else write at all. Lifecycle rules:
   noncurrent versions of `state/` and `control/` kept 30 days; objects
   under `snapshots/<pool>/` other than `current` expired two days after
   creation (*State history*, *Snapshot objects*). The audit archive, if
   used (`auditArchive`): an Object Lock bucket in a separate account,
   writable by golem-control's role.
2. **The publishing key.** A KMS key, `ECC_NIST_P256`, `SIGN_VERIFY`; its
   key policy grants golem-control's role `kms:Sign` and
   `kms:GetPublicKey` and no one else `kms:Sign`. golem-control's
   `signingKey` is `kms:<its ARN>`.
3. **The IAM roles, by IRSA or Pod Identity.** `golem-control`: the
   bucket as above, the key, the audit archive. One role per gateway
   pool: the bucket as above, and `bedrock:InvokeModel*` if a backend is
   Bedrock (*Bedrock*, below). Each role trusts its ServiceAccount
   (`golem/golem-control`, `golem/golem-gateway-<pool>`); the charts'
   `serviceAccount.annotations` name it.
4. **The site's Valkey and ClickHouse.** ElastiCache for Valkey with
   in-transit encryption and an ACL user `golem-gateway` (the chart's
   `valkey.user`). ClickHouse with three users: `golem_admin` (DDL, for
   golem-control's `clickhouse.ddl` Job or run by hand from
   deploy/clickhouse/spend.sql), `golem_ingest` (inserts, for the
   gateways' collectors), `golem_control` (read-only on `golem.spend`).
5. **The Secrets** (`kubectl -n golem create secret generic ...`):
   - `golem-config-deploy-key`: keys `ssh` (a read-only deploy key of the
     config repository) and `known_hosts` (the repository host's);
   - `golem-valkey`: key `password`, the ACL user's;
   - `golem-clickhouse`: key `password`, `golem_control`'s (with
     `clickhouse.url`); `golem-clickhouse-admin`, `golem_admin`'s (with
     the DDL Job); `golem-clickhouse-ingest`, `golem_ingest`'s (the
     gateways' collectors);
   - each provider Secret a pool's backends name (`providerSecrets`; a
     Backend's `auth.secretRef` is `<Secret>/<key>`), unless the backend
     uses workload identity.
6. **The ConfigMaps**, outside Argo CD (it must not own or prune them):
   - `golem-publishing-keys`: the publishing key's public half, which
     every gateway's `-keys` trusts:
     `golem-control pubkey kms:<ARN> > publishing.pub`, then
     `kubectl -n golem create configmap golem-publishing-keys --from-file=publishing.pub`;
   - `golem-emergency-denylist`, key `denylist`: the site's emergency
     denylist, empty to start (a `#` comment line is enough; format in
     control-plane.md §5); the gateways do not start without it (ADR 0041
     §1);
   - the gateways' collector config, unless the chart carries its copy
     (`files/otel-collector.yaml`): `kubectl -n golem create configmap
     golem-gateway-<pool>-collector --from-file=collector.yaml=deploy/otel/collector.yaml`;
   - a private CA bundle for a remote collector, only with
     `collector.enabled: false` (`caBundle`).
7. **The config repository.** Backends, ModelRoutes, a PriceList for
   every model a budget reaches, and a ScopePolicy per scope to be
   created; `golemctl config validate` passes on its branch (and runs in
   its CI); branch protection forbids force-pushes (ADR 0054). It is not an
   Argo Application.
8. **The Entra registrations**: the gateway's (`api://golem`, the
   gateway chart's `entra`) and the control API's (`api://golem-control`,
   `golem.manage`, ADR 0046; its tenant is golem-control's
   `api.entraTenant`); the first admin is whoever holds the `golem.admin`
   app role.
9. **Sync golem-control first** (on the first install only; upgrades go
   the other way, §2). On a bucket it has never run on it creates the state
   document, claims the fence and publishes every pool in `pools`: its log
   shows `published` for each, with the config's `configRevision`.
10. **Then the gateway pools.** The observe-phase site sets
    `acceptUnreleased: true` with its recorded decision (golem#7); the
    shared values agree with golem-control's (`maxLease` ≥ `lease`, the
    same `periodZone`). Each gateway raises its floor from the pool's
    checkpoint, applies the publication and becomes ready.
11. **Create the scopes** (`golemctl scopes create <id>`, matching their
    ScopePolicies), their members or bound Entra groups, and keys.

## Bedrock: the gateway's AWS role

A bedrock backend's calls are signed with the gateway's credentials, the
AWS SDK's default chain: in the cluster, the gateway's service account
bound to an IAM role (IRSA or EKS Pod Identity); elsewhere `AWS_*`
environment credentials. The role needs:

- `bedrock:InvokeModel` and `bedrock:InvokeModelWithResponseStream` on the
  foundation models and inference profiles the routes name (an inference
  profile's ARN, and the foundation models in each region it routes to);
- nothing else of Bedrock, and no other principal in the dedicated account
  may invoke it (ADR 0038, 0039: the account's SCP denies invocation to
  every role but the gateway's).

Attribution on the AWS side, set up with the dedicated account:

- **Cost by caller:** a CUR 2.0 export with "Include caller identity (IAM
  principal) allocation data", and the gateway's role tagged and the tags
  activated as cost allocation tags. CUR then shows golem's Bedrock spend
  as the gateway role's, apart from any other caller's (bypass, ADR 0039).
  Per scope, cost is golem's ledger's; CUR does not split one role.
- **Per request:** model invocation logging on in every region the routes
  use, delivering metadata only: text, image, embedding and video data
  delivery off, so no prompt or completion is stored at the provider (ADR
  0027). Each record carries golem's request metadata (gateway.md §4a),
  which joins it to its ledger row: reconciliation per request, not only
  per day. (Verify on the account that a log with every data delivery off
  holds the metadata alone.)

Without credentials a bedrock call fails with `upstream.unavailable` and
the gateway logs, at start, that it has none; the snapshot still applies.
A deployment in another region needs model access granted there too.

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

## The ledger's collector

The ledger reaches ClickHouse through an OpenTelemetry Collector in the
gateway's pod (ADR 0043), configured by `deploy/otel/collector.yaml`
(contracts.md §3.1 has the record's shape):

- **Image:** `otel/opentelemetry-collector-contrib:0.115.1`, the gateway
  chart's (`collector.image`), the version `test/ledger` runs the
  configuration on (and 0.163.2, which it also passes on). A new version
  is taken only with that test passing: the exporter's insert must match
  `golem.spend_otlp`'s columns.
- **Configuration:** the chart mounts its copy
  (`deploy/charts/golem-gateway/files/otel-collector.yaml`, kept identical
  to `deploy/otel/collector.yaml` by `internal/crdgen`'s test) at
  `/etc/otelcol/collector.yaml`. Alone, it writes the ledger and drops the
  gateway's spans, metrics and other logs; with `deploy/otel/forward.yaml`
  loaded after it (`GOLEM_OTEL_UPSTREAM_ENDPOINT`), it forwards those to
  the enterprise collector.
- **Ports:** the OTLP/HTTP receiver on `127.0.0.1:4318`, the gateway's
  telemetry endpoint (plaintext, loopback; `GOLEM_OTLP_LISTEN` to move it:
  any other address needs the receiver's `tls`, as "Telemetry transport"
  requires); the health check on `127.0.0.1:13133` (`GET /`,
  `GOLEM_OTEL_HEALTH_LISTEN`); the collector's own metrics on
  `localhost:8888`, its default.
- **ClickHouse:** `CLICKHOUSE_ENDPOINT` (clickhouse-go's DSN, for example
  `tcp://clickhouse.golem.svc:9440?secure=true&async_insert=1&wait_for_async_insert=1`:
  each gateway export is one insert, and async inserts let ClickHouse
  batch the pods' small ones, acknowledged once written),
  `CLICKHOUSE_USERNAME`, `CLICKHOUSE_PASSWORD` from a secret, and
  `CLICKHOUSE_DATABASE` (default `golem`). The user needs `INSERT` on
  `golem.spend_otlp` and on the tables its views write (`golem.spend`,
  `golem.spend_checked`, `golem.spend_rejected`, `golem.spend_by_event`,
  `golem.ingest_by_site`), nothing else: the exporter's schema creation is
  off, and spend.sql (the DDL Job) creates every table first.
- **The queue:** `OTELCOL_QUEUE_DIR` (the chart's
  `/var/lib/otelcol/queue`), writable by uid 10001. The chart's
  `emptyDir` keeps queued rows through the collector's restarts and a
  ClickHouse outage while the pod lives, not the pod's deletion (a
  `hostPath` would). Writes are synced. Exports are retried without end,
  so an outage fills the queue (50,000 of the gateway's exports, hours of
  rows) rather than dropping rows; past that the receiver refuses, and
  the gateway's own queue counts what it loses (`golem.ledger.dropped`,
  `golem.ledger.failed`).
- **Alerts:** the collector's `otelcol_exporter_queue_size` near
  `otelcol_exporter_queue_capacity` (ClickHouse is not taking rows), and
  any row in `golem.spend_rejected` (a record that would not convert:
  `reason` names its column; fix the sender, then insert the repaired
  rows into `golem.spend`).

## Incident and delivery requirements (ADR 0041)

[ADR 0041](adr/0041-security-boundaries-and-incident-controls.md) specifies the denylist lifecycle, reserved restrictive-action
audit capacity, dedicated capture pipeline, inherited-project readers, and
least-privileged Argo/Crossplane deployment. Bootstrap must establish the denylist
before gateway readiness; charts never render, own or prune its contents.
The control-plane provisioning job may update only the capture collector's Secret (§7.4).
