//go:build e2e

package e2e_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Every service-emitted error carries the §17 envelope: error == code, a
// numeric status equal to the HTTP status, and correlation ids.
func TestErrorEnvelopeShape(t *testing.T) {
	e := newE2EEnv(t)
	cases := []reqOpts{
		{path: "/api/v1/audit/probe", headers: gatewayHeaders(adminUser, tenantA, "member")},
		{method: http.MethodPost, path: "/api/v1/internal/probe", headers: adminHeaders(tenantA)},
		{method: http.MethodPost, path: "/api/v1/internal/probe", headers: systemHeaders(tenantA)},
		{path: "/api/v1/audit/probe", headers: gatewayHeaders(adminUser, "bad", "tenant_admin")},
	}
	for _, o := range cases {
		o.headers["x-request-id"] = "req-shape"
		code, _, body := e.do(t, o)
		var er struct {
			Error     string `json:"error"`
			Code      string `json:"code"`
			Message   string `json:"message"`
			Status    int    `json:"status"`
			RequestID string `json:"request_id"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &er), body)
		assert.NotEmpty(t, er.Code, body)
		assert.Equal(t, er.Code, er.Error, body)
		assert.Equal(t, code, er.Status, body)
		assert.NotEmpty(t, er.Message, body)
		assert.Equal(t, "req-shape", er.RequestID, body)
	}
}

// A DB outage on a request surfaces as 503 dependency_unavailable (§17).
func TestDatabaseOutage503(t *testing.T) {
	e := newE2EEnv(t)
	e.appPool.Close()
	code, _, body := e.do(t, reqOpts{path: "/api/v1/audit/probe", headers: adminHeaders(tenantA)})
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Contains(t, body, `"code":"dependency_unavailable"`)
}
