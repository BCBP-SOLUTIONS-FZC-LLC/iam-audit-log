package unit_test

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// registeredMetrics returns every collector name constructed in
// registerMetrics() (business.go is the sole registration file, §11).
func registeredMetrics(t *testing.T) map[string]bool {
	t.Helper()
	src := string(repoFile(t, "internal/adapter/outbound/metrics/business.go"))
	out := map[string]bool{}
	for _, m := range regexp.MustCompile(`Name:\s*"([a-z0-9_]+)"`).FindAllStringSubmatch(src, -1) {
		out[m[1]] = true
	}
	if len(out) == 0 {
		t.Fatal("no collectors found in business.go")
	}
	return out
}

// lldTier3Metrics parses the LLD §11 Tier-3 table's metric names.
func lldTier3Metrics(t *testing.T) map[string]bool {
	t.Helper()
	lld := string(repoFile(t, "docs/lld/iam-lld-audit-log-service.md"))
	start := strings.Index(lld, "**Tier-3 (`iam_audit_log_*`):**")
	end := strings.Index(lld[start:], "**Tracing & logging") // tolerate a "(rev N)" suffix on the next section marker
	if start < 0 || end < 0 {
		t.Fatal("LLD §11 Tier-3 table not found")
	}
	out := map[string]bool{}
	for _, line := range strings.Split(lld[start:start+end], "\n") {
		if m := regexp.MustCompile("^\\| `(iam_audit_log_[a-z0-9_]+)").FindStringSubmatch(line); m != nil {
			out[m[1]] = true
		}
	}
	if len(out) < 10 {
		t.Fatalf("parsed only %d Tier-3 metrics from the LLD", len(out))
	}
	return out
}

// deprecatedMetrics lists metric-registry.yaml entries with status
// deprecated: still emitted during the compatibility period, but never read
// by a dashboard, alert, recording rule, SLO or HPA (migration steps 2-6 of
// the Enterprise Platform Observability Standard).
func deprecatedMetrics(t *testing.T) map[string]string {
	t.Helper()
	var reg struct {
		Metrics []struct {
			Name       string `yaml:"name"`
			Status     string `yaml:"status"`
			ReplacedBy string `yaml:"replaced_by"`
		} `yaml:"metrics"`
	}
	repoYAML(t, "deploy/monitoring/metric-registry.yaml", &reg)
	out := map[string]string{}
	for _, m := range reg.Metrics {
		if m.Status == "deprecated" {
			out[m.Name] = m.ReplacedBy
		}
	}
	return out
}

// LLD §11: every Tier-3 metric the LLD names is registered, and every
// iam_audit_log_* collector is in the LLD table (deprecated names are listed
// there too, marked deprecated).
func TestMetrics_LLDTier3Registered(t *testing.T) {
	code, spec := registeredMetrics(t), lldTier3Metrics(t)
	for name := range spec {
		if !code[name] {
			t.Errorf("LLD §11 metric %s is not registered in business.go", name)
		}
	}
	for name := range code {
		if strings.HasPrefix(name, "iam_audit_log_") && !spec[name] {
			t.Errorf("business.go registers %s, which is not in the LLD §11 Tier-3 table", name)
		}
	}
	for name := range deprecatedMetrics(t) {
		if !spec[name] {
			t.Errorf("deprecated metric %s must stay in the LLD §11 table (marked deprecated) until its sunset", name)
		}
	}
}

// libraryMetrics are metrics registered by shared libraries or the platform,
// with names verified against the module cache (platform-gincommon v1.3.0,
// platform-events v1.4.0) and kube-state-metrics.
var libraryMetrics = map[string]bool{
	"up":                            true,
	"http_requests_total":           true, // gincommon {method,route,status_class,error_class}
	"http_request_duration_seconds": true, // gincommon
	"events_consumed_total":         true, // platform-events {queue,event_type,status}
	"kube_job_status_failed":        true, // kube-state-metrics
}

