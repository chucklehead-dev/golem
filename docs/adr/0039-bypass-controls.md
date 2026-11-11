# 0039: Traffic that bypasses golem is blocked where the enterprise can block it

Status: Accepted.

## Context

Every budget, capture, residency and audit guarantee assumes model traffic
goes through golem. Users and applications can otherwise call providers
directly, use personal subscriptions in their agents, or invoke Bedrock
with other roles.

## Decision

- **AWS**: a service control policy denies `bedrock:InvokeModel*` (and
  `Converse*`) in every account except to golem's gateway roles.
- **Network**: the mesh's egress policy and the enterprise proxy block
  provider API hosts (api.openai.com, api.anthropic.com, Foundry and other
  provider endpoints) for everything except golem's egress.
- **Provider organisations**: key creation in the OpenAI and Anthropic
  organisations is limited to the platform team; existing keys outside
  golem are revoked as teams onboard (roadmap M2).
- **Endpoints and agents**: managed settings for Claude Code, Codex and
  IDEs point them at golem (`golemctl agent setup` for individuals; MDM or
  managed settings where the enterprise has them).
- Bypass attempts are visible: blocked egress and denied Bedrock calls are
  logged and dashboarded.

## Consequences

Cheap controls that make golem's guarantees true in practice; they are
rolled out with onboarding, team by team, starting in observe mode where
the control supports it.
