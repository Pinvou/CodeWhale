package token

import (
	"context"
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

type AdminOverview struct {
	TokenAccounts         int64 `json:"token_accounts"`
	ActiveAccounts        int64 `json:"active_accounts"`
	ActiveCredentials     int64 `json:"active_credentials"`
	HealthyRoutes         int64 `json:"healthy_routes"`
	UsageRequests         int64 `json:"usage_requests"`
	ReviewUsageRequests   int64 `json:"review_usage_requests"`
	ReconciliationPending int64 `json:"reconciliation_pending"`
	ReconciliationStopped int64 `json:"reconciliation_stopped"`
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

type AdminCredentialMetadata struct {
	ID               uuid.UUID  `json:"credential_id"`
	Provider         string     `json:"provider"`
	Fingerprint      string     `json:"fingerprint"`
	Version          int        `json:"version"`
	Status           string     `json:"status"`
	ValidationStatus string     `json:"validation_status"`
	AllowedModels    []string   `json:"allowed_models"`
	ValidatedAt      *time.Time `json:"validated_at,omitempty"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

type AdminTokenAccountDetail struct {
	AdminTokenAccount
	Credentials []AdminCredentialMetadata `json:"credentials"`
	Routes      []RoutingPolicy           `json:"routes"`
}

type AdminUsageRequest struct {
	ID                      uuid.UUID  `json:"usage_request_id"`
	UserID                  uuid.UUID  `json:"user_id"`
	SessionID               string     `json:"session_id"`
	OrganizationID          *uuid.UUID `json:"organization_id,omitempty"`
	TokenAccountID          uuid.UUID  `json:"token_account_id"`
	CredentialID            uuid.UUID  `json:"credential_id"`
	WalletID                uuid.UUID  `json:"wallet_id"`
	PriceVersionID          *uuid.UUID `json:"price_version_id,omitempty"`
	HoldID                  *uuid.UUID `json:"hold_id,omitempty"`
	Model                   string     `json:"model"`
	Status                  string     `json:"status"`
	MaximumMicrocredits     int64      `json:"maximum_microcredits"`
	ActualMicrocredits      *int64     `json:"actual_microcredits,omitempty"`
	ErrorClass              *string    `json:"error_class,omitempty"`
	ProviderID              *string    `json:"provider_id,omitempty"`
	AdapterVersion          *string    `json:"adapter_version,omitempty"`
	UpstreamRequestID       *string    `json:"upstream_request_id,omitempty"`
	AttemptStatus           *string    `json:"attempt_status,omitempty"`
	InputTokens             *int64     `json:"input_tokens,omitempty"`
	OutputTokens            *int64     `json:"output_tokens,omitempty"`
	CachedInputTokens       *int64     `json:"cached_input_tokens,omitempty"`
	ReconciliationAttempts  int        `json:"reconciliation_attempts"`
	ReconciliationLastError *string    `json:"reconciliation_last_error,omitempty"`
	ReconciliationStoppedAt *time.Time `json:"reconciliation_stopped_at,omitempty"`
	StartedAt               time.Time  `json:"started_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
}

type adminRepository interface {
	AdminOverview(context.Context) (AdminOverview, error)
	AdminTokenAccounts(context.Context, int) ([]AdminTokenAccount, error)
	AdminTokenAccount(context.Context, uuid.UUID) (AdminTokenAccountDetail, error)
	AdminDeleteRoute(context.Context, uuid.UUID, string, uuid.UUID) error
	AdminCredentialMetadata(context.Context, uuid.UUID, uuid.UUID) (AdminCredentialMetadata, error)
	AdminRecordCredentialValidation(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, bool) (AdminCredentialMetadata, error)
	AdminUsageRequests(context.Context, int) ([]AdminUsageRequest, error)
	AdminUsageRequest(context.Context, uuid.UUID) (AdminUsageRequest, error)
	AdminRetryReconciliation(context.Context, uuid.UUID, string, uuid.UUID) (AdminUsageRequest, error)
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

func adminUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	value, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "a valid identifier is required", httpserver.RequestID(r.Context()))
		return uuid.Nil, false
	}
	return value, true
}

