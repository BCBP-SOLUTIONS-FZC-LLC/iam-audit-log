package jobs

import "testing"

// LLD §12: the reconcile job is registered (processed-events-prune: Phase 7).
func TestRegistry_JobsForThisPhase(t *testing.T) {
	r := Registry()
	if _, ok := r["reconcile"]; !ok || len(r) != 1 {
		t.Fatalf("registry = %v; update this test as jobs land", r)
	}
}
