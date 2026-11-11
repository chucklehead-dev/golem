# 0030: Object storage is the only cross-site channel; multicluster-runtime not adopted for now

Status: Accepted.

## Context

With one control plane on EKS serving gateway pools on AKS and on-prem
(ADR 0029), two pieces could reach into other clusters:
kubernetes-sigs/multicluster-runtime extends controller-runtime so one
manager reconciles objects in many clusters, discovered by a provider
(kubeconfig Secrets, Cluster API, the Cluster Inventory API's
ClusterProfiles, and others).

- **golem-control** could watch config CRDs in each site's own cluster and
  write their status there, instead of keeping every site's CRDs on EKS.
- **golem-operator** could run once, centrally, and install and upgrade
  gateway pools on every cluster, site by site.

Either needs the EKS cluster to hold credentials for, and network reach to,
the API servers of the AKS and on-prem clusters.

## Decision

- **No cross-cluster Kubernetes access.** Object storage stays the only
  channel between sites: remote clusters need no inbound access, and no
  cluster holds another's credentials. A compromise of the EKS control
  plane does not extend to the other clusters' API servers.
- **Config stays on EKS** in the initial phase (ADR 0029). If sites need
  their own config CRDs (owned by the site's team, reviewed in its GitOps
  repository), a **site relay** does it without cross-cluster access: the
  site's operator watches local config CRDs and uploads them to
  `config/<site>/` in the bucket; golem-control reads them from there; the
  relay copies status objects back into the CRDs' status. Pull only, the
  same pattern as snapshots.
- **One operator per cluster**, each installing its own gateway pools.
  Fleet-wide rollouts (site by site, canary first) are the GitOps tool's
  job (Argo CD ApplicationSets or Flux), which already has the clusters.
- **Revisit** if the platform adopts a cluster inventory (ClusterProfiles)
  with managed credentials and the network makes API servers reachable:
  then a central operator on multicluster-runtime could replace the
  per-cluster ones, and gotogether's `qenv/kubesim` (simulated API servers;
  examples/fleetrollout is a multicluster-runtime controller tested that
  way) gives it a testing path.

## Consequences

- Each site's failure domain is its own; the only shared dependency is
  the bucket (and, for budgets, site leases from the control plane).
- Per-site config requires the relay, a small addition to the site
  operator, rather than a central watch.

## Alternatives

- multicluster-runtime in golem-control (central watch of per-site CRDs):
  per-site config with no relay, at the cost of central credentials to
  every API server.
- A central fleet operator on multicluster-runtime: one operator to
  upgrade, same cost; and it overlaps the GitOps tool.
