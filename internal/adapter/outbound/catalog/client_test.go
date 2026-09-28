package catalog_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/adapter/outbound/catalog"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// catalogBody is Catalog's real InternalPlansResponse shape (iam-catalog-admin
// dto.go), extra fields included — only code/audit_query_window_days and
// record_versions are consumed.
const catalogBody = `{
  "plans": [
    {"code":"starter","display_name":"Starter","workflow_template_limit":5,"tender_limit":null,
     "trial_duration_days":14,"sso_enabled":false,"custom_branding":"none",
     "audit_query_window_days":365,"feature_set":{"x":true},"record_version":3},
    {"code":"enterprise","display_name":"Enterprise","workflow_template_limit":null,"tender_limit":null,
     "trial_duration_days":30,"sso_enabled":true,"custom_branding":"logo",
     "audit_query_window_days":2555,"feature_set":{},"record_version":7}
  ],
  "record_versions": {"starter":3,"enterprise":7}
}`

func TestPlans_RequestContractAndDecode(t *testing.T) {
	var got *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Clone(r.Context())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(catalogBody))
	}))
	defer srv.Close()

	plans, versions, err := catalog.New(srv.URL+"/", time.Second).Plans(context.Background()) // trailing slash trimmed
	require.NoError(t, err)

	require.NotNil(t, got)
	assert.Equal(t, http.MethodGet, got.Method)
	assert.Equal(t, "/api/v1/internal/plans", got.URL.Path)
	assert.Equal(t, "iam-system", got.Header.Get("x-user-id"))
	assert.Equal(t, "00000000-0000-0000-0000-000000000000", got.Header.Get("x-tenant-id"))
	assert.Equal(t, "iam-system", got.Header.Get("x-tenant-roles"))
	assert.Equal(t, "application/json", got.Header.Get("Accept"))

	assert.Equal(t, []port.CatalogPlan{
		{Code: "starter", AuditQueryWindowDays: 365},
		{Code: "enterprise", AuditQueryWindowDays: 2555},
	}, plans)
	assert.Equal(t, map[string]int64{"starter": 3, "enterprise": 7}, versions)
}

func serve(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPlans_Non200IsError(t *testing.T) {
	srv := serve(t, http.StatusForbidden, `{"error":"forbidden"}`)
	_, _, err := catalog.New(srv.URL, time.Second).Plans(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "403")
	assert.False(t, errors.Is(err, port.ErrCatalogTimeout))
}

func TestPlans_BodyOverOneMiBIsError(t *testing.T) {
	pad := strings.Repeat(" ", 1<<20)
	srv := serve(t, http.StatusOK, `{"plans":[],"record_versions":{}}`+pad)
	_, _, err := catalog.New(srv.URL, time.Second).Plans(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds")

	// Exactly at the cap is accepted.
	body := `{"plans":[],"record_versions":{}}`
	srv = serve(t, http.StatusOK, body+strings.Repeat(" ", 1<<20-len(body)))
	_, _, err = catalog.New(srv.URL, time.Second).Plans(context.Background())
	assert.NoError(t, err)
}

func TestPlans_MalformedJSONIsError(t *testing.T) {
	srv := serve(t, http.StatusOK, `{"plans":[`)
	_, _, err := catalog.New(srv.URL, time.Second).Plans(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode")
}

func TestPlans_TimeoutIsMarked(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	_, _, err := catalog.New(srv.URL, 50*time.Millisecond).Plans(context.Background())
	require.Error(t, err)
	assert.ErrorIs(t, err, port.ErrCatalogTimeout, "client timeout")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, _, err = catalog.New(srv.URL, time.Minute).Plans(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, err, port.ErrCatalogTimeout, "context deadline")
}

func TestPlans_ConnectionRefusedIsPlainError(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	require.NoError(t, l.Close())

	_, _, err = catalog.New("http://"+addr, time.Second).Plans(context.Background())
	require.Error(t, err)
	assert.False(t, errors.Is(err, port.ErrCatalogTimeout))
	assert.Contains(t, err.Error(), "CAT-I2 request")
}

func TestPlans_BadBaseURL(t *testing.T) {
	_, _, err := catalog.New("http://bad host\x7f", time.Second).Plans(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "build request")
}

// A body read that fails mid-stream (connection cut) is reported, not decoded.
func TestPlans_TruncatedBodyIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"plans":`))
		if hj, ok := w.(http.Hijacker); ok {
			if c, _, err := hj.Hijack(); err == nil {
				_ = c.Close()
			}
		}
	}))
	defer srv.Close()
	hc := &http.Client{Timeout: time.Second}
	_, _, err := catalog.NewWithHTTPClient(srv.URL, hc).Plans(context.Background())
	require.Error(t, err)
}
