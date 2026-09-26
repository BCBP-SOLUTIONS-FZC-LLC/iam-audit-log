package unit_test

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"
)

type helmValues struct {
	ReplicaCount  int               `yaml:"replicaCount"`
	Env           map[string]string `yaml:"env"`
	ServerEnv     map[string]string `yaml:"serverEnv"`
	ReconcilerEnv map[string]string `yaml:"reconcilerEnv"`
	Secrets       map[string]struct {
		Keys []string `yaml:"keys"`
	} `yaml:"secrets"`
	Ingress struct {
		Hosts []struct {
			Paths []struct {
				Path string `yaml:"path"`
			} `yaml:"paths"`
		} `yaml:"hosts"`
	} `yaml:"ingress"`
	Cronjobs map[string]struct {
		JobName           string `yaml:"jobName"`
		ConcurrencyPolicy string `yaml:"concurrencyPolicy"`
	} `yaml:"cronjobs"`
}

func loadValues(t *testing.T) helmValues {
	t.Helper()
	var v helmValues
	repoYAML(t, "deploy/helm/values.yaml", &v)
	return v
}

// Implementation rule 5 / LLD §3.3.2: each composition root's Secret holds
// only its own role's DSN — the server can never read the audit_reconciler
// DSN and the reconciler can never read the audit_app one.
func TestHelm_PerRootSecretsNeverShareRoleDSNs(t *testing.T) {
	v := loadValues(t)
	server, recon := v.Secrets["server"].Keys, v.Secrets["reconciler"].Keys
	if !slices.Contains(server, "DATABASE_URL") || slices.Contains(server, "RECONCILER_DATABASE_URL") {
		t.Errorf("server secret keys = %v", server)
	}
	if !slices.Contains(recon, "RECONCILER_DATABASE_URL") || slices.Contains(recon, "DATABASE_URL") || slices.Contains(recon, "MIGRATION_DATABASE_URL") {
		t.Errorf("reconciler secret keys = %v", recon)
	}
	if v.ServerEnv["DB_APP_ROLE"] != "audit_app" || v.ReconcilerEnv["DB_RECONCILER_ROLE"] != "audit_reconciler" {
		t.Error("role names must match LLD §4.3")
	}
	if _, leak := v.ServerEnv["DB_RECONCILER_ROLE"]; leak {
		t.Error("server env must not carry the reconciler role")
	}
}

// HLD §5.7 / LLD §13: 3 replicas; production-safe defaults.
func TestHelm_ProductionDefaults(t *testing.T) {
	v := loadValues(t)
	if v.ReplicaCount != 3 {
		t.Errorf("replicaCount = %d, want 3", v.ReplicaCount)
	}
	if v.Env["AUDIT_ARCHIVE_OBJECT_LOCK_MODE"] != "COMPLIANCE" || v.Env["APP_ENV"] != "production" {
		t.Errorf("env defaults = %v", v.Env)
	}
	if v.ServerEnv["PG_MAX_CONNS"] != "20" {
		t.Error("audit pool is 20 conns via PgBouncer (HLD §7.1)")
	}
	for _, j := range v.Cronjobs {
		if j.ConcurrencyPolicy != "Forbid" {
			t.Errorf("CronJob %s must be singleton per run (LLD §13)", j.JobName)
		}
	}
}

// LLD §5.2 / §10.2: only /api/v1/audit is gateway-routed; /internal never.
func TestHelm_IngressExposesOnlyPublicAuditAPI(t *testing.T) {
	for _, h := range loadValues(t).Ingress.Hosts {
		for _, p := range h.Paths {
			if p.Path != "/api/v1/audit" {
				t.Errorf("ingress path %q — only /api/v1/audit may be gateway-routed", p.Path)
			}
		}
	}
}

// LLD §13 rev 0.19: pod labels published for callers' NetworkPolicy selectors.
func TestHelm_PodLabels(t *testing.T) {
	helpers := string(repoFile(t, "deploy/helm/templates/_helpers.tpl"))
	for _, l := range []string{"app.kubernetes.io/name:", "app.kubernetes.io/instance:"} {
		if !strings.Contains(helpers, l) {
			t.Errorf("selector labels missing %s", l)
		}
	}
}