// ruleExprs returns every PromQL expression in the observability artifacts:
// alerts, recording rules, SLOs, the dashboard and the prometheus-adapter
// rules, keyed "file: name".
func ruleExprs(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range []string{"app-alerts.yml", "recording-rules.yml", "slo-rules.yml"} {
		var af struct {
			Groups []struct {
				Rules []struct {
					Alert  string `yaml:"alert"`
					Record string `yaml:"record"`
					Expr   string `yaml:"expr"`
				} `yaml:"rules"`
			} `yaml:"groups"`
		}
		repoYAML(t, "deploy/monitoring/"+f, &af)
		for _, g := range af.Groups {
			for _, r := range g.Rules {
				out[f+": "+r.Alert+r.Record] = r.Expr
			}
		}
	}
	var dash struct {
		Panels []struct {
			Title   string `json:"title"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
		Templating struct {
			List []struct {
				Name  string `json:"name"`
				Query any    `json:"query"`
			} `json:"list"`
		} `json:"templating"`
	}
	if err := json.Unmarshal(repoFile(t, "deploy/monitoring/dashboard-audit-log.json"), &dash); err != nil {
		t.Fatalf("dashboard-audit-log.json: %v", err)
	}
	for _, p := range dash.Panels {
		for i, tg := range p.Targets {
			out[fmt.Sprintf("dashboard: %s #%d", p.Title, i)] = tg.Expr
		}
	}
	for _, v := range dash.Templating.List {
		if q, ok := v.Query.(string); ok && strings.Contains(q, "{") {
			out["dashboard var: "+v.Name] = q
		}
	}
	var adapter struct {
		Rules map[string][]struct {
			SeriesQuery string `yaml:"seriesQuery"`
		} `yaml:"rules"`
	}
	repoYAML(t, "deploy/monitoring/prometheus-adapter-rule.yaml", &adapter)
	for kind, rs := range adapter.Rules {
		for i, r := range rs {
			out[fmt.Sprintf("prometheus-adapter %s #%d", kind, i)] = r.SeriesQuery
		}
	}
	return out
}

// Every metric an artifact reads exists (a typo'd alert never fires), is a
// recording rule defined here, or is a known library/platform metric, and no
// artifact reads a deprecated metric.
func TestAlerts_ReferenceKnownMetrics(t *testing.T) {
	code, deprecated := registeredMetrics(t), deprecatedMetrics(t)
	exprs := ruleExprs(t)
	recorded := map[string]bool{}
	for k := range exprs {
		if strings.HasPrefix(k, "recording-rules.yml: ") || strings.HasPrefix(k, "slo-rules.yml: ") {
			recorded[strings.SplitN(k, ": ", 2)[1]] = true
		}
	}
	ident := regexp.MustCompile(`([a-zA-Z_:][a-zA-Z0-9_:]*)\{`)
	for where, expr := range exprs {
		ms := ident.FindAllStringSubmatch(expr, -1)
		bare := regexp.MustCompile(`\b((?:service|slo):[a-z0-9_:]+)\b`).FindAllStringSubmatch(expr, -1)
		if len(ms)+len(bare) == 0 {
			t.Errorf("%s: no metric selector found in %q", where, expr)
		}
		for _, m := range append(ms, bare...) {
			name := m[1]
			base := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_sum"), "_count")
			if r, dep := deprecated[base]; dep {
				t.Errorf("%s reads deprecated metric %s — use %s", where, base, r)
			}
			if !code[name] && !code[base] && !libraryMetrics[name] && !recorded[name] {
				t.Errorf("%s reads unknown metric %s", where, name)
			}
		}
	}
	if len(exprs) < 60 {
		t.Errorf("only %d expressions parsed across the observability artifacts", len(exprs))
	}
}

// The static rule files (alerts, recording rules, SLOs, in that order)
// equal the Helm PrometheusRule's groups once templating is stripped
// (release name iam-audit-log). Regenerate with scripts/gen-prometheusrule.py.
func TestAlerts_StaticMirrorMatchesHelm(t *testing.T) {
	tpl := string(repoFile(t, "deploy/helm/templates/prometheusrule.yaml"))
	i := strings.Index(tpl, "spec:\n  groups:\n")
	if i < 0 {
		t.Fatal("prometheusrule.yaml has no spec.groups")
	}
	body := strings.TrimSuffix(strings.TrimRight(tpl[i+len("spec:\n  groups:\n"):], "\n"), "{{- end }}")
	body = strings.NewReplacer(
		"{{`{{", "{{", "}}`}}", "}}",
		`{{ include "iam-audit-log.fullname" . | quote }}`, `"iam-audit-log"`,
		`{{ include "iam-audit-log.fullname" . }}`, "iam-audit-log",
	).Replace(body)
	var lines []string
	for _, l := range strings.Split(body, "\n") {
		lines = append(lines, strings.TrimPrefix(l, "  "))
	}
	var helm, static any
	if err := yaml.Unmarshal([]byte(strings.Join(lines, "\n")), &helm); err != nil {
		t.Fatalf("parse stripped helm groups: %v", err)
	}
	var all []any
	for _, name := range []string{"app-alerts.yml", "recording-rules.yml", "slo-rules.yml"} {
		var f map[string]any
		repoYAML(t, "deploy/monitoring/"+name, &f)
		gs, _ := f["groups"].([]any)
		all = append(all, gs...)
	}
	static = all
	if !reflect.DeepEqual(helm, static) {
		t.Error("deploy/monitoring/{app-alerts,recording-rules,slo-rules}.yml have drifted from deploy/helm/templates/prometheusrule.yaml — run scripts/gen-prometheusrule.py")
	}
	if !strings.Contains(tpl, "{{- if .Values.prometheusRule.enabled }}") {
		t.Error("PrometheusRule must be gated by .Values.prometheusRule.enabled")
	}
}

// logDenylist: fields that could carry audit payload or PII. Logs carry IDs
// only; the durable record is the store, not the log (LLD §11).
var logDenylist = map[string]bool{
	"metadata": true, "payload": true, "body": true, "email": true, "user_agent": true,
	"ip_address": true, "actor_display": true, "attempted_username": true, "display": true,
}

var logMethods = map[string]bool{
	"Info": true, "Warn": true, "Error": true, "Debug": true,
	"InfoContext": true, "WarnContext": true, "ErrorContext": true, "DebugContext": true,
}

// No log call in production code names a payload/PII field, whether as a
// map[string]any key or as a slog-style key/value argument.
func TestLogHygiene_NoPayloadFields(t *testing.T) {
	root := filepath.Join("..", "..")
	var violations []string
	calls := 0
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			ast.Inspect(file, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok || !logMethods[sel.Sel.Name] {
					return true
				}
				calls++
				check := func(lit *ast.BasicLit) {
					if lit.Kind != token.STRING {
						return
					}
					if v, err := strconv.Unquote(lit.Value); err == nil && logDenylist[v] {
						violations = append(violations, fset.Position(lit.Pos()).String()+": log field "+strconv.Quote(v))
					}
				}
				for _, arg := range call.Args {
					switch a := arg.(type) {
					case *ast.BasicLit: // slog-style key
						check(a)
					case *ast.CompositeLit: // map[string]any{...}
						for _, el := range a.Elts {
							if kv, ok := el.(*ast.KeyValueExpr); ok {
								if k, ok := kv.Key.(*ast.BasicLit); ok {
									check(k)
								}
							}
						}
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls < 20 {
		t.Fatalf("only %d log calls found — the walker is not seeing the code", calls)
	}
	sort.Strings(violations)
	for _, v := range violations {
		t.Error(v)
	}
}
