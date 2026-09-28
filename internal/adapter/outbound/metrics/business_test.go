package metrics

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

func TestMain(m *testing.M) {
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{ServiceName: "iam-audit-log", BuildVersion: "test"})
	Register("test")
	os.Exit(m.Run())
}

// Tier labels are injected centrally (LLD §11): platform_* collectors carry
// domain+environment, iam_* collectors carry environment and no domain.
func TestRegister_TierConstLabels(t *testing.T) {
	Register("test") // idempotent
	MessagesReceived.WithLabelValues("auth-audit-q", "LoginSuccess").Inc()
	RLSViolations.WithLabelValues("cross_tenant_access").Inc()

	g, ok := gincommon.MetricsRegisterer().(prometheus.Gatherer)
	if !ok {
		t.Fatal("gincommon registerer is not a Gatherer")
	}
	mfs, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]map[string]string{}
	for _, mf := range mfs {
		if !strings.HasPrefix(mf.GetName(), "platform_messages_received") && mf.GetName() != "iam_rls_violations_total" {
			continue
		}
		labels := map[string]string{}
		for _, lp := range mf.GetMetric()[0].GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		seen[mf.GetName()] = labels
	}
	p := seen["platform_messages_received_total"]
	// service is the standard's in-domain service name, overriding
	// gincommon's app-name const label (central injection, rule 8).
	if p["domain"] != "iam" || p["environment"] != "test" || p["service"] != "audit-log" || p["version"] == "" {
		t.Errorf("platform_* labels = %v", p)
	}
	s := seen["iam_rls_violations_total"]
	if _, hasDomain := s["domain"]; hasDomain || s["environment"] != "test" {
		t.Errorf("iam_* labels = %v", s)
	}
}

// Tier-1 message collectors with the registry's label sets; a failure is
// also a retry (the message is left for redelivery).
func TestConsumerAdapter_IncrementsTier1(t *testing.T) {
	c := Consumer{}
	failedBefore := counterOr0(t, "platform_messages_failed_total", map[string]string{"event_type": "UserDeleted", "reason": "internal"})
	retryBefore := counterOr0(t, "platform_retry_total", map[string]string{"event_type": "UserDeleted", "reason": "internal"})
	c.Received("auth-audit-q", "LoginSuccess")
	c.Processed("LoginSuccess")
	c.Failed("UserDeleted", "internal")
	c.DeadLettered("auth-audit-q")
	c.Propagated("LoginSuccess", 250*time.Millisecond)
	c.Propagated("LoginSuccess", -time.Second) // a producer clock ahead of ours observes 0, never negative

	if got := labeled(t, "platform_messages_received_total", map[string]string{"queue": "auth-audit-q", "event_type": "LoginSuccess"}); got < 1 {
		t.Errorf("received = %v", got)
	}
	if got := labeled(t, "platform_messages_processed_total", map[string]string{"event_type": "LoginSuccess"}); got < 1 {
		t.Errorf("processed = %v", got)
	}
	if got := labeled(t, "platform_messages_failed_total", map[string]string{"event_type": "UserDeleted", "reason": "internal"}); got != failedBefore+1 {
		t.Errorf("failed = %v, want %v", got, failedBefore+1)
	}
	if got := labeled(t, "platform_retry_total", map[string]string{"event_type": "UserDeleted", "reason": "internal"}); got != retryBefore+1 {
		t.Errorf("retry = %v, want %v", got, retryBefore+1)
	}
	if got := labeled(t, "platform_dlq_messages_total", map[string]string{"queue": "auth-audit-q", "reason": "max_receive_exceeded"}); got < 1 {
		t.Errorf("dlq = %v", got)
	}
	count, sum := labeledHistogram(t, "platform_event_propagation_seconds", map[string]string{"event_type": "LoginSuccess"})
	if count < 2 || sum < 0.25 || sum > 0.26 {
		t.Errorf("propagation count=%d sum=%v, want ≥2 samples summing 0.25 (negative clamped to 0)", count, sum)
	}
}

// counterOr0 is labeled() for a family that may have no series yet (an
// un-observed *Vec is absent from Gather).
func counterOr0(t *testing.T, name string, want map[string]string) float64 {
	t.Helper()
	g, _ := gincommon.MetricsRegisterer().(prometheus.Gatherer)
	mfs, err := g.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return labeled(t, name, want)
		}
	}
	return 0
}
