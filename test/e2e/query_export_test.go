//go:build e2e

package e2e_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/test/fixtures"
)

// §14 e2e rows for Phase 4 (LLD §5.4 AL-1..AL-4): producer → direct-write →
// query returns it; export → signed URL → object contents.

type eventOut struct {
	ID         string    `json:"id"`
	OccurredAt time.Time `json:"occurred_at"`
	EntryType  string    `json:"entry_type"`
	Action     string    `json:"action"`
	Actor      struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Display string `json:"display"`
	} `json:"actor"`
	Target *struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	} `json:"target"`
	SourceService   string          `json:"source_service"`
	SourceEventType string          `json:"source_event_type"`
	RetentionTier   string          `json:"retention_tier"`
	TraceID         string          `json:"trace_id"`
	Metadata        json.RawMessage `json:"metadata"`
}

type eventsOut struct {
	Events        []eventOut `json:"events"`
	NextCursor    *string    `json:"next_cursor"`
	WindowClamped bool       `json:"window_clamped"`
	EffectiveFrom time.Time  `json:"effective_from"`
}

type exportOut struct {
	ExportID             string     `json:"export_id"`
	Status               string     `json:"status"`
	StatusURL            string     `json:"status_url"`
	RowCount             *int64     `json:"row_count"`
	DownloadURL          string     `json:"download_url"`
	DownloadURLExpiresAt *time.Time `json:"download_url_expires_at"`
	ExpiresAt            *time.Time `json:"expires_at"`
	Error                string     `json:"error"`
}

// ingestAt direct-writes one entry (AL-5) at occurredAt and returns its id.
func (e *e2eEnv) ingestAt(t *testing.T, tenant string, occurredAt time.Time) string {
	t.Helper()
	body := fmt.Sprintf(`{"tenant_id":%q,"entry_type":"config.tenant_setting.changed","action":"update",
	  "actor":{"type":"user","id":%q,"display":"asha@acme.example"},
	  "target":{"type":"tenant_setting","id":"mfa_freshness_seconds"},
	  "occurred_at":%q,"source_service":"iam-org-membership",
	  "source_event_type":"TenantSettingChanged","metadata":{"from":900,"to":1800}}`,
		tenant, adminUser, occurredAt.UTC().Format(time.RFC3339Nano))
	code, _, resp := e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/audit-entries",
		body: body, headers: ingestHeaders(tenant, uuid.NewString())})
	require.Equal(t, http.StatusCreated, code, resp)
	return decodeEntry(t, resp).ID
}

func (e *e2eEnv) listEvents(t *testing.T, tenant string, q url.Values) (int, eventsOut, string) {
	t.Helper()
	code, _, body := e.do(t, reqOpts{path: "/api/v1/audit/events?" + q.Encode(), headers: adminHeaders(tenant)})
	var out eventsOut
	if code == http.StatusOK {
		require.NoError(t, json.Unmarshal([]byte(body), &out), body)
	}
	return code, out, body
}

