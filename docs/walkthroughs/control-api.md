# Walkthrough: the control API and golemctl

This walkthrough is for an engineer new to golem-control's API side: the
org and key services, their transport, and the CLI that calls them. The
design lives elsewhere: [control-plane.md](../control-plane.md) §2 (the
service layer), [org-model.md](../org-model.md) §2 (roles),
[tools.md](../tools.md) §1 (golemctl), [entra.md](../entra.md) (the app
registrations) and ADRs [0003](../adr/0003-components-and-layering.md)
(layering), [0009](../adr/0009-identity-and-authentication.md) (identity),
[0010](../adr/0010-organisation-scopes-and-roles.md) (roles),
[0046](../adr/0046-control-api-registration.md) (the control API on its
own app registration, scope `golem.manage`) and
[0047](../adr/0047-identity-from-entra-groups.md) (identity from Entra
groups). Line numbers are as of this writing; the function names are the
stable reference.

## 1. Layering

ADR 0003 fixes the order inside golem-control: transport, then service,
then store. Each layer has one job.

| Layer | Package | Does | Does not |
|---|---|---|---|
| Transport | `control/api` | Serves the generated Connect handlers, authenticates every call, converts protobuf messages to the service's types and back, maps errors to Connect codes | Decide who may do what, or write the store |
| Service | `control/service` | Holds the rules: authorization (org-model §2), validation with field paths, the change and its audit event | Know about HTTP, protobuf or tokens |
| Store | `control/state` | Keeps the state document in the bucket: applies a change to a copy, checks the invariants, writes with `If-Match`, retries when another writer won | Know who is calling |

`internal/archtest` holds the walls that a test can check: the gateway
links no control-plane code (`TestGatewayLinksNoControlPlane`), and
golemctl links only `lib/` and the generated clients
(`TestGolemctlLinksNoComponent`). The one place the transport reads the
store is `EntraAuth`, which looks up a token's principal in the current
document; it never writes.

`cmd/golem-control` wires it up (`apiServer`,
cmd/golem-control/main.go:263): an `api.Server` over a `service.Service`
over the shared `state.Store`, with `api.EntraAuth` as the
authenticator. The API is served only with `-listen`, and refuses to
start without `-entra-tenant` (`TestAPINeedsTenant`); `-entra-audience`
(default `api://golem-control`, main.go:153) is the control API
registration's, never the gateway's. The `http.Server` cuts off a slow
peer (main.go:279): headers within 10 seconds, the whole request within
30, the response within a minute, idle connections after two, headers
at most 64 KiB.

## 2. Authentication

