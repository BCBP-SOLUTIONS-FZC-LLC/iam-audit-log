package config

import (
	"strings"
	"testing"
	"time"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"APP_ENV", "DATABASE_URL", "MIGRATION_DATABASE_URL", "RECONCILER_DATABASE_URL",
		"AUDIT_ARCHIVE_OBJECT_LOCK_MODE", "PROCESSED_EVENTS_TTL_DAYS",
	} {
		t.Setenv(k, "")
	}
	for _, q := range InboundQueues() {
		t.Setenv(q.EnvVar, "")
	}
}

// LLD §12 defaults.
func TestLoadServer_Defaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://app")
	c, err := LoadServer("v")
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string][2]any{
		"DB_APP_ROLE":                     {c.DBAppRole, "audit_app"},
		"SERVICE_NAME":                    {c.ServiceName, "iam-audit-log"},
		"APP_PORT":                        {c.AppPort, "8080"},
		"METRICS_PORT":                    {c.MetricsPort, "9090"},
		"GLUE_REGISTRY_REGION":            {c.GlueRegistryRegion, "ap-south-1"},
		"AUDIT_ARCHIVE_BUCKET":            {c.ArchiveBucket, "iam-audit-archive"},
		"AUDIT_ARCHIVE_KMS_KEY":           {c.ArchiveKMSKey, "alias/iam-audit-archive"},
		"AUDIT_ARCHIVE_OBJECT_LOCK_MODE":  {c.ArchiveObjectLockMode, "COMPLIANCE"},
		"AUDIT_HOT_WINDOW_DAYS":           {c.HotWindowDays, 90},
		"AUDIT_PRECREATE_MONTHS":          {c.PrecreateMonths, 3},
		"AUDIT_WRITABLE_TRAILING_MONTHS":  {c.WritableTrailingMonths, 3},
		"AUDIT_DEFAULT_QUERY_WINDOW_DAYS": {c.DefaultQueryWindowDays, 365},
		"CATALOG_BASE_URL":                {c.CatalogBaseURL, "http://iam-catalog-admin.iam.svc.cluster.local:8080"},
		"CATALOG_PLANS_POLL_INTERVAL":     {c.CatalogPollInterval, 600 * time.Second},
		"CATALOG_PLANS_POLL_TIMEOUT":      {c.CatalogPollTimeout, 3 * time.Second},
		"MAX_INGEST_BATCH":                {c.MaxIngestBatch, 500},
		"MAX_METADATA_BYTES":              {c.MaxMetadataBytes, 8192},
		"EXPORT_SIGNED_URL_TTL":           {c.ExportSignedURLTTL, 168 * time.Hour},
		"INGEST_BATCH_RATE_LIMIT_RPS":     {c.IngestBatchRateLimitRPS, 10},
		"INGEST_BATCH_RATE_LIMIT_BURST":   {c.IngestBatchRateLimitBurst, 20},
	}
	for k, v := range checks {
		if v[0] != v[1] {
			t.Errorf("%s = %v, want %v", k, v[0], v[1])
		}
	}
	if c.MigrationDSN() != "postgres://app" {
		t.Errorf("dev MigrationDSN should fall back to DATABASE_URL")
	}
}

// Frozen inbound catalog (LLD §7.1, §7.5, §25).
func TestInboundQueues_Frozen(t *testing.T) {
	want := map[string]string{
		"auth-audit-q": "auth", "user-audit-q": "user", "membership-audit-q": "membership",
		"tenant-audit-q": "tenant", "delegation-audit-q": "delegation",
		"serviceaccount-audit-q": "serviceaccount", "tender-audit-q": "tender",
		"billing-audit-q": "billing", "usage-audit-q": "usage",
		"wf-workflow-audit-q": "wf_workflow", "wf-template-audit-q": "wf_template",
	}
	qs := InboundQueues()
	if len(qs) != len(want) {
		t.Fatalf("got %d queues, want %d", len(qs), len(want))
	}
	for _, q := range qs {
		if want[q.Name] != q.Consumer {
			t.Errorf("%s: consumer %q, want %q", q.Name, q.Consumer, want[q.Name])
		}
		if q.Consumer == "direct_write" {
			t.Errorf("direct_write is the ingest endpoint's discriminator, not a queue")
		}
	}
}

