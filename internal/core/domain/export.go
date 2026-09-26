package domain

import "time"

// ExportStatus mirrors the audit_export_status enum (LLD §4.1).
type ExportStatus string

// ExportStatus values.
const (
	ExportPending ExportStatus = "pending"
	ExportRunning ExportStatus = "running"
	ExportReady   ExportStatus = "ready"
	ExportFailed  ExportStatus = "failed"
	ExportExpired ExportStatus = "expired"
)

// ExportJob is one audit_export_jobs row (LLD §4.2, §5.4 AL-3/AL-4). Filter
// is stored already clamped to the plan window at request time, so the
// worker never widens it (AL-INV-8).
type ExportJob struct {
	ID                 string
	TenantID           string
	RequestedBy        string
	Filter             QueryFilter
	Status             ExportStatus
	S3Key              string
	SignedURLExpiresAt *time.Time
	RowCount           *int64
	CreatedAt          time.Time
	CompletedAt        *time.Time
	Error              string
}

// Downloadable reports whether a ready job's object may still be handed out
// (decision D-11: retrievable until signed_url_expires_at).
func (j ExportJob) Downloadable(now time.Time) bool {
	return j.Status == ExportReady && j.S3Key != "" && j.SignedURLExpiresAt != nil && now.Before(*j.SignedURLExpiresAt)
}

// Lapsed reports whether a ready job has passed its retrieval window and
// should be reported (and lazily marked) expired.
func (j ExportJob) Lapsed(now time.Time) bool {
	return j.Status == ExportReady && j.SignedURLExpiresAt != nil && !now.Before(*j.SignedURLExpiresAt)
}
