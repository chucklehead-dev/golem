# 0014: APIs in protobuf, served with Connect

Status: Proposed. Amended by [0025](0025-smaller-surfaces.md) (no long-running operations); the distribution API it mentioned went with [0028](0028-state-and-snapshots-in-object-storage.md).

## Context

The control API has Go clients (operator, CLI, console, gateway's
distribution client) and TypeScript ones (Backstage, possibly a richer
console later), plus people with `curl`.

## Decision

- APIs are defined in protobuf (`proto/golem/...`, built with buf) and
  served with connect-go on `net/http`: one handler serves the Connect
  protocol (HTTP/JSON or binary), gRPC and gRPC-Web.
- Resource design follows Google's AIPs: standard methods, etags,
  field masks, page tokens, idempotent creates by `request_id`,
  long-running operations for approvals, `validate_only`.
- Errors carry structured details (field violations with paths, the
  violated rule, what to do next).
- `buf breaking` gates CI against the last release.

## Consequences

- Generated, typed clients in Go and TypeScript from one definition.
- JSON over plain HTTP POST for scripts, without a separate REST gateway.
- Not resource-URL REST: `curl` uses POST to `/golem.keys.v1.KeyService/CreateKey`.

## Alternatives

- OpenAPI-first REST: friendlier URLs, weaker streaming, two generators to
  keep in step.
- gRPC only: no browser or `curl` without a proxy.
