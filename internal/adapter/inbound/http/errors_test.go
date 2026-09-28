package http

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
)

func TestHandleError_MapsDomainErrorsAndFallsBackTo500(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{domain.NewError(domain.ErrUnknownEntryType, "x"), http.StatusUnprocessableEntity, "unknown_entry_type"},
		{domain.NewError(domain.ErrDependencyUnavailable, "x"), http.StatusServiceUnavailable, "dependency_unavailable"},
		{errors.New("boom"), http.StatusInternalServerError, "internal_error"},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
		HandleError(c, tc.err)
		assert.Equal(t, tc.status, w.Code)
		assert.Contains(t, w.Body.String(), `"code":"`+tc.code+`"`)
		assert.Contains(t, w.Body.String(), `"error":"`+tc.code+`"`)
	}
}
