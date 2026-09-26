package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

const (
	exportID = "33333333-3333-3333-3333-333333333333"
	entryID  = "44444444-4444-4444-4444-444444444444"
)

type fakeQuerier struct {
	req    service.QueryRequest
	res    service.QueryResult
	err    error
	getID  string
	getTen string
	entry  domain.AuditEntry
	getErr error
}

func (f *fakeQuerier) Query(_ context.Context, req service.QueryRequest) (service.QueryResult, error) {
	f.req = req
	return f.res, f.err
}

func (f *fakeQuerier) Get(_ context.Context, tenantID, id string) (domain.AuditEntry, error) {
	f.getTen, f.getID = tenantID, id
	return f.entry, f.getErr
}

type fakeExporter struct {
	calls     int
	tenant    string
	requester string
	filter    domain.QueryFilter
	job       domain.ExportJob
	err       error
	view      service.ExportView
	statusErr error
	statusID  string
}

func (f *fakeExporter) Request(_ context.Context, tenantID, requestedBy string, fl domain.QueryFilter) (domain.ExportJob, error) {
	f.calls++
	f.tenant, f.requester, f.filter = tenantID, requestedBy, fl
	return f.job, f.err
}

func (f *fakeExporter) Status(_ context.Context, _, id string) (service.ExportView, error) {
	f.statusID = id
	return f.view, f.statusErr
}

func queryRouter(q Querier, e Exporter, l *TenantRateLimiter) *Router {
	return NewRouter(RouterConfig{
		GinConfig: gincommon.Config{ServiceName: "iam-audit-log", BuildVersion: "test"},
		Query:     NewQueryHandler(q, e, l),
	})
}

func adminHeaders(role string) map[string]string {
	return map[string]string{"x-user-id": userA, "x-tenant-id": tenantA, "x-tenant-roles": role}
}

func sendJSON(t *testing.T, r *Router, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req)
	return w
}

func TestQueryRoutes_AuthZ(t *testing.T) {
	r := queryRouter(&fakeQuerier{}, &fakeExporter{job: domain.ExportJob{ID: exportID}}, nil)
	for _, rt := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/audit/events"},
		{http.MethodGet, "/api/v1/audit/events/" + entryID},
		{http.MethodPost, "/api/v1/audit/exports"},
		{http.MethodGet, "/api/v1/audit/exports/" + exportID},
	} {
		w := sendJSON(t, r, rt.method, rt.path, "", nil)
		assert.Equal(t, http.StatusUnauthorized, w.Code, rt.path)

		w = sendJSON(t, r, rt.method, rt.path, "", adminHeaders("member"))
		assert.Equal(t, http.StatusForbidden, w.Code, rt.path)
		assert.Equal(t, "insufficient_permissions", decode(t, w)["code"])

		w = sendJSON(t, r, rt.method, rt.path, "", adminHeaders("tenant_owner"))
		assert.NotEqual(t, http.StatusNotFound, w.Code, "route %s must be mounted", rt.path)
		assert.Less(t, w.Code, 300, rt.path)
	}
}

func TestQueryRoutes_NotMountedWithoutHandler(t *testing.T) {
	r := NewRouter(RouterConfig{GinConfig: gincommon.Config{ServiceName: "iam-audit-log", BuildVersion: "test"}})
	w := sendJSON(t, r, http.MethodGet, "/api/v1/audit/events", "", adminHeaders("tenant_admin"))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestListEvents_ParsesFilterAndUsesGatewayIdentity(t *testing.T) {
	q := &fakeQuerier{}
	r := queryRouter(q, &fakeExporter{}, nil)
	path := "/api/v1/audit/events?from=2026-01-01T05:30:00%2B05:30&to=2026-03-31T23:59:59Z" +
		"&entry_type=a.b&entry_type=c.d&actor_id=" + userA + "&actor_type=user&target_type=tender&target_id=t1" +
		"&source_service=iam-org-membership&retention_tier=compliance_7y&limit=50&cursor=abc" +
		"&tenant_id=99999999-9999-9999-9999-999999999999"
	w := sendJSON(t, r, http.MethodGet, path, "", adminHeaders("tenant_admin"))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	assert.Equal(t, tenantA, q.req.TenantID, "tenant comes from x-tenant-id, never the query")
	assert.Equal(t, "abc", q.req.Cursor)
	assert.Equal(t, 50, q.req.Limit)
	f := q.req.Filter
	require.NotNil(t, f.From)
	assert.Equal(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), *f.From)
	assert.Equal(t, time.UTC, f.From.Location())
	assert.Equal(t, time.Date(2026, 3, 31, 23, 59, 59, 0, time.UTC), *f.To)
	assert.Equal(t, []string{"a.b", "c.d"}, f.EntryTypes)
	assert.Equal(t, domain.QueryFilter{
		From: f.From, To: f.To, EntryTypes: f.EntryTypes, ActorID: userA, ActorType: "user",
		TargetType: "tender", TargetID: "t1", SourceService: "iam-org-membership", RetentionTier: "compliance_7y",
	}, f)
}

