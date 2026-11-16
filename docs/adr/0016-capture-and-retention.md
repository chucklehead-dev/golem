# 0016: Content capture per scope, with override control, per-record retention, a Langfuse project per scope

Amended where specified by [ADR 0029](0029-security-and-correctness-revision.md)
and its normative security and correctness requirements. Baseline text below
must be read with those amendments.

Status: Accepted (v1 baseline). Amended by [0035](0035-capture-projects-and-the-collector.md) (projects per top-level team, retention per project, collector as broker). **Applies from M2**: nothing of this record is built or planned for M1 (m1-plan.md).

## Decision

- Settings at three levels: the `ScopePolicy` ceiling (capture allowed or
  not, maximum retention, required redaction hooks); the team or project
  (`capture`: off, metadata, content; `retention`: 7, 30, 90, 365 days or 3,
  5, 7 years, the multi-year classes only where the policy allows;
  `override`: locked, narrow, open); keys and services (inherit, or an
  override the scope permits). Users set their own keys; managers set
  scopes and services. Switches may be time-boxed; every change is audited.
- Each capturing scope gets its **own Langfuse project** (provisioned by the
  operator with an organisation key; per-project credentials in a Secret
  mounted into gateways), so Langfuse access matches the scope. Gateways
  export captured traces to it directly (OTLP/HTTP), after required
  redaction; export failure drops the capture, never the request.
- **Retention per record**: each trace carries its expiry; a job deletes
  expired traces through Langfuse's batch deletion; the project's own
  retention is the scope's maximum, as a backstop.
- Coding-agent keys and tokens default to `metadata` (a heavy agent sends
  50–200k tokens of context per request); the console shows a setting's
  projected storage before it is saved.

## To verify

Langfuse's organisation API (project creation, retention), batch trace
deletion, per-project OTLP basic auth, against the version in use.

## Alternatives

A Langfuse project per retention class (shared content across teams);
content in ClickStack (broader access than content warrants).