`Server.Handler` (control/api/api.go:52) wraps the Connect routes in a
plain HTTP handler that authenticates every call from its headers alone,
before Connect reads its body (api.go:59): an unauthenticated caller
costs a header check, never a decode. It asks the `Authenticator` for a
`service.Caller`, answers `unauthenticated` (written by Connect's error
writer in the caller's protocol) if it fails, and stores the caller in
the request's context (api.go:64), where each handler reads it with
`caller(ctx)`. A message over `MaxRequest` (default 1 MiB,
`defaultMaxRequest`) is refused, `resource_exhausted`, without being
decoded. `TestLimits` checks both: the unauthenticated call's body is
never read, and the oversized one is refused.

`EntraAuth.Authenticate` (control/api/entra.go:28):

1. reads `Authorization: Bearer <token>` (`bearer`, entra.go:49);
2. verifies the token with `lib/entra`'s `Verifier.Verify`: RS256 only,
   golem's tenant and a v2 token, the control API registration's
   audience (`api://golem-control`, ADR 0046; a gateway token's
   `api://golem` is refused), and either `scp` `golem.manage` with an
   `oid` (a user) or, with no `scp`, app role `golem.service`, `idtyp:
   app` and an `azp` (a service). An app-only token without the `idtyp`
   claim is refused with a message naming the registration's fix;
3. starts from the caller `entra:<id>` (the user's `oid`, or the
   service's `azp`), admin if the token carries app role `golem.admin`
   (entra.go:37);
4. replaces that with the principal whose `Entra` field equals the ID and
   whose kind matches (entra.go:39; a user token matches only a user
   principal, so a service whose app ID equals a user's `oid` is not
   that user). The store keeps Entra IDs unique, so at most one matches.

A caller with no principal yet (an admin bootstrapping, or a user before
the group sync or an admin made them one) keeps `entra:<id>`. It is a
member of nothing and manages nothing, so it has no rights but an
admin's, and the audit records it by its Entra ID. No principal may be
created with an ID beginning `entra:` (`state.ReservedPrefix`, refused by
`AddMember` and by the store's invariants), so nobody can create a
principal that a person signing in later would inherit. The lookup reads
the document as last refreshed by `state.Store.Run`, so a principal just
added on another replica is found once this replica has refreshed.
`TestEntraAuth` covers the mapping and the refusals, including a gateway
token.

Admin is never stored: it comes from the token on every call.

## 3. Authorization, per method

The rules are in `control/service/service.go`, one method each. Four
shared helpers sit together near the top (service.go:123-156):

- `manages(d, c, scope)`: an admin, or a manager binding on the scope;
- `sees(d, c, scope)`: that, or a member (granted or synced);
- `visibleScope(d, c, scope)`: the scope's record, or `not found` if it
  does not exist or `c` does not see it. A scope's existence is not an
  outsider's to learn, so a refusal is never `permission denied` to
  someone who could not see the scope;
- `requireManager(d, c, scope, what)`: `visibleScope`'s `not found`,
  then `permission denied` ("only scope X's managers *what*") unless `c`
  manages it.

| Method | Who may | Notes |
|---|---|---|
| `CreateScope` | admins | top-level scopes only for now; an ID and nothing else. Its policy (models, budget and budget mode, limits) is its ScopePolicy, in the config repository (ADR 0050): until that is merged, the scope is published with no models (`TestCreateScope`) |
| `AddMember` | the scope's managers, admins (`requireManager`) | who the member is comes from `principalToAdd`: an existing principal, unchanged (a different kind or Entra ID given is `invalid`); else a new one with a kind. A manager may create only a keys-only service: never a user, never an Entra ID (ADR 0047: users come from bound groups, and an Entra ID is bound by the sync or an admin). IDs beginning `entra:` are refused |
| `RemoveMember` | the scope's managers, admins (`requireManager`); a fellow manager only by admins | removes a membership granted in golem; a synced one is the group's (`invalid`: remove them in Entra or unbind the group). When the principal is no longer a member at all, its keys in the scope are revoked, its default scope cleared and its manager role there dropped; a principal still a member through a bound group keeps its keys and role |
| `BindGroup`, `UnbindGroup` | admins | an Entra group's members become the scope's at the next sync (control/entrasync); unbinding ends them at the next sync |
| `BindManager` | admins | `manager` is the only role bound so far; a scope or principal that does not exist is `not found` (`TestBindManager`). The principal need not be a member: managing is its own right (org-model.md §2: a parent team's managers manage its sub-teams), and a manager reads the scope without being one (`TestManagerReadsScope`) |
| `MintKey` | a member, its own keys; the scope's managers, its services' keys; admins, any (`mayMint`) | owner must be a member; models within the scope's ScopePolicy's, as golem-control's config gives them (`narrows`, `Service.ScopeModels`; not checked while no config has been read: the gateway intersects a key's models with its scope's anyway, `TestKeyModelsFromConfig`); expiry in the future; a personal (user's) key within 90 days, by default at 90 (`keyExpiry`, `MaxPersonalKeyLife`); a service's key needs no expiry |
| `RevokeKey` | the key's owner, admins; a service's key, its scope's managers | a manager cannot revoke a member's personal key (they remove the member instead); revoking a revoked key is no change; an outsider gets `not found` |
| `Whoami` | anyone signed in | |
| `ListScopes` | lists what the caller `sees` | |
| `GetScope` | members, managers, admins | its members, managers and bound groups; anyone else gets `not found` (`visibleScope`). Its policy is not the API's: `golemctl scopes get` names the ScopePolicy to read |
| `ListKeys` | the key's owner, its scope's managers, admins | never the key itself |

## 4. Invariants: where each is enforced and tested

| Invariant | Enforced in | Tested by |
|---|---|---|
| Every call is authenticated before its body is read | `Server.Handler` (api.go:52) | `TestLimits`, `TestAPI` |
| A request message is at most 1 MiB | `connect.WithReadMaxBytes` in `Handler` (api.go:53) | `TestLimits` (with a 1 KiB limit) |
| An internal error's text (bucket, S3) never reaches the caller | `connectError` (api.go:89): logged with an id, answered "internal error <id>" | `TestLimits` |
| Only control API tokens (`golem.manage`, `api://golem-control`) are accepted; a gateway token is not | `entra.Verifier` with `Scope: entra.ScopeManage` (main.go:267) | `TestEntraAuth` |
| Each method's callers are exactly those of §3 | the methods, through `manages`, `sees`, `visibleScope`, `requireManager`, `mayMint` | `TestAuthorization` (service_test.go:47), `TestRevokeKey` (:98), `TestBindGroup` (:373) |
| An outsider learns nothing of a scope or key: `not found`, not `permission denied` | `visibleScope`, `RevokeKey` | `TestAuthorization`, `TestRevokeKey`, `TestReads` (:231) |
| A manager cannot remove a fellow manager | `RemoveMember` | `TestManagerRules` (:127) |
| A manager is bound on a scope and of a principal that exist (`not found` otherwise, before the store's invariant); it need not be a member | `BindManager`; `checkRoles` in control/state | `TestBindManager`, `TestManagerReadsScope` |
| A manager cannot create a user or bind an Entra ID; no principal ID begins `entra:` | `principalToAdd`, `AddMember`; `checkPrincipals` in control/state | `TestIdentityBinding` (:319), `TestEntitlementInvariants` |
| A synced membership is removed only in Entra or by unbinding | `RemoveMember` | `TestIdentityBinding` |
| A removed member's keys in the scope are revoked, and its role there dropped | `RemoveMember`, `Document.RevokeOutside` | `TestRemoveMember` (:207) |
| A key narrows its scope's models; its owner is a member; its expiry is in the future | `MintKey`, `keyExpiry`; `checkKeys` for live keys' owners | `TestKeyRules` (:172) |
| A personal key lives at most 90 days, by default exactly 90 | `keyExpiry` | `TestManagerRules` |
| A key's plaintext is never stored or audited, only its hash | `MintKey` stores `state.Key` by hash; the audit target is the display prefix | `TestKeyRules` |
| A dry run checks everything and writes nothing | `Service.apply` → `state.Store.Validate` | `TestValidateOnly` (:156), `TestGolemctl` |
| A change and its audit event are one write | `Service.apply` | `TestKeyRules` (the event is in the document written with the key) |
| A revocation is never undone | `checkRevocations` in control/state | `TestEntitlementInvariants` |
| A minted key's response is not cached | `keys.MintKey` sets `Cache-Control: no-store` (keys.go:25) | none |
| golemctl's gateway token cannot call the control API | per-resource tokens in golemctl (`Tokens`, `accessToken`); the audience check above | `TestGolemctl` ("the gateway's token is its own"), `TestEntraAuth` |

## 5. One change: apply, validate-only, audit

Every mutating method ends in `Service.apply` (service.go:99). It wraps
the method's change in a function that runs the change and appends one
`state.AuditEvent` (time, actor, action such as `key.mint`, target, and a
small detail map; service.go:105), then hands it to the store:

- normally to `state.Store.Update` (service.go:111), which applies it to
  a copy of the document, numbers the new audit events, checks the
  invariants (`state.check`), and writes with `If-Match`. When another
  writer won, it re-reads and runs the change again, so a change must
  work from the document it is given and do its authorization checks
  inside it: every check that needs the document runs in the closure,
  and none of them writes to the variables it captured;
- with `validate_only` (`Options.ValidateOnly`), to `state.Store.Validate`
  (service.go:109), which does the same up to the check and writes
  nothing. So a dry run reports the same authorization, validation and
  invariant errors a real run would.

The change and its audit event are one write: there is no change without
its event, and no event without its change. The leader's `control/audit`
then ships the outbox to the archive and to ClickHouse. A key's plaintext
is never in an event; `TestKeyRules` checks it.

## 6. Errors

The service returns errors that wrap one of three sentinels;
`connectError` (api.go:89) maps them, with the store's and the
context's:

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
| a message over `MaxRequest` (Connect's own) | `resource_exhausted` | 429 |
| anything else | `internal` | 500 |

The message is the service's, which names the field and what to do; tests
assert on the codes and on field paths (`TestAPI`). An internal error
(the bucket's or S3's, whose text names buckets, keys and endpoints) is
logged in full with an id (`Server.Log`, default `slog.Default()`), and
the caller is told only "internal error <id>: see golem-control's log"
(`TestLimits`).

## 7. The protos and code generation

`proto/golem/org/v1/org.proto` (`OrgService`) and
`proto/golem/keys/v1/keys.proto` (`KeyService`) are the contract. Every
mutating request has `validate_only`. Generated code goes to `gen/` with
protoc-gen-go and protoc-gen-connect-go (`proto/buf.gen.yaml`), and is
committed; never edit `gen/` by hand:

    cd proto && buf lint && buf generate

`buf lint` uses the STANDARD rules. Connect serves each method at
`/<package>.<Service>/<Method>` as gRPC, gRPC-Web and plain JSON, so curl
works (`TestPlainJSON`):

    curl -H 'Authorization: Bearer …' -H 'Content-Type: application/json' \
      -d '{"scope":"team-a"}' https://golem-control/golem.keys.v1.KeyService/ListKeys

## 8. golemctl

**Login.** `golemctl login --tenant … --client-id … --api …`
(cmd/golemctl/login.go:146) runs Entra's device code flow: it posts to
`/devicecode` for the scope `<control>/golem.manage offline_access`,
prints Entra's message, and `awaitSignIn` (login.go:199) polls `/token`
at Entra's interval (slower on `slow_down`) until the user signs in or
the code expires. Tenant, client ID, API URL, both resources' URIs
(`--control`, default `api://golem-control`; `--gateway`, default
`api://golem`) and authority are remembered, so a second login needs no
flags.

**Token cache** (ADR 0046). Settings and tokens live in one JSON file,
mode 0600, in `$GOLEMCTL_HOME` or the user's config directory
(`settingsPath`), written by a rename so it is never half-written. It
holds one refresh token (`Refresh`) and an access token per resource
(`Tokens`, keyed `control` and `gateway`). `accessToken(ctx, r)`
(login.go:238) returns `r`'s cached token while it has more than a
minute left, else redeems the refresh token for `r`'s scope
(`golem.manage` or `golem.access`), saves it under `r` and keeps the
other resource's as it was; an answer without a token is an error, never
saved. The API commands use the control API's; `golemctl token` prints
the gateway's, for coding agents' credential helpers, and that token
cannot call the control API (wrong audience and scope).

**Commands.** `run` (main.go:58) dispatches the offline commands
(`config validate`, `keys mint --dev`), `login` and `token`, and sends
everything else to `runAPI` (api.go:196). The API commands are one table,
`apiCommands` (api.go:64), keyed by their name (`whoami`, or two words
such as `keys create`). Each entry says how many positional arguments it
takes, and its `flags` function defines its own flags and returns the
`runFunc` that runs it. `parse` (api.go:218) reads the positional
arguments (they come first), defines `--dry-run` on every command, and
parses the flags. A `call` carries the clients, the arguments,
`--dry-run` and the output; a mutating command ends with
`c.done(err, what)` (api.go:35), which returns the call's error or
prints what changed.

**--dry-run** sets the request's `validate_only`; a mutation then prints
`ok (dry run, nothing written): …`. Reads accept the flag and ignore it.

**Exit codes**: 0 success; 1 the command failed (the API's error is
printed, its Connect code first, such as `permission_denied: …`); 2 used
wrongly (an unknown command, a missing argument, a bad flag), with the
usage.

## 9. Worked example: `golemctl keys create --scope team-a`

1. `run` (cmd/golemctl/main.go:58) finds no offline command and calls
   `runAPI` (main.go:78; cmd/golemctl/api.go:196).
2. `runAPI` builds the clients (`clients`, api.go:237): it loads the
   settings for the API URL and installs an interceptor that, on each
   call, gets the control API's token from `accessToken(ctx, control)`
   (api.go:244), refreshing it if it has under a minute left, and sets
   `Authorization: Bearer` (api.go:248).
3. `runAPI` looks up `keys create` in `apiCommands` (api.go:203); `parse`
   (api.go:208) defines `--scope`, `--owner`, `--models`, `--expires`
   (api.go:163) and `--dry-run`, and parses them.
4. The command checks `--scope` (api.go:169), turns `--expires 30d` into
   a timestamp (api.go:173), builds a `MintKeyRequest` with no owner (the
   caller's own key) and calls `KeyService.MintKey` (api.go:180): a
   Connect POST to `/golem.keys.v1.KeyService/MintKey`.
5. On the server, the handler `Server.Handler` wraps around the routes
   (control/api/api.go:52) calls `EntraAuth.Authenticate`
   (control/api/entra.go:28) before the body is read: the token is
   verified for `golem.manage` and its `oid` mapped to, say, principal
   `alice`. Connect then decodes the body, refusing one over 1 MiB.
6. `keys.MintKey` (control/api/keys.go:16) converts the request to a
   `service.KeyRequest` and calls `Service.MintKey` (keys.go:22;
   control/service/service.go:372).
7. `MintKey` defaults the owner to the caller (service.go:373), mints the
   plaintext with `lib/key` and hashes it (service.go:377), then calls
   `apply` (service.go:379) with the action `key.mint` and a change that,
   on the document:
   - finds the scope with `visibleScope` (service.go:380), or `not found`;
   - finds the owner (service.go:384), or `not found`, and checks it is a
     member (service.go:388), or `invalid key.owner`;
   - checks the caller may mint for that owner with `mayMint`
     (service.go:391, defined at :418), or `permission denied`;
   - checks each model is in the scope's (service.go:394), or `invalid
     key.models[i]`;
   - works out the expiry with `keyExpiry` (service.go:399, defined at
     :426): not in the past, and, because alice is a user, at most 90
     days away (with no `--expires`, exactly 90);
   - stores the key's record by its hash (service.go:406), never the
     plaintext.
8. `apply` (service.go:99) appends the audit event (actor `alice`, target
   the key's display prefix, detail owner and scope) and calls
   `state.Store.Update`, which checks the invariants (a live key's owner
   is a member of its scope, among others) and writes the document.
9. Back in the transport, `reply` (api.go:78) wraps the response, and
   `MintKey` sets `Cache-Control: no-store` (keys.go:25), since the key is
   in it. An error instead goes through `connectError` (api.go:89).
10. golemctl prints the key once, and its hash, which names it from then
    on (`keys revoke <hash>`; api.go:184).

`TestGolemctl` (cmd/golemctl/golemctl_test.go:74) runs this path end to
end against a fake Entra and a real `api.Server`.

## 10. Adding a method end to end

1. **Proto.** Add the RPC and its messages to the service's `.proto`, with
   `validate_only` if it changes anything, and a comment saying who may
   call it. Run `buf lint && buf generate` in `proto/` and commit `gen/`.
2. **Service.** Add the method to `control/service`: take a `Caller` (and
   `Options` if it mutates), check authorization inside the change (with
   `requireManager`, `visibleScope`, `manages` or `sees` where they fit),
   validate with `invalid(field, …)`, and go through `apply` with an
   action name such as `scope.update`. Reads use `read`.
3. **Tests.** Add the operation to `TestAuthorization`'s tables with the
   callers it allows; add its validation cases. Check that removing the
   rule makes a row fail.
4. **Transport.** Add the handler to `org` or `keys` in `control/api`:
   convert the request, call the service with `options(m.ValidateOnly)`,
   and return `reply(o.Server, &Response{…}, err)`. The compiler tells
   you when a generated handler interface method is missing.
5. **CLI.** Add an entry to `apiCommands`: its positional arguments, its
   flags, and the call, ending with `c.done(err, what)` if it mutates.
   Add it to `usage` in main.go and a case to `TestGolemctl`.
6. **Docs.** Update control-plane.md §2's "Built" paragraph, tools.md §1
   and this walkthrough.

## 11. Known gaps

- Tokens are in a file, not the OS keychain (tools.md §1).
- Sign-in is device code only; browser sign-in with loopback PKCE
  (entra.md #2) is not built.
- No `-o json|yaml`: output is tables for people.
- `golemctl state restore` over the admin API is not built. A restore
  exists as `golem-control restore` on the control plane's host (a dry
  run unless `-apply`; control-plane.md, History).
- Most of the org model is still to come: sub-teams and projects, the
  services entity, member and service caps, budgets and grants, capture
  settings, unbinding a manager, and rotating or constraining keys.
- A request with fields the API no longer has (a `policy` in
  `CreateScope`, from before ADR 0050) is not refused: Connect's JSON and
  binary codecs discard unknown fields. golemctl sends none.
- Responses carry no etag (control-plane.md §2 step 5) and there are no
  conditional updates by the client.
- `golemctl` loads its settings before it looks up the command, so an
  unknown command when signed out reports "not signed in" (exit 1) rather
  than the usage (exit 2).
- `golemctl members add` defaults `--kind` to `user`, so adding an
  existing service without `--kind service` is refused as "exists as a
  service with another identity".
