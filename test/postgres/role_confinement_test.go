//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgcommon"
)

// Rule 5 / AL-INV-1: the reconciler's repositories only work as
// audit_reconciler. Pointed at the audit_app pool, every archival, prune,
// redaction-retry and drop step is refused by grants (42501), and the
// error surfaces instead of being swallowed — so a mis-wired composition
// root fails loudly rather than archiving or deleting as the wrong role.
func TestReconcilerRepositories_RefusedAsAuditApp(t *testing.T) {
	db := setupTestDB(t, dbOpts{})
	ctx := context.Background()
	seedEvent(t, db, newEvent(tenantA, monthStart(0).Add(time.Hour)))
	archive := pgadapter.NewArchiveRepository(db.appPool)
	redaction := pgadapter.NewRedactionRepository(db.appPool)
	p, tier := partitionName(0), domain.TierSecurity3y

	denied := map[string]error{}
	_, denied["ReopenCandidates"] = archive.ReopenCandidates(ctx)
	_, denied["States"] = archive.States(ctx, p)
	_, denied["PendingRedaction"] = archive.PendingRedaction(ctx, p)
	denied["MarkArchiving"] = archive.MarkArchiving(ctx, p, tier, monthStart(0))
	denied["FinishArchive"] = archive.FinishArchive(ctx, p, tier, nil, 0, "", "security_3y/")
	_, denied["PruneProcessedEvents"] = archive.PruneProcessedEvents(ctx, 8, 10)
	_, denied["PendingTasks"] = redaction.PendingTasks(ctx, time.Now(), 10)
	_, denied["Sweep"] = redaction.Sweep(ctx, time.Hour)
	_, denied["Drop"] = archive.Drop(ctx, p)
	for step, err := range denied {
		if assert.Error(t, err, "%s must be refused as audit_app", step) {
			assert.Equal(t, "42501", pgcommon.SQLState(err), "%s: %v", step, err)
		}
	}

	// What audit_app may read is still fail-closed: with no tenant GUC bound,
	// RLS returns nothing — no row of the seeded event, no manifest rows.
	var streamed int
	assert.NoError(t, archive.StreamTier(ctx, p, tier, func(domain.AuditEntry) error { streamed++; return nil }))
	assert.Zero(t, streamed, "StreamTier as audit_app must see no rows (RLS, no GUC)")
	objs, err := archive.UnsealedObjects(ctx, p, tier)
	assert.NoError(t, err)
	assert.Empty(t, objs)
}
