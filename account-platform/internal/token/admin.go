package token

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

type AdminOverview struct {
	TokenAccounts       int64 `json:"token_accounts"`
	ActiveAccounts      int64 `json:"active_accounts"`
	ActiveCredentials   int64 `json:"active_credentials"`
	HealthyRoutes       int64 `json:"healthy_routes"`
	UsageRequests       int64 `json:"usage_requests"`
	ReviewUsageRequests int64 `json:"review_usage_requests"`
}

type AdminTokenAccount struct {
	ID                    uuid.UUID  `json:"token_account_id"`
	OwnerType             string     `json:"owner_type"`
	OwnerID               uuid.UUID  `json:"owner_id"`
	OwnerOrganization     *uuid.UUID `json:"owner_organization_id,omitempty"`
	WalletID              uuid.UUID  `json:"wallet_id"`
	Name                  string     `json:"name"`
	Status                string     `json:"status"`
	ShareScope            string     `json:"share_scope"`
	ActiveCredentialCount int64      `json:"active_credential_count"`
	RouteCount            int64      `json:"route_count"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

type adminRepository interface {
	AdminOverview(context.Context) (AdminOverview, error)
	AdminTokenAccounts(context.Context, int) ([]AdminTokenAccount, error)
	AdminUsageRequests(context.Context, int) ([]UsageRequest, error)
}

type adminUsageRequest struct {
	UsageRequestID      uuid.UUID `json:"usage_request_id"`
	Model               string    `json:"model"`
	Status              string    `json:"status"`
	MaximumMicrocredits int64     `json:"maximum_microcredits,string"`
	ActualMicrocredits  *int64    `json:"actual_microcredits,string"`
	ErrorClass          *string   `json:"error_class"`
	StartedAt           time.Time `json:"started_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

func tokenAdminLimit(r *http.Request) int {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		return 50
	}
	if limit > 200 {
		return 200
	}
	return limit
}

func (s *Service) requireAdminRepository(w http.ResponseWriter, r *http.Request) (adminRepository, bool) {
	repository, ok := s.repository.(adminRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "token administration is unavailable", httpserver.RequestID(r.Context()))
	}
	return repository, ok
}

func (s *Service) adminOverview(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	result, err := repository.AdminOverview(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminTokenAccounts(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	items, err := repository.AdminTokenAccounts(r.Context(), tokenAdminLimit(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminUsageRequests(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	items, err := repository.AdminUsageRequests(r.Context(), tokenAdminLimit(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	responses := make([]adminUsageRequest, 0, len(items))
	for _, item := range items {
		responses = append(responses, adminUsageRequest{UsageRequestID: item.ID, Model: item.Model, Status: item.Status, MaximumMicrocredits: item.MaximumMicrocredits, ActualMicrocredits: item.ActualMicrocredits, ErrorClass: item.ErrorClass, StartedAt: item.StartedAt, UpdatedAt: item.UpdatedAt})
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": responses})
}

func (r *PostgresRepository) AdminOverview(ctx context.Context) (AdminOverview, error) {
	var result AdminOverview
	err := r.pool.QueryRow(ctx, `
SELECT
  (SELECT count(*) FROM token_accounts),
  (SELECT count(*) FROM token_accounts WHERE status='ACTIVE'),
  (SELECT count(*) FROM provider_credentials WHERE status='ACTIVE'),
  (SELECT count(*) FROM routing_policies WHERE health_status='HEALTHY'),
  (SELECT count(*) FROM usage_requests),
  (SELECT count(*) FROM usage_requests WHERE status IN ('SETTLEMENT_PENDING','REVIEW'))`).Scan(
		&result.TokenAccounts, &result.ActiveAccounts, &result.ActiveCredentials,
		&result.HealthyRoutes, &result.UsageRequests, &result.ReviewUsageRequests,
	)
	return result, err
}

func (r *PostgresRepository) AdminTokenAccounts(ctx context.Context, limit int) ([]AdminTokenAccount, error) {
	rows, err := r.pool.Query(ctx, `
SELECT a.id,a.owner_type,a.owner_id,a.owner_organization_id,a.wallet_id,a.name,a.status,
       COALESCE(s.scope,'OWNER_UNIT_ONLY'),
       count(DISTINCT c.id) FILTER (WHERE c.status='ACTIVE'),count(DISTINCT p.id),a.updated_at
FROM token_accounts a
LEFT JOIN token_share_policies s ON s.token_account_id=a.id
LEFT JOIN provider_credentials c ON c.token_account_id=a.id
LEFT JOIN routing_policies p ON p.token_account_id=a.id
GROUP BY a.id,s.scope ORDER BY a.updated_at DESC,a.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminTokenAccount, 0)
	for rows.Next() {
		var item AdminTokenAccount
		if err := rows.Scan(&item.ID, &item.OwnerType, &item.OwnerID, &item.OwnerOrganization, &item.WalletID, &item.Name, &item.Status, &item.ShareScope, &item.ActiveCredentialCount, &item.RouteCount, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminUsageRequests(ctx context.Context, limit int) ([]UsageRequest, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,idempotency_key,user_id,session_id,organization_id,token_account_id,credential_id,wallet_id,price_version_id,hold_id,model,status,maximum_microcredits,actual_microcredits,error_class,started_at,updated_at FROM usage_requests ORDER BY updated_at DESC,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]UsageRequest, 0)
	for rows.Next() {
		var item UsageRequest
		if err := rows.Scan(&item.ID, &item.IdempotencyKey, &item.UserID, &item.SessionID, &item.OrganizationID, &item.TokenAccountID, &item.CredentialID, &item.WalletID, &item.PriceVersionID, &item.HoldID, &item.Model, &item.Status, &item.MaximumMicrocredits, &item.ActualMicrocredits, &item.ErrorClass, &item.StartedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
