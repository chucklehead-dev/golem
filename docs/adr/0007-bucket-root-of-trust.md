# 0007: The bucket is the root of trust

Amended where specified by [ADR 0029](0029-security-and-correctness-revision.md)
and its normative security and correctness requirements. Baseline text below
must be read with those amendments.

Status: Accepted (v1 baseline). From archive 0033.

## Decision

- **Signed snapshots and pointers**, by the leader only, with an asymmetric
  KMS key (ECC P-256, `Sign` granted only to golem-control's role).
  Gateways verify against the public key pinned in their deployment
  config, and ignore and alarm on anything unsigned or wrongly signed.
- **One publish sequence**: the pointer carries `publish_seq` (incremented
  by CAS on the pointer) and `issued_at`. A gateway accepts a pointer only
  above its floor, the larger of its local last-good copy and its own
  status object, so an empty disk does not reset it. The leader re-signs
  hourly; a gateway alarms on a pointer older than a day.
- **Restore is a new write**: old content written as the next
  `state_version`, with every revocation and membership removal made since
  re-applied; admin only, audited.
- **Locked writes**: bucket policy allows writes to the state document and
  `snapshots/` only by golem-control's role, and to `status/<pool>/` only
  by that pool's gateways; a break-glass role (MFA, two-person) is never
  used by tooling. CloudTrail data events alert on any other writer. Block
  Public Access, VPC endpoints, TLS only.
- **Offsite backup**: replication to a separate backup account under
  Object Lock in compliance mode (90 days).
- **Audiences**: an edge pool's snapshot holds only its external
  consumers' keys; internal pools hold internal principals.
- **Emergency denylist**: a ConfigMap per site, mounted into its gateways
  and applied over every snapshot, for revocation while the control plane
  or the bucket is unreachable.

## Consequences

Bucket write access alone cannot admit a key; history survives a
compromised account; revocation has a path that needs neither the control
plane nor S3.

## Alternatives

Trusting the bucket policy alone; signing with an extractable key in a
Secret.
