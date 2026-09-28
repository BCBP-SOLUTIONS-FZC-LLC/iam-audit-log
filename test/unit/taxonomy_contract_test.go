package unit_test

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// The compiled taxonomy equals the LLD §25 frozen entry_type vocabulary,
// parsed straight from the spec — a drift in either fails CI (AL-D3).
func TestTaxonomy_MatchesLLDFrozenVocabulary(t *testing.T) {
	lld := string(repoFile(t, "docs/lld/iam-lld-audit-log-service.md"))
	var line string
	for _, l := range strings.Split(lld, "\n") {
		if strings.HasPrefix(l, "**`entry_type` vocabulary (frozen") {
			line = l
		}
	}
	if line == "" {
		t.Fatal("LLD §25 entry_type vocabulary line not found")
	}
	var spec []string
	for _, m := range regexp.MustCompile("`([a-z][a-z0-9_]*(?:\\.[a-z0-9_]+)+)`").FindAllStringSubmatch(line, -1) {
		spec = append(spec, m[1])
	}
	var code []string
	for et := range domain.EntryTypes() {
		code = append(code, et)
	}
	sort.Strings(spec)
	sort.Strings(code)
	if strings.Join(spec, ",") != strings.Join(code, ",") {
		t.Errorf("taxonomy drift from LLD §25:\n spec %v\n code %v", spec, code)
	}
}
