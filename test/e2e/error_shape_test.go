//go:build e2e

package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every service-emitted error is an RFC 9457 Problem Details body (§17, rev 0.30):
// type, title, status, errorCode (UPPER_SNAKE_CASE), traceId. No error text,
// no request_id in the body (it is in the X-Request-ID response header).
func TestErrorEnvelopeShape(t *testing.T) {
	e := newE2EEnv(t)
	cases := []reqOpts{
		{path: "/api/v1/audit/probe", headers: gatewayHeaders(adminUser, tenantA, "member")},
		{method: http.MethodPost, path: "/api/v1/internal/probe", headers: adminHeaders(tenantA)},
		{method: http.MethodPost, path: "/api/v1/internal/probe", headers: systemHeaders(tenantA)},
		{path: "/api/v1/audit/probe", headers: gatewayHeaders(adminUser, "bad", "tenant_admin")},
	}
	for _, o := range cases {
		code, hdrs, body := e.do(t, o)
		var er struct {
			Type      string `json:"type"`
			Title     string `json:"title"`
			Status    int    `json:"status"`
			ErrorCode string `json:"errorCode"`
			TraceID   string `json:"traceId"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &er), body)
		assert.NotEmpty(t, er.ErrorCode, body)
		assert.Equal(t, code, er.Status, body)
		assert.NotEmpty(t, er.Title, body)
		assert.NotEmpty(t, er.Type, body)
		assert.NotEmpty(t, er.TraceID, body)
		assert.Equal(t, "application/problem+json", hdrs.Get("Content-Type"), body)
	}
}

// A DB outage on a request surfaces as 503 dependency_unavailable (§17).
func TestDatabaseOutage503(t *testing.T) {
	e := newE2EEnv(t)
	e.appPool.Close()
	code, _, body := e.do(t, reqOpts{path: "/api/v1/audit/probe", headers: adminHeaders(tenantA)})
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Contains(t, body, `"errorCode":"DEPENDENCY_UNAVAILABLE"`)
}
