package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// ArchiveRepository implements port.ArchiveRepository on the
// audit_reconciler (BYPASSRLS) pool (LLD §8.5, §15.4).
type ArchiveRepository struct {
	pool *pgcommon.Pool
}

var _ port.ArchiveRepository = (*ArchiveRepository)(nil)

// NewArchiveRepository returns a repository over the reconciler pool.
func NewArchiveRepository(pool *pgcommon.Pool) *ArchiveRepository {
	return &ArchiveRepository{pool: pool}
}

// partitionRange validates a monthly partition name and returns its
// [from, to) bounds. Partition rows are read through the parent with this
// occurred_at range (pruned to exactly that partition): audit_reconciler's
// grants are on audit_events, and PostgreSQL does not extend them to direct
// reads of a partition (42501).
func partitionRange(p string) (from, to time.Time, err error) {
	m, ok := domain.ParsePartitionName(p)
	if !ok {
		return time.Time{}, time.Time{}, fmt.Errorf("not a monthly partition name: %q", p)
	}
	return m, m.AddDate(0, 1, 0), nil
}

const partitionsSQL = `SELECT c.relname FROM pg_inherits i
JOIN pg_class c ON c.oid = i.inhrelid
WHERE i.inhparent = 'audit_events'::regclass AND c.relname ~ '^audit_events_[0-9]{4}_[0-9]{2}$'
ORDER BY c.relname`

// Partitions implements port.ArchiveRepository.
func (r *ArchiveRepository) Partitions(ctx context.Context) ([]string, error) {
	return r.strings(ctx, partitionsSQL)
}

// reopenCandidatesSQL: months with rows in DEFAULT whose partition was
// dropped after archival (D-19). Other DEFAULT rows (a bad clock) stay
// for the operator (RB-3).
const reopenCandidatesSQL = `SELECT DISTINCT 'audit_events_' || to_char(e.occurred_at AT TIME ZONE 'UTC', 'YYYY_MM') AS p
FROM audit_events e
WHERE e.tableoid = 'audit_events_default'::regclass
  AND EXISTS (SELECT 1 FROM audit_event_archive_state s
               WHERE s.partition_name = 'audit_events_' || to_char(e.occurred_at AT TIME ZONE 'UTC', 'YYYY_MM')
                 AND s.status = 'dropped')
ORDER BY p`

// ReopenCandidates implements port.ArchiveRepository.
func (r *ArchiveRepository) ReopenCandidates(ctx context.Context) ([]string, error) {
	return r.strings(ctx, reopenCandidatesSQL)
}

// Reopen implements port.ArchiveRepository via audit_reopen_partition().
func (r *ArchiveRepository) Reopen(ctx context.Context, partition string) (int64, error) {
	var n int64
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT audit_reopen_partition($1)`, partition).Scan(&n)
	})
	return n, err
}

// States implements port.ArchiveRepository.
func (r *ArchiveRepository) States(ctx context.Context, partition string) (map[domain.RetentionTier]port.ArchiveTierState, error) {
	out := map[domain.RetentionTier]port.ArchiveTierState{}
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT retention_tier::text, status::text, coalesce(sha256_manifest, '')
FROM audit_event_archive_state WHERE partition_name = $1`, partition)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tier string
			var st port.ArchiveTierState
			if err := rows.Scan(&tier, &st.Status, &st.SHA256Manifest); err != nil {
				return err
			}
			out[domain.RetentionTier(tier)] = st
		}
		return rows.Err()
	})
	return out, err
}

// PendingRedaction implements port.ArchiveRepository (AL-INV-12).
func (r *ArchiveRepository) PendingRedaction(ctx context.Context, partition string) (bool, error) {
	from, to, err := partitionRange(partition)
	if err != nil {
		return false, err
	}
	var pending bool
	err = withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_events e JOIN audit_redaction_tasks t
    ON t.tenant_id = e.tenant_id AND t.status = 'pending'
   AND (e.actor_id = t.subject_actor_id OR (e.target_type = 'user' AND e.target_id = t.subject_actor_id::text))
 WHERE e.occurred_at >= $1 AND e.occurred_at < $2 AND e.retention_tier = 'security_3y')`, from, to).Scan(&pending)
	})
	return pending, err
}

const upsertStateSQL = `INSERT INTO audit_event_archive_state (partition_name, retention_tier, period_month, status)
VALUES ($1, $2::audit_retention_tier, $3, $4::audit_archive_status)
ON CONFLICT (partition_name, retention_tier)
DO UPDATE SET status = EXCLUDED.status, error = NULL`

// MarkArchiving implements port.ArchiveRepository.
func (r *ArchiveRepository) MarkArchiving(ctx context.Context, partition string, tier domain.RetentionTier, month time.Time) error {
	return r.exec(ctx, upsertStateSQL, partition, string(tier), month, domain.ArchiveArchiving)
}

// MarkAccessExpiring implements port.ArchiveRepository: access_90d is
// recorded (never archived) and flips to expired when the partition drops.
func (r *ArchiveRepository) MarkAccessExpiring(ctx context.Context, partition string, month time.Time) error {
	from, to, err := partitionRange(partition)
	if err != nil {
		return err
	}
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO audit_event_archive_state (partition_name, retention_tier, period_month, status, row_count)
SELECT $1, 'access_90d', $2, 'pending', count(*) FROM audit_events
 WHERE occurred_at >= $3 AND occurred_at < $4 AND retention_tier = 'access_90d'
ON CONFLICT (partition_name, retention_tier) DO UPDATE SET row_count = EXCLUDED.row_count`, partition, month, from, to)
		return err
	})
}

