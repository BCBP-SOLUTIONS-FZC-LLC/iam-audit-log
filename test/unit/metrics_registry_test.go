package unit_test

import (
	"bytes"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/metrics"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/config"
	coredomain "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-events/v2/pkg/events"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/obsregistry"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-pgcommon/v2/pkg/pgmetrics"
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
	families map[string]*dto.MetricFamily // the service's own families
	all      []*dto.MetricFamily          // every family, libraries included
	warnings []string                     // platform-events / pgcommon RegistrationWarnings
}

// gatherAll registers every collector on gincommon's registry in the
// composition roots' order (gincommon → business.go → platform-events →
// platform-pgcommon), observes a sample child for each of the service's
// collectors so every family appears, and gathers them. The service's own
// families are those that appear between gincommon's init and the library
// inits.
func gatherAll(t *testing.T) map[string]*dto.MetricFamily {
	t.Helper()
	gatherOnce.Do(func() {
		_ = gincommon.ObservabilityMiddlewares(gincommon.Config{
			Domain: "iam", Environment: "test", ServiceName: "iam-audit-log", BuildVersion: "conformance",
		})
		g := gincommon.MetricsGatherer()
		lib := map[string]bool{}
		before, err := g.Gather()
		if err != nil {
			panic(err)
		}
		for _, mf := range before {
			lib[mf.GetName()] = true
		}
		if err := metrics.Register(); err != nil {
			panic(err)
		}

		metrics.Consumer{}.DeadLettered("UserDeleted")

		in := metrics.Ingest{}
		in.Ingested(coredomain.AuditEntry{SourceService: "iam-user-profile", IngestMode: coredomain.IngestBus, EntryType: "user.deleted",
			OccurredAt: time.Now().Add(-time.Second), RecordedAt: time.Now()})
		in.Duplicate(coredomain.ConsumerDirectWrite)
		in.DuplicateMessage("user-audit-q", "UserDeleted")
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

		mfs, err := g.Gather()
		if err != nil {
			panic(err)
		}
		gatherOnce.families = map[string]*dto.MetricFamily{}
		for _, mf := range mfs {
			if !lib[mf.GetName()] {
				gatherOnce.families[mf.GetName()] = mf
			}
		}

		// The libraries register after the service, as in cmd/server: the
		// platform_* names they share with business.go must be reused, not
		// refused (a refusal is a RegistrationWarning).
		labels := gincommon.MetricsConstLabels()
		evw, err := events.InitMetrics(events.MetricsIdentityFromLabels(labels), gincommon.MetricsRegisterer())
		if err != nil {
			panic(err)
		}
		for _, w := range evw {
			gatherOnce.warnings = append(gatherOnce.warnings, w.Error())
		}
		pgw, err := pgmetrics.InitWithIdentity(pgmetrics.IdentityFromLabels(labels), gincommon.MetricsRegisterer())
		if err != nil {
			panic(err)
		}
		for _, w := range pgw {
			gatherOnce.warnings = append(gatherOnce.warnings, w.Metric+": "+w.Err.Error())
		}
		if gatherOnce.all, err = g.Gather(); err != nil {
			panic(err)
		}
	})
	return gatherOnce.families
}

// ownFamilies is the set of families this service registers (business.go);
// gincommon's, platform-events' and platform-pgcommon's own families are
// excluded.
func ownFamilies(t *testing.T) map[string]*dto.MetricFamily {
	t.Helper()
	return gatherAll(t)
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

		// Deprecation metadata is a registry-level rule: it applies to every
		// declared entry, emitted or not (a deprecated entry is often no
		// longer registered in-process), so it runs before the inventory check.
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
		if !labels["domain"][reg.Domain] || len(labels["domain"]) != 1 {
			add("central_injection", m.Name+": domain label must be exactly "+reg.Domain)
		}

		// Dimension keys beyond the injected set (version is not exempt: it
		// is only on gincommon's platform_build_info).
		var dims []string
		for k := range labels {
			if !slices.Contains(injected, k) {
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
			case (e.Status == metrics.StatusCanonical) != (m.Status == "canonical") ||
				(e.Status == metrics.StatusProposed) != (m.Status == "proposed"):
				add("shared_registry_compliance", m.Name+" has a different status in registry.go and the yaml")
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
		"reason":     {port.ReasonMaxReceiveCount},
		"outcome":    {port.OutcomeSuccess, port.OutcomeFailure, port.OutcomeError},
		"dependency": {port.DependencyCatalogAdmin, port.DependencyS3},
		"operation": {port.OperationConsume, port.OperationListPlans, port.OperationReadArchive, port.OperationPutArchive,
			port.OperationVerifyArchive, port.OperationPutExport},
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
}

// The ledger (registry.go) agrees with the central Platform Observability
// Registry: same label set, and status Canonical exactly where the central
// registry says approved, Proposed where it says proposed. A name the
// central registry removed or never defined fails here.
func TestRegistryLedger_MatchesPlatformRegistry(t *testing.T) {
	central := obsregistry.Default()
	for _, e := range append(append([]metrics.RegistryEntry{}, metrics.PlatformRegistry...), metrics.LibraryRegistry...) {
		m, ok := central.Lookup(e.Name)
		if !ok {
			t.Errorf("%s is not in the Platform Observability Registry", e.Name)
			continue
		}
		want := metrics.StatusProposed
		switch m.Status {
		case obsregistry.StatusApproved:
			want = metrics.StatusCanonical
		case obsregistry.StatusProposed:
		default:
			t.Errorf("%s has registry status %s", e.Name, m.Status)
			continue
		}
		if e.Status != want {
			t.Errorf("%s: ledger status %v, registry %s", e.Name, e.Status, m.Status)
		}
		if !slices.Equal(e.Labels, m.Labels) {
			t.Errorf("%s: ledger labels %v, registry %v", e.Name, e.Labels, m.Labels)
		}
	}
}

// platform-events and platform-pgcommon register after business.go and
// reuse the collectors it built from the registry: no shared platform_*
// metric is disabled (a RegistrationWarning would mean the library's
// recordings are lost).
func TestMetrics_LibrariesShareServiceCollectors(t *testing.T) {
	gatherAll(t)
	for _, w := range gatherOnce.warnings {
		t.Errorf("library metric disabled: %s", w)
	}
}

// The full process scrape (gincommon, business.go, platform-events,
// platform-pgcommon) passes the Platform Observability Registry validation
// that metricslint check runs; Tier-2 iam_* names the central IAM domain
// registry does not define yet (iam_rls_violations_total) are the only
// accepted findings. METRICS_SCRAPE_OUT=<file> also writes the scrape for
// metricslint check.
func TestMetrics_ScrapeValidatesAgainstRegistry(t *testing.T) {
	gatherAll(t)
	var buf bytes.Buffer
	enc := expfmt.NewEncoder(&buf, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, mf := range gatherOnce.all {
		if err := enc.Encode(mf); err != nil {
			t.Fatal(err)
		}
	}
	if out := os.Getenv("METRICS_SCRAPE_OUT"); out != "" {
		if err := os.WriteFile(out, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	rep, err := obsregistry.ValidateText(bytes.NewReader(buf.Bytes()), obsregistry.Options{})
	if err != nil {
		t.Fatal(err)
	}
	pending := map[string]bool{}
	for _, e := range metrics.DomainRegistry {
		pending[e.Name] = true
	}
	for _, f := range rep.Errors() {
		if !pending[f.Metric] {
			t.Error(f.String())
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
			r.Metrics = append(r.Metrics, regMetric{Name: "iam_audit_log_old_total", Tier: "service", Type: "counter", Status: "deprecated"})
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
