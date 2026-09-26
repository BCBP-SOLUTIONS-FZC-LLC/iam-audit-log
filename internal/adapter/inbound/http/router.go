// Package http implements the inbound HTTP surface (LLD §5): the admin-only
// query/export API under /api/v1/audit/* and the mesh-only ingest + read API
// under /api/v1/internal/*. router.go is the single source of truth for the
// route table and middleware order (LLD §3.3.1).
package http

import (
	"context"
	"crypto/subtle"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// Pinger is satisfied by any dependency /readyz must check.
type Pinger interface {
	Health(ctx context.Context) error
}

// DocsConfig controls the AsyncAPI surface: always on outside production;
// in production opt-in via Enabled and, if AuthToken is set, bearer-gated.
type DocsConfig struct {
	Environment string
	Enabled     bool
	AuthToken   string
}

func (d DocsConfig) active() bool { return d.Environment != "production" || d.Enabled }

// RouteRegistrar mounts a phase's handlers onto an already-authorized group.
type RouteRegistrar func(g *gin.RouterGroup)

// RouterConfig bundles every dependency NewRouter needs.
type RouterConfig struct {
	GinConfig gincommon.Config
	Docs      DocsConfig

	// Readiness checks, by name (LLD §11 Health: Postgres reachable + each
	// SQS consumer connected). A nil entry is skipped.
	Ready map[string]Pinger

	// Ingest serves AL-5/AL-6 (nil → not mounted).
	Ingest *IngestHandler
	// BatchLimiter rate-limits AL-6 per tenant (§10.5, D-5); nil → unlimited.
	BatchLimiter *TenantRateLimiter

	// Query serves AL-1..AL-4 (nil → not mounted).
	Query *QueryHandler

	// Audit mounts extra routes under /api/v1/audit (tenant_admin/owner).
	Audit RouteRegistrar
	// Internal mounts AL-5..AL-7 under /api/v1/internal (mesh peers).
	Internal RouteRegistrar
}

// Router owns the Gin engine for this service.
type Router struct{ engine *gin.Engine }

// Handler returns the http.Handler to serve.
func (r *Router) Handler() http.Handler { return r.engine }

// maxBodyBytes caps request bodies: AL-6 carries up to MAX_INGEST_BATCH
// (500) entries of ≤ 8 KiB metadata each.
const maxBodyBytes = 8 << 20

// NewRouter builds the engine. Middleware order (LLD §3.3.1):
//
//	TimeoutMiddleware → Observability (PanicRecovery → RequestID → Tracing →
//	CorrelationHeaders → Metrics → Logging)
//	  /api/v1/audit/*    → RequireAuth → Context → IdentityBridge → RequireAuditReader
//	  /api/v1/internal/* → RequireAuth → Context → IdentityBridge → RequireSystemRole
//
// /healthz, /readyz and the AsyncAPI docs are registered before auth.
// /metrics is served on its own METRICS_PORT listener, not here.
func NewRouter(cfg RouterConfig) *Router {
	errorLogger = cfg.GinConfig.Logger

	r := gin.New()
	r.HandleMethodNotAllowed = true
	r.Use(func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
		c.Next()
	})
	r.Use(gincommon.TimeoutMiddleware(30 * time.Second))
	r.Use(gincommon.ObservabilityMiddlewares(cfg.GinConfig)...)

	registerInfraRoutes(r, cfg)
	registerDocsRoutes(r, cfg)

	protected := append(gincommon.ProtectedMiddlewares(cfg.GinConfig), IdentityBridgeMiddleware(), RequireJSONContentType())

	audit := r.Group("/api/v1/audit", append(protected, RequireAuditReader())...)
	if cfg.Query != nil {
		audit.GET("/events", cfg.Query.ListEvents)     // AL-1
		audit.GET("/events/:id", cfg.Query.GetEvent)   // AL-2
		audit.POST("/exports", cfg.Query.CreateExport) // AL-3 (rate-limited in-handler, §10.5)
		audit.GET("/exports/:id", cfg.Query.GetExport) // AL-4
	}
	if cfg.Audit != nil {
		cfg.Audit(audit)
	}
	internal := r.Group("/api/v1/internal", append(protected, RequireSystemRole())...)
	if cfg.Ingest != nil {
		internal.POST("/audit-entries", RequireIdempotencyKey(), cfg.Ingest.CreateEntry) // AL-5
		batch := []gin.HandlerFunc{RequireIdempotencyKey()}
		if cfg.BatchLimiter != nil {
			batch = append(batch, cfg.BatchLimiter.Middleware())
		}
		// Escaped colon: gin routes the literal LLD path …/audit-entries:batch.
		internal.POST(`/audit-entries\:batch`, append(batch, cfg.Ingest.CreateBatch)...) // AL-6
	}
	if cfg.Internal != nil {
		cfg.Internal(internal)
	}
	return &Router{engine: r}
}

func registerInfraRoutes(r *gin.Engine, cfg RouterConfig) {
	r.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	r.GET("/readyz", readyzHandler(cfg.Ready))
}

// readyzHandler runs every readiness check concurrently so one slow check
// cannot consume the probe's whole timeout budget.
func readyzHandler(checks map[string]Pinger) gin.HandlerFunc {
	return func(c *gin.Context) {
		type result struct {
			name string
			ok   bool
		}
		results := make(chan result, len(checks))
		pending := 0
		for name, p := range checks {
			if p == nil {
				continue
			}
			pending++
			go func(name string, p Pinger) {
				results <- result{name: name, ok: p.Health(c.Request.Context()) == nil}
			}(name, p)
		}
		healthy := true
		out := gin.H{}
		for range pending {
			res := <-results
			if res.ok {
				out[res.name] = "ok"
			} else {
				out[res.name] = "down"
				healthy = false
			}
		}
		if !healthy {
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "not ready", "checks": out})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ready", "checks": out})
	}
}

func registerDocsRoutes(r *gin.Engine, cfg RouterConfig) {
	if !cfg.Docs.active() {
		return
	}
	auth := func(c *gin.Context) { c.Next() }
	if cfg.Docs.Environment == "production" && cfg.Docs.AuthToken != "" {
		expected := []byte("Bearer " + cfg.Docs.AuthToken)
		auth = func(c *gin.Context) {
			if subtle.ConstantTimeCompare([]byte(c.GetHeader("Authorization")), expected) != 1 {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
				return
			}
			c.Next()
		}
	}
	sec := func(c *gin.Context) {
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Next()
	}
	r.GET("/asyncapi", sec, auth, AsyncAPIHandler)
	r.GET("/asyncapi.yaml", sec, auth, AsyncAPIYAMLHandler)
}
