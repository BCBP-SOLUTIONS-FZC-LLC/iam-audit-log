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
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/v2/pkg/problem"
)

func testCtx(req *http.Request) (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	return c, w
}

// abort() must produce an RFC 9457 problem body (application/problem+json) with
// the correct HTTP status and UPPER_SNAKE_CASE errorCode.
func TestAbort_ProducesRFC9457Problem(t *testing.T) {
	c, w := testCtx(httptest.NewRequest(http.MethodGet, "/", nil))
	abort(c, domain.ErrInvalidRequest, "something is wrong")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))
	body := decode(t, w)
	assert.Equal(t, "INVALID_REQUEST", body["errorCode"])
	assert.Equal(t, "something is wrong", body["detail"])
}

// A leaked connectivity-class PgError still maps to 503 (§17).
func TestHandleError_LeakedConnectionPgError_503(t *testing.T) {
	c, w := testCtx(httptest.NewRequest(http.MethodGet, "/", nil))
	HandleError(c, &pgconn.PgError{Code: "08006", Message: "connection failure"})
	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))
	assert.Equal(t, "DEPENDENCY_UNAVAILABLE", decode(t, w)["errorCode"])
}

// A domain.Error with extra Details maps to the correct problem status and
// errorCode; structured details are not echoed in the RFC 9457 body.
func TestHandleError_DomainError_MapsToCorrectType(t *testing.T) {
	c, w := testCtx(httptest.NewRequest(http.MethodGet, "/", nil))
	HandleError(c, domain.NewError(domain.ErrMetadataTooLarge, "too big").WithDetails(map[string]any{"limit": 8192}))
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Equal(t, problem.ContentType, w.Header().Get("Content-Type"))
	assert.Equal(t, "METADATA_TOO_LARGE", decode(t, w)["errorCode"])
	assert.Equal(t, "too big", decode(t, w)["detail"])
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
