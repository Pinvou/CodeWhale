package account

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type AdminUser struct {
	ID          uuid.UUID  `json:"user_id"`
	UserCode    string     `json:"user_code"`
	Username    *string    `json:"username,omitempty"`
	DisplayName *string    `json:"display_name,omitempty"`
	Email       *string    `json:"email,omitempty"`
	Status      string     `json:"status"`
	LastSeenAt  *time.Time `json:"last_seen_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

type AdminUserFilter struct {
	Search          string
	Limit           int
	CursorUpdatedAt *time.Time
	CursorID        *uuid.UUID
}

type AdminUserPage struct {
	Items      []AdminUser `json:"items"`
	NextCursor *string     `json:"next_cursor"`
}

type adminRepository interface {
	AdminUsers(context.Context, AdminUserFilter) (AdminUserPage, error)
	AdminSetUserStatus(context.Context, identity.Principal, uuid.UUID, string) error
}

func (s *Service) requireAdminRepository(w http.ResponseWriter, r *http.Request) (adminRepository, bool) {
	repository, ok := s.repository.(adminRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "account administration is unavailable", httpserver.RequestID(r.Context()))
	}
	return repository, ok
}

func adminLimit(r *http.Request) int {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		return 50
	}
	if limit > 200 {
		return 200
	}
	return limit
}

func parseAdminUserFilter(r *http.Request) (AdminUserFilter, error) {
	filter := AdminUserFilter{Search: strings.TrimSpace(r.URL.Query().Get("q")), Limit: adminLimit(r)}
	cursor := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if cursor == "" {
		return filter, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return AdminUserFilter{}, err
	}
	var value struct {
		UpdatedAt time.Time `json:"updated_at"`
		ID        uuid.UUID `json:"user_id"`
	}
	if err := json.Unmarshal(raw, &value); err != nil || value.UpdatedAt.IsZero() || value.ID == uuid.Nil {
		return AdminUserFilter{}, ErrConflict
	}
	value.UpdatedAt = value.UpdatedAt.UTC()
	filter.CursorUpdatedAt, filter.CursorID = &value.UpdatedAt, &value.ID
	return filter, nil
}

func encodeAdminUserCursor(item AdminUser) (string, error) {
	body, err := json.Marshal(map[string]any{"updated_at": item.UpdatedAt.UTC(), "user_id": item.ID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(body), nil
}

func (s *Service) adminUsers(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	filter, err := parseAdminUserFilter(r)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_USER_FILTER", "user filters are invalid", httpserver.RequestID(r.Context()))
		return
	}
	page, err := repository.AdminUsers(r.Context(), filter)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, page)
}

func (s *Service) adminSetUserStatus(w http.ResponseWriter, r *http.Request) {
	s.adminSetStatus(w, r, "user")
}

func (s *Service) adminSetStatus(w http.ResponseWriter, r *http.Request, resource string) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	pathName := resource + "_id"
	resourceID, ok := pathUUID(w, r, pathName)
	if !ok {
		return
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid user status request", httpserver.RequestID(r.Context()))
		return
	}
	body.Status = strings.ToUpper(strings.TrimSpace(body.Status))
	if body.Status != "ACTIVE" && body.Status != "DISABLED" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "status must be ACTIVE or DISABLED", httpserver.RequestID(r.Context()))
		return
	}
	principal := mustPrincipal(r.Context())
	var err error
	err = repository.AdminSetUserStatus(r.Context(), principal, resourceID, body.Status)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (r *PostgresRepository) AdminUsers(ctx context.Context, filter AdminUserFilter) (AdminUserPage, error) {
	search := strings.ToLower(strings.TrimSpace(filter.Search))
	pattern := "%" + search + "%"
	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.pool.Query(ctx, `
SELECT u.id,u.user_code,u.username,u.display_name,u.email,u.status,
       (SELECT max(s.last_seen_at) FROM user_sessions s WHERE s.user_id=u.id),
       u.created_at,u.updated_at
FROM users u
WHERE u.status<>'DELETED' AND ($1='' OR lower(COALESCE(u.username,'')) LIKE $2 OR lower(COALESCE(u.display_name,'')) LIKE $2 OR u.id::text=$1)
	AND ($3::timestamptz IS NULL OR u.updated_at<$3 OR (u.updated_at=$3 AND u.id<$4::uuid))
ORDER BY u.updated_at DESC,u.id DESC LIMIT $5`, search, pattern, filter.CursorUpdatedAt, filter.CursorID, limit+1)
	if err != nil {
		return AdminUserPage{}, err
	}
	defer rows.Close()
	items := make([]AdminUser, 0, limit+1)
	for rows.Next() {
		var item AdminUser
		if err := rows.Scan(&item.ID, &item.UserCode, &item.Username, &item.DisplayName, &item.Email, &item.Status, &item.LastSeenAt, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return AdminUserPage{}, err
		}
		item.UserCode = FormatUserCode(item.UserCode)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return AdminUserPage{}, err
	}
	page := AdminUserPage{Items: items}
	if len(items) > limit {
		cursor, err := encodeAdminUserCursor(items[limit-1])
		if err != nil {
			return AdminUserPage{}, err
		}
		page.Items = items[:limit]
		page.NextCursor = &cursor
	}
	return page, nil
}

const adminAuditSelect = `SELECT log.id,log.actor_user_id,actor.display_name,actor.email,log.active_organization_id,log.action,log.resource_type,log.resource_id,log.result,log.request_id,log.change_summary,log.occurred_at FROM audit_logs log LEFT JOIN users actor ON actor.id=log.actor_user_id`

func (r *PostgresRepository) AdminSetUserStatus(ctx context.Context, principal identity.Principal, userID uuid.UUID, status string) error {
	if status != "ACTIVE" && status != "DISABLED" {
		return ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actorID, err := adminActorID(ctx, tx, principal)
	if err != nil {
		return err
	}
	if actorID != nil && *actorID == userID {
		return ErrConflict
	}
	var currentStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&currentStatus)
	if errors.Is(err, pgx.ErrNoRows) || currentStatus == "DELETED" {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if currentStatus == status {
		return tx.Commit(ctx)
	}
	if (status == "ACTIVE" && currentStatus != "DISABLED") || (status == "DISABLED" && currentStatus != "ACTIVE" && currentStatus != "PROVISIONING") {
		return ErrConflict
	}
	if status == "DISABLED" {
		if err := assertNotLastActiveSuperAdmin(ctx, tx, userID); err != nil {
			return err
		}
	} else if err := assertPersonalActivationReady(ctx, tx, userID); err != nil {
		return err
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `UPDATE users SET status=$2,version=version+1,membership_version=membership_version+1,updated_at=$3 WHERE id=$1`, userID, status, now); err != nil {
		return err
	}
	if status == "DISABLED" {
		if _, err := tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id=$1`, userID); err != nil {
			return err
		}
	}
	if err := appendAdminAudit(ctx, tx, actorID, principal, nil, "admin.user.status.update", "user", userID.String(), map[string]any{"from": currentStatus, "to": status}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type activationReadinessQuery interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func assertPersonalActivationReady(ctx context.Context, query activationReadinessQuery, userID uuid.UUID) error {
	var knownIdentity, walletConfirmed bool
	// The caller holds the user row in a serializable transaction. Reading the
	// wallet job without another row lock avoids reversing the worker's lock
	// order; concurrent completion is either observed or retried after conflict.
	err := query.QueryRow(ctx, `
SELECT
 EXISTS(SELECT 1 FROM identities WHERE user_id=$1 AND btrim(issuer)<>'' AND btrim(subject)<>''),
 EXISTS(SELECT 1 FROM wallet_provisioning_jobs WHERE user_id=$1 AND status='COMPLETED' AND completed_at IS NOT NULL)`, userID).Scan(&knownIdentity, &walletConfirmed)
	if err != nil {
		return err
	}
	if !knownIdentity || !walletConfirmed {
		return ErrAccountProvisioning
	}
	return nil
}

func adminActorID(ctx context.Context, tx pgx.Tx, principal identity.Principal) (*uuid.UUID, error) {
	var actorID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT user_id FROM identities WHERE issuer=$1 AND subject=$2`, principal.Issuer, principal.Subject).Scan(&actorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &actorID, nil
}

func appendAdminAudit(ctx context.Context, tx pgx.Tx, actorID *uuid.UUID, principal identity.Principal, organizationID *uuid.UUID, action, resourceType, resourceID string, summary map[string]any) error {
	return appendAdminAuditResult(ctx, tx, actorID, principal, organizationID, action, resourceType, resourceID, "SUCCEEDED", summary)
}

func appendAdminAuditResult(ctx context.Context, tx pgx.Tx, actorID *uuid.UUID, principal identity.Principal, organizationID *uuid.UUID, action, resourceType, resourceID, result string, summary map[string]any) error {
	if summary == nil {
		summary = map[string]any{}
	}
	summary["actor_issuer"] = principal.Issuer
	summary["actor_subject"] = principal.Subject
	body, err := json.Marshal(summary)
	if err != nil {
		return err
	}
	requestID := httpserver.RequestID(ctx)
	if requestID == "" {
		requestID = "internal"
	}
	_, err = tx.Exec(ctx, `
INSERT INTO audit_logs(id,actor_user_id,session_id,active_organization_id,action,resource_type,resource_id,result,request_id,trace_id,change_summary,occurred_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$9,$10,now())`, uuid.Must(uuid.NewV7()), actorID, principal.SessionID, organizationID, action, resourceType, resourceID, result, requestID, body)
	return err
}
