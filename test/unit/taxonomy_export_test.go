package unit_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

// taxonomyExportPath is the machine-readable taxonomy other repositories read
// (LLD §7.1, rev 0.34; platform-audit PA-OQ-16). It is a copy for
// development-time checks: this service stays the only runtime validator.
const taxonomyExportPath = "api/taxonomy.json"

type taxonomyExport struct {
	Comment    string                `json:"_comment"`
	EntryTypes []taxonomyExportEntry `json:"entry_types"`
}

type taxonomyExportEntry struct {
	EntryType   string   `json:"entry_type"`
	Tier        string   `json:"tier"`
	DirectWrite bool     `json:"direct_write"`
	Producers   []string `json:"producers"` // empty: any mesh-admitted caller
}

// buildTaxonomyExport renders the compiled taxonomy, sorted by entry_type, in
// the committed file's exact byte form.
func buildTaxonomyExport(t *testing.T) []byte {
	t.Helper()
	types := domain.EntryTypes()
	names := make([]string, 0, len(types))
	for et := range types {
		names = append(names, et)
	}
	sort.Strings(names)
	out := taxonomyExport{
		Comment: "Generated from internal/core/domain/taxonomy.go by `make taxonomy-json`; do not edit. " +
			"Every known entry_type (<domain>.unknown excluded), its retention tier, whether AL-5/AL-6 accept it, " +
			"and the source_service values it is restricted to (empty = any admitted caller). LLD §7.1, §25.",
		EntryTypes: make([]taxonomyExportEntry, 0, len(names)),
	}
	for _, et := range names {
		producers := domain.DirectWriteProducers(et)
		if producers == nil {
			producers = []string{}
		}
		out.EntryTypes = append(out.EntryTypes, taxonomyExportEntry{
			EntryType: et, Tier: string(types[et]),
			DirectWrite: domain.IsDirectWriteEntryType(et), Producers: producers,
		})
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// api/taxonomy.json equals taxonomy.go. TAXONOMY_JSON_WRITE=1 (make
// taxonomy-json) rewrites the file instead of comparing.
func TestTaxonomyExport_MatchesCode(t *testing.T) {
	want := buildTaxonomyExport(t)
	if os.Getenv("TAXONOMY_JSON_WRITE") == "1" {
		if err := os.WriteFile(filepath.Join("..", "..", taxonomyExportPath), want, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !bytes.Equal(repoFile(t, taxonomyExportPath), want) {
		t.Fatalf("%s is stale: run `make taxonomy-json` and commit it", taxonomyExportPath)
	}
}
