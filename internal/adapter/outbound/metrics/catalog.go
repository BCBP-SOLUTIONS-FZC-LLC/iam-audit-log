package metrics

import (
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// CatalogPoll implements port.PollMetrics for the CAT-I2 poller (AL-D15).
type CatalogPoll struct{}

// PollResult counts one poll and times the dependency call.
func (CatalogPoll) PollResult(result string, took time.Duration) {
	CatalogPlansPolls.WithLabelValues(result).Inc()
	Dependency{}.ObserveDependency(port.DependencyCatalogAdmin, port.OperationListPlans, result, took)
}

// PollSucceeded resets the staleness clock.
func (CatalogPoll) PollSucceeded(at time.Time) { catalogLastSuccess.Store(at.UnixNano()) }
