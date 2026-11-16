# 0022: The control API in protobuf, served with Connect

Status: Accepted (v1 baseline).

## Decision

- Defined in protobuf (`proto/golem/`, buf), served with connect-go on
  `net/http`: one handler speaks the Connect protocol (HTTP/JSON or binary),
  gRPC and gRPC-Web; generated Go and TypeScript clients.
- Packages: config (effective config, explain, overrides), org, keys,
  budget, spend, audit, admin (gateways, state history and restore),
  registration. The snapshot format is protobuf too, in `lib/snapshot`.
- Resource design after Google's AIPs: standard methods, etags, field masks,
  page tokens, idempotent creates by `request_id`, `validate_only`.
  Requests that wait on people are resources with a state and methods
  (`Approve`, `Reject`, `Withdraw`), not long-running operations.
- Errors carry structured details: field violations with paths, the
  violated rule, what to do next.
- `buf breaking` gates CI; the API serves n−1 clients.

## Alternatives

OpenAPI-first REST (two generators, weaker typing); gRPC only (no browser
or `curl` without a proxy).
