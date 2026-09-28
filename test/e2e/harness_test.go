//go:build e2e

// Package e2e_test spins up the iam-audit-log HTTP stack against a real
// PostgreSQL 15 testcontainer and exposes it over an httptest.Server, the
// way iam-org-membership/test/e2e does. Tests hit it with the same
// gateway-/mesh-forwarded identity headers production callers send.
//
// Design:
//   - Postgres 15 container per test, audit_app / audit_reconciler roles
//     (test/fixtures), embedded migrations applied.
//   - The real router (httpadapter.NewRouter) and middleware chain, a real
//     audit_app pgcommon pool (RLS-bound, transaction-local GUC), and a
//     /readyz database check — the same wiring as cmd/server/main.go.
//   - Phase 0 has no AL-* handlers yet, so each protected group mounts one
//     probe route that runs a real query through the app pool and echoes
//     the app.tenant_id Postgres actually saw — proving identity headers
//     travel end-to-end to the RLS GUC (AL-INV-3). Phase 2/4 replace the
//     probes with the real AL-1..AL-7 flows.
//   - process_test.go additionally builds and runs the real binaries.
package e2e_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	httpadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/inbound/http"
	catalogadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/catalog"
	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	s3adapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/s3"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/test/fixtures"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/pkg/pgcommon"
)

const (
	e2eMaxBatch   = 5
	e2eBatchBurst = 3
	// e2eExportBurst is generous: the suite asserts AL-3 behavior, not its limiter.
	e2eExportBurst = 20

	tenantA     = "11111111-1111-1111-1111-111111111111"
	tenantB     = "22222222-2222-2222-2222-222222222222"
	adminUser   = "33333333-3333-3333-3333-333333333333"
	systemActor = "00000000-0000-0000-0000-0000000000a1"
	platformTen = "00000000-0000-0000-0000-0000000000b1"
)

type e2eEnv struct {
	roles   fixtures.Roles
	appPool *pgcommon.Pool
	server  *httptest.Server
}

type pingerFunc func(context.Context) error

func (f pingerFunc) Health(ctx context.Context) error { return f(ctx) }

func newE2EEnv(t *testing.T) *e2eEnv {
	t.Helper()
	return newE2EEnvWithS3(t, nil)
}

// e2eBucket is the archive/export bucket scripts/init-floci.sh creates.
const e2eBucket = "iam-audit-archive"

// newE2EEnvWithS3 additionally mounts the real AL-1..AL-4 stack (query
// service, export service + in-process worker, as cmd/server wires it,
// decision D-2). awsCfg nil → an S3 client pointed at an unreachable
// endpoint: fine for tests that never archive or export.
func newE2EEnvWithS3(t *testing.T, awsCfg *aws.Config) *e2eEnv {
	t.Helper()
	return newE2EEnvOpts(t, e2eOpts{aws: awsCfg})
}

// e2eOpts selects optional real dependencies for the e2e stack.
type e2eOpts struct {
	aws *aws.Config // nil → unreachable S3 endpoint
	// catalogURL, when set, wires a real CAT-I2 PlanPoller (AL-D15) against
	// it as QueryConfig.Windows, polling every e2ePollInterval.
	catalogURL string
}

// e2ePollInterval is the CAT-I2 poll cadence in e2e (production: 600s).
const e2ePollInterval = 200 * time.Millisecond

