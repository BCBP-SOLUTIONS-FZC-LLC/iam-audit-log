# Request flows

The sequence diagrams live in LLD §8. This file summarizes the flows as implemented, with the code entry points.

## Ingest: one write path (AL-INV-2)

1. **Bus:** `consumer.Handler` → `toBusEvent` (`ID: env.ID`, the dedup key) → `IngestService.IngestBus`. It runs `domain.BuildBusEntry` (taxonomy, actor/target D-8, truncation D-9), then `appendBus`.
2. **Direct-write:** the AL-5/AL-6 handler runs `IngestService.DirectWrite` → `domain.BuildDirectWriteEntry` (validation; the tier is derived and a caller-supplied tier ignored).
3. `AuditRepository.Append` runs one pgcommon transaction bound to the entry's tenant GUC:
   1. ledger insert into `processed_events`;
   2. **D-18 check:** if the entry is `security_3y` and its actor or user target appears in `redacted_subjects`, it is stored already redacted (marker plus `_redacted_on_ingest`, `actor_display` cleared);
   3. `INSERT … ON CONFLICT DO NOTHING`;
   4. replay lookup. A key already used by another tenant gives 400.
4. **Redaction trigger:** for `user.deleted`, `AppendWithRedaction` also inserts an `audit_redaction_tasks` row in the same transaction. Once committed, `apply_redaction(task)` runs immediately. If that fails, the task stays `pending` and the message still succeeds.
5. **Plan projection:** tenant plan events also update `tenant_plan_window` (recency-guarded). The window comes from the CAT-I2 map when it is known.

## Query (AL-1, AL-7)

`QueryService.Query`:
1. Validate the filter, limit and cursor.
2. Resolve the window (D-13: live map → the row's stored value → the default).
3. Clamp it; a range entirely older than the window returns an empty 200.
4. Load the sealed manifest objects in range. If their estimate is over the bounds, defer to an export (202). AL-7 has no clamp and returns 422 instead (D-14).
5. Take a hot keyset page from RDS plus archived rows from those objects, merged under one cursor.

Sealed objects are the only S3 source, and their rows no longer exist in RDS, so no row is counted twice.

## Export worker (AL-3/AL-4, D-2)

`ExportService.Run`:
1. `claim_export_job(lease)` returns the oldest pending job across tenants, or one whose lease has expired.
2. The worker heartbeats every lease/3.
3. It streams hot keyset pages plus archived objects into a gzipped JSONL temp file.
4. It uploads to `exports/{tenant}/{id}.jsonl.gz` with SSE-KMS and sets the job to `ready` with `signed_url_expires_at` = now + 7 d.
5. Each AL-4 poll presigns a fresh short-lived URL. A failure stores only the §17 code.

## Archival: `reconcile` job (`ArchiveService.Run`)

1. **Pre-create partitions** (`audit_ensure_partitions`).
2. **Re-open (D-19):** a DEFAULT row belonging to a month whose state is `dropped` → `audit_reopen_partition()`. It swaps DEFAULT out, recreates the month and re-routes the rows through the parent (no row UPDATE/DELETE), then resets the tiers to pending. DEFAULT rows for months that were never dropped are left for manual RB-3.
3. **Eligible months** (`domain.ArchiveEligible`: past the hot window and before the trailing writable months):
   - Skip the month if a pending redaction touches it (AL-INV-12).
   - For each retained tier not yet verified:
     - write per-tenant `jsonl.gz` parts of up to `ARCHIVE_PART_MAX_ROWS`, SSE-KMS, Object Lock retained until max_occurred_at plus the tier's duration;
     - record a manifest row per part (checksum, `subject_ids`, id ranges);
     - verify by re-reading each object (body SHA-256, manifest checksum, and the lock echo in AWS).
   - Record the `access_90d` tier.
4. **`audit_drop_partition()`** runs under an ACCESS EXCLUSIVE lock and requires:
   - both retained tiers `verified`;
   - live counts equal to the unsealed manifest (otherwise `count_mismatch` → reset → re-archive once);
   - no pending redaction.

   It then seals the manifest, marks the tiers `dropped`/`expired`, and DETACHes and DROPs the partition.
5. **Re-archive (D-20)** rewrites only unsealed parts, under the same keys, as new object versions. A re-opened month appends parts after its sealed ones.

## Redaction (AL-INV-12, D-15..D-18)

- **Immediate:** `apply_redaction(task)`, a reconciler-owned definer. On `security_3y` rows where the subject is the actor or the user target, it replaces `metadata` with the marker and clears the subject's own `actor_display`. It then:
  - invalidates the `security_3y` archive of the partitions it rewrote;
  - sets the status `applied` / `not_applicable` / `missed` (missed = the subject appears in a sealed object; routine under AL-Q15 Option A);
  - upserts `redacted_subjects`.
- **Retry:** the `redaction-retry` job re-applies tasks pending longer than `REDACTION_RETRY_MIN_AGE`.
- **Sweep:** the `redaction-sweep` job runs `sweep_redactions(REDACTION_SWEEP_WINDOW)`, which re-redacts rows that slipped past the ingest check. A non-zero count is logged at warn.
