package config

import (
	"testing"
	"time"
)

func clearOpsEnv(t *testing.T) {
	t.Helper()
	clearEnv(t)
	for _, k := range []string{"OPS_STATS_INTERVAL", "OPS_ARCHIVE_STALL_GRACE", "OPS_REDACTION_PENDING_AGE"} {
		t.Setenv(k, "")
	}
	t.Setenv("DATABASE_URL", "postgres://app")
}

// Decision D-21: ops-gauge cadence and thresholds.
func TestLoadServer_OpsDefaults(t *testing.T) {
	clearOpsEnv(t)
	c, err := LoadServer("v")
	if err != nil {
		t.Fatal(err)
	}
	if c.OpsStatsInterval != time.Minute || c.OpsStallGrace != 48*time.Hour || c.OpsRedactionPendingAge != 15*time.Minute {
		t.Errorf("ops defaults = %v / %v / %v", c.OpsStatsInterval, c.OpsStallGrace, c.OpsRedactionPendingAge)
	}
}

func TestLoadServer_OpsOverrides(t *testing.T) {
	clearOpsEnv(t)
	t.Setenv("OPS_STATS_INTERVAL", "30s")
	t.Setenv("OPS_ARCHIVE_STALL_GRACE", "72h")
	t.Setenv("OPS_REDACTION_PENDING_AGE", "5m")
	c, err := LoadServer("v")
	if err != nil {
		t.Fatal(err)
	}
	if c.OpsStatsInterval != 30*time.Second || c.OpsStallGrace != 72*time.Hour || c.OpsRedactionPendingAge != 5*time.Minute {
		t.Errorf("ops overrides = %v / %v / %v", c.OpsStatsInterval, c.OpsStallGrace, c.OpsRedactionPendingAge)
	}
}
