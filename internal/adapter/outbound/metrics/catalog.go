package metrics

import "time"

// CatalogPoll implements port.PollMetrics for the CAT-I2 poller (AL-D15).
type CatalogPoll struct{}

// PollResult counts one poll and times the dependency call.
func (CatalogPoll) PollResult(result string, took time.Duration) {
	CatalogPlansPolls.WithLabelValues(result).Inc()
	DependencyRequestSeconds.WithLabelValues("iam-catalog-admin", "plans").Observe(took.Seconds())
}

// PollSucceeded resets the staleness clock.
func (CatalogPoll) PollSucceeded(at time.Time) { catalogLastSuccess.Store(at.UnixNano()) }
