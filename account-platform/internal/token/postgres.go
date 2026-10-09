package token

import (
	"context"

	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type TokenAccount struct {
	ID                uuid.UUID  `json:"token_account_id"`
	OwnerType         string     `json:"owner_type"`
	OwnerID           uuid.UUID  `json:"owner_id"`
	OwnerOrganization *uuid.UUID `json:"owner_organization_id,omitempty"`
	OwnerUnit         *uuid.UUID `json:"owner_unit_id,omitempty"`
	WalletID          uuid.UUID  `json:"wallet_id"`
	Name              string     `json:"name"`
	Status            string     `json:"status"`
	ShareScope        string     `json:"share_scope"`
	DefaultPriority   int        `json:"default_priority"`
	ManagementMode    string     `json:"-"`
}

type Credential struct {
	ID             uuid.UUID  `json:"credential_id"`
	TokenAccountID uuid.UUID  `json:"token_account_id"`
	Provider       string     `json:"provider"`
	BaseURL        string     `json:"base_url"`
	Fingerprint    string     `json:"fingerprint"`
	Version        int        `json:"version"`
	Status         string     `json:"status"`
	AllowedModels  []string   `json:"allowed_models"`
	ValidatedAt    *time.Time `json:"validated_at,omitempty"`
}

type RoutingPolicy struct {
	ID                uuid.UUID  `json:"route_id"`
	TokenAccountID    uuid.UUID  `json:"token_account_id"`
	Model             string     `json:"model"`
	ModelID           *uuid.UUID `json:"model_id"`
	ProtocolVersionID *uuid.UUID `json:"protocol_version_id"`
	ProtocolCode      string     `json:"protocol_code"`
	Priority          int        `json:"priority"`
	DefaultScope      string     `json:"default_scope"`
	ReferenceStatus   string     `json:"reference_status"`
	HealthStatus      string     `json:"health_status"`
	HealthDiagnostic  string     `json:"health_diagnostic_code,omitempty"`
	HealthObservedAt  *time.Time `json:"health_observed_at,omitempty"`
	HealthExpiresAt   *time.Time `json:"health_expires_at,omitempty"`
	RouteVersion      int64      `json:"route_version"`
	RouteGeneration   int64      `json:"route_generation"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// StableRouteResult is the versioned write projection. It excludes the legacy
// text model reference so v2 clients can only consume immutable catalog keys.
type StableRouteResult struct {
	ID                uuid.UUID  `json:"route_id"`
	TokenAccountID    uuid.UUID  `json:"token_account_id"`
	ModelID           uuid.UUID  `json:"model_id"`
	ProtocolVersionID uuid.UUID  `json:"protocol_version_id"`
	ProtocolCode      string     `json:"protocol_code"`
	Priority          int        `json:"priority"`
	DefaultScope      string     `json:"default_scope"`
	HealthStatus      string     `json:"health_status"`
	HealthDiagnostic  string     `json:"health_diagnostic_code"`
	HealthObservedAt  *time.Time `json:"health_observed_at"`
	HealthExpiresAt   *time.Time `json:"health_expires_at"`
	RouteVersion      int64      `json:"route_version"`
	RouteGeneration   int64      `json:"route_generation"`
	AccountVersion    int64      `json:"account_version"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

type CreateAccountRequest struct {
	OwnerType         string
	OwnerID           uuid.UUID
	OwnerOrganization *uuid.UUID
	OwnerUnit         *uuid.UUID
	WalletID          uuid.UUID
	Name              string
	ShareScope        string
	DefaultPriority   int
	ManagementMode    string
	CreatedBy         uuid.UUID
}

type PostgresRepository struct {
	pool                    *pgxpool.Pool
	secrets                 SecretProvider
	now                     func() time.Time
	adminPagination         adminPaginationRuntime
	adminPaginationMetrics  *AdminPaginationMetrics
	routeHealthMetrics      *RouteHealthMetrics
	routeActivationGuard    TokenRouteActivationTransactionGuard
	credentialAuthGuard     CredentialIsolationAuthorityGuard
	protocolFormulaT1Commit func(context.Context, pgx.Tx) error
}

type Repository interface {
	CreateAccount(context.Context, CreateAccountRequest) (TokenAccount, error)
	ListCandidates(context.Context, string, string) ([]Candidate, error)
	CredentialSecret(context.Context, uuid.UUID, uuid.UUID) (Credential, []byte, error)
	GetUsage(context.Context, uuid.UUID) (UsageRequest, error)
	Ping(context.Context) error
	Close()
}

func (r *PostgresRepository) GetUsage(ctx context.Context, usageID uuid.UUID) (UsageRequest, error) {
	query := `SELECT ` + usageSelectColumns + ` FROM usage_requests WHERE id=$1`
	var usage UsageRequest
	err := scanUsage(r.pool.QueryRow(ctx, query, usageID), &usage)
	if errors.Is(err, pgx.ErrNoRows) {
		return UsageRequest{}, ErrNotFound
	}
	return usage, err
}

const usageSelectColumns = `id,idempotency_key_digest,idempotency_namespace,canonicalization_version,request_fingerprint,user_id,session_id,organization_id,token_account_id,credential_id,wallet_id,price_version_id,hold_id,model,model_id,catalog_version,model_capability_version_id,protocol_definition_id,route_generation,model_snapshot,request_snapshot,usage_snapshot,status,execution_status,execution_version,delivery_status,cancel_status,settlement_status,cancel_requested_at,user_stopped_at,provider_terminal_at,final_usage_observed_at,last_persisted_sequence,last_client_confirmed_sequence,maximum_microcredits,actual_microcredits,error_class,started_at,updated_at`

type rowScanner interface{ Scan(...any) error }

func scanUsage(row rowScanner, usage *UsageRequest) error {
	return row.Scan(&usage.ID, &usage.IdempotencyKeyDigest, &usage.IdempotencyNamespace, &usage.CanonicalVersion, &usage.RequestFingerprint, &usage.UserID, &usage.SessionID, &usage.OrganizationID, &usage.TokenAccountID, &usage.CredentialID, &usage.WalletID, &usage.PriceVersionID, &usage.HoldID, &usage.Model, &usage.ModelID, &usage.CatalogVersion, &usage.CapabilityVersionID, &usage.ProtocolDefinitionID, &usage.RouteGeneration, &usage.ModelSnapshot, &usage.RequestSnapshot, &usage.UsageSnapshot, &usage.Status, &usage.ExecutionStatus, &usage.ExecutionVersion, &usage.DeliveryStatus, &usage.CancelStatus, &usage.SettlementStatus, &usage.CancelRequestedAt, &usage.UserStoppedAt, &usage.ProviderTerminalAt, &usage.FinalUsageObservedAt, &usage.LastPersistedSequence, &usage.LastClientConfirmedSequence, &usage.MaximumMicrocredits, &usage.ActualMicrocredits, &usage.ErrorClass, &usage.StartedAt, &usage.UpdatedAt)
}

func NewPostgresRepository(ctx context.Context, databaseURL string, secrets SecretProvider) (*PostgresRepository, error) {
	if secrets == nil {
		return nil, ErrInvalidSecret
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open token database: %w", err)
	}
	repository := &PostgresRepository{pool: pool, secrets: secrets, now: func() time.Time { return time.Now().UTC() }, adminPaginationMetrics: newAdminPaginationMetrics(), routeHealthMetrics: newRouteHealthMetrics()}
	if err := repository.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return repository, nil
}

func (r *PostgresRepository) Ping(ctx context.Context) error { return r.pool.Ping(ctx) }

func (r *PostgresRepository) Close() { r.pool.Close() }

func (r *PostgresRepository) CreateAccount(ctx context.Context, request CreateAccountRequest) (TokenAccount, error) {
	request.OwnerType = strings.ToUpper(strings.TrimSpace(request.OwnerType))
	request.ShareScope = strings.ToUpper(strings.TrimSpace(request.ShareScope))
	request.ManagementMode = strings.ToUpper(strings.TrimSpace(request.ManagementMode))
	request.Name = strings.TrimSpace(request.Name)
	if !validOwner(request) || !validShareScope(request.ShareScope) || request.ManagementMode != "PLATFORM" || request.WalletID == uuid.Nil || request.CreatedBy == uuid.Nil || request.Name == "" || request.DefaultPriority < 0 || request.DefaultPriority > 10000 {
		return TokenAccount{}, ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return TokenAccount{}, err
	}
	defer tx.Rollback(ctx)
	if err = lockTokenAccountOwner(ctx, tx, request.OwnerType, request.OwnerID); err != nil {
		return TokenAccount{}, err
	}
	if err = lockTokenAccountOwnerCapPolicy(ctx, tx); err != nil {
		return TokenAccount{}, err
	}
	capacity, err := loadTokenAccountOwnerCapacity(ctx, tx, request.OwnerType, request.OwnerID)
	if err != nil {
		return TokenAccount{}, err
	}
	if capacity.Count >= capacity.Policy.MaxPerOwner {
		return TokenAccount{}, ErrTokenAccountCapReached
	}
	account, err := createAccountEffect(ctx, tx, request, r.now())
	if err != nil {
		return TokenAccount{}, err
	}
	return account, tx.Commit(ctx)
}

func (r *PostgresRepository) GetAccount(ctx context.Context, accountID uuid.UUID) (TokenAccount, error) {
	var account TokenAccount
	err := r.pool.QueryRow(ctx, `SELECT ta.id,ta.owner_type,ta.owner_id,ta.owner_organization_id,ta.owner_unit_id,ta.wallet_id,ta.name,ta.status,sp.scope,ta.default_priority,ta.management_mode FROM token_accounts ta JOIN token_share_policies sp ON sp.token_account_id=ta.id WHERE ta.id=$1`, accountID).Scan(&account.ID, &account.OwnerType, &account.OwnerID, &account.OwnerOrganization, &account.OwnerUnit, &account.WalletID, &account.Name, &account.Status, &account.ShareScope, &account.DefaultPriority, &account.ManagementMode)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenAccount{}, ErrNotFound
	}
	return account, err
}

func (r *PostgresRepository) ListManagedAccounts(ctx context.Context, resolved AuthorizationContext) ([]TokenAccount, error) {
	var rows pgx.Rows
	var err error
	if resolved.ContextType == "PERSONAL" {
		rows, err = r.pool.Query(ctx, `SELECT ta.id,ta.owner_type,ta.owner_id,ta.owner_organization_id,ta.owner_unit_id,ta.wallet_id,ta.name,ta.status,sp.scope,ta.default_priority,ta.management_mode FROM token_accounts ta JOIN token_share_policies sp ON sp.token_account_id=ta.id WHERE ta.owner_type='USER' AND ta.owner_id=$1 AND ta.management_mode='USER' ORDER BY ta.name,ta.id`, resolved.UserID)
	} else if resolved.ContextType == "ORGANIZATION" && resolved.OrganizationID != nil && (slices.Contains(resolved.Roles, "OWNER") || slices.Contains(resolved.Roles, "ORG_ADMIN")) {
		rows, err = r.pool.Query(ctx, `SELECT ta.id,ta.owner_type,ta.owner_id,ta.owner_organization_id,ta.owner_unit_id,ta.wallet_id,ta.name,ta.status,sp.scope,ta.default_priority,ta.management_mode FROM token_accounts ta JOIN token_share_policies sp ON sp.token_account_id=ta.id WHERE ta.owner_organization_id=$1 AND ta.management_mode='USER' ORDER BY ta.name,ta.id`, *resolved.OrganizationID)
	} else {
		return []TokenAccount{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]TokenAccount, 0)
	for rows.Next() {
		var item TokenAccount
		if err := rows.Scan(&item.ID, &item.OwnerType, &item.OwnerID, &item.OwnerOrganization, &item.OwnerUnit, &item.WalletID, &item.Name, &item.Status, &item.ShareScope, &item.DefaultPriority, &item.ManagementMode); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) ListCredentials(ctx context.Context, accountID uuid.UUID) ([]Credential, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,token_account_id,provider,base_url,fingerprint,version,status,allowed_models,validated_at FROM provider_credentials WHERE token_account_id=$1 ORDER BY version DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Credential, 0)
	for rows.Next() {
		var item Credential
		if err := rows.Scan(&item.ID, &item.TokenAccountID, &item.Provider, &item.BaseURL, &item.Fingerprint, &item.Version, &item.Status, &item.AllowedModels, &item.ValidatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) RotateCredential(ctx context.Context, accountID, actorID uuid.UUID, provider, baseURL string, secret []byte, models []string) (Credential, error) {
	provider, baseURL = strings.TrimSpace(provider), strings.TrimRight(strings.TrimSpace(baseURL), "/")
	models = normalizeModels(models)
	if accountID == uuid.Nil || actorID == uuid.Nil || provider == "" || baseURL == "" || len(secret) < 8 || len(models) == 0 {
		return Credential{}, ErrInvalidSecret
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Credential{}, err
	}
	defer tx.Rollback(ctx)
	var exists uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM token_accounts WHERE id=$1 AND status='ACTIVE' AND management_mode='PLATFORM' FOR UPDATE`, accountID).Scan(&exists); errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrNotFound
	} else if err != nil {
		return Credential{}, err
	}
	var nextVersion int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version),0)+1 FROM provider_credentials WHERE token_account_id=$1`, accountID).Scan(&nextVersion); err != nil {
		return Credential{}, err
	}
	credentialID := uuid.Must(uuid.NewV7())
	aad := []byte(accountID.String() + ":" + credentialID.String())
	ciphertext, err := r.secrets.Encrypt(secret, aad)
	if err != nil {
		return Credential{}, err
	}
	now := r.now()
	credential := Credential{ID: credentialID, TokenAccountID: accountID, Provider: provider, BaseURL: baseURL, Fingerprint: Fingerprint(secret), Version: nextVersion, Status: "ACTIVE", AllowedModels: models, ValidatedAt: &now}
	_, err = tx.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status,validation_checked_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'PENDING',$9,$10,$11,$10,'VALID',$11)`, credential.ID, accountID, provider, baseURL, ciphertext, r.secrets.KeyID(), credential.Fingerprint, nextVersion, models, now, actorID)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE provider_credentials SET status='RETIRED',retired_at=$2 WHERE token_account_id=$1 AND status='ACTIVE'`, accountID, now)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE provider_credentials SET status='ACTIVE',activated_at=$2 WHERE id=$1`, credential.ID, now)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE routing_policies SET health_status='UNKNOWN',health_diagnostic_code='CREDENTIAL_CHANGED',health_observed_at=NULL,health_expires_at=NULL,next_probe_at=$2,health_version=health_version+1,updated_at=$2 WHERE token_account_id=$1 AND reference_status='RESOLVED'`, accountID, now)
	}
	if err != nil {
		return Credential{}, err
	}
	return credential, tx.Commit(ctx)
}

