# Database schema

Database `audit` on the shared RDS PostgreSQL. Migrations live in `internal/adapter/outbound/postgres/migrations/` and are run by `cmd/server` at startup via `platform-pgcommon/pkg/migrate` (table `pgcommon_migrations`), using the direct `MIGRATION_DATABASE_URL`, not PgBouncer.

## Migrations

| # | Adds |
|---|---|
| 000001 | Enums; `audit_events`, partitioned monthly on `occurred_at`, plus `audit_events_default`; `audit_event_archive_state`; `processed_events`; `tenant_plan_window`; `audit_export_jobs`; `audit_redaction_tasks`; indexes |
| 000002 | RLS: `app_tenant_id()`, the fail-closed `rls_check_tenant()`, `log_rls_violation()`, `rls_violation_log`; `tenant_isolation` policies |
| 000003 | Roles `audit_app`, `audit_reconciler`, `audit_migrator`, `admin_readonly` (created NOLOGIN if missing) and the grants |
| 000004 | `forbid_audit_mutation` triggers (UPDATE/DELETE only by `audit_reconciler`); `touch_row` on export jobs |
| 000005 | `audit_ensure_partitions(ahead, trailing)` definer, 0..24 each side; bootstraps the partitions |
| 000006 | `audit_archive_objects` (per-object manifest, RLS); the `claim_export_job(lease)` definer |
| 000007 | `subject_ids` (GIN), `redacted_subjects` (RLS), `redaction_marker()`, `apply_redaction(task)` (reconciler-owned definer), `sweep_redactions(window)`, column UPDATE `(metadata, actor_display)` for the reconciler |
| 000008 | `audit_archive_objects.sealed`; `invalidate_security_archive()`; `apply_redaction` / `sweep` made seal-aware and invalidating; the `audit_drop_partition(name)` and `audit_reopen_partition(name)` definers |
| 000009 | The `audit_ops_stats(hot_days, trailing, grace, pending_age)` definer, returning aggregates only |

## Tables

| Table | RLS | Notes |
|---|---|---|
| `audit_events` (+ `audit_events_YYYY_MM`, `audit_events_default`) | FORCE, `tenant_isolation` | Append-only. Unique `(source_event_id, occurred_at)`. Indexes on tenant+time/type/actor/target/tier. |
| `processed_events` | exempt | Dedup ledger `(event_id, consumer)`, pruned after 8 days |
| `tenant_plan_window` | exempt (read by own tenant_id) | `plan_code`, `query_window_days` (fallback), `last_event_at` recency guard |
| `audit_export_jobs` | FORCE | Status enum, lease via `updated_at` |
| `audit_redaction_tasks` | exempt (reconciler-only read) | Unique `trigger_source_event_id` |
| `redacted_subjects` | FORCE | `(tenant_id, subject_id)` → `task_id`, `redaction_completed_at` (D-18 ingest check) |
| `audit_event_archive_state` | exempt (reconciler-only) | Per (partition, tier): `pending → archiving → archived → verified → dropped`; access tier `expired` |
| `audit_archive_objects` | FORCE | Per S3 object: tier/tenant/month/part, counts, time and id ranges, `sha256`, `subject_ids`, `sealed` |
| `rls_violation_log` | — | Sampled RLS violations → `iam_rls_violations_total` |

## Grants (summary)

| Role | Grants |
|---|---|
| `audit_app` | `audit_events` SELECT, INSERT only; `processed_events` SELECT, INSERT; `tenant_plan_window` S/I/U; `audit_export_jobs` S/I/U; `audit_redaction_tasks` INSERT only; `audit_archive_objects` SELECT; `redacted_subjects` SELECT. EXECUTE on `audit_ensure_partitions`, `claim_export_job`, `apply_redaction`, `audit_ops_stats` and the RLS helpers. |
| `audit_reconciler` | `audit_events` SELECT, DELETE plus column UPDATE `(metadata, actor_display)`; `processed_events` SELECT, DELETE; archive state S/I/U; manifest S/I/U/D; redaction tasks S/U; `redacted_subjects` S/I/U. EXECUTE on `audit_ensure_partitions`, `sweep_redactions`, `audit_drop_partition`, `audit_reopen_partition`, `invalidate_security_archive`. Owns `apply_redaction` (D-1). |
| `admin_readonly` | SELECT on the operational tables |

**Deployment prerequisite (gap 34):** Terraform must make `audit_migrator` a member of `audit_reconciler`, or `000007`'s owner change fails. See `docs/implementation/RELEASE_CHECKLIST.md`.

## Gotchas

- **Unique violations** name the partition's index, not `uq_audit_events_source_id`. Map them on SQLSTATE 23505 (gap 27).
- **INSERT-only tables:** `audit_app` can't use `ON CONFLICT (col)` or `RETURNING` on `audit_redaction_tasks`, because both need SELECT. Use a target-less `ON CONFLICT DO NOTHING`.
- **Partition access:** the reconciler reads partitions through the parent, bounded to the month. A grant on the parent doesn't cover naming a partition directly (gap 39).
- **Routing on `sealed`:** archived reads, AL-2 and redaction's `missed` check use `sealed`, not partition existence (D-20 refines D-12).
