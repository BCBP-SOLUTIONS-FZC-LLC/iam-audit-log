package http

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/service"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/platform-gincommon/pkg/gincommon"
)

// DirectWriter is the ingest use case the AL-5/AL-6 handlers drive
// (implemented by *service.IngestService).
type DirectWriter interface {
	DirectWrite(ctx context.Context, cmd domain.DirectWriteCommand) (domain.AuditEntry, bool, error)
	DirectWriteBatch(ctx context.Context, cmds []domain.DirectWriteCommand) ([]service.BatchResult, error)
}

// IngestHandler serves the mesh-only direct-write contract (LLD §5.4, AL-D1).
type IngestHandler struct {
	svc DirectWriter
}

// NewIngestHandler wires the handler.
func NewIngestHandler(svc DirectWriter) *IngestHandler { return &IngestHandler{svc: svc} }

// decodeJSON binds the body; a malformed or oversized body is 400
// invalid_request (§17). Unknown fields (e.g. a caller-supplied
// retention_tier) are ignored on purpose — AL-INV-11.
func decodeJSON(c *gin.Context, dst any) bool {
	if err := json.NewDecoder(c.Request.Body).Decode(dst); err != nil {
		msg := "request body is not valid JSON for this endpoint"
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			msg = "request body exceeds the size limit"
		}
		abort(c, domain.ErrInvalidRequest, msg)
		return false
	}
	return true
}

// CreateEntry is AL-5.
//
//	@Summary		Direct-write one audit entry (AL-5)
//	@Description	Mesh-only ingest for non-bus audit entries (LLD §5.4, AL-D1). The Idempotency-Key header becomes the entry's source_event_id; a retry with the same key returns 200 with the already-persisted entry. retention_tier is derived from entry_type — a caller-supplied value is ignored (AL-INV-11). Only direct-write entry types are accepted (decision D-6).
//	@Tags			Ingest
//	@Accept			json
//	@Produce		json
//	@Security		TenantRoles
//	@Security		TenantID
//	@Security		UserID
//	@Security		IdempotencyKey
//	@Param			body	body		DirectWriteRequest	true	"Audit entry"
//	@Success		201		{object}	EntryResponse		"created"
//	@Success		200		{object}	EntryResponse		"idempotent replay"
//	@Failure		400		{object}	ErrorResponse		"invalid_request"
//	@Failure		403		{object}	ErrorResponse		"forbidden_peer"
//	@Failure		422		{object}	ErrorResponse		"unknown_entry_type | invalid_actor | metadata_too_large"
//	@Failure		503		{object}	ErrorResponse		"dependency_unavailable"
//	@Router			/internal/audit-entries [post]
func (h *IngestHandler) CreateEntry(c *gin.Context) {
	var req DirectWriteRequest
	if !decodeJSON(c, &req) {
		return
	}
	cmd := req.command(c.GetHeader(IdempotencyKeyHeader), gincommon.TraceIDFromContext(c))
	entry, created, err := h.svc.DirectWrite(c.Request.Context(), cmd)
	if err != nil {
		HandleError(c, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	c.JSON(status, toEntryResponse(entry))
}

// CreateBatch is AL-6.
//
//	@Summary		Direct-write a batch of audit entries (AL-6)
//	@Description	Up to MAX_INGEST_BATCH entries, each with its own idempotency_key (decision D-4). Always 207: per-index status 201 (created), 200 (replay) or the §17 error; a bad entry never rejects the batch. Rate-limited per tenant (429, §10.5).
//	@Tags			Ingest
//	@Accept			json
//	@Produce		json
//	@Security		TenantRoles
//	@Security		TenantID
//	@Security		UserID
//	@Security		IdempotencyKey
//	@Param			body	body		BatchRequest	true	"Entries"
//	@Success		207		{object}	BatchResponse	"per-entry results"
//	@Failure		400		{object}	ErrorResponse	"invalid_request"
//	@Failure		403		{object}	ErrorResponse	"forbidden_peer"
//	@Failure		422		{object}	ErrorResponse	"batch_too_large"
//	@Failure		429		{object}	ErrorResponse	"rate_limited"
//	@Router			/internal/audit-entries:batch [post]
func (h *IngestHandler) CreateBatch(c *gin.Context) {
	var req BatchRequest
	if !decodeJSON(c, &req) {
		return
	}
	trace := gincommon.TraceIDFromContext(c)
	cmds := make([]domain.DirectWriteCommand, len(req.Entries))
	for i, e := range req.Entries {
		cmds[i] = e.command(e.IdempotencyKey, trace)
	}
	results, err := h.svc.DirectWriteBatch(c.Request.Context(), cmds)
	if err != nil {
		HandleError(c, err)
		return
	}
	resp := BatchResponse{Results: make([]BatchResultItem, len(results))}
	for i, r := range results {
		item := BatchResultItem{Index: r.Index}
		switch {
		case r.Err == nil && r.Created:
			item.Status, item.ID = http.StatusCreated, r.Entry.ID
		case r.Err == nil:
			item.Status, item.ID = http.StatusOK, r.Entry.ID
		default:
			var de *domain.Error
			if errors.As(r.Err, &de) {
				item.Status, item.Code, item.Message = de.Status(), string(de.Code), de.Message
			} else {
				if errorLogger != nil {
					errorLogger.Error("batch entry failed", map[string]any{"index": r.Index, "error": r.Err.Error()})
				}
				item.Status, item.Code, item.Message = http.StatusInternalServerError, "internal_error", "internal error"
			}
		}
		resp.Results[i] = item
	}
	c.JSON(http.StatusMultiStatus, resp)
}