func errorCode(t *testing.T, body string) string {
	t.Helper()
	var er struct {
		Error string `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &er), body)
	return er.Error
}

// AL-5 → AL-1: the direct-written entry comes back with the §5.4 shape.
func TestAL1_DirectWriteThenQueryReturnsEntry(t *testing.T) {
	e := newE2EEnvWithS3(t, nil)
	occurred := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	id := e.ingestAt(t, tenantA, occurred)

	code, out, body := e.listEvents(t, tenantA, nil)
	require.Equal(t, http.StatusOK, code, body)
	require.Len(t, out.Events, 1, body)
	ev := out.Events[0]
	assert.Equal(t, id, ev.ID)
	assert.True(t, occurred.Equal(ev.OccurredAt), "occurred_at %s", ev.OccurredAt)
	assert.Equal(t, "config.tenant_setting.changed", ev.EntryType)
	assert.Equal(t, "update", ev.Action)
	assert.Equal(t, "user", ev.Actor.Type)
	assert.Equal(t, adminUser, ev.Actor.ID)
	assert.Equal(t, "asha@acme.example", ev.Actor.Display)
	require.NotNil(t, ev.Target)
	assert.Equal(t, "tenant_setting", ev.Target.Type)
	assert.Equal(t, "iam-org-membership", ev.SourceService)
	assert.Equal(t, "TenantSettingChanged", ev.SourceEventType)
	assert.Equal(t, "security_3y", ev.RetentionTier)
	assert.JSONEq(t, `{"from":900,"to":1800}`, string(ev.Metadata))
	assert.Nil(t, out.NextCursor, "single page → next_cursor null")
	assert.True(t, out.WindowClamped, "absent from is clamped to the plan window")
	assert.Contains(t, body, `"next_cursor":null`)

	// AL-2 reads the same entry.
	code, _, body = e.do(t, reqOpts{path: "/api/v1/audit/events/" + id, headers: adminHeaders(tenantA)})
	require.Equal(t, http.StatusOK, code, body)
	var one eventOut
	require.NoError(t, json.Unmarshal([]byte(body), &one))
	assert.Equal(t, id, one.ID)
}

// §5.1 keyset pagination: limit=2 walks every entry exactly once, newest
// first, and stays stable when a newer entry is ingested mid-walk.
func TestAL1_KeysetPaginationVisitsAllOnce(t *testing.T) {
	e := newE2EEnvWithS3(t, nil)
	base := time.Now().UTC().Add(-2 * time.Hour)
	want := map[string]bool{}
	for i := 0; i < 5; i++ {
		want[e.ingestAt(t, tenantA, base.Add(time.Duration(i)*time.Minute))] = true
	}
	// Two entries sharing one occurred_at exercise the id tiebreak.
	want[e.ingestAt(t, tenantA, base.Add(10*time.Minute))] = true
	want[e.ingestAt(t, tenantA, base.Add(10*time.Minute))] = true

	seen := map[string]bool{}
	var prev *eventOut
	cursor := ""
	for pages := 0; ; pages++ {
		require.Less(t, pages, 10, "pagination did not terminate")
		q := url.Values{"limit": {"2"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		code, out, body := e.listEvents(t, tenantA, q)
		require.Equal(t, http.StatusOK, code, body)
		assert.LessOrEqual(t, len(out.Events), 2)
		for i := range out.Events {
			ev := out.Events[i]
			assert.False(t, seen[ev.ID], "duplicate %s", ev.ID)
			seen[ev.ID] = true
			if prev != nil {
				assert.True(t, !ev.OccurredAt.After(prev.OccurredAt), "not newest-first")
			}
			prev = &ev
		}
		if pages == 0 {
			e.ingestAt(t, tenantA, time.Now().UTC()) // newer than the cursor: must not appear later
		}
		if out.NextCursor == nil {
			break
		}
		cursor = *out.NextCursor
	}
	assert.Equal(t, want, seen)
}

// AL-INV-3 / §14 Query row: tenant B sees none of tenant A's trail.
func TestAL1AL2_TenantIsolation(t *testing.T) {
	e := newE2EEnvWithS3(t, nil)
	id := e.ingestAt(t, tenantA, time.Now().UTC().Add(-time.Hour))

	code, out, body := e.listEvents(t, tenantB, nil)
	require.Equal(t, http.StatusOK, code, body)
	assert.Empty(t, out.Events)

	code, _, body = e.do(t, reqOpts{path: "/api/v1/audit/events/" + id, headers: adminHeaders(tenantB)})
	assert.Equal(t, http.StatusNotFound, code, body)
	assert.Equal(t, "audit_entry_not_found", errorCode(t, body))
}

// §5.2: a plain member is 403 insufficient_permissions on AL-1..AL-4;
// tenant_owner is admitted.
func TestAL1to4_MemberForbidden(t *testing.T) {
	e := newE2EEnvWithS3(t, nil)
	id := uuid.NewString()
	for _, r := range []reqOpts{
		{path: "/api/v1/audit/events"},
		{path: "/api/v1/audit/events/" + id},
		{method: http.MethodPost, path: "/api/v1/audit/exports", body: `{}`},
		{path: "/api/v1/audit/exports/" + id},
	} {
		r.headers = gatewayHeaders(adminUser, tenantA, "member")
		r.headers["Content-Type"] = "application/json"
		code, _, body := e.do(t, r)
		assert.Equal(t, http.StatusForbidden, code, "%s %s: %s", r.method, r.path, body)
		assert.Equal(t, "insufficient_permissions", errorCode(t, body), r.path)
	}

	code, _, body := e.do(t, reqOpts{path: "/api/v1/audit/events", headers: gatewayHeaders(adminUser, tenantA, "tenant_owner")})
	assert.Equal(t, http.StatusOK, code, body)
}

// AL-INV-8 / HLD §6.6: the plan window hides older rows from AL-1 and AL-2
// (they stay stored); a range entirely older than the window is 200 empty.
func TestAL1AL2_PlanWindowClamp_ALINV8(t *testing.T) {
	e := newE2EEnvWithS3(t, nil)
	_, err := e.seed(t).Exec(context.Background(),
		`INSERT INTO tenant_plan_window (tenant_id, plan_code, query_window_days, last_event_at) VALUES ($1, 'starter', 1, now())`,
		tenantA)
	require.NoError(t, err)

	old := e.ingestAt(t, tenantA, time.Now().UTC().Add(-72*time.Hour))
	recent := e.ingestAt(t, tenantA, time.Now().UTC().Add(-time.Hour))

	before := time.Now().UTC()
	code, out, body := e.listEvents(t, tenantA, url.Values{"from": {time.Now().UTC().Add(-30 * 24 * time.Hour).Format(time.RFC3339)}})
	require.Equal(t, http.StatusOK, code, body)
	require.Len(t, out.Events, 1, body)
	assert.Equal(t, recent, out.Events[0].ID)
	assert.True(t, out.WindowClamped)
	earliest := before.Add(-24 * time.Hour)
	assert.WithinDuration(t, earliest, out.EffectiveFrom, 10*time.Second)

	// Entirely older than the window → 200, empty, clamped.
	q := url.Values{
		"from": {time.Now().UTC().Add(-96 * time.Hour).Format(time.RFC3339)},
		"to":   {time.Now().UTC().Add(-48 * time.Hour).Format(time.RFC3339)},
	}
	code, out, body = e.listEvents(t, tenantA, q)
	require.Equal(t, http.StatusOK, code, body)
	assert.Empty(t, out.Events)
	assert.True(t, out.WindowClamped)

	code, _, body = e.do(t, reqOpts{path: "/api/v1/audit/events/" + old, headers: adminHeaders(tenantA)})
	assert.Equal(t, http.StatusNotFound, code, body)

	var n int
	require.NoError(t, e.seed(t).QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE id = $1`, old).Scan(&n))
	assert.Equal(t, 1, n, "the window never touches storage (AL-INV-8)")
}

