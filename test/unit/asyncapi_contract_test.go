package unit_test

import (
	"sort"
	"strings"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// api/asyncapi.yaml declares exactly the compiled bus mappings (§7.1 + D-7
// aliases), each with its classification — the published contract and the
// code cannot drift.
func TestAsyncAPI_MessagesMatchTaxonomy(t *testing.T) {
	var spec struct {
		Components struct {
			Messages map[string]struct {
				Name      string `yaml:"name"`
				Topic     string `yaml:"x-audit-topic"`
				EntryType string `yaml:"x-audit-entry-type"`
				Tier      string `yaml:"x-audit-retention-tier"`
			} `yaml:"messages"`
		} `yaml:"components"`
	}
	repoYAML(t, "api/asyncapi.yaml", &spec)

	var got, want []string
	for _, m := range spec.Components.Messages {
		got = append(got, m.Topic+"|"+m.Name+"|"+m.EntryType+"|"+m.Tier)
	}
	tiers := domain.EntryTypes()
	for _, b := range domain.BusMappings() {
		want = append(want, b.Topic+"|"+b.SourceEventType+"|"+b.EntryType+"|"+string(tiers[b.EntryType]))
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("asyncapi messages drift from the taxonomy:\n got  %v\n want %v", got, want)
	}
}
