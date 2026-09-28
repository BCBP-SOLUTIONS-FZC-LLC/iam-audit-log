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

type fakeWriter struct {
	got     []domain.DirectWriteCommand
	created bool
	err     error
	batch   []service.BatchResult
	bErr    error
}

func (f *fakeWriter) DirectWrite(_ context.Context, c domain.DirectWriteCommand) (domain.AuditEntry, bool, error) {
	f.got = append(f.got, c)
	if f.err != nil {
		return domain.AuditEntry{}, false, f.err
	}
	return domain.AuditEntry{ID: "id-1", TenantID: c.TenantID, EntryType: c.EntryType, SourceEventID: c.IdempotencyKey,
		RetentionTier: domain.TierSecurity3y, IngestMode: domain.IngestDirectWrite, Actor: c.Actor, Target: c.Target,
		SourceTopic: "", TraceID: c.TraceID}, f.created, nil
}

func (f *fakeWriter) DirectWriteBatch(_ context.Context, cs []domain.DirectWriteCommand) ([]service.BatchResult, error) {
	f.got = append(f.got, cs...)
	return f.batch, f.bErr
}

func ingestRouter(w DirectWriter, limiter *TenantRateLimiter) *Router {
	return NewRouter(RouterConfig{
		GinConfig:    gincommon.Config{ServiceName: "iam-audit-log", BuildVersion: "test"},
		Ingest:       NewIngestHandler(w),
		BatchLimiter: limiter,
	})
}

const validBody = `{"tenant_id":"11111111-1111-1111-1111-111111111111","entry_type":"config.tenant_setting.changed",
 "action":"update","actor":{"type":"user","id":"22222222-2222-2222-2222-222222222222"},
 "target":{"type":"tenant_setting","id":"mfa"},"occurred_at":"2026-03-14T09:12:04Z",
 "source_service":"iam-org-membership","source_event_type":"TenantSettingChanged",
 "retention_tier":"access_90d","metadata":{"from":900,"to":1800}}`

func post(t *testing.T, r *Router, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range map[string]string{"x-user-id": "iam-system", "x-tenant-id": tenantA, "x-tenant-roles": "iam-system"} {
		req.Header.Set(k, v)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.Handler().ServeHTTP(w, req)
	return w
}

// AL-5: 201 on create, 200 on replay; the header key and the traceparent
// default flow into the command; a caller-supplied retention_tier is
// ignored (the DTO has no such field — AL-INV-11).
func TestCreateEntry_StatusAndMapping_ALINV11(t *testing.T) {
	f := &fakeWriter{created: true}
	r := ingestRouter(f, nil)
	w := post(t, r, "/api/v1/internal/audit-entries", validBody, map[string]string{"Idempotency-Key": "k-1"})
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var resp EntryResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "security_3y", resp.RetentionTier)
	assert.Nil(t, resp.SourceTopic)
	assert.JSONEq(t, `{}`, string(resp.Metadata))

	require.Len(t, f.got, 1)
	c := f.got[0]
	assert.Equal(t, "k-1", c.IdempotencyKey)
	assert.Equal(t, "tenant_setting", c.Target.Type)
	assert.JSONEq(t, `{"from":900,"to":1800}`, string(c.Metadata))
	assert.Equal(t, time.Date(2026, 3, 14, 9, 12, 4, 0, time.UTC), c.OccurredAt.UTC())

	f.created = false
	assert.Equal(t, http.StatusOK, post(t, r, "/api/v1/internal/audit-entries", validBody, map[string]string{"Idempotency-Key": "k-1"}).Code)
}

func TestCreateEntry_Errors(t *testing.T) {
	r := ingestRouter(&fakeWriter{err: domain.NewError(domain.ErrUnknownEntryType, "no")}, nil)
	w := post(t, r, "/api/v1/internal/audit-entries", validBody, map[string]string{"Idempotency-Key": "k"})
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Contains(t, w.Body.String(), `"code":"unknown_entry_type"`)

	w = post(t, r, "/api/v1/internal/audit-entries", `{"tenant_id":`, map[string]string{"Idempotency-Key": "k"})
	assert.Equal(t, http.StatusBadRequest, w.Code)

	w = post(t, r, "/api/v1/internal/audit-entries", validBody, nil)
	assert.Equal(t, http.StatusBadRequest, w.Code, "Idempotency-Key is required")
}

