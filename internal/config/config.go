// Package config loads and validates the environment for the two composition
// roots (LLD §12). Env vars are read with plain os.Getenv, matching the
// sibling IAM services; Postgres pool tuning (PG_MAX_CONNS, PG_BOUNCER_MODE,
// …) stays with pgcommon.ConfigFromEnv. Retention-tier durations are
// deliberately NOT here — they are compiled constants so configuration can
// never weaken retention (LLD §12, AL-INV-6).
//
// Each composition root loads only its own DB role (implementation rule 5):
// Server() never reads RECONCILER_DATABASE_URL and Reconciler() never reads
// DATABASE_URL.
package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Queue is one inbound audit queue (LLD §7.1): the env var holding its URL,
// its frozen name, and its frozen processed_events.consumer discriminator
// (LLD §7.5, §25).
type Queue struct {
	EnvVar   string
	Name     string
	Topic    string // logical topic (LLD §7.1)
	Consumer string
	URL      string
}

// InboundQueues is the frozen §7.1 / §25 inbound catalog, in LLD order.
func InboundQueues() []Queue {
	return []Queue{
		{EnvVar: "AUTH_AUDIT_QUEUE_URL", Name: "auth-audit-q", Topic: "iam.auth.events", Consumer: "auth"},
		{EnvVar: "USER_AUDIT_QUEUE_URL", Name: "user-audit-q", Topic: "iam.user.events", Consumer: "user"},
		{EnvVar: "MEMBERSHIP_AUDIT_QUEUE_URL", Name: "membership-audit-q", Topic: "iam.membership.events", Consumer: "membership"},
		{EnvVar: "TENANT_AUDIT_QUEUE_URL", Name: "tenant-audit-q", Topic: "iam.tenant.events", Consumer: "tenant"},
		{EnvVar: "DELEGATION_AUDIT_QUEUE_URL", Name: "delegation-audit-q", Topic: "iam.delegation.events", Consumer: "delegation"},
		{EnvVar: "SERVICEACCOUNT_AUDIT_QUEUE_URL", Name: "serviceaccount-audit-q", Topic: "iam.serviceaccount.events", Consumer: "serviceaccount"},
		{EnvVar: "TENDER_AUDIT_QUEUE_URL", Name: "tender-audit-q", Topic: "tender.events", Consumer: "tender"},
		{EnvVar: "BILLING_AUDIT_QUEUE_URL", Name: "billing-audit-q", Topic: "billing.events", Consumer: "billing"},
		{EnvVar: "USAGE_AUDIT_QUEUE_URL", Name: "usage-audit-q", Topic: "usage.events", Consumer: "usage"},
		{EnvVar: "WF_WORKFLOW_AUDIT_QUEUE_URL", Name: "wf-workflow-audit-q", Topic: "wf.workflow.events", Consumer: "wf_workflow"},
		{EnvVar: "WF_TEMPLATE_AUDIT_QUEUE_URL", Name: "wf-template-audit-q", Topic: "wf.template.events", Consumer: "wf_template"},
	}
}

// Common holds settings both composition roots read.
type Common struct {
	AppEnv       string
	ServiceName  string
	BuildVersion string
	AWSRegion    string
	AWSEndpoint  string // AWS_ENDPOINT_URL — floci/LocalStack only; empty in AWS (IRSA)

	ArchiveBucket          string
	ArchiveKMSKey          string
	ArchiveObjectLockMode  string
	HotWindowDays          int
	PrecreateMonths        int
	WritableTrailingMonths int
}