func (r *PostgresRepository) ListCandidates(ctx context.Context, model, protocolCode string) ([]Candidate, error) {
	modelID, modelErr := uuid.Parse(strings.TrimSpace(model))
	protocolVersionID, protocolErr := uuid.Parse(strings.TrimSpace(protocolCode))
	if modelErr != nil || protocolErr != nil {
		return []Candidate{}, ErrRouteReferenceInvalid
	}
	rows, err := r.pool.Query(ctx, `
SELECT ta.id,ta.name,ta.owner_type,ta.owner_id,ta.owner_organization_id,ta.owner_unit_id,ta.wallet_id,ta.status,
       sp.scope,pc.allowed_models,
       ARRAY(SELECT em.user_id FROM token_explicit_members em WHERE em.token_account_id=ta.id ORDER BY em.user_id),
	       rp.default_scope,rp.priority,rp.health_status,rp.health_expires_at,transaction_timestamp(),pc.id,rp.protocol_code,rp.model_id,rp.protocol_version_id,rp.route_generation,ta.management_mode,pc.provider,pc.version
FROM token_accounts ta
JOIN token_share_policies sp ON sp.token_account_id=ta.id
JOIN provider_credentials pc ON pc.token_account_id=ta.id AND pc.status='ACTIVE' AND pc.validation_status='VALID'
JOIN routing_policies rp ON rp.token_account_id=ta.id AND rp.model_id=$1 AND rp.protocol_version_id=$2 AND rp.reference_status='RESOLVED'
JOIN model_identities identity ON identity.id=rp.model_id AND pc.provider=identity.provider_id AND identity.upstream_model_ref=ANY(pc.allowed_models)
LEFT JOIN route_provisionings provision ON provision.active_token_account_id=ta.id AND provision.active_credential_id=pc.id AND provision.model_id=rp.model_id AND provision.protocol_version_id=rp.protocol_version_id AND provision.status='READY' AND provision.actual_generation=rp.route_generation
WHERE ta.status='ACTIVE' AND ta.management_mode='PLATFORM' AND provision.id IS NOT NULL
  AND rp.health_status='HEALTHY' AND rp.health_expires_at>transaction_timestamp()
ORDER BY rp.priority DESC,ta.id`, modelID, protocolVersionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Candidate, 0)
	for rows.Next() {
		var item Candidate
		if err := rows.Scan(&item.TokenAccountID, &item.Name, &item.OwnerType, &item.OwnerID, &item.OwnerOrganization, &item.OwnerUnit, &item.WalletID, &item.Status, &item.ShareScope, &item.AllowedModels, &item.ExplicitMembers, &item.DefaultScope, &item.Priority, &item.HealthStatus, &item.HealthExpiresAt, &item.DatabaseNow, &item.CredentialID, &item.ProtocolCode, &item.ModelID, &item.ProtocolVersionID, &item.RouteGeneration, &item.ManagementMode, &item.ProviderID, &item.CredentialVersion); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) CredentialSecret(ctx context.Context, accountID, credentialID uuid.UUID) (Credential, []byte, error) {
	var credential Credential
	var ciphertext []byte
	var keyID string
	err := r.pool.QueryRow(ctx, `SELECT id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at FROM provider_credentials WHERE id=$1 AND token_account_id=$2 AND status='ACTIVE'`, credentialID, accountID).Scan(&credential.ID, &credential.TokenAccountID, &credential.Provider, &credential.BaseURL, &ciphertext, &keyID, &credential.Fingerprint, &credential.Version, &credential.Status, &credential.AllowedModels, &credential.ValidatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, nil, ErrNotFound
	}
	if err != nil {
		return Credential{}, nil, err
	}
	plaintext, err := r.secrets.DecryptWithKeyID(keyID, ciphertext, []byte(accountID.String()+":"+credentialID.String()))
	if err != nil {
		return Credential{}, nil, err
	}
	return credential, plaintext, nil
}

func validOwner(request CreateAccountRequest) bool {
	switch request.OwnerType {
	case "USER":
		return request.OwnerID != uuid.Nil && request.OwnerOrganization == nil && request.OwnerUnit == nil
	case "ORG":
		return request.OwnerID != uuid.Nil && request.OwnerOrganization != nil && request.OwnerID == *request.OwnerOrganization && request.OwnerUnit == nil
	case "ORG_UNIT":
		return request.OwnerID != uuid.Nil && request.OwnerOrganization != nil && request.OwnerUnit != nil && request.OwnerID == *request.OwnerUnit
	default:
		return false
	}
}

func validShareScope(scope string) bool {
	return slices.Contains([]string{"OWNER_UNIT_ONLY", "OWNER_SUBTREE", "ORG_ALL", "EXPLICIT_MEMBERS"}, scope)
}

func normalizeModels(models []string) []string {
	result := make([]string, 0, len(models))
	seen := map[string]bool{}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model != "" && !seen[model] {
			seen[model] = true
			result = append(result, model)
		}
	}
	slices.Sort(result)
	return result
}
