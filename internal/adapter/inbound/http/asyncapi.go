package http

import (
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"gopkg.in/yaml.v3"

	apispec "github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/api"
)

// AsyncAPIYAMLHandler serves the raw AsyncAPI 3.0 spec (LLD §7.3).
func AsyncAPIYAMLHandler(c *gin.Context) {
	c.Data(http.StatusOK, "application/yaml; charset=utf-8", apispec.AsyncAPISpec)
}

var (
	// asyncSpecRaw is the spec source; a var so tests can exercise the
	// parse-failure path.
	asyncSpecRaw  = apispec.AsyncAPISpec
	asyncJSONOnce sync.Once
	asyncJSONVal  any
	asyncJSONErr  error
)

// AsyncAPIHandler serves the same spec as JSON (parsed once per process).
func AsyncAPIHandler(c *gin.Context) {
	asyncJSONOnce.Do(func() {
		asyncJSONErr = yaml.Unmarshal(asyncSpecRaw, &asyncJSONVal)
	})
	if asyncJSONErr != nil {
		HandleError(c, asyncJSONErr)
		return
	}
	c.JSON(http.StatusOK, asyncJSONVal)
}