// Server is cmd/server's configuration (query + ingest API, consumer fleet).
type Server struct {
	Common

	// DatabaseURL connects as DBAppRole (audit_app: INSERT+SELECT only,
	// RLS-bound). MigrationDatabaseURL connects as audit_migrator, direct
	// to Postgres (the migration runner's advisory lock is session-scoped
	// and breaks under PgBouncer transaction pooling).
	DatabaseURL          string
	MigrationDatabaseURL string
	DBAppRole            string

	AppPort     string
	MetricsPort string

	Queues               []Queue
	SQSMaxReceiveCount   int
	SQSVisibilityTimeout time.Duration
	GlueRegistryRegion   string

	DefaultQueryWindowDays int
	CatalogBaseURL         string
	CatalogPollInterval    time.Duration
	CatalogPollTimeout     time.Duration
	MaxIngestBatch         int
	MaxMetadataBytes       int
	ExportSignedURLTTL     time.Duration

	// AL-6 per-tenant token bucket (LLD §10.5; decision D-5 — an LLD §12
	// addition).
	IngestBatchRateLimitRPS   int
	IngestBatchRateLimitBurst int

	// Archived-read sync bounds (decision D-10): an archived AL-1 read
	// estimated above either bound becomes an export (202).
	ArchiveSyncMaxRows  int64
	ArchiveSyncMaxBytes int64

	// Export API + worker (LLD §5.4 AL-3/AL-4; decisions D-2, D-11 — LLD
	// §12 additions).
	ExportDownloadURLTTL  time.Duration // per-poll presigned URL lifetime (D-11)
	ExportRateLimitPerMin int           // AL-3 per-tenant bucket (§10.5)
	ExportRateLimitBurst  int
	ExportPollInterval    time.Duration
	ExportJobLease        time.Duration
	ExportWorkDir         string // "" → os.TempDir()

	// Ops gauges from cmd/server (decision D-21, gaps 40/41).
	OpsStatsInterval       time.Duration // OPS_STATS_INTERVAL
	OpsStallGrace          time.Duration // OPS_ARCHIVE_STALL_GRACE: past eligibility before "stalled"
	OpsRedactionPendingAge time.Duration // OPS_REDACTION_PENDING_AGE: pending longer than this is stuck
}

// Reconciler is cmd/reconciler's configuration (archival, partitions, prune).
type Reconciler struct {
	Common

	// ReconcilerDatabaseURL connects as DBReconcilerRole (audit_reconciler:
	// BYPASSRLS, the sole DELETE / partition-DDL path — LLD §4.3, §10.4).
	ReconcilerDatabaseURL string
	DBReconcilerRole      string

	Job                    string
	Timeout                time.Duration
	ProcessedEventsTTLDays int

	// redaction-retry job (LLD §8.7, RB-7): a pending task older than
	// RedactionRetryMinAge is re-applied, up to RedactionRetryBatch per run.
	RedactionRetryMinAge time.Duration
	RedactionRetryBatch  int
	// redaction-sweep job (D-18): re-check subjects whose redaction
	// finished within this window (covers late rows within
	// AUDIT_WRITABLE_TRAILING_MONTHS).
	RedactionSweepWindow time.Duration

	// Archival (LLD §8.5, §15.4): rows per archive object part and the temp
	// dir parts are assembled in.
	ArchivePartMaxRows int
	ArchiveWorkDir     string
	// ProcessedEventsPruneBatch bounds each DELETE of the ledger prune.
	ProcessedEventsPruneBatch int
}

// IsDev reports whether appEnv permits local-dev placeholders.
func IsDev(appEnv string) bool {
	switch appEnv {
	case "dev", "development", "local", "test":
		return true
	}
	return false
}

func loadCommon(buildVersion string) Common {
	return Common{
		AppEnv:                 envOr("APP_ENV", "dev"),
		ServiceName:            envOr("SERVICE_NAME", "iam-audit-log"),
		BuildVersion:           envOr("BUILD_VERSION", buildVersion),
		AWSRegion:              envOr("AWS_REGION", "ap-south-1"),
		AWSEndpoint:            os.Getenv("AWS_ENDPOINT_URL"),
		ArchiveBucket:          envOr("AUDIT_ARCHIVE_BUCKET", "iam-audit-archive"),
		ArchiveKMSKey:          envOr("AUDIT_ARCHIVE_KMS_KEY", "alias/iam-audit-archive"),
		ArchiveObjectLockMode:  envOr("AUDIT_ARCHIVE_OBJECT_LOCK_MODE", "COMPLIANCE"),
		HotWindowDays:          envInt("AUDIT_HOT_WINDOW_DAYS", 90),
		PrecreateMonths:        envInt("AUDIT_PRECREATE_MONTHS", 3),
		WritableTrailingMonths: envInt("AUDIT_WRITABLE_TRAILING_MONTHS", 3),
	}
}