func TestLoadServer_ProductionRequiresEverything(t *testing.T) {
	clearEnv(t)
	t.Setenv("APP_ENV", "production")
	t.Setenv("AUDIT_ARCHIVE_OBJECT_LOCK_MODE", "GOVERNANCE")
	_, err := LoadServer("v")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, s := range []string{"DATABASE_URL", "MIGRATION_DATABASE_URL", "AUTH_AUDIT_QUEUE_URL", "WF_TEMPLATE_AUDIT_QUEUE_URL", "must be COMPLIANCE outside dev"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error does not mention %s:\n%v", s, err)
		}
	}
}

// Rule 5: each composition root loads only its own DB role. The reconciler
// never falls back to DATABASE_URL (the audit_app DSN).
func TestLoadReconciler_NeverUsesAppDSN(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://app")
	if _, err := LoadReconciler("v", "reconcile"); err == nil || !strings.Contains(err.Error(), "RECONCILER_DATABASE_URL") {
		t.Fatalf("expected RECONCILER_DATABASE_URL error, got %v", err)
	}
	t.Setenv("RECONCILER_DATABASE_URL", "postgres://recon")
	c, err := LoadReconciler("v", "reconcile")
	if err != nil {
		t.Fatal(err)
	}
	if c.ReconcilerDatabaseURL != "postgres://recon" || c.DBReconcilerRole != "audit_reconciler" {
		t.Errorf("unexpected reconciler DB config: %+v", c)
	}
	if c.ProcessedEventsTTLDays != 8 {
		t.Errorf("PROCESSED_EVENTS_TTL_DAYS default %d, want 8", c.ProcessedEventsTTLDays)
	}
}

func TestLoadReconciler_LedgerTTLMustExceedSQSLifetime(t *testing.T) {
	clearEnv(t)
	t.Setenv("RECONCILER_DATABASE_URL", "postgres://recon")
	t.Setenv("PROCESSED_EVENTS_TTL_DAYS", "7")
	if _, err := LoadReconciler("v", "x"); err == nil {
		t.Fatal("expected error for TTL <= 7 days")
	}
}

func TestEnvParsing_ValidOverridesAndInvalidFallbacks(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://app")
	t.Setenv("SQS_VISIBILITY_TIMEOUT", "45s")
	t.Setenv("CATALOG_PLANS_POLL_INTERVAL", "not-a-duration")
	t.Setenv("MAX_INGEST_BATCH", "250")
	t.Setenv("MAX_METADATA_BYTES", "-1")
	c, err := LoadServer("v")
	if err != nil {
		t.Fatal(err)
	}
	if c.MaxIngestBatch != 250 {
		t.Errorf("valid overrides ignored: %d", c.MaxIngestBatch)
	}
	if c.CatalogPollInterval != 600*time.Second || c.MaxMetadataBytes != 8192 {
		t.Errorf("invalid values must fall back to LLD defaults: %v %d", c.CatalogPollInterval, c.MaxMetadataBytes)
	}
}

func TestValidateCommon_RejectsUnknownObjectLockMode(t *testing.T) {
	clearEnv(t)
	t.Setenv("DATABASE_URL", "postgres://app")
	t.Setenv("AUDIT_ARCHIVE_OBJECT_LOCK_MODE", "OFF")
	if _, err := LoadServer("v"); err == nil || !strings.Contains(err.Error(), "must be COMPLIANCE or GOVERNANCE") {
		t.Fatalf("err = %v", err)
	}
	// GOVERNANCE is tolerated in dev only.
	t.Setenv("AUDIT_ARCHIVE_OBJECT_LOCK_MODE", "GOVERNANCE")
	if _, err := LoadServer("v"); err != nil {
		t.Fatalf("dev GOVERNANCE: %v", err)
	}
}

func TestIsDev(t *testing.T) {
	for _, e := range []string{"dev", "development", "local", "test"} {
		if !IsDev(e) {
			t.Errorf("%s should be dev", e)
		}
	}
	for _, e := range []string{"production", "staging", ""} {
		if IsDev(e) {
			t.Errorf("%q should not be dev", e)
		}
	}
}
