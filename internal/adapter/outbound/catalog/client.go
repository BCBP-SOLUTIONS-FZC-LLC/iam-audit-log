package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// plansPath is CAT-I2 (Catalog LLD §5.4; mesh-only, RequireSystemRole).
const plansPath = "/api/v1/internal/plans"

// maxBodyBytes caps the CAT-I2 response (BUILD_PLAN Phase 5: 1 MiB).
const maxBodyBytes = 1 << 20

// Client calls Catalog's CAT-I2 as the iam-system sentinel (§10.2).
type Client struct {
	baseURL string
	http    *http.Client
}

var _ port.PlanCatalog = (*Client)(nil)

// New returns a client for baseURL (CATALOG_BASE_URL) with a per-request
// timeout (CATALOG_PLANS_POLL_TIMEOUT).
func New(baseURL string, timeout time.Duration) *Client {
	return NewWithHTTPClient(baseURL, &http.Client{Timeout: timeout})
}

// NewWithHTTPClient returns a client over hc (tests).
func NewWithHTTPClient(baseURL string, hc *http.Client) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), http: hc}
}

// plansResponse mirrors Catalog's InternalPlansResponse; only the fields
// this service uses are decoded.
type plansResponse struct {
	Plans []struct {
		Code                 string `json:"code"`
		AuditQueryWindowDays int    `json:"audit_query_window_days"`
	} `json:"plans"`
	RecordVersions map[string]int64 `json:"record_versions"`
}

// Plans implements port.PlanCatalog.
func (c *Client) Plans(ctx context.Context) ([]port.CatalogPlan, map[string]int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+plansPath, http.NoBody)
	if err != nil {
		return nil, nil, fmt.Errorf("catalog: build request: %w", err)
	}
	// The iam-system sentinel header set Catalog's RequireSystemRole and
	// identity bridge accept; CAT-I2 is cross-tenant, so x-tenant-id is the
	// nil-UUID placeholder (the iam-org-membership client does the same).
	req.Header.Set("x-user-id", "iam-system")
	req.Header.Set("x-tenant-id", domain.NilUUID)
	req.Header.Set("x-tenant-roles", "iam-system")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, nil, classify(err)
	}
	defer func() { _ = resp.Body.Close() }() //nolint:errcheck // read-only body; nothing to recover
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512)) //nolint:errcheck // best-effort error detail
		return nil, nil, fmt.Errorf("catalog: CAT-I2 returned %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, nil, classify(err)
	}
	if len(body) > maxBodyBytes {
		return nil, nil, fmt.Errorf("catalog: CAT-I2 response exceeds %d bytes", maxBodyBytes)
	}
	var out plansResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, nil, fmt.Errorf("catalog: decode CAT-I2 response: %w", err)
	}
	plans := make([]port.CatalogPlan, len(out.Plans))
	for i, p := range out.Plans {
		plans[i] = port.CatalogPlan{Code: p.Code, AuditQueryWindowDays: p.AuditQueryWindowDays}
	}
	return plans, out.RecordVersions, nil
}

// classify marks timeouts with port.ErrCatalogTimeout (result="timeout").
func classify(err error) error {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return fmt.Errorf("%w: %w", port.ErrCatalogTimeout, err)
	}
	return fmt.Errorf("catalog: CAT-I2 request: %w", err)
}
