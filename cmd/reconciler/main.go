// Package main is the reconciler composition root (LLD §3, §13): a CronJob
// entry point that selects one job by --job (or RECONCILER_JOB) and runs it
// once, singleton per run, on the audit_reconciler role — BYPASSRLS, the
// sole DELETE / partition-DDL path (LLD §4.3, §10.4).
//
// This binary never opens an audit_app pool (implementation rule 5), never
// runs migrations (cmd/server owns them), and never publishes (AL-INV-10).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"go.opentelemetry.io/otel"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/cmd/reconciler/jobs"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/metrics"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/config"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/logger"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgmetrics"
)

// buildVersion is injected by -ldflags at build time.
var buildVersion = "dev"

// jobRegistry is an indirection over jobs.Registry so tests can dispatch a
// stub job through the real run() path (the registry is empty until the
// Phase 1/7 jobs land).
var jobRegistry = jobs.Registry

// libMetricsOnce keeps run() re-entrant: pgmetrics init is not idempotent.
var libMetricsOnce sync.Once

func main() { os.Exit(run()) }

// run holds all deferred cleanup so every exit path runs it via a normal
// return rather than an os.Exit that would skip it.
func run() int {
	var jobName string
	// A local FlagSet (not the global one) so run() is re-entrant.
	fs := flag.NewFlagSet(os.Args[0], flag.ContinueOnError)
	fs.StringVar(&jobName, "job", os.Getenv("RECONCILER_JOB"), "reconciler job name")
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2
	}

	registry := jobRegistry()
	if jobName == "" {
		fmt.Fprintln(os.Stderr, "no job specified — pass --job=<name> or set RECONCILER_JOB")
		return 1
	}
	fn, ok := registry[jobName]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown job %q — valid: %v\n", jobName, names(registry))
		return 1
	}

	cfg, err := config.LoadReconciler(buildVersion, jobName)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}

	rawLog, err := logger.NewLogger(cfg.AppEnv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "init logger: "+err.Error())
		return 1
	}
	log := port.NewSlogStyleLogger(rawLog)
	shutdownTracing := gincommon.InitTracingFromEnv()
	defer shutdownTracing()
	defer func() {
		if err := gincommon.Shutdown(rawLog); err != nil {
			log.Error("logger/tracer flush error", "error", err.Error())
		}
	}()

	// No /metrics listener on a CronJob; collectors still exist because
	// jobs increment them (pushed via the platform's CronJob scrape path).
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{Logger: rawLog, ServiceName: cfg.ServiceName, BuildVersion: cfg.BuildVersion})
	metrics.Register(cfg.AppEnv)
	libMetricsOnce.Do(func() {
		pgmetrics.InitWithRegisterer(cfg.ServiceName, cfg.BuildVersion, gincommon.MetricsRegisterer())
	})

	ctx, cancel := context.WithTimeout(context.Background(), cfg.Timeout)
	defer cancel()
	ctx, span := otel.Tracer(cfg.ServiceName).Start(ctx, "reconciler."+jobName)
	defer span.End()

	pgCfg, pgWarnings := pgadapter.ReconcilerPoolConfig(cfg.ReconcilerDatabaseURL, rawLog, pgadapter.NewOTelTracer(cfg.ServiceName))
	for _, w := range pgWarnings {
		log.Warn("postgres config warning", "key", w.Key, "reason", w.Reason)
	}
	pool, err := pgcommon.NewPool(ctx, pgCfg)
	if err != nil {
		log.Error("connect to postgres", "error", err.Error())
		return 1
	}
	defer func() {
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancelDrain()
		if err := pool.DrainAndClose(drainCtx); err != nil {
			log.Error("pool drain error", "error", err.Error())
		}
	}()

	ri, err := pgadapter.CurrentRole(ctx, pool)
	if err == nil {
		err = pgadapter.CheckReconcilerRole(ri, cfg.DBReconcilerRole)
	}
	if err != nil {
		if !config.IsDev(cfg.AppEnv) {
			log.Error("reconciler DB role check failed", "error", err.Error())
			return 1
		}
		log.Warn("reconciler DB role check failed (tolerated in dev)", "error", err.Error())
	}

	log.Info("reconciler starting", "job", jobName)
	res, err := fn(ctx, &jobs.Context{
		Pool:                   pool,
		Logger:                 log,
		RawLogger:              rawLog,
		Partitions:             pgadapter.NewPartitionRepository(pool),
		Redactions:             pgadapter.NewRedactionRepository(pool),
		RedactionMetrics:       metrics.Redaction{},
		HotWindowDays:          cfg.HotWindowDays,
		PrecreateMonths:        cfg.PrecreateMonths,
		WritableTrailingMonths: cfg.WritableTrailingMonths,
		ProcessedEventsTTLDays: cfg.ProcessedEventsTTLDays,
		RedactionRetryMinAge:   cfg.RedactionRetryMinAge,
		RedactionRetryBatch:    cfg.RedactionRetryBatch,
		RedactionSweepWindow:   cfg.RedactionSweepWindow,
	})
	if err != nil {
		log.Error("reconciler job failed", "job", jobName, "error", err.Error())
		return 1
	}
	log.Info("reconciler complete", "job", jobName,
		"attempted", res.Attempted, "succeeded", res.Succeeded, "failed", res.Failed, "skipped", res.Skipped)
	return 0
}

func names(r map[string]jobs.Func) []string {
	out := make([]string, 0, len(r))
	for k := range r {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
