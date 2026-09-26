package config

import (
	"strings"
	"testing"
	"time"
)

// redaction-retry job config (LLD §8.7, RB-7).
func TestLoadReconciler_RedactionRetry(t *testing.T) {
	clearEnv(t)
	t.Setenv("RECONCILER_DATABASE_URL", "postgres://recon")
	t.Setenv("REDACTION_RETRY_MIN_AGE", "")
	t.Setenv("REDACTION_RETRY_BATCH", "")
	c, err := LoadReconciler("v", "redaction-retry")
	if err != nil {
		t.Fatal(err)
	}
	if c.RedactionRetryMinAge != 5*time.Minute || c.RedactionRetryBatch != 500 {
		t.Errorf("defaults = %v / %d", c.RedactionRetryMinAge, c.RedactionRetryBatch)
	}

	t.Setenv("REDACTION_RETRY_MIN_AGE", "30m")
	t.Setenv("REDACTION_RETRY_BATCH", "10000")
	c, err = LoadReconciler("v", "redaction-retry")
	if err != nil || c.RedactionRetryMinAge != 30*time.Minute || c.RedactionRetryBatch != 10000 {
		t.Errorf("overrides = %v / %d, err %v", c.RedactionRetryMinAge, c.RedactionRetryBatch, err)
	}

	t.Setenv("REDACTION_RETRY_BATCH", "10001")
	if _, err := LoadReconciler("v", "redaction-retry"); err == nil || !strings.Contains(err.Error(), "REDACTION_RETRY_BATCH") {
		t.Errorf("batch 10001: err = %v", err)
	}
	// envInt maps non-positive values to the default, so the lower bound
	// is defensive only.
	for _, v := range []string{"0", "-1"} {
		t.Setenv("REDACTION_RETRY_BATCH", v)
		if c, err := LoadReconciler("v", "redaction-retry"); err != nil || c.RedactionRetryBatch != 500 {
			t.Errorf("batch %s: %d, err %v", v, c.RedactionRetryBatch, err)
		}
	}
}

// D-18: redaction-sweep window (sweep_redactions bound: 1 d .. 400 d).
func TestLoadReconciler_RedactionSweepWindow(t *testing.T) {
	clearEnv(t)
	t.Setenv("RECONCILER_DATABASE_URL", "postgres://recon")
	t.Setenv("REDACTION_SWEEP_WINDOW", "")
	c, err := LoadReconciler("v", "redaction-sweep")
	if err != nil || c.RedactionSweepWindow != 2160*time.Hour {
		t.Fatalf("default = %v, err %v", c.RedactionSweepWindow, err)
	}
	for _, ok := range []string{"24h", "720h", "9600h"} {
		t.Setenv("REDACTION_SWEEP_WINDOW", ok)
		if c, err := LoadReconciler("v", "redaction-sweep"); err != nil || c.RedactionSweepWindow.String() == "" {
			t.Errorf("%s: err %v", ok, err)
		}
	}
	t.Setenv("REDACTION_SWEEP_WINDOW", "720h")
	if c, _ := LoadReconciler("v", "redaction-sweep"); c.RedactionSweepWindow != 720*time.Hour {
		t.Errorf("override = %v", c.RedactionSweepWindow)
	}
	for _, bad := range []string{"23h", "9601h"} {
		t.Setenv("REDACTION_SWEEP_WINDOW", bad)
		if _, err := LoadReconciler("v", "redaction-sweep"); err == nil || !strings.Contains(err.Error(), "REDACTION_SWEEP_WINDOW") {
			t.Errorf("%s: err = %v", bad, err)
		}
	}
}
