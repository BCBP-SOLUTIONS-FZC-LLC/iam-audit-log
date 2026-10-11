//go:build integration || e2e

package fixtures

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

const ownProcessEnv = "AUDIT_TEST_OWN_PROCESS"

// InOwnProcess runs the calling test in a child process of the same test
// binary, for a test that needs a different observability identity (APP_ENV)
// than the rest of the binary: platform-gincommon fixes the metrics identity
// once per process and refuses another (metrics.ErrIdentityMismatch).
//
// It returns true in the child, where the caller runs its body. In the
// parent it re-executes only this test (the parent test's setup runs again
// there), fails t with the child's output if the child fails, and returns
// false; the caller then returns.
//
//	if !fixtures.InOwnProcess(t) {
//		return
//	}
func InOwnProcess(t *testing.T) bool {
	t.Helper()
	if os.Getenv(ownProcessEnv) == "1" {
		return true
	}
	parts := strings.Split(t.Name(), "/")
	for i, p := range parts {
		parts[i] = "^" + regexp.QuoteMeta(p) + "$"
	}
	args := []string{"-test.run=" + strings.Join(parts, "/"), "-test.count=1", "-test.v"}
	// Under `go test -cover…` the binary writes its counters to
	// -test.gocoverdir, and go test merges every file there into the
	// profile. Forwarding only that flag keeps the child's coverage without
	// letting it overwrite the parent's -test.coverprofile.
	for _, a := range os.Args[1:] {
		if strings.HasPrefix(a, "-test.gocoverdir=") {
			args = append(args, a)
		}
	}
	cmd := exec.CommandContext(t.Context(), os.Args[0], args...) //nolint:gosec // re-executes this test binary with a QuoteMeta-escaped test name
	cmd.Env = append(os.Environ(), ownProcessEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s in its own process: %v\n%s", t.Name(), err, out)
	}
	if !strings.Contains(string(out), "--- PASS: "+parts0(t.Name())) {
		t.Fatalf("%s in its own process ran no test:\n%s", t.Name(), out)
	}
	return false
}

// parts0 is the top-level test name, which -test.v reports as passed.
func parts0(name string) string { return strings.SplitN(name, "/", 2)[0] }
