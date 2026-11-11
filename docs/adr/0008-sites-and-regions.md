# 0008: One control plane and one S3 store for every site; no cross-cluster access

Status: Accepted (v1 baseline). From archive 0029, 0030, 0032.

## Decision

- **Sites**: EKS (us-east-2) first, then AKS and on-prem clusters. In the
  initial phase every site shares the us-east-2 bucket and the one
  golem-control on EKS, which is the only writer and publishes every pool's
  snapshots. Config CRDs for every pool live in the EKS cluster, selected
  by a `site` label. AKS and on-prem clusters run gateway pools, a Valkey,
  and (from M2) their own operator.
- **Remote gateways reach S3** through Entra-to-AWS federation (AKS) or IAM
  Roles Anywhere with a PKI or SPIRE certificate (on-prem), and need KMS
  reachable. Budgets reach them as site ceilings (ADR 0013); provider
  quotas are split statically per site (ADR 0012).
- **No cross-cluster Kubernetes access**: object storage is the only
  channel between sites; no cluster holds another's credentials; one
  operator per cluster; fleet-wide rollouts by Argo CD (ADR 0040). If a site
  later wants its own config CRDs, a site relay uploads them to
  `config/<site>/` and copies status back, pull only.
  multicluster-runtime is revisited if a cluster inventory with managed
  credentials appears.
- **Second region** (M3, us-west-2): the bucket replicated there; its
  gateways read the replica; a golem-control there scaled to zero, started
  by the failover runbook (promote the replica, scale up) within the
  30-minute RTO. One active control plane at a time.
- Azure Blob or Nutanix Objects adapters (store interface) only if a site
  needs its own store.

## Consequences

Each site's failure domain is its own; the bucket is the only shared
dependency. Remote sites need AWS reachability for changes, not for
traffic.

## Alternatives

A store and control plane per site (splits one enterprise's state); a
central fleet operator on multicluster-runtime (central credentials to
every API server).
