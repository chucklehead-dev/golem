# 0029: Security and correctness requirements after design review

Status: Proposed. Amends ADRs 0002, 0005, 0006, 0007, 0009, 0012,
0013, 0014, 0015, 0016, 0017, 0023, 0025, 0027 and 0028 in the areas
specified below. Unaffected decisions remain in force.

## Context

The design review found that several baseline guarantees did not follow
from their protocols: ceiling totals did not account for disconnected
sites, signed publication did not fence stale writers, status objects were
not trusted rollback checkpoints, and telemetry transport did not establish
durable or duplicate-safe accounting. Security review also exposed missing
identity-header provenance, object ownership, restore restrictions and
capture credential isolation.

## Decision

Adopt [security-and-correctness.md](../security-and-correctness.md) as the
normative revision and acceptance criteria.

- Issue fixed site allocations per period; defer dynamic reclamation.
- Bound authorization age, fence publication, and preserve restrictive
  security changes on restore; status remains diagnostic.
- Validate complete requests and revalidate hook rewrites.
- Defer stored provider object operations pending an ownership registry.
- Enforce outbound destination policy, live CRD RBAC, immutable audit
  archival, resource bounds and authenticated standalone operation.
- Isolate read-capable capture credentials behind a broker unless verified
  write-only credentials are available.
- Require durable per-attempt accounting and duplicate-safe event balances.
  First-byte commitment alone does not guarantee upstream idempotency.

## Consequences

M1 gains explicit durability and publication spikes and security release
gates. Internal authorization staleness exceptions require recorded approval.
The five-second revocation target is conditional on connected healthy
gateways; partitions have an explicit lease bound. No implementation is
claimed by accepting these requirements. Dynamic site transfers, stored
objects and some provider features are deliberately deferred until their
protocols and tests satisfy these requirements.

## Alternatives

Keeping informal operational assumptions or relying solely on later threat
model review leaves known gaps in core guarantees; rejected. Selecting a
journal or fencing implementation without measuring its behavior is also
premature; the requirements make those choices explicit production gates.
