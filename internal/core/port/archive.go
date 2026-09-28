package port

import (
	"context"
	"io"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// ArchiveTierState is one audit_event_archive_state row's progress.
type ArchiveTierState struct {
	Status         string
	SHA256Manifest string
}

// ArchiveRepository is the reconciler's Postgres side of archival (LLD §8.5,
// §15.4), on the audit_reconciler (BYPASSRLS) pool. Partition DDL goes
// through the audit_drop_partition() / audit_reopen_partition() definer
// functions (migration 000008).
type ArchiveRepository interface {
	// Partitions lists the attached monthly partitions (never the DEFAULT).
	Partitions(ctx context.Context) ([]string, error)
	// ReopenCandidates lists dropped months that now have rows in
	// audit_events_default (late arrivals, D-19).
	ReopenCandidates(ctx context.Context) ([]string, error)
	// Reopen runs audit_reopen_partition() and returns the rows moved in.
	Reopen(ctx context.Context, partition string) (int64, error)
	// States returns the partition's per-tier archive states (absent = pending).
	States(ctx context.Context, partition string) (map[domain.RetentionTier]ArchiveTierState, error)
	// PendingRedaction reports whether a pending redaction task touches the
	// partition's security_3y rows (AL-INV-12).
	PendingRedaction(ctx context.Context, partition string) (bool, error)
	// MarkArchiving upserts the tier's state as archiving.
	MarkArchiving(ctx context.Context, partition string, tier domain.RetentionTier, month time.Time) error
	// NextParts returns, per tenant, the first part number free after the
	// tier's sealed objects (a re-opened month appends, D-20).
	NextParts(ctx context.Context, partition string, tier domain.RetentionTier) (map[string]int, error)
	// StreamTier streams the tier's rows from one consistent snapshot,
	// ordered by tenant_id, occurred_at, id.
	StreamTier(ctx context.Context, partition string, tier domain.RetentionTier, fn func(domain.AuditEntry) error) error
	// UpsertObject records one uploaded (unsealed) object.
	UpsertObject(ctx context.Context, partition string, o domain.ArchiveObject) error
	// FinishArchive deletes the tier's unsealed manifest rows not in keep
	// (stale parts of an earlier attempt) and marks the tier archived.
	FinishArchive(ctx context.Context, partition string, tier domain.RetentionTier, keep []string, rows int64, manifestSHA, prefix string) error
	// UnsealedObjects lists the tier's current (unsealed) objects.
	UnsealedObjects(ctx context.Context, partition string, tier domain.RetentionTier) ([]domain.ArchiveObject, error)
	MarkVerified(ctx context.Context, partition string, tier domain.RetentionTier) error
	MarkFailed(ctx context.Context, partition string, tier domain.RetentionTier, reason string) error
	// MarkAccessExpiring records the access_90d tier (never archived; it
	// expires with the partition drop).
	MarkAccessExpiring(ctx context.Context, partition string, month time.Time) error
	// ResetForRearchive sets the retained tiers back to pending (a late
	// arrival since archival, AL-D4).
	ResetForRearchive(ctx context.Context, partition string) error
	// Drop runs audit_drop_partition() — the AL-INV-9 gate — and returns a
	// domain.Drop* result.
	Drop(ctx context.Context, partition string) (string, error)
}

// ArchiveStore writes and verifies archive objects (S3, LLD §15.4).
type ArchiveStore interface {
	// PutArchive uploads one object: SSE-KMS, Object Lock retained until
	// retainUntil.
	PutArchive(ctx context.Context, key string, body io.ReadSeeker, size int64, retainUntil time.Time) error
	// ArchiveChecksum re-reads an object and returns the hex SHA-256 of its
	// body, failing if the object is missing or its Object Lock retention is
	// shorter than retainUntil.
	ArchiveChecksum(ctx context.Context, key string, retainUntil time.Time) (string, error)
	// Bucket is the archive bucket name.
	Bucket() string
}

// ArchiveMetrics counts archival outcomes (LLD §11).
type ArchiveMetrics interface {
	PartitionArchived(tier domain.RetentionTier, result string) // iam_audit_log_archive_partitions_total
	RedactionBlocked()                                          // iam_audit_log_redaction_blocked_archive_total
	Pruned(tier domain.RetentionTier)                           // iam_audit_log_retention_pruned_total
	Stalled(n int)                                              // iam_audit_log_archive_stalled
}

// LedgerPruner deletes processed_events rows older than ttlDays in batches
// and returns the count (LLD §4.2).
type LedgerPruner interface {
	PruneProcessedEvents(ctx context.Context, ttlDays, batch int) (int64, error)
}
