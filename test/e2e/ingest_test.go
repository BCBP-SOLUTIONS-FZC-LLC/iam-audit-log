//go:build e2e

package e2e_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/test/dbseed"
)

func ingestHeaders(tenant, key string) map[string]string {
	h := systemHeaders(tenant)
	h["Content-Type"] = "application/json"
	if key != "" {
		h["Idempotency-Key"] = key
	}
	return h
}

func entryBody(tenant, entryType string, extra string) string {
	return fmt.Sprintf(`{"tenant_id":%q,"entry_type":%q,"action":"update",
	  "actor":{"type":"user","id":%q,"display":"asha@acme.example"},
	  "target":{"type":"tenant_setting","id":"mfa_freshness_seconds"},
	  "occurred_at":"2026-09-14T09:12:04Z","source_service":"iam-org-membership",
	  "source_event_type":"TenantSettingChanged","metadata":{"from":900,"to":1800}%s}`,
		tenant, entryType, adminUser, extra)
}

type entryResp struct {
	ID            string          `json:"id"`
	TenantID      string          `json:"tenant_id"`
	RetentionTier string          `json:"retention_tier"`
	IngestMode    string          `json:"ingest_mode"`
	SourceTopic   *string         `json:"source_topic"`
	SourceEventID string          `json:"source_event_id"`
	TraceID       string          `json:"trace_id"`
	Metadata      json.RawMessage `json:"metadata"`
}

func decodeEntry(t *testing.T, body string) entryResp {
	t.Helper()
	var e entryResp
	require.NoError(t, json.Unmarshal([]byte(body), &e), body)
	return e
}

func (e *e2eEnv) seed(t *testing.T) *dbseed.Pool {
	t.Helper()
	p, err := dbseed.New(context.Background(), e.roles.SuperDSN)
	require.NoError(t, err)
	t.Cleanup(p.Close)
	return p
}

// §14 Ingest row: AL-5 creates (201) then an idempotency-key replay returns
// 200 with the same id; the row is direct_write, topic NULL, derived tier.
func TestAL5_CreateThenReplaySameID_ALINV4(t *testing.T) {
	e := newE2EEnv(t)
	key := uuid.NewString()
	code, _, body := e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/audit-entries",
		body: entryBody(tenantA, "config.tenant_setting.changed", ""), headers: ingestHeaders(tenantA, key)})
	require.Equal(t, http.StatusCreated, code, body)
	first := decodeEntry(t, body)
	assert.Equal(t, "direct_write", first.IngestMode)
	assert.Nil(t, first.SourceTopic)
	assert.Equal(t, "security_3y", first.RetentionTier)
	assert.Equal(t, key, first.SourceEventID)

	code, _, body = e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/audit-entries",
		body: entryBody(tenantA, "config.tenant_setting.changed", ""), headers: ingestHeaders(tenantA, key)})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, first.ID, decodeEntry(t, body).ID)

	var n int
	require.NoError(t, e.seed(t).QueryRow(context.Background(), `SELECT count(*) FROM audit_events WHERE source_event_id = $1`, key).Scan(&n))
	assert.Equal(t, 1, n)
}

// AL-INV-11: a caller-supplied retention_tier is ignored; the tier comes
// from entry_type (security.cross_tenant_access → compliance_7y).
func TestAL5_CallerTierIgnored_ALINV11(t *testing.T) {
	e := newE2EEnv(t)
	code, _, body := e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/audit-entries",
		body:    entryBody(tenantA, "security.cross_tenant_access", `,"retention_tier":"access_90d"`),
		headers: ingestHeaders(tenantA, uuid.NewString())})
	require.Equal(t, http.StatusCreated, code, body)
	assert.Equal(t, "compliance_7y", decodeEntry(t, body).RetentionTier)
}

// §5.4: trace_id defaults to the W3C traceparent when omitted.
func TestAL5_TraceIDFromTraceparent(t *testing.T) {
	e := newE2EEnv(t)
	h := ingestHeaders(tenantA, uuid.NewString())
	h["traceparent"] = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	code, _, body := e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/audit-entries",
		body: entryBody(tenantA, "config.tenant_setting.changed", ""), headers: h})
	require.Equal(t, http.StatusCreated, code, body)
	assert.Equal(t, "4bf92f3577b34da6a3ce929d0e0e4736", decodeEntry(t, body).TraceID)
}

