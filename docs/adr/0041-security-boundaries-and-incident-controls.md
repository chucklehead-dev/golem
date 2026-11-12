# 0041: Security boundaries and incident controls after the second review

Status: Accepted, with the adaptations in §7. Amends ADRs 0029, 0031,
0032, 0035, 0037 and 0040 in the areas below. These are requirements and assumptions to validate,
not implemented controls or proofs. Four-hour internal leases, soft budgets,
collector-held capture credentials and platform delivery remain the design.

## 1. Emergency revocation survives delivery and audit outages (M1)

Entra disablement and secret-scanning revocation require synchronization and
fresh publication to reach a gateway; local JWT validation does not check
Entra account status. Credential expiry works only at its encoded expiry.
The emergency denylist is the independent per-site revocation path. During
a partition, an internal gateway may accept a newly revoked right until its
four-hour authorization lease ends unless a local denial reaches it.

A site bootstrap procedure creates the emergency-denylist separately from
Argo-managed resources. Charts reference/mount it but never render its
contents, own it or prune it. Site security responders alone may modify it;
ordinary GitOps and gateway identities cannot clear it. Reinstallation and
namespace cleanup must preserve/export it or keep the gateway unready.
Do not rely solely on ignoreDifferences: sync, prune, replacement and field
ownership behavior must all be tested. Mount the whole projected volume,
not a subPath. Measure file propagation and matching-stream termination;
Kubernetes projection is asynchronous, so do not promise a seconds bound
until the site has demonstrated it. A local API-server/kubelet failure can
also prevent delivery; lease expiry remains the fallback bound.

Keep denials until durable authoritative revocation and an applied fresh
publication are confirmed at that site. Clearing a denial is a separate
privilege-widening operation with actor, reason and audit evidence. An
outage drill must include Argo self-heal, sync, chart upgrade, pod restart,
prune attempts and concurrent edits without losing a denial.

Audit backpressure is directional. Reject privilege grants, new credentials,
capture enablement and policy widening before exhausting ordinary outbox
capacity. Reserve separate bounded capacity for restrictive actions and their
audit records. Classification uses the effective authorization diff, not
method names; mixed restrictive/widening changes are not emergency writes.
At emergency-capacity exhaustion, do not silently discard audit records or
claim unlimited control-plane writes: use the independently authorized site
denylist with the site's durable incident audit channel, and page responders.
Do not couple local denial application to the central archive's availability.
Test archive failure and both capacity thresholds while revoking access.

## 2. Capture uses pool and project trust boundaries (M2)

A compromised shared gateway can forge scope attributes for any scope in
its pool. Collector authentication proves the pool identity, not the truth
of the gateway's scope label. Pool-to-project allowlists prevent destinations
outside the pool; they cannot prevent relabeling between its allowed projects.
This is an accepted compromise boundary only with security approval. Scopes
requiring isolation from another scope's compromised gateway need separate
gateway pools/workload identities and separate capture destinations; do not
claim cryptographic per-scope provenance from labels.

Inherited Langfuse projects are shared human-read boundaries. Enabling
inheritance requires approval of the actual project readers, including
cross-team members; membership in a child scope does not automatically grant
read access to the parent's project. A scope needing narrower readers gets
a separate project. Provisioning and removal must reconcile Langfuse access
explicitly and audit it.

Captured content uses a dedicated authenticated receiver and isolated
collector pipeline with explicit exporter allowlists, queue/storage controls
and no operational-exporter fan-out. Ordinary OTLP senders cannot reach this
receiver or set routing authority. Drop/reject unknown project destinations,
strip untrusted exporter-selection attributes, and derive allowed projects
from authenticated pool identity and managed configuration. Restrict the
provisioning job to named capture Secrets, not every enterprise-collector
Secret. A collector compromise exposes all project credentials it holds;
separate instances/identities are required where that exposure is unacceptable.

Required evidence: forged destination and scope labels, ordinary-sender
injection, collector configuration reload, queue retries and exporter failures
cannot export content outside the approved pool destinations or into the
operational telemetry sinks. Test shared-project read membership separately.

## 3. Request compatibility has an explicit risk mode (M1)

ScopePolicy adds a request-feature mode:
- restricted (default): approved API versions, beta headers, request fields
  and feature combinations only; reject unknown features before forwarding.
- compatible: forward unknown fields/headers with explicit scope-owner and
  security approval, while enforcing all known policy controls.

Both modes force known storage controls and reject M1 stateful object paths.
Provider-side contractual retention/training controls remain necessary;
neither parsing nor version pinning proves unchanged provider behavior.
Scopes requiring enforceable storage, egress or residency rules use restricted
mode. Compatible mode accepts new data-moving or billing semantics until
detected and remediated. Nightly review is detection, not a one-day maximum
exposure bound. Existing field values and provider defaults can change too.

