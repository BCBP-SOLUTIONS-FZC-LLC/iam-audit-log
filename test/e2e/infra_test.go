//go:build e2e

package e2e_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHealthzUnauthenticated(t *testing.T) {
	e := newE2EEnv(t)
	code, _, body := e.do(t, reqOpts{path: "/healthz"})
	assert.Equal(t, http.StatusOK, code)
	assert.JSONEq(t, `{"status":"ok"}`, body)
}

func TestReadyzWhenDBUp(t *testing.T) {
	e := newE2EEnv(t)
	code, _, body := e.do(t, reqOpts{path: "/readyz"})
	assert.Equal(t, http.StatusOK, code)
	assert.JSONEq(t, `{"status":"ready","checks":{"database":"ok"}}`, body)
}

// /readyz flips to 503 when Postgres goes away (LLD §11 Health).
func TestReadyzWhenDBDown(t *testing.T) {
	e := newE2EEnv(t)
	e.appPool.Close()
	code, _, body := e.do(t, reqOpts{path: "/readyz"})
	assert.Equal(t, http.StatusServiceUnavailable, code)
	assert.Contains(t, body, `"database":"down"`)
}

func TestAsyncAPIServedWithoutAuth(t *testing.T) {
	e := newE2EEnv(t)
	code, hdr, body := e.do(t, reqOpts{path: "/asyncapi.yaml"})
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "nosniff", hdr.Get("X-Content-Type-Options"))
	assert.Contains(t, body, "asyncapi: 3.0.0")
	assert.NotContains(t, body, "action: send", "AL-INV-10: receive-only")

	code, _, _ = e.do(t, reqOpts{path: "/asyncapi"})
	assert.Equal(t, http.StatusOK, code)
}

func TestXRequestIDEchoedWhenProvided(t *testing.T) {
	e := newE2EEnv(t)
	_, hdr, _ := e.do(t, reqOpts{path: "/healthz", headers: map[string]string{"x-request-id": "req-e2e-1"}})
	assert.Equal(t, "req-e2e-1", hdr.Get("X-Request-ID"))
}

func TestXRequestIDGeneratedWhenAbsent(t *testing.T) {
	e := newE2EEnv(t)
	_, hdr, _ := e.do(t, reqOpts{path: "/healthz"})
	assert.NotEmpty(t, hdr.Get("X-Request-ID"))
}

// The router caps bodies (AL-6 batches of ≤500 × 8 KiB fit; much more doesn't).
func TestBodyCapEnforced(t *testing.T) {
	e := newE2EEnv(t)
	h := systemHeaders(tenantA)
	h["Content-Type"] = "application/json"
	h["Idempotency-Key"] = "k-body-cap"
	big := `{"pad":"` + strings.Repeat("x", 9<<20) + `"}`
	code, _, _ := e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/probe", body: big, headers: h})
	assert.Equal(t, http.StatusRequestEntityTooLarge, code)

	small := `{"pad":"` + strings.Repeat("x", 1<<20) + `"}` // well under the cap
	h["Idempotency-Key"] = "k-body-ok"
	code, _, _ = e.do(t, reqOpts{method: http.MethodPost, path: "/api/v1/internal/probe", body: small, headers: h})
	assert.Equal(t, http.StatusOK, code)

	code, _, _ = e.do(t, reqOpts{path: "/healthz"})
	assert.Equal(t, http.StatusOK, code)
}

func TestUnknownRouteAndMethod(t *testing.T) {
	e := newE2EEnv(t)
	code, _, _ := e.do(t, reqOpts{path: "/no/such/route"})
	assert.Equal(t, http.StatusNotFound, code)
	code, _, _ = e.do(t, reqOpts{method: http.MethodDelete, path: "/healthz"})
	assert.Equal(t, http.StatusMethodNotAllowed, code)
}