// §17 rejection matrix over the real stack.
func TestAL5_Rejections(t *testing.T) {
	e := newE2EEnv(t)
	cases := []struct {
		name, body, key string
		headers         map[string]string
		want            int
		code            string
	}{
		{"unknown entry_type", entryBody(tenantA, "config.nope", ""), "k1", nil, 422, "unknown_entry_type"},
		{"bus type via direct-write (D-6)", entryBody(tenantA, "tender.section.approved", ""), "k2", nil, 422, "unknown_entry_type"},
		{"anonymous with id", strings.Replace(entryBody(tenantA, "config.idp.changed", ""), `"type":"user"`, `"type":"anonymous"`, 1), "k3", nil, 422, "invalid_actor"},
		{"metadata too large", strings.Replace(entryBody(tenantA, "config.idp.changed", ""), `"from":900`, `"blob":"`+strings.Repeat("x", 9000)+`"`, 1), "k4", nil, 422, "metadata_too_large"},
		{"missing Idempotency-Key", entryBody(tenantA, "config.idp.changed", ""), "", nil, 400, "invalid_request"},
		{"malformed body", `{"tenant_id":`, "k5", nil, 400, "invalid_request"},
		{"tenant admin is not a mesh peer", entryBody(tenantA, "config.idp.changed", ""), "k6", adminHeaders(tenantA), 403, "forbidden_peer"},
	}
	for _, tc := range cases {
		h := tc.headers
		if h == nil {
			h = ingestHeaders(tenantA, tc.key)
		} else {
			h["Idempotency-Key"] = tc.key
			h["Content-Type"] = "application/json"
		}
		code, _, body := e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/audit-entries", body: tc.body, headers: h})
		assert.Equal(t, tc.want, code, "%s: %s", tc.name, body)
		assert.Contains(t, body, `"code":"`+tc.code+`"`, tc.name)
	}
}

// §14 "cross-tenant forgery → rejected": the row lands in the BODY tenant
// only, and reusing another tenant's key cannot read that tenant's entry.
func TestAL5_CrossTenantForgeryImpossible_ALINV3(t *testing.T) {
	e := newE2EEnv(t)
	key := uuid.NewString()
	code, _, body := e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/audit-entries",
		body: entryBody(tenantA, "config.tenant_setting.changed", ""), headers: ingestHeaders(tenantB, key)})
	require.Equal(t, http.StatusCreated, code, body)
	assert.Equal(t, tenantA, decodeEntry(t, body).TenantID, "row bound to body tenant, not x-tenant-id")

	code, _, body = e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/audit-entries",
		body: entryBody(tenantB, "config.tenant_setting.changed", ""), headers: ingestHeaders(tenantB, key)})
	assert.Equal(t, http.StatusBadRequest, code)
	assert.NotContains(t, body, tenantA, "must not leak tenant A's entry")
}

type batchResp struct {
	Results []struct {
		Index  int    `json:"index"`
		Status int    `json:"status"`
		ID     string `json:"id"`
		Code   string `json:"code"`
	} `json:"results"`
}

// §14 "batch partial success" (D-4): 207 with per-index status; a bad entry
// never rejects the batch; per-entry keys dedupe within and across calls.
func TestAL6_BatchPartialSuccess_D4(t *testing.T) {
	e := newE2EEnv(t)
	k1, k2 := uuid.NewString(), uuid.NewString()
	entry := func(key, entryType string) string {
		return strings.TrimSuffix(entryBody(tenantA, entryType, ""), "}") + fmt.Sprintf(`,"idempotency_key":%q}`, key)
	}
	body := `{"entries":[` + strings.Join([]string{
		entry(k1, "config.group_mapping.departments.changed"),
		entry(k2, "auth.login.success"), // bus type → 422 for this index only
		entry(k1, "config.group_mapping.departments.changed"),
		entry("", "config.group_mapping.departments.changed"),
	}, ",") + `]}`
	code, _, raw := e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/audit-entries:batch",
		body: body, headers: ingestHeaders(tenantA, "batch-"+uuid.NewString())})
	require.Equal(t, http.StatusMultiStatus, code, raw)
	var r batchResp
	require.NoError(t, json.Unmarshal([]byte(raw), &r))
	require.Len(t, r.Results, 4)
	assert.Equal(t, 201, r.Results[0].Status)
	assert.Equal(t, 422, r.Results[1].Status)
	assert.Equal(t, "unknown_entry_type", r.Results[1].Code)
	assert.Equal(t, 200, r.Results[2].Status)
	assert.Equal(t, r.Results[0].ID, r.Results[2].ID)
	assert.Equal(t, 400, r.Results[3].Status, "missing per-entry idempotency_key")
}

func TestAL6_BatchTooLargeAndRateLimited_D5(t *testing.T) {
	e := newE2EEnv(t)
	var entries []string
	for range e2eMaxBatch + 1 {
		entries = append(entries, `{"idempotency_key":"x"}`)
	}
	code, _, body := e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/audit-entries:batch",
		body: `{"entries":[` + strings.Join(entries, ",") + `]}`, headers: ingestHeaders(tenantB, "b")})
	assert.Equal(t, http.StatusUnprocessableEntity, code)
	assert.Contains(t, body, "batch_too_large")

	// The bucket for tenantB has e2eBatchBurst tokens; the call above used one.
	var last int
	for range e2eBatchBurst {
		last, _, _ = e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/audit-entries:batch",
			body: `{"entries":[{"idempotency_key":"y"}]}`, headers: ingestHeaders(tenantB, "b")})
	}
	assert.Equal(t, http.StatusTooManyRequests, last)
}
