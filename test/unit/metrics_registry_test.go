package unit_test

import (
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/config"
	coredomain "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// The Enterprise Platform Observability Standard's CI validation (gap 46):
// every collector cmd/server/cmd/reconciler register is checked in-process
// against deploy/monitoring/metric-registry.yaml (the inventory) and
// metric-lint.yaml (which checks run), and the shared tiers against
// metrics.PlatformRegistry / DomainRegistry (the ratification ledger).

type regMetric struct {
	Name            string   `yaml:"name"`
	Tier            string   `yaml:"tier"`
	Type            string   `yaml:"type"`
	Labels          []string `yaml:"labels"`
	Status          string   `yaml:"status"`
	ReplacedBy      string   `yaml:"replaced_by"`
	DeprecatedSince string   `yaml:"deprecated_since"`
	Sunset          string   `yaml:"sunset"`
}

type vocabEntry struct {
	Meaning     string   `yaml:"meaning"`
	Cardinality any      `yaml:"cardinality"`
	Values      []string `yaml:"values"`
}

type metricRegistryFile struct {
	Service         string                `yaml:"service"`
	Domain          string                `yaml:"domain"`
	Tier3Prefix     string                `yaml:"tier3_prefix"`
	Metrics         []regMetric           `yaml:"metrics"`
	InjectedLabels  map[string][]string   `yaml:"injected_labels"`
	LabelVocabulary map[string]vocabEntry `yaml:"label_vocabulary"`
	Forbidden       []string              `yaml:"forbidden_label_keys"`
}

type metricLintFile struct {
	Checks         map[string]bool `yaml:"checks"`
	GaugeTokens    []string        `yaml:"gauge_quantity_tokens"`
	ForbiddenLabel []string        `yaml:"forbidden_label_keys"`
}

var gatherOnce struct {
	sync.Once
	families map[string]*dto.MetricFamily
}

// gatherAll registers every collector on gincommon's registry (as both
// composition roots do), observes a sample child for each so every family
// appears, and gathers them.
func gatherAll(t *testing.T) map[string]*dto.MetricFamily {
	t.Helper()
	gatherOnce.Do(func() {
		_ = gincommon.ObservabilityMiddlewares(gincommon.Config{ServiceName: "iam-audit-log", BuildVersion: "conformance"})
		metrics.Register("test")

		c := metrics.Consumer{}
		c.Received("user-audit-q", "UserDeleted")
		c.Processed("UserDeleted")
		c.Failed("UserDeleted", port.ReasonInternal)
		c.DeadLettered("user-audit-q")
		c.Propagated("UserDeleted", time.Second)

		in := metrics.Ingest{}
		in.Ingested(coredomain.AuditEntry{SourceService: "iam-user-profile", IngestMode: coredomain.IngestBus, EntryType: "user.deleted",
			OccurredAt: time.Now().Add(-time.Second), RecordedAt: time.Now()})
		in.Duplicate(coredomain.ConsumerDirectWrite)
		in.DuplicateMessage("UserDeleted")
		in.Unknown("iam-user-profile")
		in.DirectWrite("iam-catalog-admin", "created")

		q := metrics.Query{}
		q.WindowClamped("starter")
		q.ArchivedRead()
		q.ExportJob("pending")

		o := metrics.Ops{}
		o.SetOpsStats(coredomain.OpsStats{})
		o.SetQueueDepth("user-audit-q", 0)
		o.SetDLQDepth("user-audit-q", 0)
		o.AddRLSViolations("cross_tenant_access", 1)

		metrics.Dependency{}.ObserveDependency(port.DependencyS3, port.OperationReadArchive, port.OutcomeSuccess, time.Millisecond)
		metrics.CatalogPoll{}.PollResult("success", time.Millisecond)
		metrics.Redaction{}.TaskOutcome("applied")
		a := metrics.Archive{}
		a.PartitionArchived(coredomain.TierSecurity3y, "verified")
		a.Pruned(coredomain.TierSecurity3y)
		a.RedactionBlocked()
		a.Stalled(0)

		g, ok := gincommon.MetricsRegisterer().(prometheus.Gatherer)
		if !ok {
			panic("gincommon.MetricsRegisterer is not a Gatherer")
		}
		mfs, err := g.Gather()
		if err != nil {
			panic(err)
		}
		gatherOnce.families = map[string]*dto.MetricFamily{}
		for _, mf := range mfs {
			gatherOnce.families[mf.GetName()] = mf
		}
	})
	return gatherOnce.families
}

// ownFamilies is the set of families this service registers (business.go):
// library-owned families (gincommon HTTP, pgmetrics, platform-events,
// Go/process collectors) carry no platform_/iam_ prefix.
func ownFamilies(t *testing.T) map[string]*dto.MetricFamily {
	t.Helper()
	out := map[string]*dto.MetricFamily{}
	for name, mf := range gatherAll(t) {
		if strings.HasPrefix(name, "platform_") || strings.HasPrefix(name, "iam_") {
			out[name] = mf
		}
	}
	return out
}

func loadMetricRegistry(t *testing.T) metricRegistryFile {
	t.Helper()
	var r metricRegistryFile
	repoYAML(t, "deploy/monitoring/metric-registry.yaml", &r)
	if len(r.Metrics) == 0 {
		t.Fatal("metric-registry.yaml lists no metrics")
	}
	return r
}

func loadMetricLint(t *testing.T) metricLintFile {
	t.Helper()
	var l metricLintFile
	repoYAML(t, "deploy/monitoring/metric-lint.yaml", &l)
	if len(l.Checks) == 0 {
		t.Fatal("metric-lint.yaml declares no checks")
	}
	return l
}

func familyLabels(mf *dto.MetricFamily) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, m := range mf.GetMetric() {
		for _, lp := range m.GetLabel() {
			if out[lp.GetName()] == nil {
				out[lp.GetName()] = map[string]bool{}
			}
			out[lp.GetName()][lp.GetValue()] = true
		}
	}
	return out
}

