// Package main is the composition root for iam-audit-log's server process
// (LLD §3, §13): the Gin query + ingest API and the SQS consumer fleet,
// connected to the `audit` DB as audit_app (INSERT+SELECT only, RLS-bound).
//
// This binary never acquires the audit_reconciler role (implementation rule
// 5) and never wires an SNS publisher or outbox (AL-INV-10).
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	awsglue "github.com/aws/aws-sdk-go-v2/service/glue"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/inbound/consumer"
	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/inbound/http"
	glueadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/glue"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/metrics"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	s3adapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/s3"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/service"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/logger"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgmetrics"
)

// buildVersion is injected by -ldflags at build time (Dockerfile / Makefile).
var buildVersion = "dev"

// runMigrations is an indirection over pgadapter.RunMigrations so tests can
// drive the migration-failure exit path.
var runMigrations = pgadapter.RunMigrations

var libMetricsOnce sync.Once

func main() { os.Exit(run()) }

// run holds all deferred cleanup so every exit path runs it via a normal
// return rather than an os.Exit that would skip it.
func run() int {
	cfg, err := config.LoadServer(buildVersion)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	if !config.IsDev(cfg.AppEnv) {
		gin.SetMode(gin.ReleaseMode)
	}

	// ── 1. Logger + tracing (platform-gincommon) ──────────────────────────
	log, err := logger.NewLogger(cfg.AppEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "init logger: "+err.Error())
		return 1
	}
	shutdownTracing := gincommon.InitTracingFromEnv()
	defer shutdownTracing()
	defer func() {
		if err := gincommon.Shutdown(log); err != nil {
			log.Error("logger/tracer flush error", map[string]any{"error": err.Error()})
		}
	}()

	ginCfg := gincommon.Config{Logger: log, ServiceName: cfg.ServiceName, BuildVersion: cfg.BuildVersion}
	// Metrics init must precede any collector registration so business /
	// events / pg collectors share gincommon.MetricsRegisterer() and its
	// {service, version} const labels (sync.Once inside gincommon).
	_ = gincommon.ObservabilityMiddlewares(ginCfg)
	metrics.Register(cfg.AppEnv)
	// platform-events / pgmetrics init is not idempotent; guard it so run()
	// stays re-entrant (composition-root tests call it repeatedly).
	libMetricsOnce.Do(func() {
		events.InitWithRegisterer(cfg.ServiceName, cfg.BuildVersion, gincommon.MetricsRegisterer())
		pgmetrics.InitWithRegisterer(cfg.ServiceName, cfg.BuildVersion, gincommon.MetricsRegisterer())
	})

	warnForbiddenPublisherConfig(log)

	// ── 2. Migrations (audit_migrator, direct to Postgres — LLD §4.4) ─────
	ctx, cancelBackground := context.WithCancel(context.Background())
	defer cancelBackground()
	if err := runMigrations(ctx, cfg.MigrationDSN(), log); err != nil {
		log.Error("migrations failed", map[string]any{"error": err.Error()})
		return 1
	}

	// ── 3. audit_app pool (RLS GUC bound per checkout — LLD §3.3.2) ───────
	pgCfg, pgWarnings := pgadapter.AppPoolConfig(cfg.DatabaseURL, log, pgadapter.NewOTelTracer(cfg.ServiceName))
	for _, w := range pgWarnings {
		log.Warn("postgres config warning", map[string]any{"key": w.Key, "reason": w.Reason})
	}
	pool, err := pgcommon.NewPool(ctx, pgCfg)
	if err != nil {
		log.Error("connect to postgres", map[string]any{"error": err.Error()})
		return 1
	}
	defer pool.Close()
	if !appRoleOK(ctx, pool, cfg, log) {
		return 1
	}

	// ── 3b. Partition pre-creation (LLD §4.4) — defensive, so a missed
	// reconciler run never blocks ingestion. A failure is logged, not
	// fatal: audit_events_default still accepts every row meanwhile.
	partitions, err := service.NewPartitionService(pgadapter.NewPartitionRepository(pool), log,
		cfg.PrecreateMonths, cfg.WritableTrailingMonths)
	if err != nil {
		log.Error("invalid partition window", map[string]any{"error": err.Error()})
		return 1
	}
	if _, err := partitions.EnsureAhead(ctx); err != nil {
		log.Error("startup partition pre-creation failed — rows will land in audit_events_default until the reconciler runs", map[string]any{"error": err.Error()})
	}

	// ── 3c. Ingest core (LLD §5.4, AL-D1) — the single write path shared by
	// AL-5/AL-6 now and the Phase 3 consumer fleet (AL-INV-2).
	ingest := service.NewIngestService(pgadapter.NewAuditRepository(pool), newUUIDv7,
		cfg.MaxMetadataBytes, cfg.MaxIngestBatch, log)

	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.AWSRegion))
	if err != nil {
		log.Error("load aws config", map[string]any{"error": err.Error()})
		return 1
	}

	// ── 3d. SQS consumer fleet (LLD §7.1) — one platform-events consumer per
	// inbound queue, all converging on the same ingest path (AL-INV-2).
	fleet, err := buildFleet(awsCfg, cfg, ingest.WithBus(service.BusIngestConfig{
		Plans:             pgadapter.NewPlanWindowRepository(pool),
		DefaultWindowDays: cfg.DefaultQueryWindowDays,
	}), log)
	if err != nil {
		log.Error("build consumer fleet", map[string]any{"error": err.Error()})
		return 1
	}
	fleet.Start(ctx, func(queue string, err error) {
		log.Error("SQS consumer stopped", map[string]any{"queue": queue, "error": err.Error()})
	})
	log.Info("consumer fleet started", map[string]any{"consumers": fleet.Len()})

	// ── 3e. Query + export (LLD §5.4 AL-1..AL-4; decisions D-2, D-10..D-12).
	// The export worker runs here as audit_app and claims jobs through the
	// claim_export_job() definer function (D-2).
	store := buildStore(awsCfg, cfg)
	reader := pgadapter.NewQueryRepository(pool)
	query := service.NewQueryService(reader, store, service.QueryConfig{
		DefaultWindowDays: cfg.DefaultQueryWindowDays,
		SyncMaxRows:       cfg.ArchiveSyncMaxRows,
		SyncMaxBytes:      cfg.ArchiveSyncMaxBytes,
	}, log)
	exports := service.NewExportService(pgadapter.NewExportRepository(pool), reader, store, store, query, newUUIDv7,
		service.ExportConfig{
			SignedURLTTL:   cfg.ExportSignedURLTTL,
			DownloadURLTTL: cfg.ExportDownloadURLTTL,
			PollInterval:   cfg.ExportPollInterval,
			Lease:          cfg.ExportJobLease,
			WorkDir:        cfg.ExportWorkDir,
		}, log)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		exports.Run(ctx)
	}()

	// ── 4. Router ─────────────────────────────────────────────────────────
	router := httpadapter.NewRouter(httpadapter.RouterConfig{
		GinConfig: ginCfg,
		Ingest:    httpadapter.NewIngestHandler(ingest),
		Query: httpadapter.NewQueryHandler(query, exports,
			httpadapter.NewTenantRateLimiterPerMinute(cfg.ExportRateLimitPerMin, cfg.ExportRateLimitBurst)),
		BatchLimiter: httpadapter.NewTenantRateLimiter(cfg.IngestBatchRateLimitRPS, cfg.IngestBatchRateLimitBurst),
		Docs: httpadapter.DocsConfig{
			Environment: cfg.AppEnv,
			Enabled:     os.Getenv("DOCS_ENABLED") == "true",
			AuthToken:   os.Getenv("DOCS_AUTH_TOKEN"),
		},
		Ready: map[string]httpadapter.Pinger{
			"consumers": fleet,
			"database": pingerFunc(func(ctx context.Context) error {
				if hs := pool.Health(ctx); !hs.Healthy {
					return errors.New("database not healthy")
				}
				return nil
			}),
		},
	})

	// ── 5. Listeners + graceful shutdown ─────────────────────────────────
	srv := &http.Server{
		Addr:         ":" + cfg.AppPort,
		Handler:      router.Handler(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 35 * time.Second,
		IdleTimeout:  60 * time.Second,
	}
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", promhttp.Handler())
	metricsServer := &http.Server{Addr: ":" + cfg.MetricsPort, Handler: metricsMux, ReadHeaderTimeout: 5 * time.Second}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	log.Info("service starting", map[string]any{
		"service": cfg.ServiceName, "version": cfg.BuildVersion, "env": cfg.AppEnv, "addr": srv.Addr,
	})
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("server error", map[string]any{"error": err.Error()})
			quit <- syscall.SIGTERM
		}
	}()
	go func() {
		if err := metricsServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("metrics server error", map[string]any{"error": err.Error()})
		}
	}()

	<-quit
	log.Info("shutdown signal received — draining", nil)

	// Order (mirrors iam-realm-provisioner): stop accepting HTTP → metrics →
	// consumers → background work → drain pool; tracing + logger flush run
	// from the defers above.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error("HTTP server shutdown error", map[string]any{"error": err.Error()})
	}
	if err := metricsServer.Shutdown(shutdownCtx); err != nil {
		log.Error("metrics server shutdown error", map[string]any{"error": err.Error()})
	}
	// In-flight handlers finish their DB writes before the pool drains; an
	// unfinished message is simply redelivered (AL-INV-4).
	if err := fleet.Stop(); err != nil {
		log.Error("consumer fleet stop error", map[string]any{"error": err.Error()})
	}
	cancelBackground()
	<-workerDone // an interrupted export records its failure before the pool drains
	if err := pool.DrainAndClose(shutdownCtx); err != nil {
		log.Error("pool drain error", map[string]any{"error": err.Error()})
	}
	return 0
}

