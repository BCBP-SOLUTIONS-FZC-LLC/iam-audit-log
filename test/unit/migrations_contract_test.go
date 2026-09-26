package unit_test

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"

	pgadapter "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/postgres"
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
