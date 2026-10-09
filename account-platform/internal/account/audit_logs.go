package account

import (
	"context"

	"encoding/json"
	"errors"

	"net/http"
	"slices"

	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type AdminAuditLog struct {
	ID                   uuid.UUID       `json:"audit_id"`
	ActorUserID          *uuid.UUID      `json:"actor_user_id,omitempty"`
	ActorDisplayName     *string         `json:"actor_display_name,omitempty"`
	ActorEmail           *string         `json:"actor_email,omitempty"`
	ActiveOrganizationID *uuid.UUID      `json:"active_organization_id,omitempty"`
	Action               string          `json:"action"`
	ResourceType         string          `json:"resource_type"`
	ResourceID           string          `json:"resource_id"`
	Result               string          `json:"result"`
	RequestID            string          `json:"request_id"`
	ChangeSummary        json.RawMessage `json:"change_summary"`
	OccurredAt           time.Time       `json:"occurred_at"`
}

type AdminAuditFilter struct {
	Actor    string
	Action   string
	Resource string
	Result   string
	From     *time.Time
	To       *time.Time
	Limit    int
}

func (s *Service) adminAuditLogs(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAuditRepository(w, r)
	if !ok {
		return
	}
	filter, err := parseAdminAuditFilter(r)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "audit filters are invalid", httpserver.RequestID(r.Context()))
		return
	}
	items, err := repository.AdminAuditLogs(r.Context(), filter)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminAuditLog(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAuditRepository(w, r)
	if !ok {
		return
	}
	auditID, ok := pathUUID(w, r, "audit_id")
	if !ok {
		return
	}
	item, err := repository.AdminAuditLog(r.Context(), auditID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
}

func parseAdminAuditFilter(r *http.Request) (AdminAuditFilter, error) {
	query := r.URL.Query()
	filter := AdminAuditFilter{Actor: strings.TrimSpace(query.Get("actor")), Action: strings.TrimSpace(query.Get("action")), Resource: strings.TrimSpace(query.Get("resource")), Result: strings.ToUpper(strings.TrimSpace(query.Get("result"))), Limit: adminLimit(r)}
	if filter.Result != "" && !slices.Contains([]string{"SUCCEEDED", "DENIED", "FAILED"}, filter.Result) {
		return AdminAuditFilter{}, ErrConflict
	}
	if len(filter.Actor) > 320 || len(filter.Action) > 160 || len(filter.Resource) > 320 {
		return AdminAuditFilter{}, ErrConflict
	}
	for raw, destination := range map[string]**time.Time{"from": &filter.From, "to": &filter.To} {
		if value := strings.TrimSpace(query.Get(raw)); value != "" {
			parsed, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return AdminAuditFilter{}, err
			}
			*destination = &parsed
		}
	}
	if filter.From != nil && filter.To != nil && filter.From.After(*filter.To) {
		return AdminAuditFilter{}, ErrConflict
	}
	return filter, nil
}

func (r *PostgresRepository) AdminAuditLogs(ctx context.Context, filter AdminAuditFilter) ([]AdminAuditLog, error) {
	rows, err := r.pool.Query(ctx, adminAuditSelect+`
WHERE ($1='' OR COALESCE(log.actor_user_id::text,'') ILIKE '%'||$1||'%' OR COALESCE(actor.display_name,'') ILIKE '%'||$1||'%' OR COALESCE(actor.email,'') ILIKE '%'||$1||'%' OR COALESCE(log.change_summary->>'actor_subject','') ILIKE '%'||$1||'%')
  AND ($2='' OR log.action ILIKE '%'||$2||'%')
  AND ($3='' OR log.resource_type ILIKE '%'||$3||'%' OR log.resource_id ILIKE '%'||$3||'%')
  AND ($4='' OR log.result=$4)
  AND ($5::timestamptz IS NULL OR log.occurred_at >= $5)
  AND ($6::timestamptz IS NULL OR log.occurred_at <= $6)
ORDER BY log.occurred_at DESC,log.id LIMIT $7`, filter.Actor, filter.Action, filter.Resource, filter.Result, filter.From, filter.To, filter.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminAuditLog, 0)
	for rows.Next() {
		var item AdminAuditLog
		if err := rows.Scan(&item.ID, &item.ActorUserID, &item.ActorDisplayName, &item.ActorEmail, &item.ActiveOrganizationID, &item.Action, &item.ResourceType, &item.ResourceID, &item.Result, &item.RequestID, &item.ChangeSummary, &item.OccurredAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminAuditLog(ctx context.Context, auditID uuid.UUID) (AdminAuditLog, error) {
	var item AdminAuditLog
	err := r.pool.QueryRow(ctx, adminAuditSelect+` WHERE log.id=$1`, auditID).Scan(&item.ID, &item.ActorUserID, &item.ActorDisplayName, &item.ActorEmail, &item.ActiveOrganizationID, &item.Action, &item.ResourceType, &item.ResourceID, &item.Result, &item.RequestID, &item.ChangeSummary, &item.OccurredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminAuditLog{}, ErrNotFound
	}
	return item, err
}

type auditRepository interface {
	AdminAuditLogs(context.Context, AdminAuditFilter) ([]AdminAuditLog, error)
	AdminAuditLog(context.Context, uuid.UUID) (AdminAuditLog, error)
}

func (s *Service) requireAuditRepository(w http.ResponseWriter, r *http.Request) (auditRepository, bool) {
	repository, ok := s.repository.(auditRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "audit administration is unavailable", httpserver.RequestID(r.Context()))
	}
	return repository, ok
}
