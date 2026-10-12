package token

import (
	"context"

	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type AdminOverview struct {
	TokenAccounts         int64 `json:"token_accounts"`
	ActiveAccounts        int64 `json:"active_accounts"`
	ActiveCredentials     int64 `json:"active_credentials"`
	Routes                int64 `json:"routes"`
	HealthyRoutes         int64 `json:"healthy_routes"`
	UsageRequests         int64 `json:"usage_requests"`
	ReviewUsageRequests   int64 `json:"review_usage_requests"`
	ReconciliationPending int64 `json:"reconciliation_pending"`
	ReconciliationStopped int64 `json:"reconciliation_stopped"`
}

func normalizeAdminOperationReason(value string) (string, bool) {
	reason := strings.TrimSpace(value)
	count := utf8.RuneCountInString(reason)
	return reason, count >= 1 && count <= 500
}

type AdminTokenAccount struct {
	ID                    uuid.UUID  `json:"token_account_id"`
	OwnerType             string     `json:"owner_type"`
	OwnerID               uuid.UUID  `json:"owner_id"`
	OwnerOrganization     *uuid.UUID `json:"owner_organization_id,omitempty"`
	OwnerUnit             *uuid.UUID `json:"owner_unit_id,omitempty"`
	WalletID              uuid.UUID  `json:"wallet_id"`
	Name                  string     `json:"name"`
	Status                string     `json:"status"`
	ShareScope            string     `json:"share_scope"`
	ManagementMode        string     `json:"management_mode"`
	ActiveCredentialCount int64      `json:"active_credential_count"`
	RouteCount            int64      `json:"route_count"`
	AccountVersion        int64      `json:"account_version"`
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

type AdminCredentialSafeMetadata struct {
	ID               uuid.UUID  `json:"credential_id"`
	CredentialType   string     `json:"credential_type"`
	Fingerprint      string     `json:"fingerprint"`
	Version          int        `json:"version"`
	Status           string     `json:"status"`
	ValidationStatus string     `json:"validation_status"`
	AllowedModels    []string   `json:"allowed_models"`
	ValidatedAt      *time.Time `json:"validated_at,omitempty"`
	ResponsibleBy    uuid.UUID  `json:"responsible_actor_id"`
	CreatedAt        time.Time  `json:"created_at"`
	UpdatedAt        time.Time  `json:"updated_at"`
}

type AdminTokenAccountDetail struct {
	AdminTokenAccount
	AsOf        time.Time                 `json:"as_of"`
	Credentials []AdminCredentialMetadata `json:"credentials"`
	Routes      []RoutingPolicy           `json:"routes"`
}

// AdminTokenAccountSafeDetail is the versioned Portal projection used by the
// account detail workspace. Credential and route rows are deliberately absent:
// those resources are exposed only through their independently paginated,
// redacted projections.
type AdminTokenAccountSafeDetail struct {
	AdminTokenAccount
	AsOf time.Time `json:"as_of"`
}

// AdminCredentialSafeValidationResult is the governed Portal projection. It
// intentionally excludes provider endpoint, key identifiers and ciphertext.
type AdminCredentialSafeValidationResult struct {
	ValidationStatus string                      `json:"validation_status"`
	ErrorCode        string                      `json:"error_code,omitempty"`
	Credential       AdminCredentialSafeMetadata `json:"credential"`
}

func stableReconciliationDiagnostic(value *string) *string {
	if value == nil {
		return nil
	}
	code := strings.TrimSpace(*value)
	if code == "" {
		return nil
	}
	switch code {
	case "MANUAL_RECONCILIATION_REQUESTED",
		"MANUAL_RECONCILIATION_REQUIRED",
		"USAGE_EVIDENCE_TIMEOUT",
		"USAGE_EVIDENCE_UNAVAILABLE",
		"HOLD_RELEASE_UNAVAILABLE",
		"RECONCILIATION_DIAGNOSTIC_REDACTED":
		// These are the only operational codes safe for the Portal projection.
	case "TRUSTED_USAGE_DEADLINE_EXCEEDED:MANUAL_RECONCILIATION_REQUIRED",
		"TRUSTED_USAGE_DEADLINE_EXCEEDED:USAGE_EVIDENCE_TIMEOUT",
		"TRUSTED_USAGE_DEADLINE_EXCEEDED:USAGE_EVIDENCE_UNAVAILABLE":
		// Collapse the historical compound value so internal retry detail does
		// not become part of the public diagnostic vocabulary.
		code = "TRUSTED_USAGE_DEADLINE_EXCEEDED"
	default:
		code = "RECONCILIATION_DIAGNOSTIC_REDACTED"
	}
	return &code
}

type adminRepository interface {
	AdminUsageRequests(context.Context, int) ([]AdminUsageRequest, error)
	AdminUsageRequest(context.Context, uuid.UUID) (AdminUsageRequest, error)
	AdminOverview(context.Context) (AdminOverview, error)
	AdminTokenAccounts(context.Context, int) ([]AdminTokenAccount, error)
	AdminTokenAccount(context.Context, uuid.UUID) (AdminTokenAccountDetail, error)
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

type legacyAdminOverviewRepository interface {
	AdminOverview(context.Context) (AdminOverview, error)
	reserveAdminOverviewRate(context.Context, uuid.UUID, string, string) error
}

func (s *Service) adminOverview(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(legacyAdminOverviewRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "token administration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	ctx, cancel, err := adminPaginationContext(r.Context(), AdminSnapshotBuildBudget)
	if err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	defer cancel()
	r = r.WithContext(ctx)
	if err := repository.reserveAdminOverviewRate(ctx, adminSubject(r), "legacy-overview", s.trustedAdminQuerySource(r)); err != nil {
		writeAdminQueryError(w, r, err)
		return
	}
	result, err := repository.AdminOverview(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	response := make(map[string]int64, 9)
	if authorization.GrantedPermission(r.Context(), "platform.tokens.read") {
		response["token_accounts"] = result.TokenAccounts
		response["active_accounts"] = result.ActiveAccounts
		response["active_credentials"] = result.ActiveCredentials
		response["routes"] = result.Routes
		response["healthy_routes"] = result.HealthyRoutes
	}
	if authorization.GrantedPermission(r.Context(), "platform.usage.read") {
		response["usage_requests"] = result.UsageRequests
		response["review_usage_requests"] = result.ReviewUsageRequests
		response["reconciliation_pending"] = result.ReconciliationPending
		response["reconciliation_stopped"] = result.ReconciliationStopped
	} else if authorization.GrantedPermission(r.Context(), "platform.system.status.read") {
		response["reconciliation_pending"] = result.ReconciliationPending
		response["reconciliation_stopped"] = result.ReconciliationStopped
	}
	api.WriteJSON(w, http.StatusOK, response)
}

func (s *Service) adminTokenAccounts(w http.ResponseWriter, r *http.Request) {
	if s.paginatedTokenAccounts(w, r) {
		return
	}
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

func (s *Service) adminTokenAccountV2(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(interface {
		AdminTokenAccountSafeDetail(context.Context, uuid.UUID) (AdminTokenAccountSafeDetail, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_ADMIN_QUERY_UNAVAILABLE", "token account detail projection is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	accountID, ok := adminUUID(w, r, "token_account_id")
	if !ok {
		return
	}
	result, err := repository.AdminTokenAccountSafeDetail(r.Context(), accountID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (r *PostgresRepository) AdminOverview(ctx context.Context) (AdminOverview, error) {
	ctx, cancel, err := adminPaginationContext(ctx, AdminSnapshotBuildBudget)
	if err != nil {
		return AdminOverview{}, err
	}
	defer cancel()
	var result AdminOverview
	err = r.withModelTokenPageOverviewTx(ctx, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
SELECT
  (SELECT count(*) FROM token_accounts),
  (SELECT count(*) FROM token_accounts WHERE status='ACTIVE'),
  (SELECT count(*) FROM provider_credentials WHERE status='ACTIVE' AND validation_status='VALID'),
  (SELECT count(*) FROM routing_policies WHERE reference_status='RESOLVED'),
  (SELECT count(*) FROM routing_policies WHERE reference_status='RESOLVED' AND health_status='HEALTHY' AND health_expires_at>transaction_timestamp()),
  (SELECT count(*) FROM usage_requests),
  (SELECT count(*) FROM usage_requests WHERE status IN ('SETTLEMENT_PENDING','REVIEW')),
  (SELECT count(*) FROM usage_requests WHERE status IN ('SETTLEMENT_PENDING','REVIEW') AND reconciliation_stopped_at IS NULL),
  (SELECT count(*) FROM usage_requests WHERE status IN ('SETTLEMENT_PENDING','REVIEW') AND reconciliation_stopped_at IS NOT NULL)`).Scan(
			&result.TokenAccounts, &result.ActiveAccounts, &result.ActiveCredentials, &result.Routes,
			&result.HealthyRoutes, &result.UsageRequests, &result.ReviewUsageRequests,
			&result.ReconciliationPending, &result.ReconciliationStopped,
		)
	})
	return result, err
}

func (r *PostgresRepository) AdminTokenAccounts(ctx context.Context, limit int) ([]AdminTokenAccount, error) {
	rows, err := r.pool.Query(ctx, `
SELECT a.id,a.owner_type,a.owner_id,a.owner_organization_id,a.owner_unit_id,a.wallet_id,a.name,a.status,
       COALESCE(s.scope,'OWNER_UNIT_ONLY'),a.management_mode,
       count(DISTINCT c.id) FILTER (WHERE c.status='ACTIVE' AND c.validation_status='VALID'),count(DISTINCT p.id),a.management_version,a.updated_at
FROM token_accounts a
LEFT JOIN token_share_policies s ON s.token_account_id=a.id
LEFT JOIN provider_credentials c ON c.token_account_id=a.id
LEFT JOIN routing_policies p ON p.token_account_id=a.id AND p.reference_status='RESOLVED'
WHERE a.management_mode='PLATFORM'
GROUP BY a.id,s.scope ORDER BY a.updated_at DESC,a.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminTokenAccount, 0)
	for rows.Next() {
		var item AdminTokenAccount
		if err := rows.Scan(&item.ID, &item.OwnerType, &item.OwnerID, &item.OwnerOrganization, &item.OwnerUnit, &item.WalletID, &item.Name, &item.Status, &item.ShareScope, &item.ManagementMode, &item.ActiveCredentialCount, &item.RouteCount, &item.AccountVersion, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminTokenAccount(ctx context.Context, accountID uuid.UUID) (AdminTokenAccountDetail, error) {
	var result AdminTokenAccountDetail
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return AdminTokenAccountDetail{}, err
	}
	defer tx.Rollback(ctx)
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&result.AsOf); err != nil {
		return AdminTokenAccountDetail{}, err
	}
	err = tx.QueryRow(ctx, `
SELECT a.id,a.owner_type,a.owner_id,a.owner_organization_id,a.owner_unit_id,a.wallet_id,a.name,a.status,
       COALESCE(s.scope,'OWNER_UNIT_ONLY'),a.management_mode,
       count(DISTINCT c.id) FILTER (WHERE c.status='ACTIVE' AND c.validation_status='VALID'),count(DISTINCT p.id),a.management_version,a.updated_at
FROM token_accounts a
LEFT JOIN token_share_policies s ON s.token_account_id=a.id
LEFT JOIN provider_credentials c ON c.token_account_id=a.id
LEFT JOIN routing_policies p ON p.token_account_id=a.id AND p.reference_status='RESOLVED'
WHERE a.id=$1 AND a.management_mode='PLATFORM' GROUP BY a.id,s.scope`, accountID).Scan(
		&result.ID, &result.OwnerType, &result.OwnerID, &result.OwnerOrganization, &result.OwnerUnit, &result.WalletID,
		&result.Name, &result.Status, &result.ShareScope, &result.ManagementMode, &result.ActiveCredentialCount, &result.RouteCount, &result.AccountVersion, &result.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminTokenAccountDetail{}, ErrNotFound
	}
	if err != nil {
		return AdminTokenAccountDetail{}, err
	}
	credentialRows, err := tx.Query(ctx, `
SELECT id,provider,fingerprint,version,status,validation_status,allowed_models,validated_at,
       COALESCE(revoked_at,retired_at,activated_at,validated_at,created_at)
FROM provider_credentials WHERE token_account_id=$1 ORDER BY version DESC`, accountID)
	if err != nil {
		return AdminTokenAccountDetail{}, err
	}
	result.Credentials = make([]AdminCredentialMetadata, 0)
	for credentialRows.Next() {
		var item AdminCredentialMetadata
		if err := credentialRows.Scan(&item.ID, &item.Provider, &item.Fingerprint, &item.Version, &item.Status, &item.ValidationStatus, &item.AllowedModels, &item.ValidatedAt, &item.UpdatedAt); err != nil {
			credentialRows.Close()
			return AdminTokenAccountDetail{}, err
		}
		result.Credentials = append(result.Credentials, item)
	}
	if err := credentialRows.Err(); err != nil {
		credentialRows.Close()
		return AdminTokenAccountDetail{}, err
	}
	credentialRows.Close()
	routeRows, err := tx.Query(ctx, routingPolicyProjection+` WHERE token_account_id=$1 AND reference_status<>'DELETED' ORDER BY priority DESC,model_id,id`, accountID)
	if err != nil {
		return AdminTokenAccountDetail{}, err
	}
	result.Routes = make([]RoutingPolicy, 0)
	for routeRows.Next() {
		var item RoutingPolicy
		if err := routeRows.Scan(&item.ID, &item.TokenAccountID, &item.Model, &item.ModelID, &item.ProtocolVersionID, &item.ProtocolCode, &item.Priority, &item.DefaultScope, &item.ReferenceStatus, &item.HealthStatus, &item.HealthDiagnostic, &item.HealthObservedAt, &item.HealthExpiresAt, &item.RouteVersion, &item.RouteGeneration, &item.UpdatedAt); err != nil {
			routeRows.Close()
			return AdminTokenAccountDetail{}, err
		}
		result.Routes = append(result.Routes, item)
	}
	if err := routeRows.Err(); err != nil {
		routeRows.Close()
		return AdminTokenAccountDetail{}, err
	}
	routeRows.Close()
	if err := tx.Commit(ctx); err != nil {
		return AdminTokenAccountDetail{}, err
	}
	return result, nil
}

var tokenAuditParameter = regexp.MustCompile(`\$([0-9]+)\b`)

func recordTokenAudit(ctx context.Context, tx pgx.Tx, actorID uuid.UUID, action, resourceType string, resourceID uuid.UUID, summaryExpression string, summaryArguments ...any) error {
	// jsonb_build_object accepts polymorphic values. PostgreSQL cannot infer an
	// otherwise untyped placeholder during the production extended-protocol
	// prepare step, so bind each audit argument's stable SQL type explicitly.
	summaryExpression = tokenAuditParameter.ReplaceAllStringFunc(summaryExpression, func(parameter string) string {
		position, _ := strconv.Atoi(parameter[1:])
		if position < 6 || position-6 >= len(summaryArguments) {
			return parameter
		}
		switch summaryArguments[position-6].(type) {
		case uuid.UUID, *uuid.UUID:
			return parameter + "::uuid"
		case string, *string:
			return parameter + "::text"
		case int, int32, int64, *int64:
			return parameter + "::bigint"
		case bool, *bool:
			return parameter + "::boolean"
		case time.Time, *time.Time:
			return parameter + "::timestamptz"
		case json.RawMessage:
			return parameter + "::jsonb"
		default:
			return parameter
		}
	})
	arguments := []any{uuid.Must(uuid.NewV7()), actorID, action, resourceType, resourceID}
	arguments = append(arguments, summaryArguments...)
	arguments = append(arguments, time.Now().UTC())
	_, err := tx.Exec(ctx, `INSERT INTO token_audit_logs(id,actor_user_id,action,resource_type,resource_id,summary,occurred_at) VALUES($1,$2,$3,$4,$5,`+summaryExpression+`,$`+strconv.Itoa(len(arguments))+`)`, arguments...)
	return err
}
