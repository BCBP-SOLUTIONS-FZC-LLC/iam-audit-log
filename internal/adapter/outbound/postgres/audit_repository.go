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

var (
	_ port.AuditWriter    = (*AuditRepository)(nil)
	_ port.RedactionStore = (*AuditRepository)(nil)
)

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
	return r.appendTx(ctx, e, consumer, nil)
}

// insertRedactionTaskSQL: audit_app holds INSERT only on the task table
// (D-1), so there is no RETURNING and no conflict target — both need
// SELECT (42501 otherwise). The id is freshly minted, so the only possible
// conflict is uq_redaction_trigger (a redelivered trigger); RowsAffected
// tells a new task from that no-op (BUILD_PLAN gap 35).
const insertRedactionTaskSQL = `INSERT INTO audit_redaction_tasks
    (id, tenant_id, subject_actor_id, trigger_event_type, trigger_source_event_id)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT DO NOTHING`

// AppendWithRedaction implements port.RedactionStore: the entry and its
// redaction task commit together (LLD §8.7).
func (r *AuditRepository) AppendWithRedaction(ctx context.Context, e domain.AuditEntry, consumer string, req domain.RedactionRequest) (stored domain.AuditEntry, created, taskCreated bool, err error) {
	stored, created, err = r.appendTx(ctx, e, consumer, func(ctx context.Context, tx pgx.Tx) error {
		tag, xerr := tx.Exec(ctx, insertRedactionTaskSQL, req.TaskID, req.TenantID, req.SubjectID, req.TriggerEventType, req.TriggerSourceEventID)
		taskCreated = xerr == nil && tag.RowsAffected() == 1
		return xerr
	})
	if err != nil {
		return domain.AuditEntry{}, false, false, err
	}
	return stored, created, taskCreated, nil
}

const applyRedactionSQL = `SELECT task_status, task_rows_redacted FROM apply_redaction($1::uuid)`

// ApplyRedaction implements port.RedactionStore via the apply_redaction()
// SECURITY DEFINER function (D-1). It needs no tenant binding: the task
// row names the tenant, and the definer runs as audit_reconciler.
func (r *AuditRepository) ApplyRedaction(ctx context.Context, taskID string) (domain.RedactionOutcome, error) {
	return applyRedaction(ctx, r.pool, taskID)
}

func applyRedaction(ctx context.Context, pool *pgcommon.Pool, taskID string) (domain.RedactionOutcome, error) {
	var out domain.RedactionOutcome
	err := withPool(ctx, pool, func(tx pgx.Tx) error {
		var rows *int64
		if err := tx.QueryRow(ctx, applyRedactionSQL, taskID).Scan(&out.Status, &rows); err != nil {
			return err
		}
		if rows != nil {
			out.RowsRedacted = *rows
		}
		return nil
	})
	return out, err
}

// appendTx is the shared Append transaction; extra (if set) runs in the
// same transaction after the entry is persisted or found.
func (r *AuditRepository) appendTx(ctx context.Context, e domain.AuditEntry, consumer string, extra func(context.Context, pgx.Tx) error) (domain.AuditEntry, bool, error) {
	g, _ := pgcommon.GUCSetFromContext(ctx)
	g.TenantID = e.TenantID
	ctx = pgcommon.WithGUCSet(ctx, g)

	var (
		stored  domain.AuditEntry
		created bool
	)
	err := NewTxRunner(r.pool).RunInTx(ctx, func(ctx context.Context) error {
		tx, _ := TxFromContext(ctx)
		if err := r.persist(ctx, tx, e, consumer, &stored, &created); err != nil {
			return err
		}
		if extra != nil {
			return extra(ctx, tx)
		}
		return nil
	})
	if err != nil {
		return domain.AuditEntry{}, false, err
	}
	return stored, created, nil
}

// persist is the ledger → insert → replay-lookup body of Append.
func (r *AuditRepository) persist(ctx context.Context, tx pgx.Tx, e domain.AuditEntry, consumer string, stored *domain.AuditEntry, created *bool) error {
	tag, err := tx.Exec(ctx, insertLedgerSQL, e.SourceEventID, consumer)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		if e, err = redactIfErased(ctx, tx, e); err != nil {
			return err
		}
		var recordedAt time.Time
		err := tx.QueryRow(ctx, insertEntrySQL, entryArgs(e)...).Scan(&recordedAt)
		switch {
		case err == nil:
			*stored, *created = e, true
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
	*stored = found
	return err
}

// erasedSubjectSQL finds a finished redaction for any of the entry's
// subjects (read under the entry tenant's RLS binding).
const erasedSubjectSQL = `SELECT subject_id::text, task_id::text FROM redacted_subjects
WHERE tenant_id = $1 AND subject_id = ANY($2::uuid[])
ORDER BY redaction_completed_at DESC LIMIT 1`

// redactIfErased is the ingest-time GDPR check (decision D-18, gap 37): a
// security_3y row about a subject whose redaction already finished is
// redacted BEFORE insert, so no new unredacted row is ever written for
// them, on either the bus or the direct-write path (AL-INV-2).
func redactIfErased(ctx context.Context, tx pgx.Tx, e domain.AuditEntry) (domain.AuditEntry, error) {
	subjects := domain.RedactionSubjects(e)
	if len(subjects) == 0 {
		return e, nil
	}
	var subject, taskID string
	err := tx.QueryRow(ctx, erasedSubjectSQL, e.TenantID, subjects).Scan(&subject, &taskID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return e, nil
	case err != nil:
		return e, err
	}
	return domain.RedactOnIngest(e, subject, taskID, time.Now()), nil
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
