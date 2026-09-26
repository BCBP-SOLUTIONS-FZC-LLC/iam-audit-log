package service

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// QueryConfig tunes QueryService.
type QueryConfig struct {
	DefaultWindowDays int   // AUDIT_DEFAULT_QUERY_WINDOW_DAYS
	SyncMaxRows       int64 // ARCHIVE_SYNC_MAX_ROWS (D-10)
	SyncMaxBytes      int64 // ARCHIVE_SYNC_MAX_BYTES (D-10)
	// Windows is the CAT-I2 live plan → days map (AL-D15); nil until the
	// Phase 5 poller is wired, in which case the stored row value is used.
	Windows port.PlanWindows
	Now     port.Clock
}

// QueryService serves AL-1/AL-2: the plan-window clamp on top of RLS
// (AL-INV-8), hot RDS reads, and bounded archived reads (D-10, D-12).
type QueryService struct {
	reader  port.AuditReader
	archive port.ArchiveReader
	cfg     QueryConfig
	log     port.Logger
}

// NewQueryService wires the service.
func NewQueryService(reader port.AuditReader, archive port.ArchiveReader, cfg QueryConfig, log port.Logger) *QueryService {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &QueryService{reader: reader, archive: archive, cfg: cfg, log: log}
}

// QueryRequest is one AL-1 call.
type QueryRequest struct {
	TenantID string
	Filter   domain.QueryFilter
	Cursor   string
	Limit    int // 0 → DefaultQueryLimit
}

// QueryResult is an AL-1 page, or — when Deferred is set — the clamped
// filter that must be turned into an export instead (D-10 → 202).
type QueryResult struct {
	Events        []domain.AuditEntry
	NextCursor    string
	WindowClamped bool
	EffectiveFrom time.Time
	Deferred      *domain.QueryFilter
}

// window resolves the tenant's plan window and clamps [from, to] to it.
func (s *QueryService) window(ctx context.Context, tenantID string, f domain.QueryFilter) (domain.Window, error) {
	row, err := s.reader.PlanWindow(ctx, tenantID)
	if err != nil {
		return domain.Window{}, err
	}
	var live func(string) (int, bool)
	if s.cfg.Windows != nil {
		live = s.cfg.Windows.WindowDays
	}
	days := domain.ResolveWindowDays(row, live, s.cfg.DefaultWindowDays)
	return domain.ClampWindow(f.From, f.To, s.cfg.Now().UTC(), days), nil
}

// validateRequest checks the filter, limit and cursor shared by AL-1/AL-7.
func validateRequest(req QueryRequest) (limit int, cursor *domain.Cursor, err error) {
	if err := req.Filter.Validate(); err != nil {
		return 0, nil, err
	}
	limit = req.Limit
	if limit == 0 {
		limit = domain.DefaultQueryLimit
	}
	if limit < 1 || limit > domain.MaxQueryLimit {
		return 0, nil, domain.NewError(domain.ErrInvalidRequest, fmt.Sprintf("limit must be between 1 and %d", domain.MaxQueryLimit))
	}
	cursor, err = domain.DecodeCursor(req.Cursor)
	return limit, cursor, err
}

// Query implements AL-1.
func (s *QueryService) Query(ctx context.Context, req QueryRequest) (QueryResult, error) {
	limit, cursor, err := validateRequest(req)
	if err != nil {
		return QueryResult{}, err
	}
	w, err := s.window(ctx, req.TenantID, req.Filter)
	if err != nil {
		return QueryResult{}, err
	}
	res := QueryResult{WindowClamped: w.Clamped, EffectiveFrom: w.From}
	if w.Empty {
		return res, nil // "you can't see that far back on your plan" — 200, empty (§5.4)
	}

	objects, err := s.reader.ArchivedObjects(ctx, req.TenantID, w.From, w.To, req.Filter.RetentionTier)
	if err != nil {
		return QueryResult{}, err
	}
	if len(objects) > 0 && s.exceedsSyncBounds(objects) {
		f := clampedFilter(req.Filter, w)
		res.Deferred = &f
		return res, nil
	}
	return s.page(ctx, req, w, objects, cursor, limit, res)
}

