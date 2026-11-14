# 0046: The control API has its own Entra registration

Status: Accepted. Amends [0009](0009-identity-and-authentication.md) and
entra.md (registration #1 split into #1 and #1b).

## Context

The gateway and the control API took tokens for one registration, golem
API (`api://golem`, scope `golem.access`). A token a coding agent holds
to call models (`golemctl token`, copied into an agent's credential
helper, its environment, a CI secret) was then also a token for the
control API: if it leaked, it could mint keys, change memberships and,
with `golem.admin` on the user, do anything an admin can. The two uses
have different holders, lifetimes and blast radii; one audience cannot
tell them apart.

## Decision

- **Two resource registrations.** golem API (#1, `api://golem`, scope
  `golem.access`, app role `golem.service`) is the gateway's only. The
  control API is #1b, `api://golem-control`, with delegated scope
  `golem.manage` and app roles `golem.admin` and `golem.service`. The
  gateway accepts only #1's audience, golem-control only #1b's
  (`-entra-audience`, default `api://golem-control`), each with its own
  scope (`auth.Entra.Scope`); a token for one is invalid at the other.
- **`golem.admin` is #1b's role.** An admin is an admin of the control
  API; the gateway gives the role no meaning.
- **golemctl signs in once.** Its device-code sign-in asks for
  `golem.manage` and `offline_access`; the refresh token redeems an access
  token for each resource (Entra's refresh tokens are multi-resource for
  a public client consented to both). Each access token is cached for its
  resource: the commands use the control API's, `golemctl token` prints
  the gateway's.
- **Services are recognised by `idtyp`.** An app-only token must carry
  `idtyp: app` (an optional claim both registrations request); one that
  does not, or a delegated token that claims it, is refused. The absence
  of a scope alone no longer marks a service.

## Consequences

- An agent's token is good for models only. Calling the control API takes
  a sign-in with `golem.manage`, consented for golemctl and the console
  only.
- One more registration to own and consent (the platform team, as for the
  others); services that automate the control API are assigned
  `golem.service` on #1b, separately from #1.
- Entra may still issue a token with both scopes to a client consented to
  both; each service checks only its own audience, which a token has one
  of.

## Alternatives

- **One registration, a second scope.** Smaller, but the audience is the
  same: a mistake in either service's scope check, or a library default
  that checks only the audience, would accept the other's tokens.
- **Proof of possession (DPoP, mTLS-bound tokens).** Stops a leaked token
  being replayed at all; Entra supports it for few flows, and agents'
  credential helpers do not. Revisit with them.
