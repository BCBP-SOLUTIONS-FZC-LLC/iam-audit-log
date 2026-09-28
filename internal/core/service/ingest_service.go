package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// IngestService is the single ingest path (AL-INV-2). The direct-write
// endpoints (AL-5/AL-6) use it now; the Phase 3 consumer fleet converges on
// the same Append, so a bus event and a direct-write entry persist the
// identical row shape under the identical RLS/grant story.
type IngestService struct {
	store       port.AuditWriter
	newID       port.IDGenerator
	maxMetadata int
	maxBatch    int
	log         port.Logger
	bus         *BusIngestConfig
	metrics     port.IngestMetrics
}

// WithMetrics enables the write-path metrics (LLD §11).
func (s *IngestService) WithMetrics(m port.IngestMetrics) *IngestService {
	s.metrics = m
	return s
}

// observe records one persisted-or-replayed entry.
func (s *IngestService) observe(e domain.AuditEntry, created bool, consumer string) {
	if s.metrics == nil {
		return
	}
	if created {
		s.metrics.Ingested(e)
	} else {
		s.metrics.Duplicate(consumer)
	}
}

// directWriteResult classifies an AL-5/AL-6 entry outcome for
// directwrite_requests_total (RB-6).
func directWriteResult(created bool, err error) string {
	var de *domain.Error
	switch {
	case err == nil && created:
		return "created"
	case err == nil:
		return "duplicate"
	case errors.As(err, &de) && de.Status() < 500:
		return "rejected"
	default:
		return "error"
	}
}

// NewIngestService wires the service. maxMetadata is MAX_METADATA_BYTES and
// maxBatch MAX_INGEST_BATCH (LLD §12).
func NewIngestService(store port.AuditWriter, newID port.IDGenerator, maxMetadata, maxBatch int, log port.Logger) *IngestService {
	return &IngestService{store: store, newID: newID, maxMetadata: maxMetadata, maxBatch: maxBatch, log: log}
}

// MaxBatch is the AL-6 entry cap.
func (s *IngestService) MaxBatch() int { return s.maxBatch }

// DirectWrite validates and persists one AL-5 entry. created=false means an
// idempotent replay of an already-persisted entry (LLD §5.4 → 200).
func (s *IngestService) DirectWrite(ctx context.Context, cmd domain.DirectWriteCommand) (stored domain.AuditEntry, created bool, err error) {
	if s.metrics != nil {
		defer func() { s.metrics.DirectWrite(cmd.SourceService, directWriteResult(created, err)) }()
	}
	return s.directWrite(ctx, cmd)
}

func (s *IngestService) directWrite(ctx context.Context, cmd domain.DirectWriteCommand) (domain.AuditEntry, bool, error) {
	entry, err := domain.BuildDirectWriteEntry(cmd, s.maxMetadata)
	if err != nil {
		return domain.AuditEntry{}, false, err
	}
	id, err := s.newID()
	if err != nil {
		return domain.AuditEntry{}, false, fmt.Errorf("mint entry id: %w", err)
	}
	entry.ID = id
	stored, created, err := s.store.Append(ctx, entry, domain.ConsumerDirectWrite)
	if err != nil {
		return domain.AuditEntry{}, false, err
	}
	s.observe(stored, created, domain.ConsumerDirectWrite)
	if s.log != nil {
		// IDs only — never metadata / payload at info level (LLD §11).
		s.log.Info("direct-write entry ingested", map[string]any{
			"entry_id": stored.ID, "entry_type": stored.EntryType, "tenant_id": stored.TenantID,
			"source_service": stored.SourceService, "created": created,
		})
	}
	return stored, created, nil
}

// BatchResult is one AL-6 entry's outcome (decision D-4).
type BatchResult struct {
	Index   int
	Entry   domain.AuditEntry // set on success
	Created bool
	Err     error // a *domain.Error for per-entry failures
}

// DirectWriteBatch persists each entry independently: a malformed or
// rejected entry never rejects the batch (LLD §5.4 AL-6 partial success).
// Only a whole-batch problem (size) is returned as err.
func (s *IngestService) DirectWriteBatch(ctx context.Context, cmds []domain.DirectWriteCommand) ([]BatchResult, error) {
	if len(cmds) == 0 {
		return nil, domain.NewError(domain.ErrInvalidRequest, "entries must be a non-empty array")
	}
	if s.maxBatch > 0 && len(cmds) > s.maxBatch {
		return nil, domain.NewError(domain.ErrBatchTooLarge, fmt.Sprintf("batch has %d entries; the limit is %d", len(cmds), s.maxBatch))
	}
	out := make([]BatchResult, len(cmds))
	for i, cmd := range cmds {
		entry, created, err := s.DirectWrite(ctx, cmd)
		out[i] = BatchResult{Index: i, Entry: entry, Created: created, Err: err}
		var de *domain.Error
		if err != nil && errors.As(err, &de) && de.Code == domain.ErrDependencyUnavailable {
			// The database is down: every remaining entry would fail the
			// same way — report them as such rather than hammering it.
			for j := i + 1; j < len(cmds); j++ {
				out[j] = BatchResult{Index: j, Err: err}
			}
			break
		}
	}
	return out, nil
}