// InternalQuery implements AL-7: the mesh-only provenance read. It is
// RLS-scoped to the explicit tenant_id but has no plan clamp (§5.4:
// compliance/legal retrieve beyond the plan window through AL-7). An
// archived range over the D-10 bounds is 422 range_too_large — a service
// caller has no export path (decision D-14).
func (s *QueryService) InternalQuery(ctx context.Context, req QueryRequest) (QueryResult, error) {
	limit, cursor, err := validateRequest(req)
	if err != nil {
		return QueryResult{}, err
	}
	w := domain.Window{From: time.Unix(0, 0).UTC(), To: s.cfg.Now().UTC()}
	if req.Filter.From != nil {
		w.From = req.Filter.From.UTC()
	}
	if req.Filter.To != nil {
		w.To = req.Filter.To.UTC()
	}
	objects, err := s.reader.ArchivedObjects(ctx, req.TenantID, w.From, w.To, req.Filter.RetentionTier)
	if err != nil {
		return QueryResult{}, err
	}
	if len(objects) > 0 && s.exceedsSyncBounds(objects) {
		return QueryResult{}, domain.NewError(domain.ErrRangeTooLarge,
			"the archived part of this range exceeds the synchronous read bound; narrow from/to")
	}
	return s.page(ctx, req, w, objects, cursor, limit, QueryResult{EffectiveFrom: w.From})
}

// page reads one keyset page from the hot store plus the (size-bounded)
// archived objects and merges them.
func (s *QueryService) page(ctx context.Context, req QueryRequest, w domain.Window, objects []domain.ArchiveObject,
	cursor *domain.Cursor, limit int, res QueryResult,
) (QueryResult, error) {
	hot, err := s.reader.Page(ctx, req.TenantID, req.Filter, w.From, w.To, cursor, limit+1)
	if err != nil {
		return QueryResult{}, err
	}
	cold, err := s.readArchived(ctx, objects, req.Filter, w, cursor, limit+1)
	if err != nil {
		return QueryResult{}, err
	}
	merged := mergeNewestFirst(hot, cold)
	if len(merged) > limit {
		last := merged[limit-1]
		res.NextCursor = domain.Cursor{OccurredAt: last.OccurredAt, ID: last.ID}.Encode()
		merged = merged[:limit]
	}
	res.Events = merged
	return res, nil
}

// exceedsSyncBounds applies decision D-10 to the manifest estimate (an
// upper bound: it counts every row of each overlapping object, pre-filter).
func (s *QueryService) exceedsSyncBounds(objects []domain.ArchiveObject) bool {
	var rows, bytes int64
	for _, o := range objects {
		rows += o.RowCount
		bytes += o.ByteSize
	}
	return (s.cfg.SyncMaxRows > 0 && rows > s.cfg.SyncMaxRows) || (s.cfg.SyncMaxBytes > 0 && bytes > s.cfg.SyncMaxBytes)
}

// readArchived reads the (already size-bounded) archived objects, keeping
// up to want rows after the cursor. Objects arrive newest-first by
// max_occurred_at, so reading stops once no remaining object can hold a
// row newer than the oldest one kept.
func (s *QueryService) readArchived(ctx context.Context, objects []domain.ArchiveObject, f domain.QueryFilter, w domain.Window, after *domain.Cursor, want int) ([]domain.AuditEntry, error) {
	var out []domain.AuditEntry
	for _, o := range objects {
		if after != nil && o.MinOccurredAt.After(after.OccurredAt) {
			continue // every row in it sorts before the cursor
		}
		if len(out) >= want && o.MaxOccurredAt.Before(out[want-1].OccurredAt) {
			break
		}
		err := s.archive.ReadArchive(ctx, o.Bucket, o.Key, func(r domain.ArchiveRecord) error {
			e := r.Entry()
			if e.OccurredAt.Before(w.From) || e.OccurredAt.After(w.To) || !f.Matches(e) || !after.After(e) {
				return nil
			}
			out = append(out, e)
			return nil
		})
		if errors.Is(err, port.ErrObjectMissing) {
			s.warnMissing(o, err)
			continue
		}
		if err != nil {
			return nil, err
		}
		sort.SliceStable(out, func(i, j int) bool { return domain.NewerFirst(out[i], out[j]) })
		if len(out) > want {
			out = out[:want]
		}
	}
	return out, nil
}