// consumerConcurrency is each queue's handler concurrency. Audit volume is
// ~5,000 events/day (HLD §14.1) with offboarding bursts absorbed by
// redelivery; the LLD fixes no per-queue knob.
const consumerConcurrency = 4

// buildFleet wires one platform-events consumer per configured queue. This
// is the only place a raw *sqs.Client is built (arch-lint), solely to hand
// to events.NewSQSConsumerWithClient; the Glue codec resolves schema
// versions read-only (glue:GetSchemaVersion, §7.3.1).
func buildFleet(awsCfg aws.Config, cfg config.Server, ingest consumer.BusIngester, log interface {
	Debug(string, map[string]any)
	Info(string, map[string]any)
	Warn(string, map[string]any)
	Error(string, map[string]any)
}) (*consumer.Fleet, error) {
	var sqsOpts []func(*sqs.Options)
	glueOpts := []func(*awsglue.Options){func(o *awsglue.Options) { o.Region = cfg.GlueRegistryRegion }}
	if cfg.AWSEndpoint != "" {
		ep := cfg.AWSEndpoint
		sqsOpts = append(sqsOpts, func(o *sqs.Options) { o.BaseEndpoint = &ep })
		glueOpts = append(glueOpts, func(o *awsglue.Options) { o.BaseEndpoint = &ep })
	}
	sqsClient := sqs.NewFromConfig(awsCfg, sqsOpts...)
	codec := glueadapter.NewCodec(glueadapter.NewRegistryResolver(awsglue.NewFromConfig(awsCfg, glueOpts...)))

	queues := make([]consumer.Queue, 0, len(cfg.Queues))
	for _, q := range cfg.Queues {
		if q.URL == "" {
			log.Warn(q.Name+" consumer disabled — queue URL unset", nil)
		}
		queues = append(queues, consumer.Queue{Name: q.Name, URL: q.URL, Topic: q.Topic, Consumer: q.Consumer, Concurrency: consumerConcurrency})
	}
	build := func(url string, h events.Handler, opts ...events.ConsumerOption) (events.Consumer, error) {
		return events.NewSQSConsumerWithClient(
			events.SQSConfig{QueueURL: url, Region: cfg.AWSRegion, Logger: log}, sqsClient, h, opts...)
	}
	return consumer.NewFleet(queues, build, codec, metrics.Consumer{},
		func(q consumer.Queue) events.Handler { return consumer.Handler(q, ingest) },
		events.WithVisibilityTimeout(cfg.SQSVisibilityTimeout))
}

