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
| **Emergency revocation** | Site responders edit a separately bootstrapped denylist outside Argo ownership/pruning; confirm projected-volume propagation and stream termination. No seconds guarantee until measured; local Kubernetes delivery must work. Keep denials until fresh durable revocation is confirmed (ADR 0041 §1). |
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

## Incident and delivery requirements (ADR 0041)

[ADR 0041](adr/0041-security-boundaries-and-incident-controls.md) specifies the denylist lifecycle, reserved restrictive-action
audit capacity, dedicated capture pipeline, inherited-project readers, and
least-privileged Argo/Crossplane deployment. Bootstrap must establish the denylist
before gateway readiness; charts never render, own or prune its contents.
The control-plane provisioning job may update only the capture collector's Secret (§7.4).