// NextParts implements port.ArchiveRepository.
func (r *ArchiveRepository) NextParts(ctx context.Context, partition string, tier domain.RetentionTier) (map[string]int, error) {
	out := map[string]int{}
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tenant_id::text, max(part) + 1 FROM audit_archive_objects
WHERE partition_name = $1 AND retention_tier = $2::audit_retention_tier AND sealed GROUP BY tenant_id`, partition, string(tier))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var tenant string
			var next int
			if err := rows.Scan(&tenant, &next); err != nil {
				return err
			}
			out[tenant] = next
		}
		return rows.Err()
	})
	return out, err
}

// StreamTier implements port.ArchiveRepository: one REPEATABLE READ,
// read-only snapshot, so the rows written match a single point in time; a
// row inserted after it is caught by the drop gate's count check (AL-D4).
func (r *ArchiveRepository) StreamTier(ctx context.Context, partition string, tier domain.RetentionTier, fn func(domain.AuditEntry) error) error {
	from, to, err := partitionRange(partition)
	if err != nil {
		return err
	}
	opts := pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly}
	return wrapConnErr(pgcommon.RunInTx(ctx, r.pool, opts, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+entryColumns+` FROM audit_events
WHERE occurred_at >= $1 AND occurred_at < $2 AND retention_tier = $3::audit_retention_tier
ORDER BY tenant_id, occurred_at, id`, from, to, string(tier))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			e, err := scanEntry(rows)
			if err != nil {
				return err
			}
			if err := fn(e); err != nil {
				return err
			}
		}
		return rows.Err()
	}))
}

const upsertObjectSQL = `INSERT INTO audit_archive_objects
    (partition_name, retention_tier, tenant_id, part, period_month, s3_bucket, s3_key, row_count, byte_size,
     min_occurred_at, max_occurred_at, min_id, max_id, sha256, subject_ids, sealed)
VALUES ($1, $2::audit_retention_tier, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15::uuid[], false)
ON CONFLICT (partition_name, retention_tier, tenant_id, part) DO UPDATE SET
    s3_bucket = EXCLUDED.s3_bucket, s3_key = EXCLUDED.s3_key, row_count = EXCLUDED.row_count,
    byte_size = EXCLUDED.byte_size, min_occurred_at = EXCLUDED.min_occurred_at,
    max_occurred_at = EXCLUDED.max_occurred_at, min_id = EXCLUDED.min_id, max_id = EXCLUDED.max_id,
    sha256 = EXCLUDED.sha256, subject_ids = EXCLUDED.subject_ids, created_at = now()
WHERE NOT audit_archive_objects.sealed`

// UpsertObject implements port.ArchiveRepository. A sealed row is never
// overwritten (D-20); NextParts keeps new parts clear of sealed ones.
func (r *ArchiveRepository) UpsertObject(ctx context.Context, partition string, o domain.ArchiveObject) error {
	subjects := o.SubjectIDs
	if subjects == nil {
		subjects = []string{}
	}
	return r.exec(ctx, upsertObjectSQL, partition, string(o.Tier), o.TenantID, o.Part, o.PeriodMonth, o.Bucket, o.Key,
		o.RowCount, o.ByteSize, o.MinOccurredAt, o.MaxOccurredAt, o.MinID, o.MaxID, o.SHA256, subjects)
}

// FinishArchive implements port.ArchiveRepository.
func (r *ArchiveRepository) FinishArchive(ctx context.Context, partition string, tier domain.RetentionTier, keep []string, rows int64, manifestSHA, prefix string) error {
	if keep == nil {
		keep = []string{}
	}
	return NewTxRunner(r.pool).RunInTx(ctx, func(ctx context.Context) error {
		tx, _ := TxFromContext(ctx)
		var bucket *string
		if _, err := tx.Exec(ctx, `DELETE FROM audit_archive_objects
WHERE partition_name = $1 AND retention_tier = $2::audit_retention_tier AND NOT sealed AND NOT (s3_key = ANY($3::text[]))`,
			partition, string(tier), keep); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT min(s3_bucket) FROM audit_archive_objects
WHERE partition_name = $1 AND retention_tier = $2::audit_retention_tier AND NOT sealed`, partition, string(tier)).Scan(&bucket); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE audit_event_archive_state
SET status = 'archived', row_count = $3, object_count = $4, sha256_manifest = $5, s3_prefix = $6, s3_bucket = $7,
    archived_at = now(), error = NULL
