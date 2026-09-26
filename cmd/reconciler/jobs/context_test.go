package jobs

import "testing"

// LLD §12: reconcile, processed-events-prune (Phase 7), redaction-retry and
// redaction-sweep (Phase 6, D-18) are registered.
func TestRegistry_JobsForThisPhase(t *testing.T) {
	r := Registry()
	for _, name := range []string{"reconcile", "processed-events-prune", "redaction-retry", "redaction-sweep"} {
		if _, ok := r[name]; !ok {
			t.Errorf("job %q missing", name)
		}
	}
	if len(r) != 4 {
		t.Fatalf("registry = %v; update this test as jobs land", r)
	}
}