Record compatible-mode owner, allowed data classes and response procedure.
Unknown JSON field names and headers are attacker-controlled data: log only
bounded safe names from a fixed character set or a bounded aggregate of
keyed hashes; never values or raw rejected names. Do not create unbounded
metric labels or one log entry for every distinct name. Test field names
containing secrets, control characters and excessive lengths.

## 4. Delivery identities are part of the root of trust (M0/M1)

Separate approval ownership for model config, installation claims,
compositions/provider configuration and production trust policies. Use
protected branches with enforced reviews; CODEOWNERS without branch rules
is insufficient. Pin chart, image and Crossplane package/composition revisions
to reviewed immutable artifacts and verify provenance through the platform's
existing policy.

Argo Projects restrict source repositories, destination clusters/namespaces
and permitted resource kinds. Config delivery cannot create provider configs,
RBAC grants, arbitrary Secrets or new Application sources. Crossplane claims
expose bounded business inputs, not arbitrary IAM/KMS policies, issuer trusts,
provider references, composition selection or resource templates. Admission
and RBAC enforce these constraints against direct API writes too.

Separate least-privileged provider identities for organization/SCP management,
installation infrastructure and Entra operations. Gateways cannot use them.
Restrict who may alter the compositions, package functions and credentials
that execute with those identities. Root-of-trust changes require distinct
platform/security review and an audited break-glass path outside golem.

Validate render/claim tests plus negative authorization/admission tests:
a config author or claim user cannot change a signing key, assume a new
provider identity, weaken a bucket policy, widen federation or disable the
Bedrock bypass control. A compromised authorized administrator remains a
trusted-root compromise; signatures do not remove this assumption.

## 5. Publication evidence includes config and restrictive rights (M1)

A snapshot combines two authorities: the CAS state document and accepted
Kubernetes config. The source manifest must bind state_version plus the
complete accepted config set (UID, generation and content digest, including
deletions) and a publisher-managed monotonic config revision. Kubernetes
resourceVersion is opaque and is not numerically ordered across objects.

Before renewing authorization, establish freshness of both inputs using
authoritative reads or a verified watch synchronization barrier. A running
watch connection alone does not prove freshness. Unchanged policy can be
renewed only after its freshness is re-established. Source-read expiry is
anchored to the earliest validated input read, never merely the final state
read or signing time. A newer state version cannot license stale CRDs.
If freshness cannot be established, do not extend the lease.

The publication/fencing spike must define ordering, cache resync, deletions,
leader changes and the atomic publication/checkpoint boundary. Existing
publication.qnt proves properties of key revocations in a single source;
it does not yet prove this composite protocol. The model allows an old
leader to publish before a successor advances the publication record.
Treat that as bounded stale publication subject to source expiry, not a
proof that every stale leader is immediately rejected.

Extend model scenarios and implementation tests to include role removal,
service deletion, capture/storage prohibition, route narrowing, CRD rollback,
stale watch caches, reconnection and replica promotion. Restore cannot
silently widen any of those rights. These are M1 gates; no model proof is
claimed by this documentation revision (see §7.5 for the model since written).

## 6. Budgets are not a proven scope-level abuse ceiling (M1)

Soft budgets remain financial controls with eventual enforcement. The current
single-principal accounting model assumes a true per-attempt cost upper bound,
instant local known-spend updates and successful concurrency renewal. It does
not establish aggregate scope or multi-site bounds or partition behavior.

Before advertising a numeric exposure bound, define limits at the key,
principal (across keys), scope, site and provider-account levels; a shared
scope's exposure includes all active principals. Model snapshot/local-counter
handoff without double count or lost increments, delayed ledger ingestion,
failed renewal, Valkey failover and provider work continuing after cancellation.
TTL renewal scheduling is not proof that renewal succeeded.

When slot renewal cannot be confirmed, stop admitting dependent work and
attempt cancellation before lease expiry, reporting uncertain provider work.
Cancellation cannot guarantee that provider billing stops. Any fail-open mode
explicitly waives the concurrency/spend bound and records its exposure.
Strict mode accepts only modalities and billing categories with a verified
conservative bound; body bytes alone do not bound URL-referenced images,
documents, framing or tool/cache charges.

Until those gates pass, describe budgets as eventually enforced, with
unquantified additional exposure under failures. Use global concurrency,
rate/output limits and provider-account controls as separate safeguards;
do not represent them as a hard monetary ceiling. Provider reconciliation
remains monthly with daily previews and ADR 0038's tolerance, not a guarantee
of exact daily invoice matching.

## Consequences and acceptance

These requirements resolve review findings by narrowing claims and defining
release evidence. They do not require returning to durable money reservations
or per-request journals. The platform/security owners must review the shared
pool/project boundary and compatible-feature exceptions before activation.
The listed negative tests and model extensions are outstanding work, to be
tracked in the roadmap rather than described as completed validation.

