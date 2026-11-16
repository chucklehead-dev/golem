# 0020: GitHub Copilot: BYOK through golem, enterprise models through the edge, usage as showback

Status: Accepted (v1 baseline). **Applies from M2**: nothing of this record is built or planned for M1 (m1-plan.md).

## Decision

1. **BYOK for individuals (M1)**: Copilot clients (VS Code, JetBrains,
   Copilot CLI, the Copilot app) use golem as an "OpenAI-compatible"
   provider with the user's personal key (`golemctl agent setup copilot`);
   budgets, capture and per-user attribution apply. Code completions stay
   on GitHub's models.
2. **Enterprise custom models (M2, after a spike)**: golem registered as a
   provider in the enterprise's settings with one service key per Copilot
   organisation, served by the edge pool (ADR 0018). The spike settles the
   request shapes and whether any header identifies the user.
3. **Copilot's own usage as showback (M2)**: a daily job ingests GitHub's
   usage metrics into ClickHouse, mapping logins to Entra users (through
   SAML identities until the move to Enterprise Managed Users, directly
   after). Not enforced by golem.

## Alternatives

Intercepting Copilot traffic with a forward proxy (unsupported, fragile).
