# 0021: GitHub Copilot through golem where Copilot allows it, and its own usage alongside

Status: Proposed

## Context

Copilot Enterprise can use non-GitHub models in two ways (GitHub docs,
public preview at the time of writing):

- **Enterprise custom models**: enterprise (or, if allowed, organisation)
  owners add provider keys; listed providers include Anthropic, Bedrock,
  Microsoft Foundry, OpenAI and "OpenAI-compatible providers". Users see
  the models in Copilot Chat, Copilot CLI and IDEs. Requests go through
  GitHub's Copilot API, so the endpoint is called from GitHub's side.
- **User-configured BYOK**: users add a provider and key in VS Code,
  JetBrains, Xcode, Copilot CLI, the Copilot app or the SDK; requests go
  from the user's client directly to the provider. BYOK does not apply to
  code completions.

The docs do not say whether a per-user identity reaches the endpoint, and
GitHub-hosted model usage cannot be routed through anything but GitHub.
The usage metrics API reports per-user activity and `ai_credits_used`
(not broken down by model; for analysis, not invoicing).

## Decision

1. **BYOK through golem for individuals (M1).** golem is the
   "OpenAI-compatible" provider; each user's key is their personal golem
   key, so spend is attributed per user and scope like any other request.
   `golemctl agent setup copilot` writes the client settings. Inside the
   network, no internet exposure needed.
2. **Enterprise custom models through golem (M2, after a spike).** golem
   registered as an OpenAI-compatible (or Anthropic) provider with one
   service key per Copilot organisation, each bound to the scope that pays
   for it. Needs an internet-facing route to the gateway through the mesh
   ingress, restricted to GitHub's published egress ranges and the
   service keys. The spike establishes which API shapes Copilot sends,
   whether any header identifies the user (if so, golem attributes per
   user under the service key; if not, per organisation only), and the
   ranges to allow.
3. **Copilot's own usage as showback (M2).** golem-worker ingests the
   Copilot usage metrics API daily into ClickHouse, mapping GitHub logins
   to Entra users (Enterprise Managed Users makes this direct), so Grafana
   and golem's spend views show Copilot credits beside golem spend. Not
   enforced by golem; Copilot's own budgets and policies remain the
   control for GitHub-hosted models.

## Consequences

- Copilot users who want golem's models, budgets and capture use BYOK;
  completions stay on GitHub's models.
- Enterprise custom models trade per-user attribution (unknown until the
  spike) for zero user setup.

## Alternatives

- Intercept Copilot traffic with a forward proxy: breaks TLS to GitHub,
  unsupported, and fragile; rejected.
- Ignore Copilot: leaves the largest coding-agent spend invisible.
