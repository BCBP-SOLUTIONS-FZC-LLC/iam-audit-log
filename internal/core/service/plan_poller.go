package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sync/atomic"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// PlanPollerConfig tunes PlanPoller.
type PlanPollerConfig struct {
	Interval time.Duration // CATALOG_PLANS_POLL_INTERVAL
	Timeout  time.Duration // CATALOG_PLANS_POLL_TIMEOUT
	Now      port.Clock
}

// PlanPoller keeps the plan_code → query_window_days map current from
// Catalog's CAT-I2 (AL-D15). It is stale-if-error: a failed poll keeps the
// last good map, so a Catalog outage never shrinks a tenant's window. Until
// the first successful poll WindowDays reports no entry, and callers fall
// back to the tenant row's stored value (§5.4, BUILD_PLAN D-13).
type PlanPoller struct {
	catalog port.PlanCatalog
	metrics port.PollMetrics
	cfg     PlanPollerConfig
	log     port.Logger
	current atomic.Pointer[planSnapshot]
}

// planSnapshot is one immutable polled map.
type planSnapshot struct {
	days     map[string]int
	versions map[string]int64
}

var _ port.PlanWindows = (*PlanPoller)(nil)

// NewPlanPoller wires the poller; metrics and log may be nil.
func NewPlanPoller(catalog port.PlanCatalog, metrics port.PollMetrics, cfg PlanPollerConfig, log port.Logger) *PlanPoller {
	if cfg.Interval <= 0 {
		cfg.Interval = 600 * time.Second
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 3 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &PlanPoller{catalog: catalog, metrics: metrics, cfg: cfg, log: log}
}

// WindowDays implements port.PlanWindows against the last good map.
func (p *PlanPoller) WindowDays(planCode string) (int, bool) {
	s := p.current.Load()
	if s == nil {
		return 0, false
	}
	d, ok := s.days[planCode]
	return d, ok
}

// Run polls immediately, then every Interval, until ctx is cancelled.
func (p *PlanPoller) Run(ctx context.Context) {
	t := time.NewTicker(p.cfg.Interval)
	defer t.Stop()
	for {
		if err := p.Poll(ctx); err != nil && ctx.Err() == nil && p.log != nil {
			p.log.Warn("catalog plans poll failed — serving last good map", map[string]any{"error": err.Error()})
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Poll runs one CAT-I2 poll. The map is replaced only when the response
// is valid and its record_versions differ from the current map's.
func (p *PlanPoller) Poll(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.Timeout)
	defer cancel()
	start := p.cfg.Now()
	plans, versions, err := p.catalog.Plans(ctx)
	if err == nil {
		err = p.apply(plans, versions)
	}
	took := p.cfg.Now().Sub(start)
	switch {
	case err == nil:
		p.observe("success", took)
		if p.metrics != nil {
			p.metrics.PollSucceeded(p.cfg.Now())
		}
	case errors.Is(err, port.ErrCatalogTimeout) || errors.Is(err, context.DeadlineExceeded):
		p.observe("timeout", took)
	default:
		p.observe("error", took)
	}
	return err
}

func (p *PlanPoller) observe(result string, took time.Duration) {
	if p.metrics != nil {
		p.metrics.PollResult(result, took)
	}
}

// apply validates a response and swaps it in when it changed. An empty
// plan list, a non-positive window, or a plan missing from
// record_versions is rejected whole (stale-if-error): a malformed
// response must never replace a good map.
func (p *PlanPoller) apply(plans []port.CatalogPlan, versions map[string]int64) error {
	if len(plans) == 0 {
		return errors.New("catalog returned no plans")
	}
	days := make(map[string]int, len(plans))
	for _, pl := range plans {
		if pl.Code == "" || pl.AuditQueryWindowDays <= 0 {
			return fmt.Errorf("catalog plan %q has invalid audit_query_window_days %d", pl.Code, pl.AuditQueryWindowDays)
		}
		if _, ok := versions[pl.Code]; !ok {
			return fmt.Errorf("catalog plan %q missing from record_versions", pl.Code)
		}
		days[pl.Code] = pl.AuditQueryWindowDays
	}
	if cur := p.current.Load(); cur != nil && maps.Equal(cur.versions, versions) {
		return nil // unchanged — keep the current map
	}
	p.current.Store(&planSnapshot{days: days, versions: maps.Clone(versions)})
	if p.log != nil {
		p.log.Info("catalog plan windows updated", map[string]any{"plans": len(days)})
	}
	return nil
}