func TestListEvents_LimitDefaultsAndExplicitZero(t *testing.T) {
	q := &fakeQuerier{}
	r := queryRouter(q, &fakeExporter{}, nil)
	sendJSON(t, r, http.MethodGet, "/api/v1/audit/events", "", adminHeaders("tenant_admin"))
	assert.Equal(t, 0, q.req.Limit, "absent → service default")
	assert.Nil(t, q.req.Filter.From)
	sendJSON(t, r, http.MethodGet, "/api/v1/audit/events?limit=0", "", adminHeaders("tenant_admin"))
	assert.Equal(t, -1, q.req.Limit, "explicit 0 is out of range, not default")
}

func TestListEvents_BadQueryParams_400(t *testing.T) {
	q := &fakeQuerier{}
	r := queryRouter(q, &fakeExporter{}, nil)
	for _, qs := range []string{"from=yesterday", "to=2026-01-01", "limit=ten"} {
		w := sendJSON(t, r, http.MethodGet, "/api/v1/audit/events?"+qs, "", adminHeaders("tenant_admin"))
		assert.Equal(t, http.StatusBadRequest, w.Code, qs)
		assert.Equal(t, "invalid_request", decode(t, w)["code"], qs)
	}
	assert.Empty(t, q.req.TenantID, "service never reached")
}

func TestListEvents_200Shape(t *testing.T) {
	at := time.Date(2026, 3, 1, 10, 0, 0, 0, time.FixedZone("x", 3600))
	eff := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	q := &fakeQuerier{res: service.QueryResult{
		Events: []domain.AuditEntry{
			{ID: entryID, OccurredAt: at, EntryType: "tender.section.approved", Action: "approved",
				Actor:         domain.ActorRef{Type: domain.ActorUser, ID: userA, Display: "asha"},
				Target:        &domain.TargetRef{Type: "tender_section", ID: "4.2"},
				SourceService: "iam-tender-acl", SourceEventType: "SectionApproved", IPAddress: "203.0.113.7",
				TraceID: "tr", RetentionTier: domain.TierCompliance7y, Metadata: json.RawMessage(`{"k":1}`)},
			{ID: exportID, OccurredAt: at, EntryType: "auth.login.succeeded", Action: "login",
				Actor: domain.ActorRef{Type: domain.ActorAnonymous}, RetentionTier: domain.TierSecurity3y},
		},
		WindowClamped: true, EffectiveFrom: eff,
	}}
	r := queryRouter(q, &fakeExporter{}, nil)
	w := sendJSON(t, r, http.MethodGet, "/api/v1/audit/events", "", adminHeaders("tenant_admin"))
	require.Equal(t, http.StatusOK, w.Code)
	body := decode(t, w)
	assert.Nil(t, body["next_cursor"], "exhausted → null")
	assert.Equal(t, true, body["window_clamped"])
	assert.Equal(t, "2025-03-01T00:00:00Z", body["effective_from"])
	evs := body["events"].([]any)
	require.Len(t, evs, 2)
	e0 := evs[0].(map[string]any)
	assert.Equal(t, "2026-03-01T09:00:00Z", e0["occurred_at"], "RFC 3339 UTC")
	assert.Equal(t, map[string]any{"type": "tender_section", "id": "4.2"}, e0["target"])
	assert.Equal(t, map[string]any{"k": float64(1)}, e0["metadata"])
	assert.Equal(t, "asha", e0["actor"].(map[string]any)["display"])
	e1 := evs[1].(map[string]any)
	assert.NotContains(t, e1, "target")
	assert.Equal(t, map[string]any{}, e1["metadata"], "metadata defaults to {}")

	q.res = service.QueryResult{NextCursor: "next"}
	w = sendJSON(t, r, http.MethodGet, "/api/v1/audit/events", "", adminHeaders("tenant_admin"))
	body = decode(t, w)
	assert.Equal(t, "next", body["next_cursor"])
	assert.Equal(t, []any{}, body["events"], "empty page is [], not null")
}

