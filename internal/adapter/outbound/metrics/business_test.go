package metrics

import (
	"os"
	"strings"
	"testing"

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
	MessagesReceived.WithLabelValues("auth-audit-q").Inc()
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
	if p["domain"] != "iam" || p["environment"] != "test" || p["service"] == "" {
		t.Errorf("platform_* labels = %v", p)
	}
	s := seen["iam_rls_violations_total"]
	if _, hasDomain := s["domain"]; hasDomain || s["environment"] != "test" {
		t.Errorf("iam_* labels = %v", s)
	}
}

func TestConsumerAdapter_IncrementsTier1(t *testing.T) {
	c := Consumer{}
	c.Received("auth-audit-q")
	c.Processed("auth-audit-q")
	c.Failed("auth-audit-q")
	c.DeadLettered("auth-audit-q")
}
