# golem: Entra ID app registrations

Status: draft. What golem needs in Entra, and why. Workload identity
federation (a federated credential trusting the cluster's service account
issuer) is used everywhere a component needs its own token, so no client
secrets are stored. On EKS the issuer is the cluster's OIDC provider URL;
on AKS, the AKS OIDC issuer.

| # | Registration | Type | Used by | Configuration |
|---|---|---|---|---|
| 1 | **golem API** | resource (web API) | the gateway validates tokens with this audience, and only the gateway | Application ID URI `api://golem`; delegated scope `golem.access` (users call models); app role `golem.service` (application, for services calling with client credentials or federation); optional claim `idtyp` on access tokens; `groups` claim optional (golem reads membership from its own sync) |
| 1b | **golem control API** | resource (web API) | golem-control validates tokens with this audience (ADR 0046) | Application ID URI `api://golem-control`; delegated scope `golem.manage` (managing scopes, members and keys); app roles `golem.admin` (users and services), `golem.service` (services automating the control API); optional claim `idtyp` on access tokens |
| 2 | **golemctl** | public client (native) | CLI and coding-agent setup: device code and loopback-PKCE sign-in | Redirect `http://localhost`; allow public client flows; delegated permissions to #1 `golem.access` and #1b `golem.manage` (admin consented); one sign-in redeems a token for each |
| 3 | **golem-console** | web (confidential) | console sign-in, then calls the control API as the user | Redirect to the console URL; federated credential from the console's service account; delegated permission to #1b `golem.manage` |
| 4 | **golem-control (jobs)** | daemon | Entra group sync (ADR 0047: `-entra-sync-client-id`); email notifications | Application permissions (admin consent): `GroupMember.Read.All`, `User.ReadBasic.All`; `Mail.Send` limited to one shared mailbox (Exchange RBAC for Applications); federated credential from the worker role's service account; app role `golem.service` on #1 |
| 6 | **golem-gateway (upstream)** (M2) | daemon | calls Foundry (Azure OpenAI, Claude) as itself | Federated credential from the gateway's service account; RBAC `Cognitive Services User` (or `Cognitive Services OpenAI User`) on each Foundry resource; token scope per Microsoft's docs (`https://ai.azure.com/.default` for Foundry, to confirm per endpoint) |
| 7 | **golem → AWS** (AKS/on-prem only, M2) | daemon | Bedrock and the shared S3 bucket (ADR 0008) without EKS Pod Identity | AWS IAM OIDC identity provider trusting Entra; an IAM role assumable by #6's tokens (`AssumeRoleWithWebIdentity`). Not needed on EKS |

Optional, M2: delegated `user_impersonation` on Azure Cognitive Services
for #1, if routes use on-behalf-of pass-through so Foundry sees the user.

Services that call golem with their own Entra identity need nothing from
golem's registrations beyond an assignment of app role `golem.service` on
#1 (which, with `idtyp: app`, is how golem knows a token's `azp` is a
service), plus the service's registration in golem (org-model §1). A
service that also automates the control API is assigned `golem.service`
(or `golem.admin`) on #1b; a gateway token never reaches the control
API.

Owners: the platform team owns all of them and grants admin consent for
#2–#4 (decided).