func typeName(tp dto.MetricType) string {
	switch tp {
	case dto.MetricType_COUNTER:
		return "counter"
	case dto.MetricType_GAUGE:
		return "gauge"
	case dto.MetricType_HISTOGRAM:
		return "histogram"
	default:
		return tp.String()
	}
}

// violation is one conformance failure, tagged with the metric-lint check
// that produced it, so a disabled check can be proven to skip.
type violation struct{ check, msg string }

// conformance runs every check against the registry, the lint config and
// the gathered families, returning violations for enabled checks only.
func conformance(t *testing.T, reg metricRegistryFile, lint metricLintFile, fams map[string]*dto.MetricFamily) []violation {
	t.Helper()
	var out []violation
	add := func(check, msg string) {
		if lint.Checks[check] {
			out = append(out, violation{check, msg})
		}
	}
	byName := map[string]regMetric{}
	for _, m := range reg.Metrics {
		byName[m.Name] = m
	}
	shared := map[string]metrics.RegistryEntry{}
	for _, e := range append(append([]metrics.RegistryEntry{}, metrics.PlatformRegistry...), metrics.DomainRegistry...) {
		shared[e.Name] = e
	}
	forbidden := map[string]bool{}
	for _, k := range lint.ForbiddenLabel {
		forbidden[k] = true
	}
	prefix := map[string]string{"platform": "platform_", "domain": reg.Domain + "_", "service": reg.Tier3Prefix}
	svcToken := strings.TrimSuffix(strings.TrimPrefix(reg.Tier3Prefix, reg.Domain+"_"), "_") // "audit_log"

	// Inventory: every registered family is declared, and vice versa.
	for name := range fams {
		if _, ok := byName[name]; !ok {
			add("inventory_complete", name+" is registered but not in metric-registry.yaml")
		}
	}
	for _, m := range reg.Metrics {
		// Name-level checks run on every declared entry, emitted or not.
		// Namespace classification: prefix matches the declared tier; a
		// domain metric is not a service metric in disguise.
		p, known := prefix[m.Tier]
		switch {
		case !known:
			add("namespace_classification", m.Name+": unknown tier "+m.Tier)
		case !strings.HasPrefix(m.Name, p):
			add("namespace_classification", m.Name+" is tier "+m.Tier+" but lacks prefix "+p)
		case m.Tier == "domain" && strings.HasPrefix(m.Name, reg.Tier3Prefix):
			add("namespace_classification", m.Name+" carries the service prefix but is declared domain")
		}
		if m.Tier != "service" && strings.Contains(m.Name, svcToken) {
			add("no_service_name_in_shared_names", m.Name+" is shared but encodes the service name")
		}

		// Suffixes (rules 4-6).
		if m.Type == "counter" && !strings.HasSuffix(m.Name, "_total") {
			add("counter_suffix", "counter "+m.Name+" must end _total")
		}
		if m.Type == "histogram" && !strings.HasSuffix(m.Name, "_seconds") {
			add("histogram_suffix", "histogram "+m.Name+" must end _seconds")
		}
		if m.Type == "gauge" && m.Status != "deprecated" {
			if strings.HasSuffix(m.Name, "_total") {
				add("gauge_naming", "gauge "+m.Name+" must not end _total")
			}
			if !slices.ContainsFunc(lint.GaugeTokens, func(tok string) bool { return strings.Contains(m.Name, tok) }) {
				add("gauge_naming", "gauge "+m.Name+" carries no quantity token "+strings.Join(lint.GaugeTokens, ","))
			}
		}

		mf, ok := fams[m.Name]
		if !ok {
			add("inventory_complete", m.Name+" is in metric-registry.yaml but never registered")
			continue
		}
		if got := typeName(mf.GetType()); got != m.Type {
			add("inventory_complete", m.Name+" is registered as "+got+", declared "+m.Type)
		}

		labels := familyLabels(mf)
		injected := reg.InjectedLabels[m.Tier]

		// Required, centrally injected labels with the registry's values.
		for _, k := range injected {
			if labels[k] == nil {
				add("required_labels", m.Name+" is missing required label "+k)
			}
		}
		if !labels["service"][reg.Service] || len(labels["service"]) != 1 {
			add("central_injection", m.Name+": service label must be exactly "+reg.Service)
		}
		if m.Tier == "platform" && (!labels["domain"][reg.Domain] || len(labels["domain"]) != 1) {
			add("central_injection", m.Name+": domain label must be exactly "+reg.Domain)
		}
		if m.Tier != "platform" && labels["domain"] != nil {
			add("central_injection", m.Name+": only platform_* carries a domain label")
		}

		// Dimension keys beyond the injected set (and gincommon's version).
		var dims []string
		for k := range labels {
			if !slices.Contains(injected, k) && k != "version" {
				dims = append(dims, k)
			}
		}
		sort.Strings(dims)
		want := append([]string{}, m.Labels...)
		sort.Strings(want)
		if !slices.Equal(dims, want) {
			add("inventory_complete", m.Name+" labels "+strings.Join(dims, ",")+" differ from declared "+strings.Join(want, ","))
		}

		// Shared registry compliance (rules 10-12).
		if m.Tier == "platform" || m.Tier == "domain" {
			e, ok := shared[m.Name]
			switch {
			case !ok:
				add("shared_registry_compliance", m.Name+" is a shared metric absent from registry.go (rule 11)")
			case e.Status != metrics.StatusCanonical || m.Status != "canonical":
				add("shared_registry_compliance", m.Name+" is not Canonical in both registry.go and the yaml")
			default:
				r := append([]string{}, e.Labels...)
				sort.Strings(r)
				if !slices.Equal(dims, r) {
					add("shared_registry_compliance", m.Name+" label set "+strings.Join(dims, ",")+" is not the ratified "+strings.Join(r, ","))
				}
				req := append([]string{}, e.RequiredLabels...)
				sort.Strings(req)
				inj := append([]string{}, injected...)
				sort.Strings(inj)
				if !slices.Equal(req, inj) {
					add("shared_registry_compliance", m.Name+" required labels disagree between registry.go and the yaml")
				}
			}
			// Label vocabulary: approved keys only, in-vocabulary values.
			for _, k := range dims {
				v, ok := reg.LabelVocabulary[k]
				if !ok {
					add("label_vocabulary_compliance", m.Name+" uses label "+k+", absent from label_vocabulary")
					continue
				}
				if len(v.Values) > 0 {
					for val := range labels[k] {
						if !slices.Contains(v.Values, val) {
							add("label_vocabulary_compliance", m.Name+": "+k+"="+val+" is not an approved value")
						}
					}
				}
			}
		}

		// Cardinality denylist.
		for k := range labels {
			if forbidden[k] {
				add("label_cardinality_allowlist", m.Name+" carries forbidden label "+k)
			}
		}

		// Deprecation metadata.
		if m.Status == "deprecated" {
			r, ok := byName[m.ReplacedBy]
			switch {
			case m.ReplacedBy == "" || !ok:
				add("deprecation_metadata", m.Name+" is deprecated without a registered replaced_by")
			case r.Status == "deprecated":
				add("deprecation_metadata", m.Name+" is replaced by another deprecated metric "+m.ReplacedBy)
			}
			if m.Sunset == "" || m.DeprecatedSince == "" {
				add("deprecation_metadata", m.Name+" is deprecated without deprecated_since/sunset")
			}
		}
	}
	// Every ratified ledger entry is actually emitted and declared.
	for name := range shared {
		if _, ok := byName[name]; !ok {
			add("shared_registry_compliance", name+" is in registry.go but not in metric-registry.yaml")
		}
	}
	return out
}

