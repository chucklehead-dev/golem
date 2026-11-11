# 0037: Forward unknown fields, enforce the policy-relevant ones; cache stability is semantic

Status: Accepted. Amends security-and-correctness.md §4 and [0028](0028-capabilities-and-determinism.md).

## Decision

- **Unknown request fields and beta headers are forwarded**, logged by
  name (never by value), and diffed nightly against the known set; new
  names are reviewed by the platform team. Rejecting them would break
  coding agents on the day they update.
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

Agents keep working through provider releases; a new data-moving feature
can pass for up to a day before review catches it, the accepted risk.
