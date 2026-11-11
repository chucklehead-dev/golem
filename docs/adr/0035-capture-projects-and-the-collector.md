# 0035: Capture: a Langfuse project per top-level team, inheritable; the collector holds the credentials

Proposed amendment: [ADR 0041](0041-security-boundaries-and-incident-controls.md)
updates the security boundary, requirements and acceptance evidence below.

Status: Accepted. Amends [0016](0016-capture-and-retention.md) and security-and-correctness.md §6.

## Decision

- **Projects**: each top-level team has a Langfuse project. A sub-team or
  project either inherits its parent's project or has its own (a setting on
  the scope); access in Langfuse follows the project.
- **Retention** is the Langfuse project's own setting (one of the classes:
  7, 30, 90, 365 days, 3, 5, 7 years); a scope that needs a different
  retention from its parent has its own project. **No per-record deletion
  job.**
- **Overrides**: keys and services may only turn capture down (content to
  metadata, or off); they cannot lengthen retention or turn capture on.
- **Credentials**: gateways never hold Langfuse keys. They send captured
  traces, tagged with the destination project, to the enterprise OTel
  collector, which holds each project's credentials and routes to it: the
  collector is the capture broker, and it already exists.
- **Provisioning**: a golem-control job creates a project when a scope
  first needs its own, through Langfuse's organisation API, and adds its
  credentials to the collector's Secret.

## Consequences

Tens of projects, not hundreds; retention enforced by Langfuse; no
read-capable credential on any gateway. A key can no longer keep content
longer than its scope.
