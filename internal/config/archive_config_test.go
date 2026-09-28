package config

import (
	"strings"
	"testing"
)

// Archival config (LLD §8.5, §15.4).
func TestLoadReconciler_Archival(t *testing.T) {
	clearEnv(t)
	t.Setenv("RECONCILER_DATABASE_URL", "postgres://recon")
	for _, k := range []string{"ARCHIVE_PART_MAX_ROWS", "ARCHIVE_WORK_DIR", "PROCESSED_EVENTS_PRUNE_BATCH", "AUDIT_ARCHIVE_BUCKET"} {
		t.Setenv(k, "")
	}
	c, err := LoadReconciler("v", "reconcile")
	if err != nil {
		t.Fatal(err)
	}
	if c.ArchivePartMaxRows != 50000 || c.ArchiveWorkDir != "" || c.ProcessedEventsPruneBatch != 10000 || c.ArchiveBucket != "iam-audit-archive" {
		t.Errorf("defaults = %d / %q / %d / %q", c.ArchivePartMaxRows, c.ArchiveWorkDir, c.ProcessedEventsPruneBatch, c.ArchiveBucket)
	}

	t.Setenv("ARCHIVE_PART_MAX_ROWS", "1000")
	t.Setenv("ARCHIVE_WORK_DIR", "/scratch")
	t.Setenv("PROCESSED_EVENTS_PRUNE_BATCH", "500")
	c, err = LoadReconciler("v", "reconcile")
	if err != nil || c.ArchivePartMaxRows != 1000 || c.ArchiveWorkDir != "/scratch" || c.ProcessedEventsPruneBatch != 500 {
		t.Errorf("overrides = %d / %q / %d, err %v", c.ArchivePartMaxRows, c.ArchiveWorkDir, c.ProcessedEventsPruneBatch, err)
	}
	t.Setenv("ARCHIVE_PART_MAX_ROWS", "1000000")
	if _, err := LoadReconciler("v", "reconcile"); err != nil {
		t.Errorf("upper bound inclusive: %v", err)
	}

	for _, bad := range []string{"999", "1000001"} {
		t.Setenv("ARCHIVE_PART_MAX_ROWS", bad)
		if _, err := LoadReconciler("v", "reconcile"); err == nil || !strings.Contains(err.Error(), "ARCHIVE_PART_MAX_ROWS") {
			t.Errorf("%s: want ARCHIVE_PART_MAX_ROWS error, got %v", bad, err)
		}
	}
}
