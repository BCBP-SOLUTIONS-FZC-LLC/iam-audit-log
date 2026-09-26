package service

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// ExportConfig tunes ExportService.
type ExportConfig struct {
	SignedURLTTL   time.Duration // EXPORT_SIGNED_URL_TTL — retrieval window (7 d)
	DownloadURLTTL time.Duration // EXPORT_DOWNLOAD_URL_TTL — per-poll URL (D-11)
	PollInterval   time.Duration // EXPORT_POLL_INTERVAL
	Lease          time.Duration // EXPORT_JOB_LEASE
	WorkDir        string        // EXPORT_WORK_DIR ("" → os.TempDir())
	Now            port.Clock
}

// ExportService serves AL-3/AL-4 and runs the export worker (decision D-2:
// in cmd/server, as audit_app, claiming jobs via claim_export_job()).
type ExportService struct {
	jobs    port.ExportJobs
	reader  port.AuditReader
	archive port.ArchiveReader
	store   port.ExportStore
	query   *QueryService
	newID   port.IDGenerator
	cfg     ExportConfig
	log     port.Logger
}

// NewExportService wires the service. query supplies the plan-window clamp.
func NewExportService(jobs port.ExportJobs, reader port.AuditReader, archive port.ArchiveReader, store port.ExportStore,
	query *QueryService, newID port.IDGenerator, cfg ExportConfig, log port.Logger,
) *ExportService {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}
	if cfg.Lease <= 0 {
		cfg.Lease = 15 * time.Minute
	}
	return &ExportService{jobs: jobs, reader: reader, archive: archive, store: store, query: query, newID: newID, cfg: cfg, log: log}
}

// Request implements AL-3 (and the D-10 deferral from AL-1): the filter is
// clamped to the plan window and stored that way, so the export can never
// reach further back than an interactive query (AL-INV-8). Every call
// creates a new job; abuse is bounded by the per-tenant AL-3 limiter
// (§10.5), not by deduplication.
func (s *ExportService) Request(ctx context.Context, tenantID, requestedBy string, f domain.QueryFilter) (domain.ExportJob, error) {
	if err := f.Validate(); err != nil {
		return domain.ExportJob{}, err
	}
	w, err := s.query.window(ctx, tenantID, f)
	if err != nil {
		return domain.ExportJob{}, err
	}
	f = clampedFilter(f, w)
	id, err := s.newID()
	if err != nil {
		return domain.ExportJob{}, fmt.Errorf("mint export id: %w", err)
	}
	job, err := s.jobs.Create(ctx, domain.ExportJob{ID: id, TenantID: tenantID, RequestedBy: requestedBy, Filter: f})
	if err != nil {
		return domain.ExportJob{}, err
	}
	if s.log != nil {
		s.log.Info("export requested", map[string]any{"export_id": job.ID, "tenant_id": tenantID})
	}
	return job, nil
}

// ExportView is an AL-4 status read.
type ExportView struct {
	Job                  domain.ExportJob
	DownloadURL          string
	DownloadURLExpiresAt *time.Time
}

// Status implements AL-4. A ready job gets a freshly presigned short-lived
// URL on every poll (D-11); once past its retrieval window it is reported
// (and lazily marked) expired.
func (s *ExportService) Status(ctx context.Context, tenantID, id string) (ExportView, error) {
	if !domain.IsUUID(id) {
		return ExportView{}, domain.NewError(domain.ErrInvalidRequest, "id must be a UUID")
	}
	job, err := s.jobs.Get(ctx, tenantID, id)
	if errors.Is(err, port.ErrNotFound) {
		return ExportView{}, domain.NewError(domain.ErrExportNotFound, "export not found")
	}
	if err != nil {
		return ExportView{}, err
	}
	now := s.cfg.Now()
	switch {
	case job.Downloadable(now):
		ttl := s.cfg.DownloadURLTTL
		if left := job.SignedURLExpiresAt.Sub(now); left < ttl {
			ttl = left // never outlive the retrieval window
		}
		url, err := s.store.PresignExport(ctx, job.S3Key, ttl)
		if err != nil {
			return ExportView{}, err
		}
		exp := now.Add(ttl).UTC()
		return ExportView{Job: job, DownloadURL: url, DownloadURLExpiresAt: &exp}, nil
	case job.Lapsed(now):
		if err := s.jobs.MarkExpired(ctx, tenantID, id); err != nil && s.log != nil {
			s.log.Warn("mark export expired failed", map[string]any{"export_id": id, "error": err.Error()})
		}
		job.Status = domain.ExportExpired
	}
	return ExportView{Job: job}, nil
}

