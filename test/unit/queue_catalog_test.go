package unit_test

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/config"
)

func catalogNames() []string {
	var out []string
	for _, q := range config.InboundQueues() {
		out = append(out, q.Name)
	}
	sort.Strings(out)
	return out
}

func assertSameSet(t *testing.T, where string, got []string) {
	t.Helper()
	sort.Strings(got)
	want := catalogNames()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s queues drift from config.InboundQueues (LLD §7.1):\n got  %v\n want %v", where, got, want)
	}
}

// LLD §7.1 / §25: the frozen 11-queue catalog, consumer discriminators, and
// env var names.
func TestQueueCatalog_FrozenLLDValues(t *testing.T) {
	qs := config.InboundQueues()
	if len(qs) != 11 {
		t.Fatalf("got %d queues, LLD §7.1 has 11", len(qs))
	}
	seen := map[string]bool{}
	for _, q := range qs {
		if !strings.HasSuffix(q.Name, "-audit-q") {
			t.Errorf("%s: queue names follow <topic-short>-audit-q (HLD §9.1)", q.Name)
		}
		want := strings.ToUpper(strings.ReplaceAll(strings.TrimSuffix(q.Name, "-q"), "-", "_")) + "_QUEUE_URL"
		if q.EnvVar != want {
			t.Errorf("%s: env var %s, want %s (LLD §12)", q.Name, q.EnvVar, want)
		}
		if seen[q.Consumer] {
			t.Errorf("duplicate consumer discriminator %s", q.Consumer)
		}
		seen[q.Consumer] = true
	}
}

// The AsyncAPI contract declares exactly one channel per catalog queue.
func TestQueueCatalog_MatchesAsyncAPI(t *testing.T) {
	var spec struct {
		Channels map[string]struct {
			Address string `yaml:"address"`
		} `yaml:"channels"`
	}
	repoYAML(t, "api/asyncapi.yaml", &spec)
	var got []string
	for _, ch := range spec.Channels {
		got = append(got, ch.Address)
	}
	assertSameSet(t, "api/asyncapi.yaml", got)
}

// The dev topology script provisions exactly the catalog queues.
func TestQueueCatalog_MatchesInitFloci(t *testing.T) {
	re := regexp.MustCompile(`(?m)^[a-z0-9-]+:([a-z0-9-]+-audit-q):[A-Z_]+$`)
	var got []string
	for _, m := range re.FindAllStringSubmatch(string(repoFile(t, "scripts/init-floci.sh")), -1) {
		got = append(got, m[1])
	}
	assertSameSet(t, "scripts/init-floci.sh", got)
}

// The IRSA policy grants consume on exactly the catalog queues.
func TestQueueCatalog_MatchesIAMPolicy(t *testing.T) {
	var policy struct {
		Statement []struct {
			Sid      string   `json:"Sid"`
			Resource []string `json:"Resource"`
		} `json:"Statement"`
	}
	if err := json.Unmarshal(repoFile(t, "deploy/iam/policy.json"), &policy); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, s := range policy.Statement {
		if s.Sid != "ConsumeAuditQueues" {
			continue
		}
		for _, r := range s.Resource {
			got = append(got, r[strings.LastIndex(r, ":")+1:])
		}
	}
	assertSameSet(t, "deploy/iam/policy.json", got)
}

// Every queue URL env var is wired in Helm (serverEnv) and .env-example.
func TestQueueCatalog_EnvVarsWiredEverywhere(t *testing.T) {
	var values struct {
		ServerEnv map[string]string `yaml:"serverEnv"`
	}
	repoYAML(t, "deploy/helm/values.yaml", &values)
	example := string(repoFile(t, ".env-example"))
	for _, q := range config.InboundQueues() {
		if _, ok := values.ServerEnv[q.EnvVar]; !ok {
			t.Errorf("deploy/helm/values.yaml serverEnv missing %s", q.EnvVar)
		}
		if !strings.Contains(example, "\n"+q.EnvVar+"=") {
			t.Errorf(".env-example missing %s", q.EnvVar)
		}
	}
}
