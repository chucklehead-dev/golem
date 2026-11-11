# golem: Entra ID app registrations

Status: draft. What golem needs in Entra, and why. Workload identity
federation (a federated credential trusting the cluster's service account
issuer) is used everywhere a component needs its own token, so no client
secrets are stored. On EKS the issuer is the cluster's OIDC provider URL;
on AKS, the AKS OIDC issuer.

| # | Registration | Type | Used by | Configuration |
|---|---|---|---|---|
| 1 | **golem API** | resource (web API) | gateway and golem-control validate tokens with this audience | Application ID URI `api://golem`; delegated scope `golem.access` (users call golem); app roles `golem.admin`, `golem.config-writer`, `golem.service` (application, for services calling with client credentials or federation); `groups` claim optional (golem reads membership from its own sync) |
| 2 | **golemctl** | public client (native) | CLI and coding-agent setup: device code and loopback-PKCE sign-in | Redirect `http://localhost`; allow public client flows; delegated permission to #1 `golem.access` (admin consented) |
| 3 | **golem-console** | web (confidential) | console sign-in, then calls the control API as the user | Redirect to the console URL; federated credential from the console's service account; delegated permission to #1 |
| 4 | **golem-worker** | daemon | Entra group sync; email notifications | Application permissions (admin consent): `GroupMember.Read.All`, `User.ReadBasic.All`; `Mail.Send` limited to one shared mailbox (Exchange RBAC for Applications); federated credential from the worker's service account; app role `golem.service` on #1 |
| 5 | **golem-operator** | daemon | calls the config API | Federated credential from the operator's service account; app role `golem.config-writer` on #1 |
| 6 | **golem-gateway (upstream)** | daemon | calls Foundry (Azure OpenAI, Claude) as itself | Federated credential from the gateway's service account; RBAC `Cognitive Services User` (or `Cognitive Services OpenAI User`) on each Foundry resource; token scope per Microsoft's docs (`https://ai.azure.com/.default` for Foundry, to confirm per endpoint) |
| 7 | **golem → AWS** (AKS/on-prem only, M2) | daemon | Bedrock without EKS Pod Identity | AWS IAM OIDC identity provider trusting Entra; an IAM role assumable by #6's tokens (`AssumeRoleWithWebIdentity`). Not needed on EKS |

Optional, M2: delegated `user_impersonation` on Azure Cognitive Services
for #1, if routes use on-behalf-of pass-through so Foundry sees the user.

Services that call golem with their own Entra identity need nothing from
golem's registrations beyond an assignment of app role `golem.service` on
#1 (which is how golem knows a token's `azp` is a service), plus the
service's registration in golem (org-model §1).

Owners: the platform team owns all seven (decided); an Entra
administrator grants admin consent for #2–#4.
