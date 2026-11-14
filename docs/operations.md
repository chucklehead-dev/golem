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
| Valkey | ElastiCache, composed by Crossplane per site | |
| Bucket, KMS keys, IAM roles, SCPs, Entra registrations | Crossplane claims: `GolemInstallation` once, `GolemSite` per remote site (ADR 0040) | Includes the bypass SCP of ADR 0039 and remote sites' Roles Anywhere or federation |
| Argo CD Applications | An ApplicationSet over the site list; sync waves: CRDs, then golem-control, then gateway pools | |
| ClickHouse DDL, Grafana dashboards and alerts | Chart hooks (a Job for DDL; ConfigMaps for Grafana's sidecar) | |
| Capture collector | A golem-owned OpenTelemetry collector Deployment: Langfuse exporters only, accepts only gateway pool identities, holds the capture credentials Secret (ADR 0041 §7.4) | The enterprise collector keeps operational telemetry and no Langfuse credentials |

## 2. Day-2 operations

| Operation | How |
|---|---|
| **Upgrade** | A chart version bump in the GitOps repository; Argo CD syncs in waves: golem-control first (rolling; reads the state document's n−1 schema), then gateway pools. Stored formats change by the two-release rule (PRD §9.14). |
| **Gateway rollout and canary** | Argo Rollouts: a canary step (one replica or a traffic share) with an analysis on golem's `error.origin = gateway` rate and latency, promoted when they hold, aborted otherwise. New pods become ready only with a valid snapshot. |
| **Drain** | `preStop` turns readiness off and waits for in-flight streams up to the drain grace (default 10 minutes for agent streams), then ends them with a dialect error event; `terminationGracePeriodSeconds` follows it. |
| **Provider credential rotation** | Update the Secret; kubelet refreshes the mounted file; the gateway re-reads it. Workload identity needs none. |
| **Backend maintenance** | Planned: `Backend.spec.disabled` or `weight: 0` in GitOps. Unplanned: an expiring override (`golemctl overrides create --disable deployment/... --for 2h --reason ...`). |
| **Incident freeze** | A freeze override stops publication to a pool; gateways keep their snapshot until the lease runs out (ADR 0032). |
| **Leadership and tampering** (ADR 0049) | The bucket policy lets only golem-control's role write `state/`, `lead/`, `control/` and `snapshots/`: it is the only defence against a rollback of both the lease object and the document, or of a record with an older checkpoint and pointer. `lead/lease.json` needs a noncurrent-version expiry and is left out of the backup replication (it holds no authority). Alarm on the publisher's `ErrTampered` (a pool's checkpoint does not verify or is missing: that pool stops publishing until restored from the bucket's versions), and, from the document's Object Lock history, on a fence epoch or a publication sequence that repeats or goes down. |
| **Config and prices** | golem-control trial-compiles each edit of the config directory with the state's scopes. An edit that does not compile, or that leaves a budgeted scope reaching a model no PriceList prices where the last good config priced it, is refused (`config not applied, the last good one is published`) and the last good config keeps being renewed: fix the edit. A budgeted scope written to the state over an unpriced model (the API refuses to create one) is published without that alias and logged (`scopes published without the aliases their budgets could not be charged for`, naming scope, alias and model): price the model in a PriceList, or remove the alias from the scope. Its members cannot call that alias meanwhile; every other scope and pool is unaffected. |
| **State restore** | Fetch the wanted older version of `state/document.json` from the bucket's history (or its Object Lock replica), then `golem-control restore -bucket <url> -from old.json -actor <who>`: a dry run listing what the backup holds that would be **withheld** (rights removed since: re-grant each as its own change) and what would be **rolled back** (rights added since; keys minted since are revoked). Add `-apply` to write it, audited as `state.restore`. The fence, the publication records, the revocations and the audit outbox are always the current document's; a restore never widens a right. |
| **Snapshot objects** | Every publication writes a new content-addressed snapshot object (`snapshots/<pool>/<hash>.json`; a renewal's differs in its lease), about 1,440 a pool a day, and none is deleted by golem. The bucket's lifecycle rule expires objects under `snapshots/<pool>/` other than `current` two days after creation: longer than any authorization lease (four hours), so no object a gateway could still apply is removed, and a publisher down for days has let its lease lapse long before. The Crossplane composition sets the rule; a hand-built bucket needs it. |
| **Emergency revocation** | Site responders edit a separately bootstrapped denylist (`golem-gateway -denylist`; format in control-plane.md §5) outside Argo ownership/pruning; confirm projected-volume propagation (gateways re-read it every five seconds and log each list applied, or why an edit was not). It refuses new requests; calls already in flight finish (no cancellation in flight, ADR 0041). No seconds guarantee until measured; local Kubernetes delivery must work. Keep denials until fresh durable revocation is confirmed (ADR 0041 §1). |
| **Authorization runway** | Internal alerts follow ADR 0032. Edge pools warn after one missed 60-second renewal and page after two, before their five-minute expiry; include bounded clock skew and name the actual expiry time. |
| **Langfuse projects** | A golem-control job creates a project when a scope first needs its own and adds its credentials to the capture collector's Secret (ADRs 0035, 0041). |
| **Second site** | The bucket replicated to the standby region; its gateways read the replica; a golem-control there scaled to zero; the failover runbook promotes the replica and scales up (ADR 0008). |
| **Support** | `golemctl support-bundle`: CRD statuses, gateway status objects, recent errors, a flight-recorder trace from a chosen gateway. |

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

Ledger rows, traces and metrics leave the gateway over OTLP/HTTP. What
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