// LoadServer reads and validates cmd/server's environment. It returns every
// problem at once (fail-fast, matching the siblings' validateRequiredEnv).
func LoadServer(buildVersion string) (Server, error) {
	c := Server{
		Common:                 loadCommon(buildVersion),
		DatabaseURL:            os.Getenv("DATABASE_URL"),
		MigrationDatabaseURL:   os.Getenv("MIGRATION_DATABASE_URL"),
		DBAppRole:              envOr("DB_APP_ROLE", "audit_app"),
		AppPort:                envOr("APP_PORT", "8080"),
		MetricsPort:            envOr("METRICS_PORT", "9090"),
		SQSMaxReceiveCount:     envInt("SQS_MAX_RECEIVE_COUNT", 5),
		SQSVisibilityTimeout:   envDuration("SQS_VISIBILITY_TIMEOUT", 30*time.Second),
		GlueRegistryRegion:     envOr("GLUE_REGISTRY_REGION", "ap-south-1"),
		DefaultQueryWindowDays: envInt("AUDIT_DEFAULT_QUERY_WINDOW_DAYS", 365),
		CatalogBaseURL:         envOr("CATALOG_BASE_URL", "http://iam-catalog-admin.iam.svc.cluster.local:8080"),
		CatalogPollInterval:    envDuration("CATALOG_PLANS_POLL_INTERVAL", 600*time.Second),
		CatalogPollTimeout:     envDuration("CATALOG_PLANS_POLL_TIMEOUT", 3*time.Second),
		MaxIngestBatch:         envInt("MAX_INGEST_BATCH", 500),
		MaxMetadataBytes:       envInt("MAX_METADATA_BYTES", 8192),
		ExportSignedURLTTL:     envDuration("EXPORT_SIGNED_URL_TTL", 168*time.Hour),

		IngestBatchRateLimitRPS:   envInt("INGEST_BATCH_RATE_LIMIT_RPS", 10),
		IngestBatchRateLimitBurst: envInt("INGEST_BATCH_RATE_LIMIT_BURST", 20),

		ArchiveSyncMaxRows:    int64(envInt("ARCHIVE_SYNC_MAX_ROWS", 10000)),
		ArchiveSyncMaxBytes:   int64(envInt("ARCHIVE_SYNC_MAX_BYTES", 50<<20)),
		ExportDownloadURLTTL:  envDuration("EXPORT_DOWNLOAD_URL_TTL", 15*time.Minute),
		ExportRateLimitPerMin: envInt("EXPORT_RATE_LIMIT_PER_MINUTE", 10),
		ExportRateLimitBurst:  envInt("EXPORT_RATE_LIMIT_BURST", 5),
		ExportPollInterval:    envDuration("EXPORT_POLL_INTERVAL", 5*time.Second),
		ExportJobLease:        envDuration("EXPORT_JOB_LEASE", 15*time.Minute),
		ExportWorkDir:         os.Getenv("EXPORT_WORK_DIR"),

		OpsStatsInterval:       envDuration("OPS_STATS_INTERVAL", time.Minute),
		OpsStallGrace:          envDuration("OPS_ARCHIVE_STALL_GRACE", 48*time.Hour),
		OpsRedactionPendingAge: envDuration("OPS_REDACTION_PENDING_AGE", 15*time.Minute),
	}
	for _, q := range InboundQueues() {
		q.URL = os.Getenv(q.EnvVar)
		c.Queues = append(c.Queues, q)
	}

	var problems []string
	if c.DatabaseURL == "" {
		problems = append(problems, "DATABASE_URL: audit DB DSN for the audit_app role is required")
	}
	if c.MigrationDatabaseURL == "" && !IsDev(c.AppEnv) {
		problems = append(problems, "MIGRATION_DATABASE_URL: audit_migrator DSN (direct, not via PgBouncer) is required outside dev")
	}
	if !IsDev(c.AppEnv) {
		for _, q := range c.Queues {
			if q.URL == "" {
				problems = append(problems, q.EnvVar+": inbound queue URL for "+q.Name+" is required outside dev (LLD §7.1)")
			}
		}
	}
	if u, err := url.Parse(c.CatalogBaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		problems = append(problems, "CATALOG_BASE_URL: must be an absolute http(s) URL (CAT-I2 poller, AL-D15)")
	}
	if c.CatalogPollTimeout <= 0 || c.CatalogPollInterval <= c.CatalogPollTimeout {
		problems = append(problems, "CATALOG_PLANS_POLL_TIMEOUT: must be > 0 and shorter than CATALOG_PLANS_POLL_INTERVAL")
	}
	if c.ExportJobLease < time.Minute || c.ExportJobLease > 24*time.Hour {
		problems = append(problems, "EXPORT_JOB_LEASE: must be between 1m and 24h (claim_export_job bound)")
	}
	if c.ExportDownloadURLTTL > 7*24*time.Hour {
		problems = append(problems, "EXPORT_DOWNLOAD_URL_TTL: must not exceed 168h (SigV4 presign limit)")
	}
	problems = append(problems, validateCommon(c.Common)...)
	return c, joinProblems(problems)
}