func newE2EEnvOpts(t *testing.T, o e2eOpts) *e2eEnv {
	t.Helper()
	awsCfg := o.aws
	gin.SetMode(gin.TestMode)
	ctx := context.Background()
	roles := fixtures.CreateRoles(t, fixtures.StartPostgres(t))
	require.NoError(t, pgadapter.RunMigrations(ctx, roles.SuperDSN, nil))

	cfg, _ := pgadapter.AppPoolConfig(roles.AppDSN, nil, nil)
	pool, err := pgcommon.NewPool(ctx, cfg)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	ginCfg := gincommon.Config{ServiceName: "iam-audit-log", BuildVersion: "e2e"}
	// Real ingest stack (AL-5/AL-6), as cmd/server wires it; small batch and
	// rate limits so the e2e suite can exercise 422/429 cheaply.
	ingest := service.NewIngestService(pgadapter.NewAuditRepository(pool), func() (string, error) {
		id, err := uuid.NewV7()
		return id.String(), err
	}, 8192, e2eMaxBatch, nil)
	newID := func() (string, error) {
		id, err := uuid.NewV7()
		return id.String(), err
	}
	cfgS3 := aws.Config{Region: "ap-south-1", Credentials: credentials.NewStaticCredentialsProvider("test", "test", "")}
	endpoint := "http://127.0.0.1:1"
	if awsCfg != nil {
		cfgS3, endpoint = *awsCfg, *awsCfg.BaseEndpoint
	}
	store := s3adapter.New(awss3.NewFromConfig(cfgS3, func(o *awss3.Options) {
		o.BaseEndpoint, o.UsePathStyle = &endpoint, true
	}), e2eBucket, "")
	reader := pgadapter.NewQueryRepository(pool)
	qcfg := service.QueryConfig{DefaultWindowDays: 365, SyncMaxRows: 10000, SyncMaxBytes: 50 << 20}
	if o.catalogURL != "" {
		plans := service.NewPlanPoller(catalogadapter.New(o.catalogURL, time.Second), nil,
			service.PlanPollerConfig{Interval: e2ePollInterval, Timeout: time.Second}, nil)
		qcfg.Windows = plans
		pollCtx, stopPoll := context.WithCancel(ctx)
		pollDone := make(chan struct{})
		go func() {
			defer close(pollDone)
			plans.Run(pollCtx)
		}()
		t.Cleanup(func() { stopPoll(); <-pollDone }) // LIFO: before pool.Close
	}
	query := service.NewQueryService(reader, store, qcfg, nil)
	exports := service.NewExportService(pgadapter.NewExportRepository(pool), reader, store, store, query, newID,
		service.ExportConfig{
			SignedURLTTL: 7 * 24 * time.Hour, DownloadURLTTL: 15 * time.Minute,
			PollInterval: 200 * time.Millisecond, Lease: time.Minute, WorkDir: t.TempDir(),
		}, nil)
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		exports.Run(workerCtx)
	}()
	t.Cleanup(func() { stopWorker(); <-workerDone }) // LIFO: before pool.Close

	router := httpadapter.NewRouter(httpadapter.RouterConfig{
		GinConfig:    ginCfg,
		Ingest:       httpadapter.NewIngestHandler(ingest),
		Query:        httpadapter.NewQueryHandler(query, exports, httpadapter.NewTenantRateLimiterPerMinute(60, e2eExportBurst)),
		BatchLimiter: httpadapter.NewTenantRateLimiter(1, e2eBatchBurst),
		Docs:         httpadapter.DocsConfig{Environment: "test"},
		Ready: map[string]httpadapter.Pinger{
			"database": pingerFunc(func(ctx context.Context) error {
				if hs := pool.Health(ctx); !hs.Healthy {
					return errors.New("database not healthy")
				}
				return nil
			}),
		},
		Audit:    func(g *gin.RouterGroup) { g.GET("/probe", gucProbe(pool)) },
		Internal: func(g *gin.RouterGroup) { g.POST("/probe", httpadapter.RequireIdempotencyKey(), gucProbe(pool)) },
	})
	srv := httptest.NewServer(router.Handler())
	t.Cleanup(srv.Close)
	return &e2eEnv{roles: roles, appPool: pool, server: srv}
}

// gucProbe reports the session role and the app.tenant_id Postgres sees
// inside a real transaction on the audit_app pool.
func gucProbe(pool *pgcommon.Pool) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.ContentLength != 0 {
			var mbe *http.MaxBytesError
			if _, err := io.ReadAll(c.Request.Body); errors.As(err, &mbe) {
				c.AbortWithStatus(http.StatusRequestEntityTooLarge)
				return
			}
		}
		var role, tenant string
		err := pgadapter.NewTxRunner(pool).RunInTx(c.Request.Context(), func(ctx context.Context) error {
			tx, _ := pgadapter.TxFromContext(ctx)
			return tx.QueryRow(ctx,
				`SELECT current_user::text, coalesce(current_setting('app.tenant_id', true), '')`,
			).Scan(&role, &tenant)
		})
		if err != nil {
			httpadapter.HandleError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"db_role": role, "db_tenant": tenant})
	}
}

type reqOpts struct {
	method, path, body string
	headers            map[string]string
}

func (e *e2eEnv) do(t *testing.T, o reqOpts) (int, http.Header, string) {
	t.Helper()
	if o.method == "" {
		o.method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(context.Background(), o.method, e.server.URL+o.path, strings.NewReader(o.body))
	require.NoError(t, err)
	for k, v := range o.headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func gatewayHeaders(user, tenant, roles string) map[string]string {
	return map[string]string{"x-user-id": user, "x-tenant-id": tenant, "x-tenant-roles": roles}
}

func adminHeaders(tenant string) map[string]string {
	return gatewayHeaders(adminUser, tenant, "tenant_admin")
}

func systemHeaders(tenant string) map[string]string {
	return gatewayHeaders(systemActor, tenant, "iam-system")
}