// Run polls for claimable jobs until ctx is cancelled.
func (s *ExportService) Run(ctx context.Context) {
	t := time.NewTicker(s.cfg.PollInterval)
	defer t.Stop()
	for {
		for {
			worked, err := s.ProcessNext(ctx)
			if err != nil && s.log != nil && ctx.Err() == nil {
				s.log.Error("export worker claim failed", map[string]any{"error": err.Error()})
			}
			if !worked || ctx.Err() != nil {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ProcessNext claims and runs one job; worked=false means none was
// claimable. A job's own failure is recorded on the job, not returned.
func (s *ExportService) ProcessNext(ctx context.Context) (bool, error) {
	job, err := s.jobs.Claim(ctx, s.cfg.Lease)
	if err != nil || job == nil {
		return false, err
	}
	hbCtx, stop := context.WithCancel(ctx)
	defer stop()
	go s.heartbeat(hbCtx, *job)

	key, rows, err := s.produce(ctx, *job)
	if err != nil {
		if s.log != nil {
			s.log.Error("export failed", map[string]any{"export_id": job.ID, "tenant_id": job.TenantID, "error": err.Error()})
		}
		// Use a fresh context so a shutdown still records the failure; the
		// lease would otherwise re-run the job after it lapses.
		failCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if ferr := s.jobs.Fail(failCtx, job.TenantID, job.ID, failureReason(err)); ferr != nil && s.log != nil {
			s.log.Error("record export failure", map[string]any{"export_id": job.ID, "error": ferr.Error()})
		}
		return true, nil
	}
	if err := s.jobs.Complete(ctx, job.TenantID, job.ID, key, rows, s.cfg.Now().Add(s.cfg.SignedURLTTL)); err != nil {
		return true, err
	}
	if s.log != nil {
		s.log.Info("export ready", map[string]any{"export_id": job.ID, "tenant_id": job.TenantID, "row_count": rows})
	}
	return true, nil
}

func (s *ExportService) heartbeat(ctx context.Context, job domain.ExportJob) {
	t := time.NewTicker(s.cfg.Lease / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.jobs.Heartbeat(ctx, job.TenantID, job.ID); err != nil && s.log != nil && ctx.Err() == nil {
				s.log.Warn("export heartbeat failed", map[string]any{"export_id": job.ID, "error": err.Error()})
			}
		}
	}
}

// failureReason is what is stored in audit_export_jobs.error: the §17 code
// for a classified failure, otherwise a generic marker (the detail is in
// the logs, never in a tenant-visible column).
func failureReason(err error) string {
	var de *domain.Error
	if errors.As(err, &de) {
		return string(de.Code)
	}
	return "internal_error"
}

// exportBatch is the RDS keyset page size while streaming an export.
const exportBatch = 1000

// produce streams the job's hot + archived rows into a gzipped JSONL temp
// file, uploads it, and returns its key and row count.
func (s *ExportService) produce(ctx context.Context, job domain.ExportJob) (key string, rows int64, err error) {
	f := job.Filter
	if f.From == nil || f.To == nil {
		return "", 0, errors.New("export filter is not clamped")
	}
	from, to := *f.From, *f.To

	tmp, err := os.CreateTemp(s.cfg.WorkDir, "audit-export-*.jsonl.gz")
	if err != nil {
		return "", 0, fmt.Errorf("create export temp file: %w", err)
	}
	defer func() {
		if cerr := errors.Join(tmp.Close(), os.Remove(tmp.Name())); cerr != nil && s.log != nil {
			s.log.Warn("export temp file cleanup failed", map[string]any{"export_id": job.ID, "error": cerr.Error()})
		}
	}()
	zw := gzip.NewWriter(tmp)
	enc := json.NewEncoder(zw)
	write := func(e domain.AuditEntry) error {
		rows++
		return enc.Encode(domain.ToRecord(e))
	}

	var cursor *domain.Cursor
	for {
		page, err := s.reader.Page(ctx, job.TenantID, f, from, to, cursor, exportBatch)
		if err != nil {
			return "", 0, err
		}
		for _, e := range page {
			if err := write(e); err != nil {
				return "", 0, err
			}
		}
		if len(page) < exportBatch {
			break
		}
		last := page[len(page)-1]
		cursor = &domain.Cursor{OccurredAt: last.OccurredAt, ID: last.ID}
	}

	objects, err := s.reader.ArchivedObjects(ctx, job.TenantID, from, to, f.RetentionTier)
	if err != nil {
		return "", 0, err
	}
	for _, o := range objects {
		err := s.archive.ReadArchive(ctx, o.Bucket, o.Key, func(r domain.ArchiveRecord) error {
			e := r.Entry()
			if e.OccurredAt.Before(from) || e.OccurredAt.After(to) || !f.Matches(e) {
				return nil
			}
			return write(e)
		})
		if errors.Is(err, port.ErrObjectMissing) {
			s.query.warnMissing(o, err)
			continue
		}
		if err != nil {
			return "", 0, err
		}
	}

	if err := zw.Close(); err != nil {
		return "", 0, fmt.Errorf("finish export gzip: %w", err)
	}
	size, err := tmp.Seek(0, 1)
	if err != nil {
		return "", 0, err
	}
	if _, err := tmp.Seek(0, 0); err != nil {
		return "", 0, err
	}
	key = domain.ExportKey(job.TenantID, job.ID)
	if err := s.store.PutExport(ctx, key, tmp, size); err != nil {
		return "", 0, err
	}
	return key, rows, nil
}