func (s *QueryService) warnMissing(o domain.ArchiveObject, err error) {
	if s.log != nil {
		s.log.Warn("archive object listed in manifest is missing — skipped", map[string]any{
			"bucket": o.Bucket, "key": o.Key, "tenant_id": o.TenantID, "error": err.Error(),
		})
	}
}

// Get implements AL-2: one entry, within tenant (RLS) and within the plan
// window (AL-INV-8). The hot store is tried first; a miss falls back to the
// archive objects whose id range contains id (a month lives in exactly one
// place, D-12). An entry outside the window is indistinguishable from an
// absent one (404).
func (s *QueryService) Get(ctx context.Context, tenantID, id string) (domain.AuditEntry, error) {
	if !domain.IsUUID(id) {
		return domain.AuditEntry{}, domain.NewError(domain.ErrInvalidRequest, "id must be a UUID")
	}
	w, err := s.window(ctx, tenantID, domain.QueryFilter{})
	if err != nil {
		return domain.AuditEntry{}, err
	}
	e, err := s.reader.Get(ctx, tenantID, id)
	if errors.Is(err, port.ErrNotFound) {
		e, err = s.getArchived(ctx, tenantID, id, w.From)
	}
	if errors.Is(err, port.ErrNotFound) || (err == nil && e.OccurredAt.Before(w.From)) {
		return domain.AuditEntry{}, domain.NewError(domain.ErrAuditEntryNotFound, "audit entry not found")
	}
	return e, err
}

// errFound stops an archive scan once the entry has been read.
var errFound = errors.New("found")

// getArchived looks id up in the tenant's archive objects (port.ErrNotFound
// when absent).
func (s *QueryService) getArchived(ctx context.Context, tenantID, id string, from time.Time) (domain.AuditEntry, error) {
	objects, err := s.reader.ArchivedObjectsForID(ctx, tenantID, id, from)
	if err != nil {
		return domain.AuditEntry{}, err
	}
	for _, o := range objects {
		var found domain.AuditEntry
		err := s.archive.ReadArchive(ctx, o.Bucket, o.Key, func(r domain.ArchiveRecord) error {
			if !strings.EqualFold(r.ID, id) {
				return nil
			}
			found = r.Entry()
			return errFound
		})
		switch {
		case errors.Is(err, errFound):
			return found, nil
		case errors.Is(err, port.ErrObjectMissing):
			s.warnMissing(o, err)
		case err != nil:
			return domain.AuditEntry{}, err
		}
	}
	return domain.AuditEntry{}, port.ErrNotFound
}

// clampedFilter returns f with its range replaced by the effective window.
func clampedFilter(f domain.QueryFilter, w domain.Window) domain.QueryFilter {
	from, to := w.From.UTC(), w.To.UTC()
	f.From, f.To = &from, &to
	return f
}

// mergeNewestFirst merges two (occurred_at DESC, id DESC) sorted slices.
func mergeNewestFirst(a, b []domain.AuditEntry) []domain.AuditEntry {
	out := make([]domain.AuditEntry, 0, len(a)+len(b))
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if domain.NewerFirst(a[i], b[j]) {
			out = append(out, a[i])
			i++
		} else {
			out = append(out, b[j])
			j++
		}
	}
	out = append(out, a[i:]...)
	return append(out, b[j:]...)
}