func TestListEvents_ServiceError(t *testing.T) {
	q := &fakeQuerier{err: domain.NewError(domain.ErrInvalidRequest, "cursor is malformed")}
	r := queryRouter(q, &fakeExporter{}, nil)
	w := sendJSON(t, r, http.MethodGet, "/api/v1/audit/events", "", adminHeaders("tenant_admin"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "cursor is malformed", decode(t, w)["message"])
}

func TestListEvents_DeferredToExport_202_D10(t *testing.T) {
	from := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
	deferred := domain.QueryFilter{From: &from, EntryTypes: []string{"a.b"}}
	q := &fakeQuerier{res: service.QueryResult{Deferred: &deferred}}
	e := &fakeExporter{job: domain.ExportJob{ID: exportID, Status: domain.ExportPending}}
	r := queryRouter(q, e, NewTenantRateLimiterPerMinute(60, 1))

	w := sendJSON(t, r, http.MethodGet, "/api/v1/audit/events", "", adminHeaders("tenant_admin"))
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	assert.Equal(t, map[string]any{
		"export_id": exportID, "status": "pending", "status_url": "/api/v1/audit/exports/" + exportID,
	}, decode(t, w))
	assert.Equal(t, tenantA, e.tenant)
	assert.Equal(t, userA, e.requester)
	assert.Equal(t, deferred, e.filter)

	// The deferral draws from the AL-3 bucket (burst 1).
	w = sendJSON(t, r, http.MethodGet, "/api/v1/audit/events", "", adminHeaders("tenant_admin"))
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "60", w.Header().Get("Retry-After"))
	assert.Equal(t, "rate_limited", decode(t, w)["code"])
	assert.Equal(t, 1, e.calls)

	e2 := &fakeExporter{err: errors.New("boom")}
	w = sendJSON(t, queryRouter(q, e2, nil), http.MethodGet, "/api/v1/audit/events", "", adminHeaders("tenant_admin"))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestGetEvent(t *testing.T) {
	q := &fakeQuerier{entry: domain.AuditEntry{ID: entryID, EntryType: "a.b", RetentionTier: domain.TierAccess90d}}
	r := queryRouter(q, &fakeExporter{}, nil)
	w := sendJSON(t, r, http.MethodGet, "/api/v1/audit/events/"+entryID, "", adminHeaders("tenant_admin"))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, entryID, decode(t, w)["id"])
	assert.Equal(t, tenantA, q.getTen)
	assert.Equal(t, entryID, q.getID)

	q.getErr = domain.NewError(domain.ErrAuditEntryNotFound, "audit entry not found")
	w = sendJSON(t, r, http.MethodGet, "/api/v1/audit/events/"+entryID, "", adminHeaders("tenant_admin"))
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "audit_entry_not_found", decode(t, w)["code"])
}