WHERE partition_name = $1 AND retention_tier = $2::audit_retention_tier`,
			partition, string(tier), rows, len(keep), manifestSHA, prefix, bucket)
		return err
	})
}

// UnsealedObjects implements port.ArchiveRepository.
func (r *ArchiveRepository) UnsealedObjects(ctx context.Context, partition string, tier domain.RetentionTier) ([]domain.ArchiveObject, error) {
	var out []domain.ArchiveObject
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT s3_key, tenant_id::text, part, row_count, max_occurred_at, sha256
FROM audit_archive_objects
WHERE partition_name = $1 AND retention_tier = $2::audit_retention_tier AND NOT sealed
ORDER BY tenant_id, part`, partition, string(tier))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			o := domain.ArchiveObject{Tier: tier}
			if err := rows.Scan(&o.Key, &o.TenantID, &o.Part, &o.RowCount, &o.MaxOccurredAt, &o.SHA256); err != nil {
				return err
			}
			out = append(out, o)
		}
		return rows.Err()
	})
	return out, err
}

// MarkVerified implements port.ArchiveRepository. Only an archived tier
// may become verified: a redaction that invalidated it meanwhile (000008)
// keeps it pending, and the drop gate then refuses.
func (r *ArchiveRepository) MarkVerified(ctx context.Context, partition string, tier domain.RetentionTier) error {
	var n int64
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE audit_event_archive_state SET status = 'verified', verified_at = now()
WHERE partition_name = $1 AND retention_tier = $2::audit_retention_tier AND status = 'archived'`, partition, string(tier))
		n = tag.RowsAffected()
		return err
	})
	if err == nil && n == 0 {
		return errors.New("tier is no longer archived (invalidated by a concurrent redaction)")
	}
	return err
}

// MarkFailed implements port.ArchiveRepository.
func (r *ArchiveRepository) MarkFailed(ctx context.Context, partition string, tier domain.RetentionTier, reason string) error {
	return r.exec(ctx, `UPDATE audit_event_archive_state SET status = 'failed', error = $3
WHERE partition_name = $1 AND retention_tier = $2::audit_retention_tier`, partition, string(tier), reason)
}

// ResetForRearchive implements port.ArchiveRepository.
func (r *ArchiveRepository) ResetForRearchive(ctx context.Context, partition string) error {
	return r.exec(ctx, `UPDATE audit_event_archive_state SET status = 'pending', error = NULL
WHERE partition_name = $1 AND retention_tier IN ('security_3y', 'compliance_7y')`, partition)
}

// Drop implements port.ArchiveRepository via audit_drop_partition().
func (r *ArchiveRepository) Drop(ctx context.Context, partition string) (string, error) {
	var res string
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT audit_drop_partition($1)`, partition).Scan(&res)
	})
	return res, err
}

func (r *ArchiveRepository) exec(ctx context.Context, sql string, args ...any) error {
	return withPool(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, sql, args...)
		return err
	})
}

func (r *ArchiveRepository) strings(ctx context.Context, sql string) ([]string, error) {
	var out []string
	err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			out = append(out, s)
		}
		return rows.Err()
	})
	return out, err
}

// PruneProcessedEvents deletes processed_events ledger rows older than
// ttlDays in bounded batches (LLD §4.2: 8 d > the 7-day SQS lifetime).
// Beyond the ledger, uq_audit_events_source_id still dedups (AL-INV-4).
func (r *ArchiveRepository) PruneProcessedEvents(ctx context.Context, ttlDays, batch int) (int64, error) {
	var total int64
	for {
		var n int64
		err := withPool(ctx, r.pool, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `DELETE FROM processed_events WHERE ctid IN (
    SELECT ctid FROM processed_events WHERE processed_at < now() - make_interval(days => $1) LIMIT $2)`, ttlDays, batch)
			n = tag.RowsAffected()
			return err
		})
		if err != nil {
			return total, err
		}
		total += n
		if n < int64(batch) || ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
}
