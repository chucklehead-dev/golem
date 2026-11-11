# 0033: The bucket is the root of trust: signed snapshots, one publisher, locked writes, offsite backups

Status: Accepted. Amends 0028, 0029.

## Context

Since ADR 0028 the bucket holds the whole entitlement state and every
gateway's snapshot. A gateway trusts whatever its pool's pointer names;
the content hash only proves an object matches its name, and SSE-KMS gives
confidentiality, not integrity. Anyone who could write the pointer and one
object could add a key. The rollback guard compared `state_version` and a
CRD generation, which is not one global counter; a restore copied an old
`state_version` back; a gateway with an empty disk had no floor; and the
history lived in the same bucket and account.

## Decision

- **Signed snapshots.** The leader (ADR 0032) is the only publisher. It
  signs each snapshot and each pointer with an asymmetric KMS key (ECC P-256,
  `Sign` permission only for golem-control's role). Gateways verify against
  the public key, pinned in their deployment config; an unsigned or wrongly
  signed object is ignored and alarmed.
- **One publish sequence.** The pointer carries a `publish_seq`, written by
  CAS on the pointer itself (`If-Match`), incremented by one on every
  publication, and an `issued_at`. Gateways accept a pointer only if its
  sequence is greater than the floor, where the floor is the larger of
  their local last-good copy and their own status object in the bucket, so
  an empty disk does not reset it. The leader re-signs the pointer at least
  hourly; a gateway alarms on a pointer older than a day (a replay of an old
  signed pointer cannot pass the floor, and a stuck publisher is noticed).
- **Restore is a new write.** `golemctl state restore --version v` writes
  the old content as the next `state_version`, then re-applies every key
  revocation and every membership removal made since `v` (they are kept in
  an append-only list in the document), so a restore cannot bring back a
  revoked key or a removed member. Audited, admin only.
- **Locked writes.** Bucket policy denies `PutObject` on the state
  document and `snapshots/` to every principal except golem-control's role,
  and on `status/<pool>/` to anything but that pool's gateway role; a
  break-glass role (MFA, two-person approval) exists, is audited, and is
  never used by tooling. CloudTrail data events on the bucket feed an alert
  on any write by another principal. Block Public Access, a VPC endpoint
  policy, TLS only.
- **Offsite backup.** Replication of the state document and snapshots to a
  bucket in a separate backup account with Object Lock in compliance mode
  (90 days), so neither a compromised account nor an operator mistake can
  remove history.
- **Edge pools get external entitlements only.** A pool's snapshot holds
  only the principals and keys its audience allows: internal pools,
  internal principals; the edge pool, its external consumers' service keys.
- **KMS reachability** is part of a site's prerequisites (gateways need
  `kms:Decrypt` for SSE-KMS objects; S3 Bucket Keys are enabled to keep KMS
  calls low).

## Consequences

- Write access to the bucket no longer implies the ability to admit a key:
  the attacker also needs the signing key, which never leaves KMS.
- Publication is serialized through one leader, which at this scale costs
  nothing.

## Alternatives

- Trust the bucket policy alone: one misconfigured role from key
  injection.
- Sign with a key held in a Kubernetes Secret: portable, but extractable.