// TestMetricRegistry_Conformance is the metric-lint CI gate: every check
// metric-lint.yaml enables must hold for every registered collector.
func TestMetricRegistry_Conformance(t *testing.T) {
	reg, lint := loadMetricRegistry(t), loadMetricLint(t)
	for _, v := range conformance(t, reg, lint, ownFamilies(t)) {
		t.Errorf("[%s] %s", v.check, v.msg)
	}
	for name, on := range lint.Checks {
		if !on {
			t.Errorf("metric-lint.yaml disables %s; the standard requires every check", name)
		}
	}
}

// The two copies of the forbidden label set agree.
func TestMetricRegistry_ForbiddenLabelsAgree(t *testing.T) {
	reg, lint := loadMetricRegistry(t), loadMetricLint(t)
	a, b := append([]string{}, reg.Forbidden...), append([]string{}, lint.ForbiddenLabel...)
	sort.Strings(a)
	sort.Strings(b)
	if !slices.Equal(a, b) {
		t.Errorf("metric-registry.yaml %v != metric-lint.yaml %v", a, b)
	}
}

// The label vocabulary lists exactly the values the code emits: the queue
// names equal the inbound catalog, and the port constants are in-vocabulary.
func TestMetricRegistry_VocabularyMatchesCode(t *testing.T) {
	voc := loadMetricRegistry(t).LabelVocabulary
	var queues []string
	for _, q := range config.InboundQueues() {
		queues = append(queues, q.Name)
	}
	got := append([]string{}, voc["queue"].Values...)
	sort.Strings(got)
	sort.Strings(queues)
	if !slices.Equal(got, queues) {
		t.Errorf("queue vocabulary %v != config.InboundQueues %v", got, queues)
	}
	for key, vals := range map[string][]string{
		"reason":     {port.ReasonInvalidEvent, port.ReasonDependencyUnavailable, port.ReasonInternal, port.ReasonMaxReceiveExceeded},
		"outcome":    {port.OutcomeSuccess, port.OutcomeError, port.OutcomeTimeout, port.OutcomeNotFound},
		"dependency": {port.DependencyCatalogAdmin, port.DependencyS3},
		"operation":  {port.OperationListPlans, port.OperationReadArchive, port.OperationPutArchive, port.OperationVerifyArchive, port.OperationPutExport},
	} {
		allowed := voc[key].Values
		for _, v := range vals {
			if !slices.Contains(allowed, v) {
				t.Errorf("%s value %q (code) is not in label_vocabulary %v", key, v, allowed)
			}
		}
		if len(allowed) != len(vals) {
			t.Errorf("%s vocabulary %v lists values the code never emits (%v)", key, allowed, vals)
		}
	}
	// The catalog poller's result strings are the outcome vocabulary too.
	for _, r := range []string{"success", "error", "timeout"} {
		if !slices.Contains(voc["outcome"].Values, r) {
			t.Errorf("catalog poll result %q is not an outcome value", r)
		}
	}
}

