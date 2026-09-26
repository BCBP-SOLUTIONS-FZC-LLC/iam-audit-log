package http

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

func testCtx(req *http.Request) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	return c, w
}

// request_id falls back from the gincommon context → the request header →
// the response header set by RequestIDMiddleware.
func TestNewErrorResponse_RequestIDFallbacks(t *testing.T) {
	c, _ := testCtx(httptest.NewRequest(http.MethodGet, "/", nil))
	c.Request.Header.Set(gincommon.HeaderRequestID, "req-from-header")
	assert.Equal(t, "req-from-header", newErrorResponse(c, domain.ErrInvalidRequest, "m", 400).RequestID)

	c, _ = testCtx(httptest.NewRequest(http.MethodGet, "/", nil))
	c.Writer.Header().Set(gincommon.HeaderRequestIDResponse, "req-from-response")
	assert.Equal(t, "req-from-response", newErrorResponse(c, domain.ErrInvalidRequest, "m", 400).RequestID)

	er := newErrorResponse(nil, domain.ErrInvalidRequest, "m", 400)
	assert.Equal(t, "invalid_request", er.Code)
	assert.Empty(t, er.RequestID)
}

// A leaked connectivity-class PgError still maps to 503 (§17).
func TestHandleError_LeakedConnectionPgError_503(t *testing.T) {
	c, w := testCtx(httptest.NewRequest(http.MethodGet, "/", nil))
	HandleError(c, &pgconn.PgError{Code: "08006", Message: "connection failure"})
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Contains(t, w.Body.String(), `"code":"dependency_unavailable"`)
}

func TestHandleError_CarriesDetails(t *testing.T) {
	c, w := testCtx(httptest.NewRequest(http.MethodGet, "/", nil))
	HandleError(c, domain.NewError(domain.ErrMetadataTooLarge, "too big").WithDetails(map[string]any{"limit": 8192}))
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Contains(t, w.Body.String(), `"limit":8192`)
}

func TestIdentityBridge_WithoutGatewayContext_401(t *testing.T) {
	r := gin.New()
	r.GET("/x", IdentityBridgeMiddleware(), func(c *gin.Context) { c.Status(http.StatusOK) })
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestIdentityBridge_MalformedUserID_401(t *testing.T) {
	w := do(t, probeRouter(nil), http.MethodGet, "/api/v1/audit/probe", map[string]string{
		"x-user-id": "not-a-uuid", "x-tenant-id": tenantA, "x-tenant-roles": "tenant_admin",
	})
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Contains(t, w.Body.String(), "x-user-id")
}

func TestRequireJSONContentType(t *testing.T) {
	r := gin.New()
	ok := func(c *gin.Context) { c.Status(http.StatusOK) }
	r.POST("/x", RequireJSONContentType(), ok)
	r.GET("/x", RequireJSONContentType(), ok)

	send := func(method, ct, body string) int {
		req := httptest.NewRequest(method, "/x", strings.NewReader(body))
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		return w.Code
	}
	assert.Equal(t, http.StatusBadRequest, send(http.MethodPost, "text/plain", "{}"))
	assert.Equal(t, http.StatusOK, send(http.MethodPost, "application/json", "{}"))
	assert.Equal(t, http.StatusOK, send(http.MethodPost, "", ""), "empty body needs no content type")
	assert.Equal(t, http.StatusOK, send(http.MethodGet, "", ""))
}

func TestReadyz_SkipsNilChecks(t *testing.T) {
	w := do(t, probeRouter(map[string]Pinger{"unset": nil}), http.MethodGet, "/readyz", nil)
	assert.Equal(t, http.StatusOK, w.Code)
}

// Not parallel: swaps package-level spec state.
func TestAsyncAPIHandler_ParseFailure_500(t *testing.T) {
	origRaw := asyncSpecRaw
	t.Cleanup(func() {
		asyncSpecRaw = origRaw
		asyncJSONOnce, asyncJSONVal, asyncJSONErr = sync.Once{}, nil, nil
	})
	asyncSpecRaw = []byte("key: [unterminated")
	asyncJSONOnce, asyncJSONVal, asyncJSONErr = sync.Once{}, nil, nil

	c, w := testCtx(httptest.NewRequest(http.MethodGet, "/asyncapi", nil))
	AsyncAPIHandler(c)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
