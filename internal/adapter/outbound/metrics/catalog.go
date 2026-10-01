package metrics

import (
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// CatalogPoll implements port.PollMetrics for the CAT-I2 poller (AL-D15).
type CatalogPoll struct{}

// PollResult counts one poll (result success | error | timeout) and times
// the dependency call; the dependency outcome is the registry's
// success | error (a timeout is an error there; the poll counter keeps it).
func (CatalogPoll) PollResult(result string, took time.Duration) {
	CatalogPlansPolls.WithLabelValues(result).Inc()
	outcome := port.OutcomeError
	if result == port.OutcomeSuccess {
		outcome = port.OutcomeSuccess
	}
	Dependency{}.ObserveDependency(port.DependencyCatalogAdmin, port.OperationListPlans, outcome, took)
}

// PollSucceeded resets the staleness clock.
func (CatalogPoll) PollSucceeded(at time.Time) { catalogLastSuccess.Store(at.UnixNano()) }
