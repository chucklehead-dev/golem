# Walkthrough: the control API and golemctl

This walkthrough is for an engineer new to golem-control's API side: the
org and key services, their transport, and the CLI that calls them. The
design lives elsewhere: [control-plane.md](../control-plane.md) §2 (the
service layer), [org-model.md](../org-model.md) §2 (roles),
[tools.md](../tools.md) §1 (golemctl), [entra.md](../entra.md) (the app
registrations) and ADRs [0003](../adr/0003-components-and-layering.md)
(layering), [0009](../adr/0009-identity-and-authentication.md) (identity)
and [0010](../adr/0010-organisation-scopes-and-roles.md) (roles). Line
numbers are as of this writing; the function names are the stable
reference.

## 1. Layering

ADR 0003 fixes the order inside golem-control: transport, then service,
then store. Each layer has one job.

| Layer | Package | Does | Does not |
|---|---|---|---|
| Transport | `control/api` | Serves the generated Connect handlers, authenticates every call, converts protobuf messages to the service's types and back, maps errors to Connect codes | Decide who may do what, or write the store |
| Service | `control/service` | Holds the rules: authorization (org-model §2), validation with field paths, the change and its audit event | Know about HTTP, protobuf or tokens |
| Store | `control/state` | Keeps the state document in the bucket: applies a change to a copy, checks the invariants, writes with `If-Match`, retries when another writer won | Know who is calling |

`internal/archtest` holds the walls that a test can check: the gateway
links no control-plane code, and golemctl links only `lib/` and the
generated clients (`TestGolemctlLinksNoComponent`). The one place the
transport reads the store is `EntraAuth`, which looks up a token's
principal in the current document; it never writes.

`cmd/golem-control` wires it up (`apiServer`, cmd/golem-control/main.go:135):
an `api.Server` over a `service.Service` over the shared `state.Store`,
with `api.EntraAuth` as the authenticator. The API is served only with
`-listen`, and refuses to start without `-entra-tenant`.

## 2. Authentication

Every call is authenticated by the HTTP handler `Server.Handler` wraps
around the Connect routes, from its headers alone, before Connect reads
its body: an unauthenticated caller costs a header check, never a decode.
It asks the `Authenticator` for a `service.Caller`, answers
`unauthenticated` (written by Connect's error writer in the caller's
protocol) if it fails, and stores the caller in the request's context,
where each handler reads it with `caller(ctx)`. A message over
`MaxRequest` (1 MiB) is refused (`resource_exhausted`), and the server
cuts off a request not read in 30 seconds or a response not written in a
minute.

`EntraAuth.Authenticate` (control/api/entra.go):

1. reads `Authorization: Bearer <token>` (`bearer`);
2. verifies the token with `gateway/auth`'s `Entra.Verify`: RS256 only,
   golem's tenant, the control API registration's audience
   (`api://golem-control`, ADR 0046; a gateway token's `api://golem` is
   refused), and `scp` `golem.manage` for a user or app role
   `golem.service` with `idtyp: app` for a service;
