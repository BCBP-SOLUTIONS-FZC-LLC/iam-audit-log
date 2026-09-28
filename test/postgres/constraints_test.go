//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// §4.2 chk_anonymous_actor (AL-D11): anonymous ⇔ actor_id IS NULL.
func TestConstraint_AnonymousActor_ALD11(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()

	anon := newEvent(tenantA, time.Now())
	anon.ActorType, anon.ActorID = "anonymous", nil
	_, err := db.raw.Exec(ctx, insertEventSQL, anon.args()...)
	require.NoError(t, err, "anonymous with NULL actor_id is valid")

	bad := newEvent(tenantA, time.Now())
	bad.ActorType = "anonymous" // with an id
	_, err = db.raw.Exec(ctx, insertEventSQL, bad.args()...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chk_anonymous_actor")

	noID := newEvent(tenantA, time.Now())
	noID.ActorID = nil // user without id
	_, err = db.raw.Exec(ctx, insertEventSQL, noID.args()...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chk_anonymous_actor")
}

// §4.2 chk_metadata_object: metadata must be a JSON object.
func TestConstraint_MetadataMustBeObject(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	e := newEvent(tenantA, time.Now())
	e.Metadata = `["not","an","object"]`
	_, err := db.raw.Exec(context.Background(), insertEventSQL, e.args()...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chk_metadata_object")
}

// AL-INV-4: uq_audit_events_source_id is the hard dedup backstop that
// survives a processed_events prune. NOTE for Phase 2 error mapping: the
// violation names the PARTITION's index
// (audit_events_YYYY_MM_source_event_id_occurred_at_idx), not the parent
// index — map on SQLSTATE 23505 + the "_source_event_id_occurred_at_idx"
// suffix, not on the parent name.
func TestConstraint_SourceEventDedupBackstop_ALINV4(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	e := newEvent(tenantA, time.Now().UTC().Truncate(time.Microsecond))
	seedEvent(t, db, e)
	dup := newEvent(tenantA, e.OccurredAt)
	dup.SourceID = e.SourceID
	_, err := db.raw.Exec(context.Background(), insertEventSQL, dup.args()...)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "SQLSTATE 23505")
	assert.Contains(t, err.Error(), "_source_event_id_occurred_at_idx")
}

// AL-INV-4: the (event_id, consumer) ledger makes a redelivery a no-op, and
// the same event may still be recorded once per consumer.
func TestConstraint_ProcessedEventsLedger_ALINV4(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	id := uuid.NewString()
	insert := func(consumer string) int64 {
		var n int64
		require.NoError(t, inTx(ctx, db.appPool, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `INSERT INTO processed_events (event_id, consumer) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, consumer)
			n = tag.RowsAffected()
			return err
		}))
		return n
	}
	assert.EqualValues(t, 1, insert("auth"))
	assert.EqualValues(t, 0, insert("auth"), "redelivery must be a no-op")
	assert.EqualValues(t, 1, insert("direct_write"), "distinct consumer is a distinct ledger row")
}

// AL-INV-12 prep (decision D-1): audit_app inserts redaction tasks, and a
// redelivered trigger is a no-op (uq_redaction_trigger).
func TestConstraint_RedactionTaskIdempotent_ALINV12(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	trigger := uuid.NewString()
	for range 2 {
		require.NoError(t, inTx(ctx, db.appPool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO audit_redaction_tasks
				(tenant_id, subject_actor_id, trigger_event_type, trigger_source_event_id)
				VALUES ($1, $2, 'UserDeleted', $3) ON CONFLICT DO NOTHING`, tenantA, uuid.NewString(), trigger)
			return err
		}))
	}
	var n int
	require.NoError(t, db.raw.QueryRow(ctx, `SELECT count(*) FROM audit_redaction_tasks WHERE trigger_source_event_id = $1`, trigger).Scan(&n))
	assert.Equal(t, 1, n)
}
