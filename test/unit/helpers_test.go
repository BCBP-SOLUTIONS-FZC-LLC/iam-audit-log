// Package unit_test holds black-box contract tests (no Docker, no network),
// mirroring iam-org-membership/test/unit: they assert that this repo's own
// artifacts — config, AsyncAPI, floci script, Helm chart, IAM policy, env
// example, migrations, Dockerfile — agree with the LLD and with each other,
// so a drift in one is caught before it reaches a cluster.
package unit_test

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// repoFile reads a file relative to the repository root.
func repoFile(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return b
}

func repoYAML(t *testing.T, rel string, out any) {
	t.Helper()
	if err := yaml.Unmarshal(repoFile(t, rel), out); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
}