3. starts from the caller `entra:<id>` (the user's `oid`, or the
   service's `azp`), admin if the token carries app role `golem.admin`;
4. replaces that with the principal whose `Entra` field equals the ID and
   whose kind matches (a user token matches only a user principal). The
   store keeps Entra IDs unique, so at most one matches.

A caller with no principal yet (an admin bootstrapping, or a user before
anyone added them) keeps `entra:<id>`. It is a member of nothing and
manages nothing, so it has no rights but an admin's, and the audit records
it by its Entra ID. The lookup reads the document as last refreshed by
`state.Store.Run`, so a principal just added on another replica is found
once this replica has refreshed.

Admin is never stored: it comes from the token on every call.

## 3. Authorization, per method

The rules are in `control/service/service.go`, one method each. `manages`
(admin, or a manager binding on the scope) and `sees` (that, or a member)
are the two shared predicates.

| Method | Who may | Notes |
|---|---|---|
| `CreateScope` | admins | top-level scopes only for now |
| `AddMember` | the scope's managers, admins | a manager adds an existing principal or creates a keys-only service; only admins create a principal with an Entra ID or a user (ADR 0047); an existing one keeps its kind and Entra ID; a new one needs a kind |
| `RemoveMember` | the scope's managers, admins; a fellow manager only by admins | a membership granted in golem; revokes the member's keys in the scope and drops its role there, unless still a member through a bound group |
| `BindGroup`, `UnbindGroup` | admins | an Entra group's members become the scope's at the next sync |
| `BindManager` | admins | `manager` is the only role bound so far |
| `MintKey` | a member, its own keys; the scope's managers, its services' keys; admins, any | owner must be a member; models within the scope's; expiry in the future; a personal key within 90 days, by default at 90 |
| `RevokeKey` | the key's owner, admins; a service's key, its scope's managers | revoking a revoked key is no change; an outsider gets `not found` |
| `Whoami` | anyone signed in | |
| `ListScopes` | lists what the caller `sees` | |
| `GetScope` | members, managers, admins | anyone else gets `not found`, not `permission denied`: a scope's existence is not theirs to learn |
| `ListKeys` | the key's owner, its scope's managers, admins | never the key itself |

Where it is tested, in control/service/service_test.go:

- `TestAuthorization` (line 47): a table of operations against five
  callers (an admin, team-a's manager, team-b's manager, a member, an
  outsider); each operation lists the callers it allows, and every other
  must get `ErrPermission`. Removing a rule makes a row fail.
- `TestRevokeKey` (line 91): the revoke rule, including that the key is
  revoked in the store exactly when allowed.
- `TestReads` (line 187) and `TestManagerReadsScope` (line 257): who sees
  which scopes and keys, including a manager who is not a member.
- `TestKeyRules`, `TestRemoveMember`, `TestValidateOnly`: validation, the
  removal's consequences, and validate-only.

## 4. One change: apply, validate-only, audit

Every mutating method ends in `Service.apply` (service.go:86). It wraps the
method's change in a function that runs the change and appends one
`state.AuditEvent` (time, actor, action such as `key.mint`, target, and a
small detail map), then hands it to the store:

- normally to `state.Store.Update`, which applies it to a copy of the
  document, numbers the new audit events, checks the invariants
  (`state.check`), and writes with `If-Match`. When another writer won, it
  re-reads and runs the change again, so a change must work from the
  document it is given and do its authorization checks inside it (most
  do: the checks that need the document run in the closure);
- with `validate_only` (`Options.ValidateOnly`), to `state.Store.Validate`,
  which does the same up to the check and writes nothing. So a dry run
  reports the same authorization, validation and invariant errors a real
  run would.

The change and its audit event are one write: there is no change without
its event, and no event without its change. The leader's `control/audit`
then ships the outbox to the archive and to ClickHouse. A key's plaintext
is never in an event; `TestKeyRules` checks it.

## 5. Errors

The service returns errors that wrap one of three sentinels;
`connectError` maps them, with the store's:

| Error | Connect code | HTTP (JSON) |
|---|---|---|
| no or bad token | `unauthenticated` | 401 |
| `service.ErrPermission` | `permission_denied` | 403 |
| `service.ErrNotFound` | `not_found` | 404 |
| `service.ErrInvalid` (an `*Invalid` with a field path, such as `key.models[0]`) | `invalid_argument` | 400 |
| `state.ErrInvariant` | `failed_precondition` | 400 |
| `state.ErrContended` | `aborted` | 409 |
| context past its deadline | `deadline_exceeded` | 504 |
| context cancelled | `canceled` | 499 |
| anything else | `internal` | 500 |

The message is the service's, which names the field and what to do; tests
assert on the codes and on field paths (`TestAPI`). An internal error (the
bucket's or S3's, whose text names buckets and keys) is logged with an id,
and the caller is told only "internal error <id>: see golem-control's
log" (`TestLimits`).

## 6. The protos and code generation

`proto/golem/org/v1/org.proto` (`OrgService`) and
`proto/golem/keys/v1/keys.proto` (`KeyService`) are the contract. Every
mutating request has `validate_only`. Generated code goes to `gen/` with
protoc-gen-go and protoc-gen-connect-go (`proto/buf.gen.yaml`), and is
committed:

    cd proto && buf lint && buf generate

`buf lint` uses the STANDARD rules. Connect serves each method at
`/<package>.<Service>/<Method>` as gRPC, gRPC-Web and plain JSON, so curl
works (`TestPlainJSON`):

    curl -H 'Authorization: Bearer …' -H 'Content-Type: application/json' \
      -d '{"scope":"team-a"}' https://golem-control/golem.keys.v1.KeyService/ListKeys

## 7. golemctl

**Login.** `golemctl login --tenant … --client-id … --api …`
runs Entra's device code flow: it posts to `/devicecode` for the scope
`<control>/golem.manage offline_access`, prints Entra's message, and polls
`/token` at Entra's interval (slower on `slow_down`) until the code
expires. Tenant, client ID, API URL, both resources' URIs and authority are
remembered, so a second login needs no flags.

**Token cache.** Settings and tokens live in one JSON file, mode 0600, in
`$GOLEMCTL_HOME` or the user's config directory (`settingsPath`), written
by a rename so it is never half-written. One refresh token, and an access
token per resource (ADR 0046): `accessToken(ctx, r)` returns `r`'s cached
token while it has more than a minute left, else redeems the refresh
token for `r`'s scope and saves the result; an answer without a token is
an error, never saved. The API commands use the control API's;
`golemctl token` prints the gateway's, for coding agents' credential
helpers, and it cannot call the control API.

**Commands.** `run` (main.go:55) dispatches the offline commands
(`config validate`, `keys mint --dev`), `login` and `token`, and sends
everything else to `runAPI` (api.go:196). The API commands are one table,
`apiCommands` (api.go:56), keyed by their name (`whoami`, or two words
such as `keys create`). Each entry says how many positional arguments it
takes, and its `flags` function defines its own flags and returns the
function that runs it. `parse` reads the positional arguments (they come
first), defines `--dry-run` on every command, and parses the flags. A
`call` carries the clients, the arguments, `--dry-run` and the output.

**--dry-run** sets the request's `validate_only`; a mutation then prints
`ok (dry run, nothing written): …`. Reads accept the flag and ignore it.

**Exit codes**: 0 success; 1 the command failed (the API's error is
printed, its Connect code first, such as `permission_denied: …`); 2 used
wrongly (an unknown command, a missing argument, a bad flag), with the
usage.

## 8. Worked example: `golemctl keys create --scope team-a`

1. `run` (cmd/golemctl/main.go:55) finds no offline command and calls
   `runAPI` (cmd/golemctl/api.go:196).
2. `runAPI` builds the clients (`clients`, api.go:237): it loads the
   settings for the API URL and installs an interceptor that, on each
   call, gets a token from `accessToken(ctx, control)`, refreshing it if
   it is about to expire, and sets `Authorization: Bearer` (api.go:248).
3. `runAPI` looks up `keys create` in `apiCommands` (api.go:156); `parse`
   defines `--scope`, `--owner`, `--models`, `--expires` and `--dry-run`
   and parses them.
4. The command checks `--scope` (api.go:163), turns `--expires 30d` into
   a timestamp, builds a `MintKeyRequest` with no owner (the caller's own
   key) and calls `KeyService.MintKey` (api.go:173): a Connect POST to
   `/golem.keys.v1.KeyService/MintKey`.
5. On the server, the interceptor in `Server.Handler`
   (control/api/api.go:38) calls `EntraAuth.Authenticate`
   (control/api/entra.go): the token is verified and its `oid` mapped
   to, say, principal `alice`.
6. `keys.MintKey` (control/api/keys.go:16) converts the request to a
   `service.KeyRequest` and calls `Service.MintKey`
   (control/service/service.go:228).
7. `MintKey` defaults the owner to the caller, mints the plaintext with
   `lib/key` (service.go:232) and hashes it, then calls `apply`
   (service.go:235) with the action `key.mint` and a change that, on the
   document: finds the scope (or `not found`), finds the owner and checks
   it is a member (or `invalid key.owner`), checks the caller may mint for
   that owner (service.go:246, or `permission denied`), checks each model
   is in the scope's and the expiry is in the future, and stores the
   key's record by its hash (service.go:263), never the plaintext.
8. `apply` (service.go:86) appends the audit event (actor `alice`, target
   the key's display prefix, detail owner and scope) and calls
   `state.Store.Update`, which checks the invariants (a live key's owner
   is a member of its scope, among others) and writes the document.
9. Back in the transport, `reply` (api.go:63) wraps the response, and
   `MintKey` sets `Cache-Control: no-store` (keys.go:25), since the key is
   in it. An error instead goes through `connectError`.
10. golemctl prints the key once, and its hash, which names it from then
    on (`keys revoke <hash>`).

`TestGolemctl` (cmd/golemctl/golemctl_test.go:68) runs this path end to
end against a fake Entra and a real `api.Server`.

## 9. Adding a method end to end

1. **Proto.** Add the RPC and its messages to the service's `.proto`, with
   `validate_only` if it changes anything, and a comment saying who may
   call it. Run `buf lint && buf generate` in `proto/` and commit `gen/`.
2. **Service.** Add the method to `control/service`: take a `Caller` (and
   `Options` if it mutates), check authorization inside the change (with
   `manages`/`sees` where they fit), validate with `invalid(field, …)`,
   and go through `apply` with an action name such as `scope.update`.
   Reads use `read`.
3. **Tests.** Add the operation to `TestAuthorization`'s tables with the
   callers it allows; add its validation cases. Check that removing the
   rule makes a row fail.
4. **Transport.** Add the handler to `org` or `keys` in `control/api`:
   convert the request, call the service with `options(m.ValidateOnly)`,
   and return `reply(&Response{…}, err)`. The compiler tells you when a
   generated handler interface method is missing.
5. **CLI.** Add an entry to `apiCommands`: its positional arguments, its
   flags, and the call, using `c.dry` and `c.done` if it mutates. Add it
   to `usage` in main.go and a case to `TestGolemctl`.
6. **Docs.** Update control-plane.md §2's "Built" paragraph and tools.md
   §1.

## 10. Known gaps

- Tokens are in a file, not the OS keychain (tools.md §1).
- Sign-in is device code only; browser sign-in with loopback PKCE
  (entra.md #2) is not built.
- No `-o json|yaml`: output is tables for people.
- Most of the org model is still to come: sub-teams and projects, the
  services entity, member and service caps, budgets and grants, capture
  settings, unbinding a manager, rotating or constraining keys, and
  required expiry for personal keys (org-model.md §3 says they must
  expire; `MintKey` does not insist).
- Responses carry no etag (control-plane.md §2 step 5) and there are no
  conditional updates by the client.
- `golemctl` loads its settings before it looks up the command, so an
  unknown command when signed out reports "not signed in" (exit 1) rather
  than the usage (exit 2).
