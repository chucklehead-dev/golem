# 0037: Forward unknown fields, enforce the policy-relevant ones; cache stability is semantic

Amended by [ADR 0041](0041-security-boundaries-and-incident-controls.md)
updates the security boundary, requirements and acceptance evidence below.

Status: Accepted. Amends security-and-correctness.md §4 and [0028](0028-capabilities-and-determinism.md).

## Decision

- **Feature mode** (ADR 0041 §3, §7.1): restricted mode rejects unapproved
  request fields, beta headers and API versions, and is the default for the
  edge pool, non-agent applications and sensitive or residency-bound scopes.
  Compatible mode forwards unknown features and is the default for
  coding-agent routes under one standing platform and security approval; logging
  uses bounded sanitized names or keyed hashes, never values. Nightly diffs
  support review but do not establish a maximum exposure window.
- **Policy-relevant fields are enforced** wherever they appear: `store`,
  `previous_response_id`, `background`, file, container and conversation
  identifiers, `mcp_servers`, server-side tools with network access, `n`,
  and anything that changes billing or where data goes; this list is part
  of the contract (contracts.md §8) and grows with the nightly review.
- The cheap structural checks stay: malformed JSON, duplicate members,
  trailing values, size and depth limits.
- **Prompt-cache stability is semantic**: encoders must keep content
  blocks, tool definitions and their order stable for the same
  conversation; byte-level JSON stability is not required.

## Consequences

Compatible-mode agents can keep working through provider releases, accepting
new data-moving semantics until detection and remediation. Restricted scopes
may require a reviewed compatibility update before an agent upgrade works.
Neither mode eliminates provider behavior or contractual data-handling risk.
