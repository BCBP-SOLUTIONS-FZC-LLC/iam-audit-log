//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Phase 5 e2e: AL-7 (mesh-only provenance read, LLD §5.2/§5.3, decision
// D-14) and the CAT-I2 plans poller (AL-D15).

func (e *e2eEnv) internalEvents(t *testing.T, q url.Values, headers map[string]string) (int, eventsOut, string) {
	t.Helper()
	code, _, body := e.do(t, reqOpts{path: "/api/v1/internal/audit/events?" + q.Encode(), headers: headers})
	var out eventsOut
	if code == http.StatusOK {
		require.NoError(t, json.Unmarshal([]byte(body), &out), body)
	}
	return code, out, body
}

func eventIDs(out eventsOut) []string {
	ids := make([]string, len(out.Events))
	for i, ev := range out.Events {
		ids[i] = ev.ID
	}
	return ids
}

func (e *e2eEnv) seedPlanRow(t *testing.T, tenant, plan string, days int) {
	t.Helper()
	_, err := e.seed(t).Exec(context.Background(),
		`INSERT INTO tenant_plan_window (tenant_id, plan_code, query_window_days, last_event_at) VALUES ($1, $2, $3, now())`,
		tenant, plan, days)
	require.NoError(t, err)
}

// §5.4 / §15.3: AL-7 is not plan-clamped — compliance/legal retrieve rows
// the tenant's own AL-1 window hides.
func TestAL7_ProvenanceReadNoClamp(t *testing.T) {
	e := newE2EEnv(t)
	e.seedPlanRow(t, tenantA, "starter", 1)
	old := e.ingestAt(t, tenantA, time.Now().UTC().Add(-72*time.Hour))

	code, out, body := e.listEvents(t, tenantA, url.Values{})
	require.Equal(t, http.StatusOK, code, body)
	assert.NotContains(t, eventIDs(out), old, "AL-1 is clamped to the 1-day plan window")

	code, out, body = e.internalEvents(t, url.Values{"tenant_id": {tenantA}}, systemHeaders(tenantA))
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{old}, eventIDs(out))
	assert.False(t, out.WindowClamped)
	assert.Nil(t, out.NextCursor)
}

// §5.2: AL-7 is RLS-scoped to the explicit tenant_id and mesh-only.
func TestAL7_TenantIsolationAndAuth(t *testing.T) {
	e := newE2EEnv(t)
	a := e.ingestAt(t, tenantA, time.Now().UTC().Add(-time.Hour))

	code, out, body := e.internalEvents(t, url.Values{"tenant_id": {tenantB}}, systemHeaders(tenantA))
	require.Equal(t, http.StatusOK, code, body)
	assert.Empty(t, out.Events, "tenant_id=B never sees A's rows")

	code, out, body = e.internalEvents(t, url.Values{"tenant_id": {tenantA}}, systemHeaders(tenantB))
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{a}, eventIDs(out), "the query tenant_id, not x-tenant-id, scopes the read")

	code, _, body = e.internalEvents(t, url.Values{"tenant_id": {tenantA}}, adminHeaders(tenantA))
	assert.Equal(t, http.StatusForbidden, code, body)
	assert.Equal(t, "forbidden_peer", errorCode(t, body))

	for name, q := range map[string]url.Values{
		"missing tenant_id": {},
		"bad tenant_id":     {"tenant_id": {"nope"}},
		"bad cursor":        {"tenant_id": {tenantA}, "cursor": {"!!"}},
	} {
		code, _, body := e.internalEvents(t, q, systemHeaders(tenantA))
		assert.Equal(t, http.StatusBadRequest, code, "%s: %s", name, body)
		assert.Equal(t, "invalid_request", errorCode(t, body), name)
	}
}

