package config

import (
	"strings"
	"testing"
)

// CAT-I2 poller config (AL-D15): the base URL must be an absolute http(s)
// URL and the per-poll timeout must be positive and shorter than the
// interval.
func TestLoadServer_CatalogValidation(t *testing.T) {
	bad := []struct{ key, val, want string }{
		{"CATALOG_BASE_URL", "iam-catalog-admin:8080", "CATALOG_BASE_URL"},
		{"CATALOG_BASE_URL", "ftp://iam-catalog-admin", "CATALOG_BASE_URL"},
		{"CATALOG_BASE_URL", "http://", "CATALOG_BASE_URL"},
		{"CATALOG_BASE_URL", "http://bad host\x7f", "CATALOG_BASE_URL"},
		{"CATALOG_PLANS_POLL_TIMEOUT", "600s", "CATALOG_PLANS_POLL_TIMEOUT"}, // == interval
		{"CATALOG_PLANS_POLL_TIMEOUT", "700s", "CATALOG_PLANS_POLL_TIMEOUT"},
	}
	clearCatalog := func() {
		clearQueryExportEnv(t)
		for _, k := range []string{"CATALOG_BASE_URL", "CATALOG_PLANS_POLL_INTERVAL", "CATALOG_PLANS_POLL_TIMEOUT"} {
			t.Setenv(k, "")
		}
	}
	for _, tc := range bad {
		clearCatalog()
		t.Setenv(tc.key, tc.val)
		if _, err := LoadServer("v"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s=%q: err = %v", tc.key, tc.val, err)
		}
	}
	for _, ok := range []map[string]string{
		{"CATALOG_BASE_URL": "https://catalog.example:8443/"},
		{"CATALOG_PLANS_POLL_INTERVAL": "10s", "CATALOG_PLANS_POLL_TIMEOUT": "9s"},
		{"CATALOG_PLANS_POLL_TIMEOUT": "-1s"}, // non-positive durations fall back to the 3s default
	} {
		clearCatalog()
		for k, v := range ok {
			t.Setenv(k, v)
		}
		c, err := LoadServer("v")
		if err != nil {
			t.Errorf("%v must be accepted: %v", ok, err)
		}
		if ok["CATALOG_PLANS_POLL_TIMEOUT"] == "-1s" && c.CatalogPollTimeout.Seconds() != 3 {
			t.Errorf("negative timeout: got %v, want the 3s default", c.CatalogPollTimeout)
		}
	}
}
