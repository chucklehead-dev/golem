# Walkthroughs

Guided reads of golem's main components, for an engineer new to the code.
Each says what the component is for and why it is shaped that way, maps
its packages and types, traces one worked example through the code with
file:line references, and lists its invariants (where each is enforced
and tested) and its known gaps. The design documents (docs/gateway.md,
docs/control-plane.md, the ADRs) say what golem should be; these say what
the code does now.

| Walkthrough | Components | Worked example |
|---|---|---|
| [request-path.md](request-path.md) | `gateway/proxy`, `admission`, `dialect`, `sse`, `canon`, `route`, `price` | an OpenAI streaming request from authentication to its ledger row |
| [gateway-edges.md](gateway-edges.md) | `gateway/auth`, `snapshot`, `telemetry`, `cmd/golem-gateway` | a publication arriving at a gateway; a shutdown with a stream in flight |
| [accounting.md](accounting.md) | `gateway/limit`, `control/watermark`, the accounting and exposure models | two gateways admitting for one scope, a cut call held for its tail, the watermark advancing |
| [control-plane.md](control-plane.md) | `control/state`, `lead`, `publish`, `controller`, `audit`, `entrasync`, `cmd/golem-control` | a key revoked through the API reaching a gateway |
| [control-api.md](control-api.md) | `control/service`, `control/api`, `proto`, `cmd/golemctl` | `golemctl keys create` from the CLI to the state and back |
| [config-and-snapshots.md](config-and-snapshots.md) | `lib/` (CRDs, the compiler, snapshots, pointers, buckets, signing, keys, periods, contract codes) | a directory of CRD YAML compiled, published behind a signed pointer and opened by a verifier |

Read request-path first, then accounting; the control-plane trio
(control-plane, control-api, config-and-snapshots) can be read in any
order. File:line references are to the code as of the commit that last
changed the walkthrough: a reference that has drifted is a bug in the
walkthrough.