func TestCreateExport(t *testing.T) {
	e := &fakeExporter{job: domain.ExportJob{ID: exportID, Status: domain.ExportPending}}
	r := queryRouter(&fakeQuerier{}, e, nil)

	w := sendJSON(t, r, http.MethodPost, "/api/v1/audit/exports", "", adminHeaders("tenant_admin"))
	require.Equal(t, http.StatusAccepted, w.Code, "empty body = unfiltered export")
	assert.Equal(t, domain.QueryFilter{}, e.filter)
	assert.Equal(t, exportID, decode(t, w)["export_id"])

	w = sendJSON(t, r, http.MethodPost, "/api/v1/audit/exports",
		`{"from":"2026-01-01T00:00:00Z","to":"2026-02-01T00:00:00Z","entry_type":["a.b"],"actor_id":"`+userA+
			`","actor_type":"user","target_type":"t","target_id":"i","source_service":"s","retention_tier":"security_3y"}`,
		adminHeaders("tenant_admin"))
	require.Equal(t, http.StatusAccepted, w.Code, w.Body.String())
	from, to := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, domain.QueryFilter{From: &from, To: &to, EntryTypes: []string{"a.b"}, ActorID: userA,
		ActorType: "user", TargetType: "t", TargetID: "i", SourceService: "s", RetentionTier: "security_3y"}, e.filter)
	assert.Equal(t, tenantA, e.tenant)
	assert.Equal(t, userA, e.requester)

	calls := e.calls
	w = sendJSON(t, r, http.MethodPost, "/api/v1/audit/exports", `{"from":`, adminHeaders("tenant_admin"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "invalid_request", decode(t, w)["code"])
	assert.Equal(t, calls, e.calls)

	e.err = domain.NewError(domain.ErrInvalidRequest, "from must not be after to")
	w = sendJSON(t, r, http.MethodPost, "/api/v1/audit/exports", `{}`, adminHeaders("tenant_admin"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateExport_RateLimitedPerTenant(t *testing.T) {
	e := &fakeExporter{job: domain.ExportJob{ID: exportID, Status: domain.ExportPending}}
	r := queryRouter(&fakeQuerier{}, e, NewTenantRateLimiterPerMinute(1, 2))
	for i := 0; i < 2; i++ {
		assert.Equal(t, http.StatusAccepted, sendJSON(t, r, http.MethodPost, "/api/v1/audit/exports", "", adminHeaders("tenant_admin")).Code)
	}
	w := sendJSON(t, r, http.MethodPost, "/api/v1/audit/exports", "", adminHeaders("tenant_admin"))
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "60", w.Header().Get("Retry-After"))
	assert.Equal(t, 2, e.calls)

	other := adminHeaders("tenant_admin")
	other["x-tenant-id"] = "55555555-5555-5555-5555-555555555555"
	assert.Equal(t, http.StatusAccepted, sendJSON(t, r, http.MethodPost, "/api/v1/audit/exports", "", other).Code,
		"buckets are per tenant")
}

func TestTenantRateLimiterPerMinute_Allow(t *testing.T) {
	l := NewTenantRateLimiterPerMinute(60, 1)
	assert.True(t, l.Allow("a"))
	assert.False(t, l.Allow("a"))
	assert.True(t, l.Allow("b"))
}

func TestGetExport(t *testing.T) {
	created := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	done := created.Add(time.Minute)
	exp := created.Add(7 * 24 * time.Hour)
	dl := created.Add(15 * time.Minute)
	rows := int64(42)
	e := &fakeExporter{view: service.ExportView{
		Job: domain.ExportJob{ID: exportID, Status: domain.ExportReady, CreatedAt: created, CompletedAt: &done,
			RowCount: &rows, SignedURLExpiresAt: &exp, Error: "stale"},
		DownloadURL: "https://s3/presigned", DownloadURLExpiresAt: &dl,
	}}
	r := queryRouter(&fakeQuerier{}, e, nil)
	w := sendJSON(t, r, http.MethodGet, "/api/v1/audit/exports/"+exportID, "", adminHeaders("tenant_admin"))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, exportID, e.statusID)
	assert.Equal(t, map[string]any{
		"export_id": exportID, "status": "ready", "created_at": "2026-03-01T00:00:00Z",
		"completed_at": "2026-03-01T00:01:00Z", "row_count": float64(42),
		"download_url": "https://s3/presigned", "download_url_expires_at": "2026-03-01T00:15:00Z",
		"expires_at": "2026-03-08T00:00:00Z",
	}, decode(t, w), "error is hidden unless failed")

	e.view = service.ExportView{Job: domain.ExportJob{ID: exportID, Status: domain.ExportFailed, CreatedAt: created, Error: "internal_error"}}
	body := decode(t, sendJSON(t, r, http.MethodGet, "/api/v1/audit/exports/"+exportID, "", adminHeaders("tenant_admin")))
	assert.Equal(t, "failed", body["status"])
	assert.Equal(t, "internal_error", body["error"])
	assert.NotContains(t, body, "download_url")

	e.statusErr = domain.NewError(domain.ErrExportNotFound, "export not found")
	w = sendJSON(t, r, http.MethodGet, "/api/v1/audit/exports/"+exportID, "", adminHeaders("tenant_admin"))
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.Equal(t, "export_not_found", decode(t, w)["code"])
}
