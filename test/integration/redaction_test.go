//go:build integration

package integration_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedAuditRow inserts one hot audit row directly (superuser seed; the
// append-only trigger guards UPDATE/DELETE only).
func (s *busStack) seedAuditRow(t *testing.T, entryType, tier string, actorID, targetType, targetID *string, display, meta string) string {
	t.Helper()
	src := uuid.NewString()
	_, err := s.seed.Exec(context.Background(), `INSERT INTO audit_events
		(id, occurred_at, tenant_id, entry_type, action, actor_type, actor_id, actor_display, target_type, target_id,
		 source_service, source_topic, source_event_type, source_event_id, retention_tier, ingest_mode, metadata)
		VALUES ($1, $2, $3, $4, 'update', 'user', $5, $6, $7, $8, 'seed', 'seed', 'Seed', $9,
		        $10::audit_retention_tier, 'bus', $11::jsonb)`,
		uuid.Must(uuid.NewV7()).String(), time.Now().UTC().Add(-time.Hour), tenantA, entryType,
		actorID, display, targetType, targetID, src, tier, meta)
	require.NoError(t, err)
	return src
}

func (s *busStack) rowState(t *testing.T, sourceEventID string) (meta string, display *string) {
	t.Helper()
	require.NoError(t, s.seed.QueryRow(context.Background(),
		`SELECT metadata::text, actor_display FROM audit_events WHERE source_event_id = $1`, sourceEventID).Scan(&meta, &display))
	return meta, display
}

func (s *busStack) tasks(t *testing.T, subject string) (n int, applied int) {
	t.Helper()
	require.NoError(t, s.seed.QueryRow(context.Background(),
		`SELECT count(*), count(*) FILTER (WHERE status = 'applied') FROM audit_redaction_tasks WHERE subject_actor_id = $1`,
		subject).Scan(&n, &applied))
	return n, applied
}

// §14 Redaction row, end to end through the real fleet (LLD §8.7,
// AL-INV-7, AL-INV-12; D-15): a UserDeleted on user-audit-q is audited, its
// redaction task commits in the same transaction and is applied at once —
// security_3y rows where the subject is actor or user target are redacted,
// compliance_7y rows are untouched — and a redelivery is a no-op.
func TestRedaction_BusUserDeletedEndToEnd(t *testing.T) {
	s := newBusStack(t, "user-audit-q")
	subject := uuid.NewString()
	admin := uuid.NewString()
	user := "user"

	asTarget := s.seedAuditRow(t, "user.updated", "security_3y", &admin, &user, &subject, "admin@acme.example",
		`{"email":"subject@acme.example"}`)
	asActor := s.seedAuditRow(t, "delegation.started", "security_3y", &subject, nil, nil, "subject@acme.example",
		`{"note":"covering for subject@acme.example"}`)
	compliance := s.seedAuditRow(t, "tender.section.approved", "compliance_7y", &subject, nil, nil, "subject@acme.example",
		`{"signature_hash":"abc123"}`)
	compMetaBefore, _ := s.rowState(t, compliance)

	id := uuid.NewString()
	body := rawEnvelope(t, map[string]any{"id": id, "type": "UserDeleted", "source": "iam-user-profile",
		"data": map[string]any{"user_id": subject, "tenant_id": tenantA}})
	s.env.publish(t, "iam-user-events", body)

	r := s.waitRow(t, id)
	assert.Equal(t, "user.deleted", r.EntryType)
	eventually(t, 30*time.Second, func() bool { _, applied := s.tasks(t, subject); return applied == 1 },
		"redaction task never reached applied")

	for name, src := range map[string]string{"target row": asTarget, "actor row": asActor} {
		meta, _ := s.rowState(t, src)
		assert.Contains(t, meta, `"_redacted": true`, name)
		assert.NotContains(t, meta, "subject@acme.example", name)
	}
	_, actorDisplay := s.rowState(t, asActor)
	assert.Nil(t, actorDisplay, "the subject's own actor_display is cleared (D-15)")
	_, adminDisplay := s.rowState(t, asTarget)
	require.NotNil(t, adminDisplay)
	assert.Equal(t, "admin@acme.example", *adminDisplay, "another actor's display is kept")

	compMeta, compDisplay := s.rowState(t, compliance)
	assert.Equal(t, compMetaBefore, compMeta, "compliance_7y metadata is never redacted (AL-INV-7)")
	require.NotNil(t, compDisplay)
	assert.Equal(t, "subject@acme.example", *compDisplay)

	var rows int64
	require.NoError(t, s.seed.QueryRow(context.Background(),
		`SELECT rows_redacted FROM audit_redaction_tasks WHERE subject_actor_id = $1`, subject).Scan(&rows))
	assert.GreaterOrEqual(t, rows, int64(2), "both seeded security_3y rows (plus the user.deleted row itself, when it targets the subject)")

	// Redelivery of the same envelope id: no second row, no second task.
	s.env.publish(t, "iam-user-events", body)
	time.Sleep(5 * time.Second)
	assert.Equal(t, 1, s.count(t, id))
	n, applied := s.tasks(t, subject)
	assert.Equal(t, 1, n)
	assert.Equal(t, 1, applied)

	// A UserDeleted naming no user: still persisted, no task (gap 36).
	noUser := uuid.NewString()
	s.env.publish(t, "iam-user-events", rawEnvelope(t, map[string]any{"id": noUser, "type": "UserDeleted",
		"source": "iam-user-profile", "data": map[string]any{"tenant_id": tenantA}}))
	s.waitRow(t, noUser)
	var noUserTasks int
	require.NoError(t, s.seed.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_redaction_tasks WHERE trigger_source_event_id = $1`, noUser).Scan(&noUserTasks))
	assert.Zero(t, noUserTasks)
}

// Decision D-18 (gap 37), end to end: once a UserDeleted has been applied, a
// later security_3y event about the same user (here a UserUpdated targeting
// them, carrying their email) is redacted before insert by the ingest-time
// check against redacted_subjects.
func TestRedaction_LateEventAfterUserDeleted_D18(t *testing.T) {
	s := newBusStack(t, "user-audit-q")
	subject := uuid.NewString()

	deleted := uuid.NewString()
	s.env.publish(t, "iam-user-events", rawEnvelope(t, map[string]any{"id": deleted, "type": "UserDeleted",
		"source": "iam-user-profile", "data": map[string]any{"user_id": subject, "tenant_id": tenantA}}))
	s.waitRow(t, deleted)
	eventually(t, 30*time.Second, func() bool { _, applied := s.tasks(t, subject); return applied == 1 },
		"redaction task never reached applied")

	late := uuid.NewString()
	s.env.publish(t, "iam-user-events", rawEnvelope(t, map[string]any{"id": late, "type": "UserUpdated",
		"source": "iam-user-profile", "data": map[string]any{"user_id": subject, "tenant_id": tenantA,
			"email": "subject@acme.example"}}))
	r := s.waitRow(t, late)
	assert.Equal(t, "user.updated", r.EntryType)
	meta, _ := s.rowState(t, late)
	assert.Contains(t, meta, `"_redacted": true`)
	assert.Contains(t, meta, `"_redacted_on_ingest": true`)
	assert.NotContains(t, meta, "subject@acme.example")
}