func tokenAdminActor(r *http.Request) uuid.UUID {
	principal, _ := identity.FromContext(r.Context())
	if actor, err := uuid.Parse(principal.Subject); err == nil {
		return actor
	}
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(principal.Issuer+"|"+principal.Subject))
}

func (s *Service) adminTokenAccount(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	accountID, ok := adminUUID(w, r, "token_account_id")
	if !ok {
		return
	}
	result, err := repository.AdminTokenAccount(r.Context(), accountID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminSetTokenAccountStatus(w http.ResponseWriter, r *http.Request) {
	accountID, ok := adminUUID(w, r, "token_account_id")
	if !ok {
		return
	}
	var body struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	account, err := s.repository.SetAccountStatus(r.Context(), accountID, body.Status, tokenAdminActor(r), body.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, account)
}

func (s *Service) adminUpsertTokenRoute(w http.ResponseWriter, r *http.Request) {
	accountID, ok := adminUUID(w, r, "token_account_id")
	if !ok {
		return
	}
	var body struct {
		Model        string `json:"model"`
		ProtocolCode string `json:"protocol_code"`
		Priority     int    `json:"priority"`
		DefaultScope string `json:"default_scope"`
		HealthStatus string `json:"health_status"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	if strings.TrimSpace(body.ProtocolCode) == "" {
		body.ProtocolCode = "CHAT"
	}
	result, err := s.repository.UpsertRoute(r.Context(), accountID, body.Model, body.ProtocolCode, body.Priority, body.DefaultScope, body.HealthStatus, tokenAdminActor(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminDeleteTokenRoute(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	accountID, ok := adminUUID(w, r, "token_account_id")
	if !ok {
		return
	}
	var body struct {
		Model string `json:"model"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	if err := repository.AdminDeleteRoute(r.Context(), accountID, body.Model, tokenAdminActor(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) adminValidateCredential(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	accountID, ok := adminUUID(w, r, "token_account_id")
	if !ok {
		return
	}
	credentialID, ok := adminUUID(w, r, "credential_id")
	if !ok {
		return
	}
	credential, secret, err := s.repository.CredentialSecret(r.Context(), accountID, credentialID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	valid := s.validator != nil && s.validator.Validate(r.Context(), credential.BaseURL, secret, credential.AllowedModels) == nil
	clear(secret)
	metadata, err := repository.AdminRecordCredentialValidation(r.Context(), accountID, credentialID, tokenAdminActor(r), valid)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"valid": valid, "credential": metadata})
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
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminUsageRequest(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	usageID, ok := adminUUID(w, r, "usage_request_id")
	if !ok {
		return
	}
	result, err := repository.AdminUsageRequest(r.Context(), usageID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminRetryReconciliation(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	usageID, ok := adminUUID(w, r, "usage_request_id")
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	result, err := repository.AdminRetryReconciliation(r.Context(), usageID, body.Reason, tokenAdminActor(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (r *PostgresRepository) AdminOverview(ctx context.Context) (AdminOverview, error) {
	var result AdminOverview
	err := r.pool.QueryRow(ctx, `
SELECT
  (SELECT count(*) FROM token_accounts),
  (SELECT count(*) FROM token_accounts WHERE status='ACTIVE'),
  (SELECT count(*) FROM provider_credentials WHERE status='ACTIVE' AND validation_status='VALID'),
  (SELECT count(*) FROM routing_policies WHERE health_status='HEALTHY'),
  (SELECT count(*) FROM usage_requests),
  (SELECT count(*) FROM usage_requests WHERE status IN ('SETTLEMENT_PENDING','REVIEW')),
  (SELECT count(*) FROM usage_requests WHERE status IN ('SETTLEMENT_PENDING','REVIEW') AND reconciliation_stopped_at IS NULL),
  (SELECT count(*) FROM usage_requests WHERE status IN ('SETTLEMENT_PENDING','REVIEW') AND reconciliation_stopped_at IS NOT NULL)`).Scan(
		&result.TokenAccounts, &result.ActiveAccounts, &result.ActiveCredentials,
		&result.HealthyRoutes, &result.UsageRequests, &result.ReviewUsageRequests,
		&result.ReconciliationPending, &result.ReconciliationStopped,
	)
	return result, err
}

func (r *PostgresRepository) AdminTokenAccounts(ctx context.Context, limit int) ([]AdminTokenAccount, error) {
	rows, err := r.pool.Query(ctx, `
SELECT a.id,a.owner_type,a.owner_id,a.owner_organization_id,a.wallet_id,a.name,a.status,
       COALESCE(s.scope,'OWNER_UNIT_ONLY'),
       count(DISTINCT c.id) FILTER (WHERE c.status='ACTIVE' AND c.validation_status='VALID'),count(DISTINCT p.id),a.updated_at
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

func (r *PostgresRepository) AdminTokenAccount(ctx context.Context, accountID uuid.UUID) (AdminTokenAccountDetail, error) {
	var result AdminTokenAccountDetail
	err := r.pool.QueryRow(ctx, `
SELECT a.id,a.owner_type,a.owner_id,a.owner_organization_id,a.wallet_id,a.name,a.status,
       COALESCE(s.scope,'OWNER_UNIT_ONLY'),
       count(DISTINCT c.id) FILTER (WHERE c.status='ACTIVE' AND c.validation_status='VALID'),count(DISTINCT p.id),a.updated_at
FROM token_accounts a
LEFT JOIN token_share_policies s ON s.token_account_id=a.id
LEFT JOIN provider_credentials c ON c.token_account_id=a.id
LEFT JOIN routing_policies p ON p.token_account_id=a.id
WHERE a.id=$1 GROUP BY a.id,s.scope`, accountID).Scan(
		&result.ID, &result.OwnerType, &result.OwnerID, &result.OwnerOrganization, &result.WalletID,
		&result.Name, &result.Status, &result.ShareScope, &result.ActiveCredentialCount, &result.RouteCount, &result.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminTokenAccountDetail{}, ErrNotFound
	}
	if err != nil {
		return AdminTokenAccountDetail{}, err
	}
	credentialRows, err := r.pool.Query(ctx, `
SELECT id,provider,fingerprint,version,status,validation_status,allowed_models,validated_at,
       COALESCE(revoked_at,retired_at,activated_at,validated_at,created_at)
FROM provider_credentials WHERE token_account_id=$1 ORDER BY version DESC`, accountID)
	if err != nil {
		return AdminTokenAccountDetail{}, err
	}
	defer credentialRows.Close()
	result.Credentials = make([]AdminCredentialMetadata, 0)
	for credentialRows.Next() {
		var item AdminCredentialMetadata
		if err := credentialRows.Scan(&item.ID, &item.Provider, &item.Fingerprint, &item.Version, &item.Status, &item.ValidationStatus, &item.AllowedModels, &item.ValidatedAt, &item.UpdatedAt); err != nil {
			return AdminTokenAccountDetail{}, err
		}
		result.Credentials = append(result.Credentials, item)
	}
	if err := credentialRows.Err(); err != nil {
		return AdminTokenAccountDetail{}, err
	}
	routeRows, err := r.pool.Query(ctx, `SELECT token_account_id,model,protocol_code,priority,default_scope,health_status,updated_at FROM routing_policies WHERE token_account_id=$1 ORDER BY priority DESC,model`, accountID)
	if err != nil {
		return AdminTokenAccountDetail{}, err
	}
	defer routeRows.Close()
	result.Routes = make([]RoutingPolicy, 0)
	for routeRows.Next() {
		var item RoutingPolicy
		if err := routeRows.Scan(&item.TokenAccountID, &item.Model, &item.ProtocolCode, &item.Priority, &item.DefaultScope, &item.HealthStatus, &item.UpdatedAt); err != nil {
			return AdminTokenAccountDetail{}, err
		}
		result.Routes = append(result.Routes, item)
	}
	return result, routeRows.Err()
}

func (r *PostgresRepository) AdminDeleteRoute(ctx context.Context, accountID uuid.UUID, model string, actorID uuid.UUID) error {
	model = strings.TrimSpace(model)
	if accountID == uuid.Nil || actorID == uuid.Nil || model == "" {
		return ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `DELETE FROM routing_policies WHERE token_account_id=$1 AND model=$2`, accountID, model)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := recordTokenAudit(ctx, tx, actorID, "TOKEN_ROUTE_DELETED", "TOKEN_ACCOUNT", accountID, `jsonb_build_object('model',$6)`, model); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AdminCredentialMetadata(ctx context.Context, accountID, credentialID uuid.UUID) (AdminCredentialMetadata, error) {
	var item AdminCredentialMetadata
	err := r.pool.QueryRow(ctx, `
SELECT id,provider,fingerprint,version,status,validation_status,allowed_models,validated_at,
       COALESCE(revoked_at,retired_at,activated_at,validated_at,created_at)
FROM provider_credentials WHERE token_account_id=$1 AND id=$2`, accountID, credentialID).Scan(
		&item.ID, &item.Provider, &item.Fingerprint, &item.Version, &item.Status, &item.ValidationStatus,
		&item.AllowedModels, &item.ValidatedAt, &item.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminCredentialMetadata{}, ErrNotFound
	}
	return item, err
}

func (r *PostgresRepository) AdminRecordCredentialValidation(ctx context.Context, accountID, credentialID, actorID uuid.UUID, valid bool) (AdminCredentialMetadata, error) {
	if accountID == uuid.Nil || credentialID == uuid.Nil || actorID == uuid.Nil {
		return AdminCredentialMetadata{}, ErrForbidden
	}
	status := "INVALID"
	if valid {
		status = "VALID"
	}
	now := r.now()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return AdminCredentialMetadata{}, err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `UPDATE provider_credentials SET validation_status=$3,validated_at=$4,validation_checked_by=$5 WHERE token_account_id=$1 AND id=$2 AND status='ACTIVE'`, accountID, credentialID, status, now, actorID)
	if err != nil {
		return AdminCredentialMetadata{}, err
	}
	if command.RowsAffected() == 0 {
		return AdminCredentialMetadata{}, ErrNotFound
	}
	if !valid {
		if _, err := tx.Exec(ctx, `UPDATE routing_policies SET health_status='UNHEALTHY',updated_at=$2,updated_by=$3 WHERE token_account_id=$1`, accountID, now, actorID); err != nil {
			return AdminCredentialMetadata{}, err
		}
	}
	if err := recordTokenAudit(ctx, tx, actorID, "PROVIDER_CREDENTIAL_VALIDATED", "PROVIDER_CREDENTIAL", credentialID, `jsonb_build_object('token_account_id',$6,'valid',$7)`, accountID, valid); err != nil {
		return AdminCredentialMetadata{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AdminCredentialMetadata{}, err
	}
	return r.AdminCredentialMetadata(ctx, accountID, credentialID)
}

func adminUsageQuery() string {
	return `SELECT request.id,request.user_id,request.session_id,request.organization_id,request.token_account_id,
request.credential_id,request.wallet_id,request.price_version_id,request.hold_id,request.model,request.status,
request.maximum_microcredits,request.actual_microcredits,request.error_class,attempt.provider_id,
attempt.adapter_version,attempt.upstream_request_id,attempt.status,attempt.input_tokens,attempt.output_tokens,attempt.cached_input_tokens,
request.reconciliation_attempts,request.reconciliation_last_error,request.reconciliation_stopped_at,
request.started_at,request.updated_at FROM usage_requests request
LEFT JOIN usage_attempts attempt ON attempt.usage_request_id=request.id`
}

func scanAdminUsage(row interface{ Scan(...any) error }) (AdminUsageRequest, error) {
	var item AdminUsageRequest
	err := row.Scan(&item.ID, &item.UserID, &item.SessionID, &item.OrganizationID, &item.TokenAccountID,
		&item.CredentialID, &item.WalletID, &item.PriceVersionID, &item.HoldID, &item.Model, &item.Status,
		&item.MaximumMicrocredits, &item.ActualMicrocredits, &item.ErrorClass, &item.ProviderID,
		&item.AdapterVersion, &item.UpstreamRequestID, &item.AttemptStatus, &item.InputTokens, &item.OutputTokens, &item.CachedInputTokens,
		&item.ReconciliationAttempts, &item.ReconciliationLastError, &item.ReconciliationStoppedAt, &item.StartedAt, &item.UpdatedAt)
	return item, err
}

func (r *PostgresRepository) AdminUsageRequests(ctx context.Context, limit int) ([]AdminUsageRequest, error) {
	rows, err := r.pool.Query(ctx, adminUsageQuery()+` ORDER BY request.updated_at DESC,request.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminUsageRequest, 0)
	for rows.Next() {
		item, err := scanAdminUsage(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminUsageRequest(ctx context.Context, usageID uuid.UUID) (AdminUsageRequest, error) {
	item, err := scanAdminUsage(r.pool.QueryRow(ctx, adminUsageQuery()+` WHERE request.id=$1`, usageID))
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminUsageRequest{}, ErrNotFound
	}
	return item, err
}

func (r *PostgresRepository) AdminRetryReconciliation(ctx context.Context, usageID uuid.UUID, reason string, actorID uuid.UUID) (AdminUsageRequest, error) {
	reason = strings.TrimSpace(reason)
	if usageID == uuid.Nil || actorID == uuid.Nil || reason == "" {
		return AdminUsageRequest{}, ErrForbidden
	}
	now := r.now()
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return AdminUsageRequest{}, err
	}
	defer tx.Rollback(ctx)
	command, err := tx.Exec(ctx, `UPDATE usage_requests SET reconciliation_next_at=$2,reconciliation_lease_until=NULL,reconciliation_last_error=$3,reconciliation_stopped_at=NULL,updated_at=$2 WHERE id=$1 AND status IN ('SETTLEMENT_PENDING','REVIEW')`, usageID, now, truncateReconciliationError("manual retry requested: "+reason))
	if err != nil {
		return AdminUsageRequest{}, err
	}
	if command.RowsAffected() == 0 {
		if _, err := r.AdminUsageRequest(ctx, usageID); err != nil {
			return AdminUsageRequest{}, err
		}
		return AdminUsageRequest{}, ErrForbidden
	}
	if err := recordTokenAudit(ctx, tx, actorID, "USAGE_RECONCILIATION_RETRIED", "USAGE_REQUEST", usageID, `jsonb_build_object('reason',$6)`, reason); err != nil {
		return AdminUsageRequest{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AdminUsageRequest{}, err
	}
	return r.AdminUsageRequest(ctx, usageID)
}

func recordTokenAudit(ctx context.Context, tx pgx.Tx, actorID uuid.UUID, action, resourceType string, resourceID uuid.UUID, summaryExpression string, summaryArguments ...any) error {
	arguments := []any{uuid.Must(uuid.NewV7()), actorID, action, resourceType, resourceID}
	arguments = append(arguments, summaryArguments...)
	arguments = append(arguments, time.Now().UTC())
	_, err := tx.Exec(ctx, `INSERT INTO token_audit_logs(id,actor_user_id,action,resource_type,resource_id,summary,occurred_at) VALUES($1,$2,$3,$4,$5,`+summaryExpression+`,$`+strconv.Itoa(len(arguments))+`)`, arguments...)
	return err
}