## 7. Adaptations on acceptance

Accepted as written except where this section says otherwise; where it
does, it governs.

**7.1 Feature mode default by route class (adapts §3).** The default is
set by the kind of traffic, not one value for everything:

- **Restricted** is the default for the edge pool, for non-agent
  applications, and for every scope whose policy names a sensitive data
  class, a residency rule, or a storage or egress restriction.
- **Compatible** is the default for coding-agent routes (keys and tokens
  issued by `golemctl agent setup` and routes marked `agent`), under one
  standing approval recorded by the platform team and security, not a
  sign-off per scope. Coding agents ship weekly; restricted mode would break
  every agent user on each release until a review, which is the failure
  ADR 0037 was written to avoid. All known policy controls (contracts.md §8)
  and the M1 stateful-object refusals apply in both modes.
- A scope can always tighten to restricted; loosening a restricted scope to
  compatible needs its owner's and security's approval, as §3 says.

**7.2 Freshness by consistent reads (adapts §5).** golem-control
establishes freshness of both inputs on every renewal directly, rather than
through watch synchronization barriers: a consistent (quorum) LIST of the
config CRDs from the API server and a GET of the state document, both cheap
at golem's size (tens of objects, a few MB). The source manifest binds
`state_version`, the accepted config set's digest (UIDs, generations and
content, deletions included) and a publisher-maintained config revision;
the lease is anchored to the earlier of the two read times. The watch
remains the trigger for prompt publication, not the proof of freshness.

**7.3 Failed slot renewal (adapts §6).** When a gateway cannot confirm
slot renewal (Valkey unreachable):

- it stops admitting new calls for the affected principals, using its own
  local in-flight count as the limit until renewal works again;
- in **soft** scopes, calls already streaming continue (cancelling a
  developer's ten-minute agent turn for an infrastructure blip is the
  worse outcome), and the exposure is recorded as a waiver of the bound
  for that interval;
- in **strict** scopes, it attempts cancellation as §6 requires.

**7.4 A golem-owned capture collector (adapts §2).** The isolated capture
pipeline is a separate OpenTelemetry collector Deployment owned by golem,
not a pipeline inside the enterprise collector: it has only the Langfuse
exporters, accepts only the gateway pools' workload identities, holds only
capture credentials, and is the only Secret the provisioning job may write.
Its compromise exposes the capture credentials it holds and nothing else;
the enterprise collector keeps operational telemetry and holds no Langfuse
credentials.

**7.5 The freshness model (adds to §5).** [model/freshness.md](../../model/freshness.md)
checks §5 and §7.2 on the model: rights from both sources, deletions and
Git rollbacks as config revisions, stale watch caches, leader changes,
pauses between read and publish, and restore. It shows each rule necessary
(a mutant per rule: freshness from the watch cache, the lease anchored to
the state read, fencing on the state version only, restore that keeps only
some restrictions) and states the guarantee per source: a restriction in
either source is honoured within one lease of that source's change. It is
not stated for the intersection, because two reads at different moments can
pair a state and a config that never coexisted (a route narrowed, the member
removed, the route widened back); no read atomic across S3 and the API
server exists, and each restriction is still bounded. Simulation, not
proof; implementation tests remain M1 gates.

**7.6 Scope exposure (adapts §6).** [model/exposure.md](../../model/exposure.md)
checks a scope shared by several principals, on several gateways of a site,
with late ledger ingestion, Valkey outages and failovers, and cancelled calls
the provider keeps billing. It makes four rules requirements, each shown
necessary by a mutant:

- **A scope in-flight limit** alongside the principal's: without it a
  scope's exposure is the sum of its active principals' limits. The bound a
  scope can be told is `scope limit × W` over its budget (soft), on one site.
- **Counter handoff by event**: the site counter is keyed by event ID and
  known spend is the snapshot plus the counter's events the snapshot does
  not include (the snapshot names its ingested events by a ledger
  watermark). A counter that restarts with each snapshot loses increments
  not yet ingested; one simply added counts twice.
- **A cancelled call keeps its slot** until its upstream has ended: the
  connection's close where the provider stops on close, else the
  deployment's maximum duration. Freeing it at cancellation lets cancel and
  resend escape the in-flight limit while the provider keeps working.
- **Slots are re-acquired** for running calls when Valkey returns, so calls
  that streamed through an outage still count.

While Valkey is unreachable, admissions against a gateway's local counts are
recorded as waivers (§7.3); with several gateways they can exceed the limits,
and the model shows the bound holds once waived calls and spend lost to
failures are added, the two terms reported rather than bounded in advance.
Several sites are not modelled: each active site adds its own `scope limit
× W` and what the others spend within one snapshot lag. Simulation, not
proof.
