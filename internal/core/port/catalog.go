package port

import (
	"context"
	"errors"
	"time"
)

// ErrCatalogTimeout marks a CAT-I2 call that timed out (counted as
// result="timeout", LLD §11).
var ErrCatalogTimeout = errors.New("catalog plans poll timed out")

// CatalogPlan is the part of one CAT-I2 plan item this service uses.
type CatalogPlan struct {
	Code                 string
	AuditQueryWindowDays int
}

// PlanCatalog fetches Catalog's plan list (CAT-I2, GET /api/v1/internal/plans;
// AL-D15). versions is the response's record_versions map.
type PlanCatalog interface {
	Plans(ctx context.Context) (plans []CatalogPlan, versions map[string]int64, err error)
}

// PollMetrics records CAT-I2 poll outcomes (LLD §11: catalog_plans_poll_total,
// catalog_plans_stale_seconds, platform_dependency_request_seconds).
type PollMetrics interface {
	// PollResult counts one poll: result is success | error | timeout.
	PollResult(result string, took time.Duration)
	// PollSucceeded records the time of the last successful poll.
	PollSucceeded(at time.Time)
}
