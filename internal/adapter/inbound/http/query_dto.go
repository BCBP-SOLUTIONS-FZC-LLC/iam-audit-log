package http

import (
	"encoding/json"
	"net/url"
	"strconv"
	"time"

	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/domain"
	"github.com/BCBP-SOLUTIONS-FZC-LLC/iam-audit-log/internal/core/service"
)

// EventDTO is one AL-1/AL-2 audit entry (LLD §5.4 response shape).
type EventDTO struct {
	ID              string          `json:"id"`
	OccurredAt      time.Time       `json:"occurred_at"`
	EntryType       string          `json:"entry_type" example:"membership.department.granted"`
	Action          string          `json:"action" example:"granted"`
	Actor           ActorDTO        `json:"actor"`
	Target          *TargetDTO      `json:"target,omitempty"`
	SourceService   string          `json:"source_service" example:"iam-org-membership"`
	SourceEventType string          `json:"source_event_type"`
	IPAddress       string          `json:"ip_address,omitempty"`
	TraceID         string          `json:"trace_id,omitempty"`
	RetentionTier   string          `json:"retention_tier" example:"compliance_7y"`
	Metadata        json.RawMessage `json:"metadata" swaggertype:"object"`
}

// EventsResponse is the AL-1 200 body.
type EventsResponse struct {
	Events        []EventDTO `json:"events"`
	NextCursor    *string    `json:"next_cursor"`
	WindowClamped bool       `json:"window_clamped"`
	EffectiveFrom time.Time  `json:"effective_from"`
}

// ExportFilterRequest is the AL-3 body: the AL-1 filter as JSON.
type ExportFilterRequest struct {
	From          *time.Time `json:"from,omitempty"`
	To            *time.Time `json:"to,omitempty"`
	EntryTypes    []string   `json:"entry_type,omitempty"`
	ActorID       string     `json:"actor_id,omitempty"`
	ActorType     string     `json:"actor_type,omitempty"`
	TargetType    string     `json:"target_type,omitempty"`
	TargetID      string     `json:"target_id,omitempty"`
	SourceService string     `json:"source_service,omitempty"`
	RetentionTier string     `json:"retention_tier,omitempty"`
}

// ExportAcceptedResponse is the 202 body of AL-3 and of a deferred AL-1
// (decision D-10).
type ExportAcceptedResponse struct {
	ExportID  string `json:"export_id"`
	Status    string `json:"status" example:"pending"`
	StatusURL string `json:"status_url" example:"/api/v1/audit/exports/0192…"`
}

// ExportStatusResponse is the AL-4 body.
type ExportStatusResponse struct {
	ExportID             string     `json:"export_id"`
	Status               string     `json:"status" enums:"pending,running,ready,failed,expired"`
	CreatedAt            time.Time  `json:"created_at"`
	CompletedAt          *time.Time `json:"completed_at,omitempty"`
	RowCount             *int64     `json:"row_count,omitempty"`
	DownloadURL          string     `json:"download_url,omitempty"`
	DownloadURLExpiresAt *time.Time `json:"download_url_expires_at,omitempty"`
	ExpiresAt            *time.Time `json:"expires_at,omitempty"`
	Error                string     `json:"error,omitempty" example:"internal_error"`
}

func (r ExportFilterRequest) filter() domain.QueryFilter {
	return domain.QueryFilter{
		From: r.From, To: r.To, EntryTypes: r.EntryTypes, ActorID: r.ActorID, ActorType: r.ActorType,
		TargetType: r.TargetType, TargetID: r.TargetID, SourceService: r.SourceService, RetentionTier: r.RetentionTier,
	}
}

// parseQuery reads the AL-1 query string (§5.4).
func parseQuery(q url.Values) (f domain.QueryFilter, cursor string, limit int, err error) {
	f = domain.QueryFilter{
		EntryTypes: q["entry_type"], ActorID: q.Get("actor_id"), ActorType: q.Get("actor_type"),
		TargetType: q.Get("target_type"), TargetID: q.Get("target_id"),
		SourceService: q.Get("source_service"), RetentionTier: q.Get("retention_tier"),
	}
	for name, dst := range map[string]**time.Time{"from": &f.From, "to": &f.To} {
		if v := q.Get(name); v != "" {
			t, perr := time.Parse(time.RFC3339, v)
			if perr != nil {
				return f, "", 0, domain.NewError(domain.ErrInvalidRequest, name+" must be an RFC 3339 timestamp")
			}
			t = t.UTC()
			*dst = &t
		}
	}
	if v := q.Get("limit"); v != "" {
		n, aerr := strconv.Atoi(v)
		if aerr != nil {
			return f, "", 0, domain.NewError(domain.ErrInvalidRequest, "limit must be an integer")
		}
		limit = n
		if n == 0 {
			limit = -1 // explicit 0 is out of range, not "default"
		}
	}
	return f, q.Get("cursor"), limit, nil
}

func toEventDTO(e domain.AuditEntry) EventDTO {
	out := EventDTO{
		ID: e.ID, OccurredAt: e.OccurredAt.UTC(), EntryType: e.EntryType, Action: e.Action,
		Actor:         ActorDTO{Type: string(e.Actor.Type), ID: e.Actor.ID, Display: e.Actor.Display},
		SourceService: e.SourceService, SourceEventType: e.SourceEventType,
		IPAddress: e.IPAddress, TraceID: e.TraceID, RetentionTier: string(e.RetentionTier), Metadata: e.Metadata,
	}
	if e.Target != nil {
		out.Target = &TargetDTO{Type: e.Target.Type, ID: e.Target.ID}
	}
	if len(out.Metadata) == 0 {
		out.Metadata = json.RawMessage(`{}`)
	}
	return out
}

func toEventsResponse(r service.QueryResult) EventsResponse {
	out := EventsResponse{Events: make([]EventDTO, len(r.Events)), WindowClamped: r.WindowClamped, EffectiveFrom: r.EffectiveFrom.UTC()}
	for i, e := range r.Events {
		out.Events[i] = toEventDTO(e)
	}
	if r.NextCursor != "" {
		c := r.NextCursor
		out.NextCursor = &c
	}
	return out
}

func toExportAccepted(j domain.ExportJob) ExportAcceptedResponse {
	return ExportAcceptedResponse{ExportID: j.ID, Status: string(j.Status), StatusURL: "/api/v1/audit/exports/" + j.ID}
}

func toExportStatus(v service.ExportView) ExportStatusResponse {
	j := v.Job
	out := ExportStatusResponse{
		ExportID: j.ID, Status: string(j.Status), CreatedAt: j.CreatedAt.UTC(), CompletedAt: j.CompletedAt,
		RowCount: j.RowCount, DownloadURL: v.DownloadURL, DownloadURLExpiresAt: v.DownloadURLExpiresAt,
		ExpiresAt: j.SignedURLExpiresAt,
	}
	if j.Status == domain.ExportFailed {
		out.Error = j.Error
	}
	return out
}
