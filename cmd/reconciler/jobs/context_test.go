package jobs

import "testing"

// LLD §12: reconcile, redaction-retry and redaction-sweep (Phase 6, D-18)
// are registered (processed-events-prune: Phase 7).
func TestRegistry_JobsForThisPhase(t *testing.T) {
	r := Registry()
	for _, name := range []string{"reconcile", "redaction-retry", "redaction-sweep"} {
		if _, ok := r[name]; !ok {
			t.Errorf("job %q missing", name)
		}
	}
	if len(r) != 3 {
		t.Fatalf("registry = %v; update this test as jobs land", r)
	}
}
