package unit_test

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// LLD §4.4 / §19: golang-migrate NNNNNN_name.{up,down}.sql, every step
// individually reversible, and no outbox schema ever (AL-INV-10).
func TestMigrations_PairedAndWellNamed(t *testing.T) {
	name := regexp.MustCompile(`^(\d{6})_[a-z0-9_]+\.(up|down)\.sql$`)
	files, err := fs.Glob(pgadapter.MigrationsFS(), "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	set := map[string]bool{}
	for _, f := range files {
		if !name.MatchString(f) {
			t.Errorf("%s: must be NNNNNN_name.up.sql / .down.sql", f)
		}
		set[f] = true
		b, _ := fs.ReadFile(pgadapter.MigrationsFS(), f)
		for i, line := range strings.Split(string(b), "\n") {
			code, _, _ := strings.Cut(line, "--") // comments may document the rule
			if strings.Contains(strings.ToLower(code), "outbox") {
				t.Errorf("%s:%d references an outbox (AL-INV-10)", f, i+1)
			}
		}
	}
	for f := range set {
		var pair string
		if strings.HasSuffix(f, ".up.sql") {
			pair = strings.TrimSuffix(f, ".up.sql") + ".down.sql"
		} else {
			pair = strings.TrimSuffix(f, ".down.sql") + ".up.sql"
		}
		if !set[pair] {
			t.Errorf("%s has no matching %s", f, pair)
		}
	}
}

// Decision D-18: the redaction marker has one SQL definition
// (redaction_marker() in 000007) and one Go definition
// (domain.RedactedMetadata); their keys must match exactly, or an ingest-time
// redacted row would differ from an apply/sweep-redacted one.
func TestMigrations_RedactionMarkerParity_D18(t *testing.T) {
	b, err := fs.ReadFile(pgadapter.MigrationsFS(), "000007_redaction.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	fn := regexp.MustCompile(`(?s)CREATE OR REPLACE FUNCTION redaction_marker\(.*?\$\$(.*?)\$\$`).FindSubmatch(b)
	if fn == nil {
		t.Fatal("redaction_marker() not found in 000007")
	}
	sqlKeys := map[string]bool{}
	for _, call := range regexp.MustCompile(`jsonb_build_object\(([^)]*)\)`).FindAllSubmatch(fn[1], -1) {
		for _, k := range regexp.MustCompile(`'(_[a-z_]+)'`).FindAllSubmatch(call[1], -1) {
			sqlKeys[string(k[1])] = true
		}
	}
	var m map[string]any
	if err := json.Unmarshal(domain.RedactedMetadata("t", time.Now(), true), &m); err != nil {
		t.Fatal(err)
	}
	goKeys := map[string]bool{}
	for k := range m {
		goKeys[k] = true
	}
	if len(sqlKeys) != 4 || fmt.Sprint(sortedKeys(sqlKeys)) != fmt.Sprint(sortedKeys(goKeys)) {
		t.Errorf("marker keys differ: SQL %v, Go %v", sortedKeys(sqlKeys), sortedKeys(goKeys))
	}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