// §5.5: malformed cursor / filter → 400 invalid_request.
func TestAL1_BadRequests(t *testing.T) {
	e := newE2EEnvWithS3(t, nil)
	for name, q := range map[string]url.Values{
		"garbage cursor": {"cursor": {"not-a-cursor!!"}},
		"bad from":       {"from": {"yesterday"}},
		"limit too big":  {"limit": {"1001"}},
		"limit zero":     {"limit": {"0"}},
		"bad actor_id":   {"actor_id": {"nope"}},
		"from after to":  {"from": {"2026-09-02T00:00:00Z"}, "to": {"2026-09-01T00:00:00Z"}},
	} {
		code, _, body := e.listEvents(t, tenantA, q)
		assert.Equal(t, http.StatusBadRequest, code, "%s: %s", name, body)
		assert.Equal(t, "invalid_request", errorCode(t, body), name)
	}
	code, _, body := e.do(t, reqOpts{path: "/api/v1/audit/events/not-a-uuid", headers: adminHeaders(tenantA)})
	assert.Equal(t, http.StatusBadRequest, code, body)
}

// §14 e2e: AL-3 → worker (in-server, D-2) → AL-4 ready with a presigned URL
// → the S3 object holds exactly the tenant's filtered rows. Another tenant
// polling the export gets 404.
func TestAL3AL4_ExportToSignedURLContents(t *testing.T) {
	f := fixtures.StartFloci(t)
	e := newE2EEnvWithS3(t, &f.AWS)

	base := time.Now().UTC().Add(-3 * time.Hour)
	want := map[string]bool{}
	for i := 0; i < 3; i++ {
		want[e.ingestAt(t, tenantA, base.Add(time.Duration(i)*time.Minute))] = true
	}
	e.ingestAt(t, tenantB, base) // other tenant: never exported

	h := adminHeaders(tenantA)
	h["Content-Type"] = "application/json"
	code, _, body := e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/audit/exports",
		body: `{"entry_type":["config.tenant_setting.changed"]}`, headers: h})
	require.Equal(t, http.StatusAccepted, code, body)
	var acc exportOut
	require.NoError(t, json.Unmarshal([]byte(body), &acc))
	require.NotEmpty(t, acc.ExportID)
	assert.Equal(t, "pending", acc.Status)
	assert.Equal(t, "/api/v1/audit/exports/"+acc.ExportID, acc.StatusURL)

	var st exportOut
	require.Eventually(t, func() bool {
		code, _, body := e.do(t, reqOpts{path: acc.StatusURL, headers: adminHeaders(tenantA)})
		require.Equal(t, http.StatusOK, code, body)
		st = exportOut{}
		require.NoError(t, json.Unmarshal([]byte(body), &st))
		require.NotEqual(t, "failed", st.Status, body)
		return st.Status == "ready"
	}, 30*time.Second, 250*time.Millisecond)

	require.NotNil(t, st.RowCount)
	assert.EqualValues(t, 3, *st.RowCount)
	require.NotEmpty(t, st.DownloadURL)
	require.NotNil(t, st.DownloadURLExpiresAt)
	require.NotNil(t, st.ExpiresAt)
	assert.WithinDuration(t, time.Now().Add(7*24*time.Hour), *st.ExpiresAt, time.Minute, "7-day retrieval window (D-11)")
	assert.WithinDuration(t, time.Now().Add(15*time.Minute), *st.DownloadURLExpiresAt, time.Minute, "short-lived per-poll URL (D-11)")

	got := downloadExport(t, st.DownloadURL)
	seen := map[string]bool{}
	for _, r := range got {
		assert.Equal(t, tenantA, r["tenant_id"])
		seen[r["id"].(string)] = true
	}
	assert.Equal(t, want, seen)

	// AL-4 is RLS-scoped: tenant B cannot see A's export.
	code, _, body = e.do(t, reqOpts{path: acc.StatusURL, headers: adminHeaders(tenantB)})
	assert.Equal(t, http.StatusNotFound, code, body)
	assert.Equal(t, "export_not_found", errorCode(t, body))
}