// Decision D-14: an archived range over the D-10 bound is 422
// range_too_large for AL-7 (no export path for services); a hot-only range
// is unaffected.
func TestAL7_ArchivedRangeTooLarge_D14(t *testing.T) {
	e := newE2EEnv(t)
	recent := e.ingestAt(t, tenantA, time.Now().UTC().Add(-10*time.Minute))
	_, err := e.seed(t).Exec(context.Background(), `INSERT INTO audit_archive_objects
		(partition_name, retention_tier, tenant_id, part, period_month, s3_bucket, s3_key, row_count, byte_size,
		 min_occurred_at, max_occurred_at, min_id, max_id, sha256)
		VALUES ('audit_events_2019_01', 'compliance_7y', $1, 0, '2019-01-01', $2, 'compliance_7y/k', 20000, 1000,
		        '2019-01-01', '2019-01-31', '01685111-0000-7000-8000-000000000000', '01685111-ffff-7000-8000-000000000000', 'x')`,
		tenantA, e2eBucket)
	require.NoError(t, err)

	code, _, body := e.internalEvents(t, url.Values{"tenant_id": {tenantA}, "from": {"2018-12-01T00:00:00Z"}}, systemHeaders(tenantA))
	assert.Equal(t, http.StatusUnprocessableEntity, code, body)
	assert.Equal(t, "range_too_large", errorCode(t, body))

	code, out, body := e.internalEvents(t, url.Values{
		"tenant_id": {tenantA}, "from": {time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)},
	}, systemHeaders(tenantA))
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{recent}, eventIDs(out))
}

// fakeCatalog serves Catalog's CAT-I2 wire shape (InternalPlansResponse)
// and checks the iam-system sentinel headers on every request.
type fakeCatalog struct {
	t        *testing.T
	mu       sync.Mutex
	days     int
	version  int64
	fail     bool
	requests int
	failures int
}

func (f *fakeCatalog) set(days int, version int64, fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.days, f.version, f.fail, f.failures = days, version, fail, 0
}

func (f *fakeCatalog) failuresSinceSet() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.failures
}

func (f *fakeCatalog) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	assert.Equal(f.t, http.MethodGet, r.Method)
	assert.Equal(f.t, "/api/v1/internal/plans", r.URL.Path)
	assert.Equal(f.t, "iam-system", r.Header.Get("x-user-id"))
	assert.Equal(f.t, "00000000-0000-0000-0000-000000000000", r.Header.Get("x-tenant-id"))
	assert.Equal(f.t, "iam-system", r.Header.Get("x-tenant-roles"))
	if f.fail {
		f.failures++
		http.Error(w, `{"error":"internal_error"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"plans": []map[string]any{{
			"code": "starter", "display_name": "Starter", "workflow_template_limit": 5, "tender_limit": nil,
			"trial_duration_days": 14, "sso_enabled": false, "custom_branding": "none",
			"audit_query_window_days": f.days, "feature_set": map[string]any{}, "record_version": f.version,
		}},
		"record_versions": map[string]int64{"starter": f.version},
	})
}

// AL-D15: the query window comes from Catalog's live plan map keyed by the
// tenant's stored plan_code, not the row's stored days; a Catalog outage
// keeps the last good map (stale-if-error); a record_version bump swaps in
// the new window.
func TestCatalogPoller_PlanWindowFromCatalog_ALD15(t *testing.T) {
	cat := &fakeCatalog{t: t}
	cat.set(30, 1, false)
	srv := httptest.NewServer(cat)
	t.Cleanup(srv.Close)
	e := newE2EEnvOpts(t, e2eOpts{catalogURL: srv.URL})
	e.seedPlanRow(t, tenantA, "starter", 1) // stored row says 1 day; Catalog says 30
	old := e.ingestAt(t, tenantA, time.Now().UTC().Add(-72*time.Hour))

	visible := func() (bool, eventsOut) {
		code, out, body := e.listEvents(t, tenantA, url.Values{})
		require.Equal(t, http.StatusOK, code, body)
		for _, id := range eventIDs(out) {
			if id == old {
				return true, out
			}
		}
		return false, out
	}

	require.Eventually(t, func() bool { ok, _ := visible(); return ok }, 10*time.Second, 50*time.Millisecond,
		"the 30-day Catalog window must replace the row's 1 day")
	_, out := visible()
	assert.WithinDuration(t, time.Now().Add(-30*24*time.Hour), out.EffectiveFrom, time.Minute)

	cat.set(30, 1, true)
	require.Eventually(t, func() bool { return cat.failuresSinceSet() >= 3 }, 10*time.Second, 50*time.Millisecond)
	ok, _ := visible()
	assert.True(t, ok, "stale-if-error: a Catalog outage keeps the last good 30-day map")

	cat.set(2, 2, false)
	require.Eventually(t, func() bool { ok, _ := visible(); return !ok }, 10*time.Second, 50*time.Millisecond,
		"a record_version bump to a 2-day window hides the 3-day-old entry")
	_, out = visible()
	assert.WithinDuration(t, time.Now().Add(-2*24*time.Hour), out.EffectiveFrom, time.Minute)
}
