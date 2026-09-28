package service

import (
	"context"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// OpsMonitorConfig tunes OpsMonitor.
type OpsMonitorConfig struct {
	Interval time.Duration // OPS_STATS_INTERVAL
	Query    domain.OpsStatsQuery
	// Queues are the inbound queues: each one's depth and its DLQ's depth
	// (<URL>-dlq, LLD §25) are published under the queue's name.
	Queues []OpsQueue
}

// OpsQueue is one inbound queue probed for depth.
type OpsQueue struct {
	Name   string // e.g. "user-audit-q"
	URL    string
	DLQURL string
}

// OpsMonitor publishes the operational metrics that alerting depends on from
// cmd/server, which is always scraped (decision D-21, gaps 40/41):
//   - archival stall/lag, DEFAULT-partition rows and stuck redaction tasks
//     from audit_ops_stats();
//   - platform_queue_depth / platform_dlq_depth per inbound queue;
//   - iam_rls_violations_total from audit_rls_violation_counts() (each
//     logged violation exactly once fleet-wide).
//
// A failed read keeps the last published value and is logged; it never
// stops the loop.
type OpsMonitor struct {
	stats   port.OpsStatsReader
	queues  port.QueueDepths
	metrics port.OpsMetrics
	cfg     OpsMonitorConfig
	log     port.Logger
}

// NewOpsMonitor wires the monitor; queues may be nil (no DLQ gauges).
func NewOpsMonitor(stats port.OpsStatsReader, queues port.QueueDepths, metrics port.OpsMetrics, cfg OpsMonitorConfig, log port.Logger) *OpsMonitor {
	if cfg.Interval <= 0 {
		cfg.Interval = time.Minute
	}
	return &OpsMonitor{stats: stats, queues: queues, metrics: metrics, cfg: cfg, log: log}
}

// Run collects immediately, then every Interval, until ctx is cancelled.
func (m *OpsMonitor) Run(ctx context.Context) {
	t := time.NewTicker(m.cfg.Interval)
	defer t.Stop()
	for {
		m.Collect(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Collect runs one collection pass.
func (m *OpsMonitor) Collect(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, m.cfg.Interval)
	defer cancel()
	if s, err := m.stats.OpsStats(cctx, m.cfg.Query); err != nil {
		m.warn("ops stats read failed — gauges keep their last value", map[string]any{"error": err.Error()})
	} else {
		m.metrics.SetOpsStats(s)
	}
	if counts, err := m.stats.RLSViolations(cctx); err != nil {
		m.warn("RLS violation count read failed — retried next pass", map[string]any{"error": err.Error()})
	} else {
		for t, n := range counts {
			if n > 0 {
				m.metrics.AddRLSViolations(t, n)
			}
		}
	}
	if m.queues == nil {
		return
	}
	for _, q := range m.cfg.Queues {
		if d, err := m.queues.Depth(cctx, q.URL); err != nil {
			m.warn("queue depth read failed — gauge keeps its last value", map[string]any{"queue": q.Name, "error": err.Error()})
		} else {
			m.metrics.SetQueueDepth(q.Name, d)
		}
		if d, err := m.queues.Depth(cctx, q.DLQURL); err != nil {
			m.warn("DLQ depth read failed — gauge keeps its last value", map[string]any{"queue": q.Name, "error": err.Error()})
		} else {
			m.metrics.SetDLQDepth(q.Name, d)
		}
	}
}

func (m *OpsMonitor) warn(msg string, f map[string]any) {
	if m.log != nil {
		m.log.Warn(msg, f)
	}
}
