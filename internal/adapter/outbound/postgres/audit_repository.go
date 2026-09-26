package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

// AuditRepository implements port.AuditWriter on the audit_app pool.
type AuditRepository struct {
	pool *pgcommon.Pool
}

var _ port.AuditWriter = (*AuditRepository)(nil)

// NewAuditRepository returns a repository over the audit_app pool.
func NewAuditRepository(pool *pgcommon.Pool) *AuditRepository {
	return &AuditRepository{pool: pool}
}

const (
	insertLedgerSQL = `INSERT INTO processed_events (event_id, consumer) VALUES ($1, $2) ON CONFLICT DO NOTHING`

	// ON CONFLICT on the partition-key-inclusive unique index is the hard
	// dedup backstop that outlives an 8-day ledger prune (AL-INV-4).
	insertEntrySQL = `INSERT INTO audit_events (
    id, occurred_at, tenant_id, entry_type, action, actor_type, actor_id, actor_display,
    target_type, target_id, source_service, source_topic, source_event_type, source_event_id,
    retention_tier, ingest_mode, ip_address, user_agent, trace_id, metadata)
VALUES ($1, $2, $3, $4, $5, $6::audit_actor_type, $7, $8,
    $9, $10, $11, $12, $13, $14,
    $15::audit_retention_tier, $16::audit_ingest_mode, $17::inet, $18, $19, $20::jsonb)
ON CONFLICT (source_event_id, occurred_at) DO NOTHING
RETURNING recorded_at`

	selectBySourceSQL = `SELECT id::text, occurred_at, recorded_at, tenant_id::text, entry_type, action,
    actor_type::text, actor_id::text, actor_display, target_type, target_id,
    source_service, source_topic, source_event_type, source_event_id,
    retention_tier::text, ingest_mode::text, host(ip_address), user_agent, trace_id, metadata::text
FROM audit_events WHERE source_event_id = $1
ORDER BY recorded_at LIMIT 1`
)

// Append implements port.AuditWriter (ledger + insert in one transaction
// bound to entry.TenantID, so tenant_isolation's WITH CHECK guards the row).
func (r *AuditRepository) Append(ctx context.Context, e domain.AuditEntry, consumer string) (domain.AuditEntry, bool, error) {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.TenantID = e.TenantID
	ctx = pgcommon.WithGUCSet(ctx, g)

	var (
		stored  domain.AuditEntry
		created bool
	)
	err := NewTxRunner(r.pool).RunInTx(ctx, func(ctx context.Context) error {
		tx, _ := TxFromContext(ctx)
		tag, err := tx.Exec(ctx, insertLedgerSQL, e.SourceEventID, consumer)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			var recordedAt time.Time
			err := tx.QueryRow(ctx, insertEntrySQL, entryArgs(e)...).Scan(&recordedAt)
			switch {
			case err == nil:
				stored, created = e, true
				stored.RecordedAt = recordedAt
				return nil
			case !errors.Is(err, pgx.ErrNoRows):
				return err
			}
			// ErrNoRows: the (source_event_id, occurred_at) backstop fired
			// — a replay after the ledger row was pruned. Fall through.
		}
		found, err := scanEntry(tx.QueryRow(ctx, selectBySourceSQL, e.SourceEventID))
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.NewError(domain.ErrInvalidRequest,
				"idempotency key was already used for a different entry")
		}
		stored = found
		return err
	})
	if err != nil {
		return domain.AuditEntry{}, false, err
	}
	return stored, created, nil
}

func entryArgs(e domain.AuditEntry) []any {
	var targetType, targetID any
	if e.Target != nil {
		targetType = e.Target.Type
		if e.Target.ID != "" {
			targetID = e.Target.ID
		}
	}
	meta := string(e.Metadata) // string, never []byte: PgBouncer simple protocol would hex-encode bytes (BUILD_PLAN gap 28)
	if meta == "" {
		meta = "{}"
	}
	return []any{
		e.ID, e.OccurredAt, e.TenantID, e.EntryType, e.Action, string(e.Actor.Type), nullIfEmpty(e.Actor.ID), nullIfEmpty(e.Actor.Display),
		targetType, targetID, e.SourceService, nullIfEmpty(e.SourceTopic), e.SourceEventType, e.SourceEventID,
		string(e.RetentionTier), string(e.IngestMode), nullIfEmpty(e.IPAddress), nullIfEmpty(e.UserAgent), nullIfEmpty(e.TraceID), meta,
	}
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func scanEntry(row pgx.Row) (domain.AuditEntry, error) {
	var (
		e                                                                 domain.AuditEntry
		actorType, tier, mode, meta                                       string
		actorID, actorDisplay, targetType, targetID, topic, ip, ua, trace *string
	)
	err := row.Scan(&e.ID, &e.OccurredAt, &e.RecordedAt, &e.TenantID, &e.EntryType, &e.Action,
		&actorType, &actorID, &actorDisplay, &targetType, &targetID,
		&e.SourceService, &topic, &e.SourceEventType, &e.SourceEventID,
		&tier, &mode, &ip, &ua, &trace, &meta)
	if err != nil {
		return domain.AuditEntry{}, err
	}
	e.Actor = domain.ActorRef{Type: domain.ActorType(actorType), ID: deref(actorID), Display: deref(actorDisplay)}
	if targetType != nil {
		e.Target = &domain.TargetRef{Type: *targetType, ID: deref(targetID)}
	}
	e.SourceTopic, e.IPAddress, e.UserAgent, e.TraceID = deref(topic), deref(ip), deref(ua), deref(trace)
	e.RetentionTier, e.IngestMode = domain.RetentionTier(tier), domain.IngestMode(mode)
	e.Metadata = json.RawMessage(meta)
	return e, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
