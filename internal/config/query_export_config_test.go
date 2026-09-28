package config

import (
	"strings"
	"testing"
	"time"
)

func clearQueryExportEnv(t *testing.T) {
	t.Helper()
	clearEnv(t)
	for _, k := range []string{
		"ARCHIVE_SYNC_MAX_ROWS", "ARCHIVE_SYNC_MAX_BYTES", "EXPORT_DOWNLOAD_URL_TTL", "EXPORT_RATE_LIMIT_PER_MINUTE",
		"EXPORT_RATE_LIMIT_BURST", "EXPORT_POLL_INTERVAL", "EXPORT_JOB_LEASE", "EXPORT_WORK_DIR",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("DATABASE_URL", "postgres://app")
}

// Decisions D-10 / D-11 / D-2 and §10.5 defaults (LLD §12 additions).
func TestLoadServer_QueryExportDefaults(t *testing.T) {
	clearQueryExportEnv(t)
	c, err := LoadServer("v")
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string][2]any{
		"ArchiveSyncMaxRows":    {c.ArchiveSyncMaxRows, int64(10000)},
		"ArchiveSyncMaxBytes":   {c.ArchiveSyncMaxBytes, int64(50 << 20)},
		"ExportDownloadURLTTL":  {c.ExportDownloadURLTTL, 15 * time.Minute},
		"ExportRateLimitPerMin": {c.ExportRateLimitPerMin, 10},
		"ExportRateLimitBurst":  {c.ExportRateLimitBurst, 5},
		"ExportPollInterval":    {c.ExportPollInterval, 5 * time.Second},
		"ExportJobLease":        {c.ExportJobLease, 15 * time.Minute},
		"ExportWorkDir":         {c.ExportWorkDir, ""},
	}
	for name, v := range checks {
		if v[0] != v[1] {
			t.Errorf("%s = %v, want %v", name, v[0], v[1])
		}
	}
}

func TestLoadServer_QueryExportOverrides(t *testing.T) {
	clearQueryExportEnv(t)
	t.Setenv("ARCHIVE_SYNC_MAX_ROWS", "500")
	t.Setenv("ARCHIVE_SYNC_MAX_BYTES", "1024")
	t.Setenv("EXPORT_DOWNLOAD_URL_TTL", "5m")
	t.Setenv("EXPORT_JOB_LEASE", "1h")
	t.Setenv("EXPORT_WORK_DIR", "/tmp/exports")
	c, err := LoadServer("v")
	if err != nil {
		t.Fatal(err)
	}
	if c.ArchiveSyncMaxRows != 500 || c.ArchiveSyncMaxBytes != 1024 || c.ExportDownloadURLTTL != 5*time.Minute ||
		c.ExportJobLease != time.Hour || c.ExportWorkDir != "/tmp/exports" {
		t.Errorf("overrides not applied: %+v", c)
	}
}

// EXPORT_JOB_LEASE must sit inside claim_export_job()'s 1m..1d bound, and
// the per-poll URL cannot exceed SigV4's 7-day presign limit.
func TestLoadServer_QueryExportValidation(t *testing.T) {
	for _, tc := range []struct{ key, val, want string }{
		{"EXPORT_JOB_LEASE", "30s", "EXPORT_JOB_LEASE"},
		{"EXPORT_JOB_LEASE", "25h", "EXPORT_JOB_LEASE"},
		{"EXPORT_DOWNLOAD_URL_TTL", "169h", "EXPORT_DOWNLOAD_URL_TTL"},
	} {
		clearQueryExportEnv(t)
		t.Setenv(tc.key, tc.val)
		if _, err := LoadServer("v"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s=%s: err = %v", tc.key, tc.val, err)
		}
	}
	for _, ok := range []struct{ key, val string }{
		{"EXPORT_JOB_LEASE", "1m"}, {"EXPORT_JOB_LEASE", "24h"}, {"EXPORT_DOWNLOAD_URL_TTL", "168h"},
	} {
		clearQueryExportEnv(t)
		t.Setenv(ok.key, ok.val)
		if _, err := LoadServer("v"); err != nil {
			t.Errorf("%s=%s boundary must be accepted: %v", ok.key, ok.val, err)
		}
	}
}
