# 0005: Declarative config is CRDs read by golem-control; incidents use expiring overrides

Status: Superseded for M1 by [0054](0054-m1-config-from-a-git-checkout.md),
which holds what still stands of this record (the kinds, one validation
path, secrets in Kubernetes Secrets). The rest applies from M2, if a second
team or a second golem-control replica needs it (roadmap.md, M2); the full
text is in git history:

- the config objects in the EKS cluster, selected per pool by labels,
  watched by golem-control, their status written (`Accepted`,
  `Applied n/m`), CEL rules enforced by the API server;
- operational overrides through the control API (disable a deployment,
  set a weight, freeze a pool, force a fallback), each with an author, a
  reason and an expiry. M1's incident levers are a pull request, the
  gateways' ejection and the emergency denylist (operations.md §2).
