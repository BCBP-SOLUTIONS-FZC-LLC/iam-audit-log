//go:build integration

package postgres_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// LLD rev 0.31 — legal.terms.accepted is compliance_7y: it is kept through
// the accepting owner's GDPR erasure (AL-INV-7). None of the three
// redaction paths (apply_redaction, ingest-time D-18, sweep_redactions)
// touches its actor, ip_address, user_agent or metadata.

const termsMetadata = `{"plan": "pro", "accepted_at": "2026-09-20T10:00:00Z", "signup_type": "trial", "terms_version": "2026-09"}`

// termsEntry builds the row exactly as AL-5 does for Realm Provisioner's
// TermsAccepted body.
func termsEntry(t *testing.T, tenant, owner string) domain.AuditEntry {
	t.Helper()
	e, err := domain.BuildDirectWriteEntry(domain.DirectWriteCommand{
		IdempotencyKey: "TermsAccepted:" + tenant + ":" + owner + ":2026-09",
		TenantID:       tenant, EntryType: "legal.terms.accepted", Action: "accept",
		Actor:         domain.ActorRef{Type: domain.ActorUser, ID: owner, Display: "asha@acme.example"},
		Target:        &domain.TargetRef{Type: "tenant", ID: tenant},
		OccurredAt:    time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond),
		SourceService: "iam-realm-provisioner", SourceEventType: "TermsAccepted",
		IPAddress: "203.0.113.7", UserAgent: "Mozilla/5.0 (Macintosh)",
		Metadata: json.RawMessage(termsMetadata),
	}, 8192)
	require.NoError(t, err)
	require.Equal(t, domain.TierCompliance7y, e.RetentionTier)
	id, _ := uuid.NewV7()
	e.ID = id.String()
	return e
}

type termsRow struct {
	ActorID, ActorDisplay, IP, UA *string
	Metadata                      string
	Tier                          string
}

func readTermsRow(t *testing.T, db *testDB, id string) termsRow {
	t.Helper()
	var r termsRow
	require.NoError(t, db.raw.QueryRow(context.Background(),
		`SELECT actor_id::text, actor_display, host(ip_address), user_agent, metadata::text, retention_tier::text
		   FROM audit_events WHERE id = $1`, id).
		Scan(&r.ActorID, &r.ActorDisplay, &r.IP, &r.UA, &r.Metadata, &r.Tier))
	return r
}

func assertTermsIntact(t *testing.T, r termsRow, owner string) {
	t.Helper()
	assert.Equal(t, "compliance_7y", r.Tier)
	require.NotNil(t, r.ActorID)
	assert.Equal(t, owner, *r.ActorID)
	require.NotNil(t, r.ActorDisplay)
	assert.Equal(t, "asha@acme.example", *r.ActorDisplay)
	require.NotNil(t, r.IP)
	assert.Equal(t, "203.0.113.7", *r.IP)
	require.NotNil(t, r.UA)
	assert.Equal(t, "Mozilla/5.0 (Macintosh)", *r.UA)
	assert.JSONEq(t, termsMetadata, r.Metadata)
	assert.NotContains(t, r.Metadata, "_redacted")
}

func TestRedaction_TermsAcceptedSurvivesErasure_ALINV7(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	repo := pgadapter.NewAuditRepository(db.appPool)
	owner := uuid.NewString()

	// Accepted before the erasure: apply_redaction leaves it byte-identical.
	before := termsEntry(t, tenantA, owner)
	_, created, err := repo.Append(ctx, before, domain.ConsumerDirectWrite)
	require.NoError(t, err)
	require.True(t, created)
	// A security_3y row of the same owner, as a control: it IS redacted.
	control := seedRedactRow(t, db, redactRow{Tenant: tenantA, Tier: "security_3y", ActorID: owner, ActorDisplay: "asha@acme.example"})

	taskID, out := scheduleAndApply(t, db, tenantA, owner)
	require.Equal(t, domain.RedactionApplied, out.Status)
	assertMarker(t, readRow(t, db, control.ID), taskID)
	assertTermsIntact(t, readTermsRow(t, db, before.ID), owner)

	// Delivered after the erasure finished (RP's drain retried late): the
	// ingest-time D-18 check does not redact it either.
	late := termsEntry(t, tenantA, owner)
	late.SourceEventID = "TermsAccepted:" + tenantA + ":" + owner + ":2026-10"
	stored, created, err := repo.Append(ctx, late, domain.ConsumerDirectWrite)
	require.NoError(t, err)
	require.True(t, created)
	assert.Equal(t, "asha@acme.example", stored.Actor.Display)
	assert.NotContains(t, string(stored.Metadata), "_redacted")
	assertTermsIntact(t, readTermsRow(t, db, late.ID), owner)

	// The daily sweep does not touch either.
	_, err = pgadapter.NewRedactionRepository(db.reconPool).Sweep(ctx, 90*24*time.Hour)
	require.NoError(t, err)
	assertTermsIntact(t, readTermsRow(t, db, before.ID), owner)
	assertTermsIntact(t, readTermsRow(t, db, late.ID), owner)
}
