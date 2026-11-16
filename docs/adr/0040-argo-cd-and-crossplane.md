# 0040: Delivery through Argo CD and Argo Rollouts; infrastructure through the platform's Crossplane

Amended by [ADR 0041](0041-security-boundaries-and-incident-controls.md)
updates the security boundary, requirements and acceptance evidence below.

Status: Accepted. Amends [0036](0036-no-mesh-identity-no-operator.md) and [0008](0008-sites-and-regions.md). **In M1 only Argo CD is used**, with the config repository not synced into the cluster (ADR 0054) and sync waves gateway pools first, then golem-control (operations.md §2, Upgrade). Argo Rollouts canaries and the Crossplane claims apply from M2; M1's cloud resources are made by hand or Terraform, recorded (operations.md §4).

## Context

The platform already runs Argo CD, Argo Rollouts and Upbound's managed
Crossplane. golem should use them rather than bring a parallel toolchain
(the design had left "Argo CD or Flux" open and proposed a Terraform
module for its cloud resources).

## Decision

- **Delivery**: Argo CD, with an ApplicationSet generating one Application
  per cluster and gateway pool from the site list; sync waves order CRDs,
  then golem-control, then gateway pools; Argo Rollouts canaries gateway
  pools with an analysis step on golem's error-origin and latency metrics.
  Config CRDs live in the same GitOps repository.
- **Infrastructure**: Crossplane, through a golem **composition** offered
  as the platform's API. A claim per installation (`GolemInstallation`)
  composes:
  - the bucket (versioning, lifecycle rules, Block Public Access, the
    bucket policy of ADR 0007, replication to the backup account's
    Object Lock bucket), the KMS keys (encryption; asymmetric signing),
    and the IAM roles (golem-control, each gateway pool, break-glass);
  - ElastiCache for Valkey;
  - the AWS Organizations SCP limiting Bedrock to golem's roles (ADR 0039),
    if the platform's Crossplane manages the organisation, else a request to
    the team that does;
  - the Entra app registrations and their federated credentials (entra.md),
    through the Azure AD provider, where the platform's Crossplane has it.
  A claim per remote site (`GolemSite`) composes its IAM Roles Anywhere
  trust anchor and profile, or its Entra-to-AWS federation, and its Valkey.
- **What stays outside Crossplane**: writing the first state document and
  registering pools (`golemctl bootstrap`, run once), Langfuse projects
  (a golem-control job, ADR 0035), and everything inside the state document.

## Consequences

- golem's cloud resources are claims reviewed in the same GitOps repository
  as its config, reconciled continuously (drift is repaired, not discovered).
- The composition is platform-owned code with its own tests (Crossplane's
  composition tests, or render checks in CI).

## Alternatives

A Terraform module (another state file and pipeline beside the platform's);
an operator (rejected in ADR 0036).