// downloadExport GETs a presigned export URL and decodes its gzipped JSONL.
func downloadExport(t *testing.T, u string) []map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u, nil)
	require.NoError(t, err)
	req.Header.Set("Accept-Encoding", "gzip") // explicit: keep the raw gzip body (no transparent decode)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(raw))

	zr, err := gzip.NewReader(bytes.NewReader(raw))
	require.NoError(t, err, "export object must be gzip")
	var out []map[string]any
	sc := bufio.NewScanner(zr)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var m map[string]any
		require.NoError(t, json.Unmarshal(sc.Bytes(), &m), sc.Text())
		out = append(out, m)
	}
	require.NoError(t, sc.Err())
	return out
}

// Gap 32: an entry whose month was archived (its partition dropped, D-12)
// is still readable through AL-2 — found via the manifest's id range and
// read from the tenant's own S3 object — and AL-1 merges it with hot rows.
// Tenant isolation holds on the archived path too.
func TestAL2_ArchivedEntryReadable_Gap32(t *testing.T) {
	f := fixtures.StartFloci(t)
	e := newE2EEnvWithS3(t, &f.AWS)
	ctx := context.Background()
	_, err := e.seed(t).Exec(ctx,
		`INSERT INTO tenant_plan_window (tenant_id, plan_code, query_window_days, last_event_at) VALUES ($1, 'enterprise', 3650, now())`,
		tenantA)
	require.NoError(t, err)

	at := time.Date(2019, 1, 15, 10, 0, 0, 0, time.UTC)
	archived := domain.AuditEntry{
		ID: "01685111-1d00-7000-8000-000000000001", OccurredAt: at, RecordedAt: at, TenantID: tenantA,
		EntryType: "config.tenant_setting.changed", Action: "update",
		Actor:         domain.ActorRef{Type: domain.ActorIAMSystem, ID: domain.IAMSystemActorID},
		SourceService: "iam-catalog-admin", SourceEventType: "TenantSettingChanged", SourceEventID: uuid.NewString(),
		RetentionTier: domain.TierCompliance7y, IngestMode: domain.IngestDirectWrite, Metadata: json.RawMessage(`{"k":"v"}`),
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	require.NoError(t, json.NewEncoder(zw).Encode(domain.ToRecord(archived)))
	require.NoError(t, zw.Close())
	month := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
	key := domain.ArchiveKey(domain.TierCompliance7y, tenantA, month, 0)
	s3c := awss3.NewFromConfig(f.AWS, func(o *awss3.Options) { o.UsePathStyle = true })
	_, err = s3c.PutObject(ctx, &awss3.PutObjectInput{Bucket: aws.String(e2eBucket), Key: aws.String(key), Body: bytes.NewReader(buf.Bytes())})
	require.NoError(t, err)
	_, err = e.seed(t).Exec(ctx, `INSERT INTO audit_archive_objects
		(partition_name, retention_tier, tenant_id, part, period_month, s3_bucket, s3_key, row_count, byte_size,
		 min_occurred_at, max_occurred_at, min_id, max_id, sha256, sealed)
		VALUES ('audit_events_2019_01', 'compliance_7y', $1, 0, $2, $3, $4, 1, $5, $6, $6, $7, $7, 'x', true)`, // sealed (D-20)
		tenantA, month, e2eBucket, key, buf.Len(), at, archived.ID)
	require.NoError(t, err)
	hot := e.ingestAt(t, tenantA, time.Now().UTC().Add(-time.Hour))

	code, _, body := e.do(t, reqOpts{path: "/api/v1/audit/events/" + archived.ID, headers: adminHeaders(tenantA)})
	require.Equal(t, http.StatusOK, code, body)
	var got eventOut
	require.NoError(t, json.Unmarshal([]byte(body), &got))
	assert.Equal(t, archived.ID, got.ID)
	assert.True(t, at.Equal(got.OccurredAt))
	assert.Equal(t, "compliance_7y", got.RetentionTier)
	assert.JSONEq(t, `{"k":"v"}`, string(got.Metadata))

	code, out, body := e.listEvents(t, tenantA, url.Values{"from": {"2018-12-01T00:00:00Z"}})
	require.Equal(t, http.StatusOK, code, body)
	require.Len(t, out.Events, 2, body)
	assert.Equal(t, []string{hot, archived.ID}, []string{out.Events[0].ID, out.Events[1].ID}, "hot + archived, newest first")

	code, _, body = e.do(t, reqOpts{path: "/api/v1/audit/events/" + archived.ID, headers: adminHeaders(tenantB)})
	assert.Equal(t, http.StatusNotFound, code, body)
	assert.Equal(t, "audit_entry_not_found", errorCode(t, body))
}