// AL-INV-10: the IRSA role cannot publish; the archive cannot be deleted or
// its lock bypassed (LLD §10.4, §13); Glue is read-only (§7.3.1).
func TestIAMPolicy_LeastPrivilege(t *testing.T) {
	var policy struct {
		Statement []struct {
			Effect string   `json:"Effect"`
			Action []string `json:"Action"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal(repoFile(t, "deploy/iam/policy.json"), &policy); err != nil {
		t.Fatal(err)
	}
	var denied []string
	for _, s := range policy.Statement {
		for _, a := range s.Action {
			if s.Effect == "Allow" && (strings.HasPrefix(a, "sns:") || a == "sqs:SendMessage" || a == "s3:DeleteObject" || strings.HasSuffix(a, ":*")) {
				t.Errorf("forbidden Allow %s", a)
			}
			if s.Effect == "Allow" && strings.HasPrefix(a, "glue:") && !strings.HasPrefix(a, "glue:Get") {
				t.Errorf("Glue must be read-only, got %s", a)
			}
			if s.Effect == "Deny" {
				denied = append(denied, a)
			}
		}
	}
	for _, a := range []string{"s3:DeleteObject", "s3:DeleteObjectVersion", "s3:BypassGovernanceRetention"} {
		if !slices.Contains(denied, a) {
			t.Errorf("policy must explicitly Deny %s", a)
		}
	}
}

// LLD §12: every env var in the configuration table is documented in
// .env-example (schedules live in the Helm CronJobs instead).
func TestEnvExample_CoversLLDConfigTable(t *testing.T) {
	example := string(repoFile(t, ".env-example"))
	for _, k := range []string{
		"DATABASE_URL", "DB_APP_ROLE", "DB_RECONCILER_ROLE", "SERVICE_NAME", "APP_PORT", "METRICS_PORT",
		"SQS_MAX_RECEIVE_COUNT", "SQS_VISIBILITY_TIMEOUT", "GLUE_REGISTRY_REGION",
		"AUDIT_ARCHIVE_BUCKET", "AUDIT_ARCHIVE_KMS_KEY", "AUDIT_ARCHIVE_OBJECT_LOCK_MODE",
		"AUDIT_HOT_WINDOW_DAYS", "AUDIT_PRECREATE_MONTHS", "AUDIT_WRITABLE_TRAILING_MONTHS",
		"AUDIT_DEFAULT_QUERY_WINDOW_DAYS", "CATALOG_BASE_URL", "CATALOG_PLANS_POLL_INTERVAL",
		"CATALOG_PLANS_POLL_TIMEOUT", "MAX_INGEST_BATCH", "MAX_METADATA_BYTES", "EXPORT_SIGNED_URL_TTL",
		"PROCESSED_EVENTS_TTL_DAYS", "INGEST_BATCH_RATE_LIMIT_RPS", "INGEST_BATCH_RATE_LIMIT_BURST",
	} {
		if !regexp.MustCompile(`(?m)^` + k + `=`).MatchString(example) {
			t.Errorf(".env-example missing %s", k)
		}
	}
	for _, sched := range []string{`schedule: "0 2 * * *"`, `schedule: "0 3 * * *"`} {
		if !strings.Contains(string(repoFile(t, "deploy/helm/values.yaml")), sched) {
			t.Errorf("Helm CronJobs missing LLD §12 default %s", sched)
		}
	}
}

// LLD §13: one image, both binaries, non-root, digest-pinned bases.
func TestDockerfile_Contract(t *testing.T) {
	df := string(repoFile(t, "Dockerfile"))
	for _, want := range []string{
		"-o /out/iam-audit-log-server ./cmd/server",
		"-o /out/iam-audit-log-reconciler ./cmd/reconciler",
		"USER nonroot:nonroot",
		`ENTRYPOINT ["/iam-audit-log-server"]`,
	} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile missing %q", want)
		}
	}
	for _, line := range strings.Split(df, "\n") {
		if strings.HasPrefix(line, "FROM ") && !strings.Contains(line, "@sha256:") {
			t.Errorf("unpinned base image: %s", line)
		}
	}
}
