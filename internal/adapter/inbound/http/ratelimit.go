package http

import (
	"sync"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/pkg/requestctx"
)

// TenantRateLimiter is an in-process per-tenant token bucket (LLD §10.5;
// decision D-5), keyed on the caller's x-tenant-id. Each replica limits
// independently; the key space is the tenant count, so buckets are kept
// for the process lifetime.
type TenantRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rate.Limiter
	rps     rate.Limit
	burst   int
}

// NewTenantRateLimiter allows rps sustained with the given burst per tenant.
func NewTenantRateLimiter(rps, burst int) *TenantRateLimiter {
	return &TenantRateLimiter{buckets: map[string]*rate.Limiter{}, rps: rate.Limit(rps), burst: burst}
}

// NewTenantRateLimiterPerMinute allows perMinute sustained with the given
// burst per tenant (AL-3 export creation, §10.5).
func NewTenantRateLimiterPerMinute(perMinute, burst int) *TenantRateLimiter {
	return &TenantRateLimiter{buckets: map[string]*rate.Limiter{}, rps: rate.Limit(float64(perMinute) / 60), burst: burst}
}

// Allow consumes one token from tenant's bucket.
func (l *TenantRateLimiter) Allow(tenant string) bool { return l.bucket(tenant).Allow() }

func (l *TenantRateLimiter) bucket(tenant string) *rate.Limiter {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[tenant]
	if !ok {
		b = rate.NewLimiter(l.rps, l.burst)
		l.buckets[tenant] = b
	}
	return b
}

// Middleware answers 429 rate_limited once a tenant's bucket is empty.
func (l *TenantRateLimiter) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := ""
		if rc, ok := requestctx.FromContext(c.Request.Context()); ok {
			key = rc.TenantID.String()
		}
		if !l.bucket(key).Allow() {
			c.Header("Retry-After", "1")
			abort(c, domain.ErrRateLimited, "rate limit exceeded for this tenant")
			return
		}
		c.Next()
	}
}