// buildStore wires the S3 archive/export adapter. Against an emulator
// endpoint (AWS_ENDPOINT_URL — floci/LocalStack) it uses path-style
// addressing and omits SSE-KMS, which the emulator has no key for; in AWS
// every export is SSE-KMS encrypted with AUDIT_ARCHIVE_KMS_KEY (§5.4).
func buildStore(awsCfg aws.Config, cfg config.Server) *s3adapter.Store {
	kmsKey := cfg.ArchiveKMSKey
	var opts []func(*awss3.Options)
	if cfg.AWSEndpoint != "" {
		ep := cfg.AWSEndpoint
		opts = append(opts, func(o *awss3.Options) { o.BaseEndpoint = &ep; o.UsePathStyle = true })
		kmsKey = ""
	}
	return s3adapter.New(awss3.NewFromConfig(awsCfg, opts...), cfg.ArchiveBucket, kmsKey)
}

// newUUIDv7 mints audit_events.id (UUIDv7: recorded order within a
// partition, LLD §4.2).
func newUUIDv7() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

// pingerFunc adapts a plain func to httpadapter.Pinger.
type pingerFunc func(context.Context) error

func (f pingerFunc) Health(ctx context.Context) error { return f(ctx) }

type warnLogger interface {
	Warn(msg string, fields map[string]any)
}

// warnForbiddenPublisherConfig logs a startup warning if publisher-side
// config is present (LLD §3.3.3). The values are never read further: this
// service has no publisher and no outbox (AL-INV-10). Unlike
// iam-authz-enrichment it does not call platform-events' LoadSNS/LoadOutbox
// even for parity — CI forbids them (check-forbidden-events-bypass.sh).
func warnForbiddenPublisherConfig(log warnLogger) {
	for _, key := range []string{"SNS_TOPIC_ARN", "OUTBOX_DATABASE_URL"} {
		if os.Getenv(key) != "" {
			log.Warn(key+" is set but ignored — iam-audit-log publishes no bus events (AL-INV-10)", nil)
		}
	}
}

// appRoleOK asserts the pool authenticates as DB_APP_ROLE without
// BYPASSRLS. Fatal outside dev; a warning in dev, where a local superuser
// DSN is common.
func appRoleOK(ctx context.Context, pool *pgcommon.Pool, cfg config.Server, log interface {
	Warn(string, map[string]any)
	Error(string, map[string]any)
}) bool {
	ri, err := pgadapter.CurrentRole(ctx, pool)
	if err == nil {
		err = pgadapter.CheckAppRole(ri, cfg.DBAppRole)
	}
	if err == nil {
		return true
	}
	if config.IsDev(cfg.AppEnv) {
		log.Warn("app DB role check failed (tolerated in dev)", map[string]any{"error": err.Error()})
		return true
	}
	log.Error("app DB role check failed", map[string]any{"error": err.Error()})
	return false
}
