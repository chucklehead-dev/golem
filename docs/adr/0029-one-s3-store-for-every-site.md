# 0029: One S3 store and one control plane for every site in the initial phase

Status: Accepted. Amends [0028](0028-state-and-snapshots-in-object-storage.md)
and [0016](0016-regional-budget-leases.md).

## Context

golem will run gateways on EKS (us-east-2) first, then on AKS and
on-prem Kubernetes. ADR 0028 gave each target its own object store (Azure
Blob, Nutanix Objects). For the initial phase, Azure and on-prem sites will
share the S3 store instead.

## Decision

- **One installation, one state document.** The enterprise's users, keys,
  scopes and budgets are one state, so every site shares the state document
  in the us-east-2 bucket. A separate document per site would split keys
  and budgets the enterprise expects to be one.
- **One control plane**, on EKS in us-east-2. It is the only writer of the
  state document and publishes snapshots for every gateway pool,
  including the AKS and on-prem pools. Config CRDs for every pool live in
  the EKS cluster, selected per pool by a `site` label. AKS and on-prem
  clusters run gateway pools only (and, from M2, an operator that installs
  them).
- **Remote gateways read S3 directly**: their pool's snapshots and their
  own status objects, under the same bucket policy as EKS gateways:
  - AKS: the pod's Entra workload identity token exchanged for AWS
    credentials (`AssumeRoleWithWebIdentity`, Entra registered as an IAM
    OIDC provider; entra.md #7);
  - on-prem: IAM Roles Anywhere with an X.509 certificate from the
    enterprise PKI (or SPIRE), or other AWS credentials where Roles
    Anywhere is not set up.
  Reached over the sites' private connectivity to AWS where it exists
  (an S3 interface endpoint), else over TLS on the internet; snapshots are
  KMS-encrypted and the role can read only its pool's prefix.
- **Budget counters across sites.** A request's reservation is on the
  request path, so where a remote site's gateways keep counters matters:
  - if the site's round trip to us-east-2 is low (≤ 30 ms, measured in
    Phase 0), its gateways use the us-east-2 Valkey over private
    connectivity: one store, no leases;
  - otherwise the site gets its own Valkey and its pools are enforced
    through **site leases**: ADR 0016's regional leases, with "region"
    read as "site", brought forward to whenever the first such site goes
    live.
  Rate limits and provider quota counters follow the same choice.
- **Telemetry and capture** from remote sites go to the central collector,
  ClickHouse and Langfuse, as from EKS.
- **Gateways keep their independence**: a remote site's gateways serve
  from their last snapshot (and a local last-good copy) if S3 or the
  control plane is unreachable, as EKS gateways do.

## Consequences

- No Azure Blob or Nutanix adapter in the initial phase; one blob adapter
  (S3) and its conformance suite. The Nutanix Objects question becomes a
  later one.
- Remote sites depend on reaching AWS for config changes (not for
  traffic).
- Moving a site to its own store later means a second installation (its
  own document and control plane) or a replicated bucket, decided then.

## Alternatives

- A store per site (0028's table): local, but splits one enterprise's
  state, or needs replication between unlike stores.
- A control plane per site against the shared document: works with CAS,
  but puts config CRDs and writers in three places for no gain at this
  size.
