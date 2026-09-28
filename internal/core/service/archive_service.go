package service

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"strings"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/port"
)

// ArchiveConfig tunes ArchiveService.
type ArchiveConfig struct {
	HotWindowDays          int    // AUDIT_HOT_WINDOW_DAYS
	WritableTrailingMonths int    // AUDIT_WRITABLE_TRAILING_MONTHS
	PartMaxRows            int    // ARCHIVE_PART_MAX_ROWS
	WorkDir                string // ARCHIVE_WORK_DIR ("" → os.TempDir())
	Now                    port.Clock
}

// ArchiveService is the reconciler's archival pass (LLD §8.5, §15.4): it
// re-opens dropped months that received late rows (D-19). For every month
// past the hot window it then archives each retained tier to per-tenant
// S3 objects, verifies them, and drops the partition only through the
// AL-INV-9 gate in audit_drop_partition(). A partition touched by a pending
// redaction task is skipped (AL-INV-12).
type ArchiveService struct {
	repo    port.ArchiveRepository
	store   port.ArchiveStore
	metrics port.ArchiveMetrics
	cfg     ArchiveConfig
	log     port.Logger
}

// NewArchiveService wires the service; metrics and log may be nil.
func NewArchiveService(repo port.ArchiveRepository, store port.ArchiveStore, metrics port.ArchiveMetrics, cfg ArchiveConfig, log port.Logger) *ArchiveService {
	if cfg.PartMaxRows <= 0 {
		cfg.PartMaxRows = 50000
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &ArchiveService{repo: repo, store: store, metrics: metrics, cfg: cfg, log: log}
}

// ArchiveSummary counts one run's partition outcomes.
type ArchiveSummary struct {
	Reopened int
	Dropped  int
	Blocked  int // pending redaction (AL-INV-12)
	Stalled  int // a tier failed, or late writes kept arriving (AL-INV-9 held)
	Skipped  int // not yet eligible
}

// Run is one archival pass. Per-partition failures are recorded on the
// partition and counted, not returned; an error means the pass itself
// could not proceed.
func (s *ArchiveService) Run(ctx context.Context) (sum ArchiveSummary, err error) {
	defer func() {
		if s.metrics != nil { // also on an early return, so the gauge never goes stale
			s.metrics.Stalled(sum.Stalled)
		}
	}()
	reopen, err := s.repo.ReopenCandidates(ctx)
	if err != nil {
		return sum, fmt.Errorf("list re-open candidates: %w", err)
	}
	for _, p := range reopen {
		n, err := s.repo.Reopen(ctx, p)
		if err != nil {
			return sum, fmt.Errorf("re-open %s: %w", p, err)
		}
		sum.Reopened++
		s.warn("dropped month re-opened for late rows (AL-D4, D-19)", map[string]any{"partition": p, "rows": n})
	}

	parts, err := s.repo.Partitions(ctx)
	if err != nil {
		return sum, fmt.Errorf("list partitions: %w", err)
	}
	now := s.cfg.Now()
	for _, p := range parts {
		if ctx.Err() != nil {
			return sum, ctx.Err()
		}
		month, ok := domain.ParsePartitionName(p)
		if !ok || !domain.ArchiveEligible(month, now, s.cfg.HotWindowDays, s.cfg.WritableTrailingMonths) {
			sum.Skipped++
			continue
		}
		switch s.partition(ctx, p, month) {
		case domain.DropDropped:
			sum.Dropped++
		case domain.DropRedactionPending:
			sum.Blocked++
		case domain.DropMissing: // gone meanwhile (a concurrent drop) — nothing left to do
			sum.Skipped++
		default:
			sum.Stalled++
		}
	}
	return sum, nil
}

// partition archives, verifies and drops one month, returning the final
// drop result (or a non-drop outcome). A count mismatch at drop time — rows
// that arrived after archival — re-opens the month and re-archives it once
// in the same run (AL-D4).
func (s *ArchiveService) partition(ctx context.Context, p string, month time.Time) string {
	fields := map[string]any{"partition": p}
	for attempt := 0; attempt < 2; attempt++ {
		pending, err := s.repo.PendingRedaction(ctx, p)
		if err != nil {
			s.errorf("redaction check failed", fields, err)
			return domain.DropNotVerified
		}
		if pending {
			s.blocked(p)
			return domain.DropRedactionPending
		}
		if !s.archiveTiers(ctx, p, month) {
			return domain.DropNotVerified
		}
		if err := s.repo.MarkAccessExpiring(ctx, p, month); err != nil {
			s.errorf("record access_90d expiry failed", fields, err)
			return domain.DropNotVerified
		}
		res, err := s.repo.Drop(ctx, p)
		if err != nil {
			s.errorf("drop failed", fields, err)
			return domain.DropNotVerified
		}
		switch res {
		case domain.DropDropped:
			for _, t := range domain.RetainedTiers {
				s.pruned(t)
			}
			s.pruned(domain.TierAccess90d)
			s.info("partition archived, verified and dropped (AL-INV-9)", fields)
			return res
		case domain.DropCountMismatch:
			s.warn("late rows since archival — re-archiving (AL-D4)", fields)
			if err := s.repo.ResetForRearchive(ctx, p); err != nil {
				s.errorf("reset for re-archive failed", fields, err)
				return domain.DropNotVerified
			}
		case domain.DropRedactionPending:
			s.blocked(p)
			return res
		default: // not_verified (a concurrent redaction invalidated a tier) or missing
			s.warn("drop refused: "+res, fields)
			return res
		}
	}
	s.warn("late rows kept arriving; drop deferred to the next run (AL-INV-9 held)", fields)
	return domain.DropCountMismatch
}

// archiveTiers brings every retained tier to verified; false if any failed.
func (s *ArchiveService) archiveTiers(ctx context.Context, p string, month time.Time) bool {
	states, err := s.repo.States(ctx, p)
	if err != nil {
		s.errorf("read archive state failed", map[string]any{"partition": p}, err)
		return false
	}
	ok := true
	for _, tier := range domain.RetainedTiers {
		if states[tier].Status == domain.ArchiveVerified {
			continue
		}
		fields := map[string]any{"partition": p, "tier": string(tier)}
		if err := s.archiveTier(ctx, p, month, tier); err != nil {
			s.fail(ctx, p, tier, "archive", err, fields)
			ok = false
			continue
		}
		if err := s.verifyTier(ctx, p, tier); err != nil {
			s.fail(ctx, p, tier, "verify", err, fields)
			ok = false
			continue
		}
		if err := s.repo.MarkVerified(ctx, p, tier); err != nil {
			s.fail(ctx, p, tier, "mark verified", err, fields)
			ok = false
			continue
		}
		s.archived(tier, "verified")
	}
	return ok
}

func (s *ArchiveService) fail(ctx context.Context, p string, tier domain.RetentionTier, step string, err error, fields map[string]any) {
	s.archived(tier, "failed")
	s.errorf(step+" failed — partition kept (AL-INV-9)", fields, err)
	// Record on a fresh context so a cancelled run still leaves the reason.
	mctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if merr := s.repo.MarkFailed(mctx, p, tier, step+": "+err.Error()); merr != nil {
		s.errorf("record archive failure failed", fields, merr)
	}
}

// archiveTier streams the tier's rows into per-tenant gzipped JSONL parts
// (D-10 keys), uploads each with SSE-KMS and Object Lock, and records the
// manifest. A re-archive rewrites the unsealed parts under the same keys
// as new object versions (D-20); sealed parts of a re-opened month are
// never touched, and new parts are numbered after them.
func (s *ArchiveService) archiveTier(ctx context.Context, p string, month time.Time, tier domain.RetentionTier) error {
	if err := s.repo.MarkArchiving(ctx, p, tier, month); err != nil {
		return err
	}
	next, err := s.repo.NextParts(ctx, p, tier)
	if err != nil {
		return err
	}
	if next == nil {
		next = map[string]int{}
	}
	var (
		cur     *partWriter
		written []domain.ArchiveObject
		rows    int64
	)
	flush := func() error {
		if cur == nil {
			return nil
		}
		o, err := s.upload(ctx, cur)
		cur = nil
		if err != nil {
			return err
		}
		written = append(written, o)
		return s.repo.UpsertObject(ctx, p, o)
	}
	err = s.repo.StreamTier(ctx, p, tier, func(e domain.AuditEntry) error {
		if cur != nil && (cur.tenant != e.TenantID || cur.rows >= s.cfg.PartMaxRows) {
			if err := flush(); err != nil {
				return err
			}
		}
		if cur == nil {
			w, err := newPartWriter(s.cfg.WorkDir, tier, e.TenantID, month, next[e.TenantID])
			if err != nil {
				return err
			}
			next[e.TenantID]++
			cur = w
		}
		rows++
		return cur.add(e)
	})
	if err == nil {
		err = flush()
	}
	if cur != nil {
		cur.discard()
	}
	if err != nil {
		return err
	}
	keep := make([]string, len(written))
	for i, o := range written {
		keep[i] = o.Key
	}
	return s.repo.FinishArchive(ctx, p, tier, keep, rows, domain.ManifestSHA256(written), string(tier)+"/")
}

// upload closes a part and puts it in S3.
func (s *ArchiveService) upload(ctx context.Context, w *partWriter) (domain.ArchiveObject, error) {
	defer w.discard()
	o, size, err := w.finish(s.store.Bucket())
	if err != nil {
		return o, err
	}
	if err := s.store.PutArchive(ctx, o.Key, w.file, size, domain.RetainUntil(o.Tier, o.MaxOccurredAt)); err != nil {
		return o, err
	}
	return o, nil
}

// verifyTier re-reads every unsealed object: its body SHA-256 must match
// the manifest row, its Object Lock must cover the tier, and the
// recomputed manifest checksum must match the recorded one (§15.4).
func (s *ArchiveService) verifyTier(ctx context.Context, p string, tier domain.RetentionTier) error {
	states, err := s.repo.States(ctx, p)
	if err != nil {
		return err
	}
	objects, err := s.repo.UnsealedObjects(ctx, p, tier)
	if err != nil {
		return err
	}
	for _, o := range objects {
		sum, err := s.store.ArchiveChecksum(ctx, o.Key, domain.RetainUntil(tier, o.MaxOccurredAt))
		if err != nil {
			return fmt.Errorf("verify %s: %w", o.Key, err)
		}
		if sum != o.SHA256 {
			return fmt.Errorf("verify %s: checksum %s != manifest %s", o.Key, sum, o.SHA256)
		}
	}
	if got, want := domain.ManifestSHA256(objects), states[tier].SHA256Manifest; got != want {
		return fmt.Errorf("manifest checksum %s != recorded %s", got, want)
	}
	return nil
}

func (s *ArchiveService) blocked(p string) {
	if s.metrics != nil {
		s.metrics.RedactionBlocked()
	}
	s.warn("archival refused: a pending redaction task touches this partition (AL-INV-12, RB-7)", map[string]any{"partition": p})
}

func (s *ArchiveService) archived(tier domain.RetentionTier, result string) {
	if s.metrics != nil {
		s.metrics.PartitionArchived(tier, result)
	}
}

func (s *ArchiveService) pruned(tier domain.RetentionTier) {
	if s.metrics != nil {
		s.metrics.Pruned(tier)
	}
}

func (s *ArchiveService) info(msg string, f map[string]any) {
	if s.log != nil {
		s.log.Info(msg, f)
	}
}

func (s *ArchiveService) warn(msg string, f map[string]any) {
	if s.log != nil {
		s.log.Warn(msg, f)
	}
}

func (s *ArchiveService) errorf(msg string, f map[string]any, err error) {
	if s.log != nil {
		out := map[string]any{"error": err.Error()}
		for k, v := range f {
			out[k] = v
		}
		s.log.Error(msg, out)
	}
}

// partWriter assembles one archive object in a temp file: gzipped JSONL,
// hashing the compressed body and counting uncompressed bytes as it goes.
type partWriter struct {
	tier    domain.RetentionTier
	tenant  string
	month   time.Time
	part    int
	file    *os.File
	sha     hash.Hash
	zw      *gzip.Writer
	enc     *json.Encoder
	raw     *countingWriter
	rows    int
	entries []domain.AuditEntry // id/time/subject bounds only
	minAt   time.Time
	maxAt   time.Time
	minID   string
	maxID   string
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.n += int64(n)
	return n, err
}

func newPartWriter(dir string, tier domain.RetentionTier, tenant string, month time.Time, part int) (*partWriter, error) {
	f, err := os.CreateTemp(dir, "audit-archive-*.jsonl.gz")
	if err != nil {
		return nil, fmt.Errorf("create archive temp file: %w", err)
	}
	h := sha256.New()
	zw := gzip.NewWriter(io.MultiWriter(f, h))
	raw := &countingWriter{w: zw}
	return &partWriter{tier: tier, tenant: tenant, month: month, part: part, file: f, sha: h, zw: zw, enc: json.NewEncoder(raw), raw: raw}, nil
}

func (w *partWriter) add(e domain.AuditEntry) error {
	if err := w.enc.Encode(domain.ToRecord(e)); err != nil {
		return fmt.Errorf("encode archive record: %w", err)
	}
	w.rows++
	if w.minAt.IsZero() || e.OccurredAt.Before(w.minAt) {
		w.minAt = e.OccurredAt
	}
	if e.OccurredAt.After(w.maxAt) {
		w.maxAt = e.OccurredAt
	}
	id := strings.ToLower(e.ID)
	if w.minID == "" || id < w.minID {
		w.minID = id
	}
	if id > w.maxID {
		w.maxID = id
	}
	w.entries = append(w.entries, domain.AuditEntry{Actor: e.Actor, Target: e.Target})
	return nil
}

// finish closes the gzip stream and rewinds the file for upload.
func (w *partWriter) finish(bucket string) (domain.ArchiveObject, int64, error) {
	if err := w.zw.Close(); err != nil {
		return domain.ArchiveObject{}, 0, fmt.Errorf("finish archive gzip: %w", err)
	}
	size, err := w.file.Seek(0, io.SeekCurrent)
	if err != nil {
		return domain.ArchiveObject{}, 0, err
	}
	if _, err := w.file.Seek(0, io.SeekStart); err != nil {
		return domain.ArchiveObject{}, 0, err
	}
	return domain.ArchiveObject{
		Bucket: bucket, Key: domain.ArchiveKey(w.tier, w.tenant, w.month, w.part), TenantID: w.tenant, Tier: w.tier,
		PeriodMonth: w.month, Part: w.part, RowCount: int64(w.rows), ByteSize: w.raw.n,
		MinOccurredAt: w.minAt, MaxOccurredAt: w.maxAt, MinID: w.minID, MaxID: w.maxID,
		SHA256: hex.EncodeToString(w.sha.Sum(nil)), SubjectIDs: domain.SubjectIDs(w.entries),
	}, size, nil
}

// discard removes the temp file (idempotent).
func (w *partWriter) discard() {
	if w.file == nil {
		return
	}
	_ = w.file.Close()           //nolint:errcheck // temp file; the upload already read it or the run failed
	_ = os.Remove(w.file.Name()) //nolint:errcheck // best-effort cleanup of our own temp file
	w.file = nil
}
