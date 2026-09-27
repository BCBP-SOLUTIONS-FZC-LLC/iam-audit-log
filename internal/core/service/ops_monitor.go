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
	// DLQs maps a queue label (e.g. "user-audit-q-dlq") to its URL.
	DLQs map[string]string
}

// OpsMonitor publishes the operational gauges that alerting depends on from
// cmd/server, which is always scraped (decision D-21, gaps 40/41): archival
// stall/lag, DEFAULT-partition rows and stuck redaction tasks from
// audit_ops_stats(), plus each DLQ's depth. A failed read keeps the last
// published value and is logged; it never stops the loop.
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
	if m.queues == nil {
		return
	}
	for name, url := range m.cfg.DLQs {
		d, err := m.queues.Depth(cctx, url)
		if err != nil {
			m.warn("DLQ depth read failed — gauge keeps its last value", map[string]any{"queue": name, "error": err.Error()})
			continue
		}
		m.metrics.SetDLQDepth(name, d)
	}
}

func (m *OpsMonitor) warn(msg string, f map[string]any) {
	if m.log != nil {
		m.log.Warn(msg, f)
	}
}