// AL-6 always answers 207 with per-index results (decision D-4).
func TestCreateBatch_207PerIndex_D4(t *testing.T) {
	f := &fakeWriter{batch: []service.BatchResult{
		{Index: 0, Entry: domain.AuditEntry{ID: "a"}, Created: true},
		{Index: 1, Entry: domain.AuditEntry{ID: "a"}},
		{Index: 2, Err: domain.NewError(domain.ErrInvalidActor, "bad actor")},
		{Index: 3, Err: errors.New("boom")},
	}}
	r := ingestRouter(f, nil)
	body := `{"entries":[` + strings.Repeat(`{"idempotency_key":"e","entry_type":"x"},`, 3) + `{"idempotency_key":"z"}]}`
	w := post(t, r, "/api/v1/internal/audit-entries:batch", body, map[string]string{"Idempotency-Key": "batch-1"})
	require.Equal(t, http.StatusMultiStatus, w.Code, w.Body.String())
	var resp BatchResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	require.Len(t, resp.Results, 4)
	assert.Equal(t, BatchResultItem{Index: 0, Status: 201, ID: "a"}, resp.Results[0])
	assert.Equal(t, BatchResultItem{Index: 1, Status: 200, ID: "a"}, resp.Results[1])
	assert.Equal(t, 422, resp.Results[2].Status)
	assert.Equal(t, "invalid_actor", resp.Results[2].Code)
	assert.Equal(t, 500, resp.Results[3].Status)
	assert.Equal(t, "e", f.got[0].IdempotencyKey, "each entry uses its own idempotency_key")
}

func TestCreateBatch_WholeBatchErrors(t *testing.T) {
	r := ingestRouter(&fakeWriter{bErr: domain.NewError(domain.ErrBatchTooLarge, "too many")}, nil)
	w := post(t, r, "/api/v1/internal/audit-entries:batch", `{"entries":[{}]}`, map[string]string{"Idempotency-Key": "b"})
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Contains(t, w.Body.String(), "batch_too_large")

	w = post(t, r, "/api/v1/internal/audit-entries:batch", `not json`, map[string]string{"Idempotency-Key": "b"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// §10.5 / D-5: the per-tenant bucket answers 429 rate_limited once empty,
// and tenants do not share a bucket.
func TestCreateBatch_RateLimitedPerTenant_D5(t *testing.T) {
	r := ingestRouter(&fakeWriter{batch: []service.BatchResult{}}, NewTenantRateLimiter(1, 2))
	h := map[string]string{"Idempotency-Key": "b"}
	for i := range 2 {
		assert.Equal(t, http.StatusMultiStatus, post(t, r, "/api/v1/internal/audit-entries:batch", `{"entries":[]}`, h).Code, i)
	}
	w := post(t, r, "/api/v1/internal/audit-entries:batch", `{"entries":[]}`, h)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Contains(t, w.Body.String(), `"code":"rate_limited"`)
	assert.Equal(t, "1", w.Header().Get("Retry-After"))

	other := map[string]string{"Idempotency-Key": "b", "x-tenant-id": "99999999-9999-9999-9999-999999999999"}
	assert.Equal(t, http.StatusMultiStatus, post(t, r, "/api/v1/internal/audit-entries:batch", `{"entries":[]}`, other).Code)
}

// The oversized-body path of decodeJSON (MaxBytesReader, 8 MiB cap).
func TestDecodeJSON_BodyTooLarge(t *testing.T) {
	r := ingestRouter(&fakeWriter{}, nil)
	big := `{"tenant_id":"` + strings.Repeat("x", 9<<20) + `"}`
	w := post(t, r, "/api/v1/internal/audit-entries", big, map[string]string{"Idempotency-Key": "k"})
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Contains(t, w.Body.String(), "size limit")
}