// MigrationDSN returns the migration DSN, falling back to DatabaseURL in dev
// only (LoadServer rejects the fallback outside dev).
func (c Server) MigrationDSN() string {
	if c.MigrationDatabaseURL != "" {
		return c.MigrationDatabaseURL
	}
	return c.DatabaseURL
}

// LoadReconciler reads and validates cmd/reconciler's environment.
func LoadReconciler(buildVersion, job string) (Reconciler, error) {
	c := Reconciler{
		Common:                    loadCommon(buildVersion),
		ReconcilerDatabaseURL:     os.Getenv("RECONCILER_DATABASE_URL"),
		DBReconcilerRole:          envOr("DB_RECONCILER_ROLE", "audit_reconciler"),
		Job:                       job,
		Timeout:                   envDuration("RECONCILER_TIMEOUT", 30*time.Minute),
		ProcessedEventsTTLDays:    envInt("PROCESSED_EVENTS_TTL_DAYS", 8),
		RedactionRetryMinAge:      envDuration("REDACTION_RETRY_MIN_AGE", 5*time.Minute),
		RedactionRetryBatch:       envInt("REDACTION_RETRY_BATCH", 500),
		RedactionSweepWindow:      envDuration("REDACTION_SWEEP_WINDOW", 90*24*time.Hour),
		ArchivePartMaxRows:        envInt("ARCHIVE_PART_MAX_ROWS", 50000),
		ArchiveWorkDir:            os.Getenv("ARCHIVE_WORK_DIR"),
		ProcessedEventsPruneBatch: envInt("PROCESSED_EVENTS_PRUNE_BATCH", 10000),
	}
	c.ServiceName = envOr("SERVICE_NAME", "iam-audit-log") + "-reconciler"

	var problems []string
	if c.ReconcilerDatabaseURL == "" {
		problems = append(problems, "RECONCILER_DATABASE_URL: audit DB DSN for the audit_reconciler role is required")
	}
	if c.ProcessedEventsTTLDays <= 7 {
		problems = append(problems, "PROCESSED_EVENTS_TTL_DAYS: must exceed the 7-day SQS message lifetime (LLD §4.2)")
	}
	if c.RedactionRetryBatch < 1 || c.RedactionRetryBatch > 10000 {
		problems = append(problems, "REDACTION_RETRY_BATCH: must be between 1 and 10000")
	}
	if c.RedactionSweepWindow < 24*time.Hour || c.RedactionSweepWindow > 400*24*time.Hour {
		problems = append(problems, "REDACTION_SWEEP_WINDOW: must be between 24h and 9600h (sweep_redactions bound)")
	}
	if c.ArchivePartMaxRows < 1000 || c.ArchivePartMaxRows > 1000000 {
		problems = append(problems, "ARCHIVE_PART_MAX_ROWS: must be between 1000 and 1000000")
	}
	problems = append(problems, validateCommon(c.Common)...)
	return c, joinProblems(problems)
}

func validateCommon(c Common) []string {
	var problems []string
	switch c.ArchiveObjectLockMode {
	case "COMPLIANCE", "GOVERNANCE":
	default:
		problems = append(problems, "AUDIT_ARCHIVE_OBJECT_LOCK_MODE: must be COMPLIANCE or GOVERNANCE")
	}
	if !IsDev(c.AppEnv) && c.ArchiveObjectLockMode != "COMPLIANCE" {
		problems = append(problems, "AUDIT_ARCHIVE_OBJECT_LOCK_MODE: must be COMPLIANCE outside dev (LLD §10.4, §15.4)")
	}
	return problems
}

func joinProblems(problems []string) error {
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("startup aborted — required env vars missing or misconfigured:\n  • %s", strings.Join(problems, "\n  • "))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}
