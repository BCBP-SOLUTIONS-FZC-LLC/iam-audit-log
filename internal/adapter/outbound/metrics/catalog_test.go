package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// metricFamily gathers one registered metric family by name.
func metricFamily(t *testing.T, name string) *dto.MetricFamily {
	t.Helper()
	g, ok := gincommon.MetricsRegisterer().(prometheus.Gatherer)
	if !ok {
		t.Fatal("gincommon registerer is not a Gatherer")
	}
	mfs, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf
		}
	}
	t.Fatalf("%s not registered", name)
	return nil
}

func gaugeValue(t *testing.T, name string) float64 {
	t.Helper()
	return metricFamily(t, name).GetMetric()[0].GetGauge().GetValue()
}

// pollCount returns iam_audit_log_catalog_plans_poll_total{result}.
func pollCount(t *testing.T, result string) float64 {
	t.Helper()
	for _, m := range metricFamily(t, "iam_audit_log_catalog_plans_poll_total").GetMetric() {
		for _, lp := range m.GetLabel() {
			if lp.GetName() == "result" && lp.GetValue() == result {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// AL-D15 / LLD §11: poll outcomes by result, and staleness since the last
// successful poll.
func TestCatalogPoll_CountsAndStaleness(t *testing.T) {
	c := CatalogPoll{}
	c.PollResult("success", 10*time.Millisecond)
	before := pollCount(t, "timeout")
	c.PollResult("timeout", 20*time.Millisecond)
	if got := pollCount(t, "timeout") - before; got != 1 {
		t.Errorf("timeout count delta = %v", got)
	}

	c.PollSucceeded(time.Now().Add(-time.Hour))
	if v := gaugeValue(t, "iam_audit_log_catalog_plans_stale_seconds"); v < 3599 || v > 3700 {
		t.Errorf("stale after an hour-old success = %v", v)
	}
	c.PollSucceeded(time.Now())
	if v := gaugeValue(t, "iam_audit_log_catalog_plans_stale_seconds"); v < 0 || v > 5 {
		t.Errorf("stale right after success = %v", v)
	}
}
