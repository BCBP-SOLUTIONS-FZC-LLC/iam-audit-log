package metrics

import (
	"errors"
	"os"
	"testing"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/gincommon"
)

func TestMain(m *testing.M) {
	_ = gincommon.ObservabilityMiddlewares(gincommon.Config{Domain: "iam", Environment: "test", ServiceName: "iam-audit-log", BuildVersion: "test"})
	if err := Register(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

// Every collector carries gincommon's {domain, service, environment} const
// labels, injected centrally (LLD §11), and none carries version (it is only
// on platform_build_info).
func TestRegister_ConstLabels(t *testing.T) {
	if err := Register(); err != nil { // idempotent
		t.Fatal(err)
	}
	DLQMessages.WithLabelValues("consume", "LoginSuccess", "max_receive_count").Inc()
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
		if mf.GetName() != "platform_dlq_messages_total" && mf.GetName() != "iam_rls_violations_total" {
			continue
		}
		labels := map[string]string{}
		for _, lp := range mf.GetMetric()[0].GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		seen[mf.GetName()] = labels
	}
	for _, name := range []string{"platform_dlq_messages_total", "iam_rls_violations_total"} {
		l := seen[name]
		if l["domain"] != "iam" || l["environment"] != "test" || l["service"] != "iam-audit-log" {
			t.Errorf("%s labels = %v", name, l)
		}
		if _, ok := l["version"]; ok {
			t.Errorf("%s carries version: %v", name, l)
		}
	}
}

// The dead-letter counter uses the registry's label set
// {operation, event_type, reason}.
func TestConsumerAdapter_DeadLettered(t *testing.T) {
	want := map[string]string{"operation": "consume", "event_type": "UserDeleted", "reason": "max_receive_count"}
	before := counterOr0(t, "platform_dlq_messages_total", want)
	Consumer{}.DeadLettered("UserDeleted")
	if got := labeled(t, "platform_dlq_messages_total", want); got != before+1 {
		t.Errorf("dlq = %v, want %v", got, before+1)
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

// A constructor error inside build aborts it and becomes Register's error;
// any other panic is not swallowed.
func TestBuild_MustAbortsWithError(t *testing.T) {
	cause := errors.New("bad name")
	var after bool
	err := build(func() {
		_ = must(0, cause)
		after = true
	})
	if !errors.Is(err, cause) || after {
		t.Fatalf("err = %v, ran past must = %v", err, after)
	}
	if err := build(func() { mustOK(nil); _ = must(1, nil) }); err != nil {
		t.Fatalf("no error: got %v", err)
	}
	// A rejected Tier 3 name is the real-world case.
	err = build(func() { _ = must(gincommon.NewCounterVec(nil, "iam_audit_log_not_a_counter", "x")) })
	if err == nil {
		t.Fatal("gincommon accepted a counter without _total")
	}
	defer func() {
		if r := recover(); r != "boom" {
			t.Fatalf("recovered %v, want the original panic", r)
		}
	}()
	_ = build(func() { panic("boom") })
}