// Each check is load-bearing: with a deliberately bad registry, a check
// reports its violation when enabled and nothing when disabled.
func TestMetricRegistry_ChecksAreGated(t *testing.T) {
	fams := ownFamilies(t)
	base := loadMetricRegistry(t)
	cases := map[string]func(r *metricRegistryFile){
		"namespace_classification": func(r *metricRegistryFile) { r.Metrics[0].Tier = "service" },
		"counter_suffix": func(r *metricRegistryFile) {
			r.Metrics = append(r.Metrics, regMetric{Name: "iam_audit_log_bad", Tier: "service", Type: "counter", Status: "current"})
		},
		"histogram_suffix": func(r *metricRegistryFile) {
			r.Metrics = append(r.Metrics, regMetric{Name: "iam_audit_log_bad_hist", Tier: "service", Type: "histogram", Status: "current"})
		},
		"gauge_naming": func(r *metricRegistryFile) {
			r.Metrics = append(r.Metrics, regMetric{Name: "iam_audit_log_widgets_total", Tier: "service", Type: "gauge", Status: "current"})
		},
		"central_injection":           func(r *metricRegistryFile) { r.Service = "someone-else" },
		"label_vocabulary_compliance": func(r *metricRegistryFile) { delete(r.LabelVocabulary, "queue") },
		"inventory_complete":          func(r *metricRegistryFile) { r.Metrics = r.Metrics[1:] },
		"deprecation_metadata": func(r *metricRegistryFile) {
			for i := range r.Metrics {
				if r.Metrics[i].Status == "deprecated" {
					r.Metrics[i].ReplacedBy = ""
					return
				}
			}
		},
		"label_cardinality_allowlist": func(r *metricRegistryFile) {},
		"shared_registry_compliance": func(r *metricRegistryFile) {
			for i := range r.Metrics {
				if r.Metrics[i].Tier == "platform" {
					r.Metrics[i].Status = "proposed"
					return
				}
			}
		},
		"no_service_name_in_shared_names": func(r *metricRegistryFile) {
			r.Metrics = append(r.Metrics, regMetric{Name: "iam_audit_log_x_total", Tier: "domain", Type: "counter"})
		},
		"required_labels": func(r *metricRegistryFile) {
			r.InjectedLabels = map[string][]string{"platform": {"domain", "service", "environment", "region"},
				"domain": r.InjectedLabels["domain"], "service": r.InjectedLabels["service"]}
		},
	}
	for check, mutate := range cases {
		t.Run(check, func(t *testing.T) {
			reg := cloneRegistry(base)
			mutate(&reg)
			lint := loadMetricLint(t)
			if check == "label_cardinality_allowlist" {
				lint.ForbiddenLabel = append(lint.ForbiddenLabel, "queue") // queue exists on platform_* series
			}
			on := hasCheck(conformance(t, reg, lint, fams), check)
			lint.Checks[check] = false
			off := hasCheck(conformance(t, reg, lint, fams), check)
			if !on {
				t.Errorf("%s: a violation was not reported while the check is enabled", check)
			}
			if off {
				t.Errorf("%s: a disabled check still reported", check)
			}
		})
	}
}

func hasCheck(vs []violation, check string) bool {
	return slices.ContainsFunc(vs, func(v violation) bool { return v.check == check })
}

func cloneRegistry(r metricRegistryFile) metricRegistryFile {
	c := r
	c.Metrics = append([]regMetric{}, r.Metrics...)
	c.LabelVocabulary = map[string]vocabEntry{}
	for k, v := range r.LabelVocabulary {
		c.LabelVocabulary[k] = v
	}
	c.InjectedLabels = map[string][]string{}
	for k, v := range r.InjectedLabels {
		c.InjectedLabels[k] = append([]string{}, v...)
	}
	return c
}
