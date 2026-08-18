package token

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
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
	TokenAccountID uuid.UUID `json:"token_account_id"`
	Model          string    `json:"model"`
	Priority       int       `json:"priority"`
	DefaultScope   string    `json:"default_scope"`
	HealthStatus   string    `json:"health_status"`
	UpdatedAt      time.Time `json:"updated_at"`
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
	CreatedBy         uuid.UUID
}

type PostgresRepository struct {
	pool    *pgxpool.Pool
	secrets SecretProvider
	now     func() time.Time
}

type Repository interface {
	CreateAccount(context.Context, CreateAccountRequest) (TokenAccount, error)
	GetAccount(context.Context, uuid.UUID) (TokenAccount, error)
	ListManagedAccounts(context.Context, AuthorizationContext) ([]TokenAccount, error)
	ListCredentials(context.Context, uuid.UUID) ([]Credential, error)
	RotateCredential(context.Context, uuid.UUID, uuid.UUID, string, string, []byte, []string) (Credential, error)
	SetAccountStatus(context.Context, uuid.UUID, string, uuid.UUID, string) (TokenAccount, error)
	RevokeCredential(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string) (Credential, error)
	UpdateSharePolicy(context.Context, uuid.UUID, string, []uuid.UUID, uuid.UUID) (TokenAccount, error)
	UpsertRoute(context.Context, uuid.UUID, string, int, string, string, uuid.UUID) (RoutingPolicy, error)
	ListCandidates(context.Context, string) ([]Candidate, error)
	CredentialSecret(context.Context, uuid.UUID, uuid.UUID) (Credential, []byte, error)
	ListUsageForContext(context.Context, AuthorizationContext, int, *UsageHistoryCursor) ([]UserUsageRecord, error)
	UsageStore
	Ping(context.Context) error
	Close()
}

func (r *PostgresRepository) BeginUsage(ctx context.Context, request UsageRequest, snapshot map[string]any) (UsageRequest, bool, error) {
	if request.ID == uuid.Nil || len(strings.TrimSpace(request.IdempotencyKey)) < 16 || request.UserID == uuid.Nil || request.SessionID == "" || request.TokenAccountID == uuid.Nil || request.CredentialID == uuid.Nil || request.WalletID == uuid.Nil || request.Model == "" || request.MaximumMicrocredits <= 0 {
		return UsageRequest{}, false, ErrForbidden
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return UsageRequest{}, false, err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return UsageRequest{}, false, err
	}
	defer tx.Rollback(ctx)
	existing, err := getUsageByIdempotency(ctx, tx, request.IdempotencyKey, true)
	if err == nil {
		if existing.UserID != request.UserID || existing.SessionID != request.SessionID || existing.TokenAccountID != request.TokenAccountID || existing.WalletID != request.WalletID || existing.Model != request.Model || existing.MaximumMicrocredits != request.MaximumMicrocredits {
			return UsageRequest{}, false, ErrForbidden
		}
		return existing, false, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return UsageRequest{}, false, err
	}
	now := r.now()
	request.Status, request.StartedAt = "CREATED", now
	_, err = tx.Exec(ctx, `INSERT INTO usage_requests(id,idempotency_key,user_id,session_id,organization_id,token_account_id,credential_id,wallet_id,model,status,maximum_microcredits,request_snapshot,started_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'CREATED',$10,$11,$12,$12)`, request.ID, request.IdempotencyKey, request.UserID, request.SessionID, request.OrganizationID, request.TokenAccountID, request.CredentialID, request.WalletID, request.Model, request.MaximumMicrocredits, encoded, now)
	if err != nil {
		return UsageRequest{}, false, err
	}
	return request, true, tx.Commit(ctx)
}

func (r *PostgresRepository) GetUsage(ctx context.Context, usageID uuid.UUID) (UsageRequest, error) {
	query := `SELECT id,idempotency_key,user_id,session_id,organization_id,token_account_id,credential_id,wallet_id,price_version_id,hold_id,model,status,maximum_microcredits,actual_microcredits,error_class,started_at,updated_at FROM usage_requests WHERE id=$1`
	var usage UsageRequest
	err := r.pool.QueryRow(ctx, query, usageID).Scan(&usage.ID, &usage.IdempotencyKey, &usage.UserID, &usage.SessionID, &usage.OrganizationID, &usage.TokenAccountID, &usage.CredentialID, &usage.WalletID, &usage.PriceVersionID, &usage.HoldID, &usage.Model, &usage.Status, &usage.MaximumMicrocredits, &usage.ActualMicrocredits, &usage.ErrorClass, &usage.StartedAt, &usage.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return UsageRequest{}, ErrNotFound
	}
	return usage, err
}

func (r *PostgresRepository) ListUsageForContext(ctx context.Context, resolved AuthorizationContext, limit int, cursor *UsageHistoryCursor) ([]UserUsageRecord, error) {
	if resolved.UserID == uuid.Nil || limit < 1 || limit > 101 {
		return nil, ErrForbidden
	}
	query := `SELECT id,model,status,actual_microcredits,usage_snapshot,started_at,terminal_at,updated_at
FROM usage_requests
WHERE user_id=$1 AND organization_id IS NULL
ORDER BY started_at DESC,id DESC LIMIT $2`
	arguments := []any{resolved.UserID, limit}
	if cursor != nil {
		query = `SELECT id,model,status,actual_microcredits,usage_snapshot,started_at,terminal_at,updated_at
FROM usage_requests
WHERE user_id=$1 AND organization_id IS NULL AND (started_at,id)<($2,$3)
ORDER BY started_at DESC,id DESC LIMIT $4`
		arguments = []any{resolved.UserID, cursor.StartedAt, cursor.ID, limit}
	}
	if resolved.ContextType == "ORGANIZATION" && resolved.OrganizationID != nil {
		query = `SELECT id,model,status,actual_microcredits,usage_snapshot,started_at,terminal_at,updated_at
FROM usage_requests
WHERE user_id=$1 AND organization_id=$2
ORDER BY started_at DESC,id DESC LIMIT $3`
		arguments = []any{resolved.UserID, *resolved.OrganizationID, limit}
		if cursor != nil {
			query = `SELECT id,model,status,actual_microcredits,usage_snapshot,started_at,terminal_at,updated_at
FROM usage_requests
WHERE user_id=$1 AND organization_id=$2 AND (started_at,id)<($3,$4)
ORDER BY started_at DESC,id DESC LIMIT $5`
			arguments = []any{resolved.UserID, *resolved.OrganizationID, cursor.StartedAt, cursor.ID, limit}
		}
	} else if resolved.ContextType != "PERSONAL" {
		return nil, ErrForbidden
	}
	rows, err := r.pool.Query(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]UserUsageRecord, 0)
	for rows.Next() {
		var item UserUsageRecord
		var snapshot []byte
		if err := rows.Scan(&item.UsageRequestID, &item.Model, &item.status, &item.ActualMicrocredits, &snapshot, &item.StartedAt, &item.CompletedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		if len(snapshot) > 0 {
			var payload struct {
				Usage *Usage `json:"usage"`
			}
			if json.Unmarshal(snapshot, &payload) == nil && payload.Usage != nil {
				input, output, cached := payload.Usage.InputTokens, payload.Usage.OutputTokens, payload.Usage.CachedInputTokens
				item.InputTokens, item.OutputTokens, item.CachedInputTokens = &input, &output, &cached
			}
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) MarkReserved(ctx context.Context, usageID, holdID, priceVersionID uuid.UUID) error {
	command, err := r.pool.Exec(ctx, `UPDATE usage_requests SET status='RESERVED',hold_id=$2,price_version_id=$3,updated_at=$4 WHERE id=$1 AND status='CREATED'`, usageID, holdID, priceVersionID, r.now())
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return r.ensureUsageState(ctx, usageID, "RESERVED", "UPSTREAM_ACTIVE", "SETTLEMENT_PENDING", "COMMITTED")
	}
	return nil
}

func (r *PostgresRepository) MarkUpstream(ctx context.Context, usageID uuid.UUID) error {
	command, err := r.pool.Exec(ctx, `UPDATE usage_requests SET status='UPSTREAM_ACTIVE',updated_at=$2 WHERE id=$1 AND status='RESERVED'`, usageID, r.now())
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return r.ensureUsageState(ctx, usageID, "UPSTREAM_ACTIVE", "SETTLEMENT_PENDING", "COMMITTED")
	}
	return nil
}

func (r *PostgresRepository) RecordUpstreamRequest(ctx context.Context, usageID uuid.UUID, newAPIRequestID string) error {
	newAPIRequestID = strings.TrimSpace(newAPIRequestID)
	if usageID == uuid.Nil || newAPIRequestID == "" || len(newAPIRequestID) > 128 {
		return ErrForbidden
	}
	command, err := r.pool.Exec(ctx, `INSERT INTO usage_attempts(id,usage_request_id,newapi_request_id,status,started_at)
VALUES($1,$2,$3,'STARTED',$4)
ON CONFLICT(usage_request_id) DO UPDATE SET newapi_request_id=EXCLUDED.newapi_request_id
WHERE usage_attempts.newapi_request_id=EXCLUDED.newapi_request_id`, uuid.Must(uuid.NewV7()), usageID, newAPIRequestID, r.now())
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrForbidden
	}
	return nil
}

func (r *PostgresRepository) MarkCommitted(ctx context.Context, usageID uuid.UUID, actual int64, snapshot map[string]any) error {
	if usageID == uuid.Nil || actual < 0 {
		return ErrForbidden
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now := r.now()
	var walletID, tokenAccountID uuid.UUID
	err = tx.QueryRow(ctx, `UPDATE usage_requests SET status='COMMITTED',actual_microcredits=$2,usage_snapshot=$3,terminal_at=$4,updated_at=$4,reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,reconciliation_stopped_at=NULL WHERE id=$1 AND status IN ('UPSTREAM_ACTIVE','SETTLEMENT_PENDING','REVIEW') RETURNING wallet_id,token_account_id`, usageID, actual, encoded, now).Scan(&walletID, &tokenAccountID)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE usage_attempts SET status='SUCCEEDED',ended_at=$2 WHERE usage_request_id=$1 AND status IN ('STARTED','STREAMING')`, usageID, now)
	}
	if err == nil {
		if err := events.AppendOutbox(ctx, tx, "token.usage.settled.v1", usageID, 1, map[string]any{"usage_request_id": usageID, "wallet_id": walletID, "token_account_id": tokenAccountID, "actual_microcredits": actual}); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var status string
	var storedActual *int64
	if err := tx.QueryRow(ctx, `SELECT status,actual_microcredits FROM usage_requests WHERE id=$1`, usageID).Scan(&status, &storedActual); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if status != "COMMITTED" || storedActual == nil || *storedActual != actual {
		return ErrForbidden
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) MarkReleased(ctx context.Context, usageID uuid.UUID, reason string) error {
	now := r.now()
	command, err := r.pool.Exec(ctx, `UPDATE usage_requests SET status='RELEASED',error_class=$2,terminal_at=$3,updated_at=$3,reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,reconciliation_stopped_at=NULL WHERE id=$1 AND status IN ('CREATED','RESERVED','UPSTREAM_ACTIVE','SETTLEMENT_PENDING','REVIEW')`, usageID, strings.TrimSpace(reason), now)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return r.ensureUsageState(ctx, usageID, "RELEASED")
	}
	_, err = r.pool.Exec(ctx, `UPDATE usage_attempts SET status='FAILED',error_class=$2,ended_at=$3 WHERE usage_request_id=$1 AND status IN ('STARTED','STREAMING')`, usageID, strings.TrimSpace(reason), now)
	if err != nil {
		return err
	}
	return nil
}

func (r *PostgresRepository) MarkReview(ctx context.Context, usageID uuid.UUID, reason string, snapshot map[string]any) error {
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	status := "REVIEW"
	if reason == "settlement_pending" || reason == "local_commit_record_pending" {
		status = "SETTLEMENT_PENDING"
	}
	_, err = r.pool.Exec(ctx, `UPDATE usage_requests SET status=$2,error_class=$3,usage_snapshot=$4,reconciliation_next_at=$5,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,reconciliation_stopped_at=NULL,updated_at=$5 WHERE id=$1 AND status NOT IN ('COMMITTED','RELEASED')`, usageID, status, reason, encoded, r.now())
	return err
}

func (r *PostgresRepository) ensureUsageState(ctx context.Context, usageID uuid.UUID, allowed ...string) error {
	var status string
	if err := r.pool.QueryRow(ctx, `SELECT status FROM usage_requests WHERE id=$1`, usageID).Scan(&status); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if !slices.Contains(allowed, status) {
		return ErrForbidden
	}
	return nil
}

func getUsageByIdempotency(ctx context.Context, database interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, key string, lock bool) (UsageRequest, error) {
	query := `SELECT id,idempotency_key,user_id,session_id,organization_id,token_account_id,credential_id,wallet_id,price_version_id,hold_id,model,status,maximum_microcredits,actual_microcredits,error_class,started_at,updated_at FROM usage_requests WHERE idempotency_key=$1`
	if lock {
		query += ` FOR UPDATE`
	}
	var usage UsageRequest
	err := database.QueryRow(ctx, query, key).Scan(&usage.ID, &usage.IdempotencyKey, &usage.UserID, &usage.SessionID, &usage.OrganizationID, &usage.TokenAccountID, &usage.CredentialID, &usage.WalletID, &usage.PriceVersionID, &usage.HoldID, &usage.Model, &usage.Status, &usage.MaximumMicrocredits, &usage.ActualMicrocredits, &usage.ErrorClass, &usage.StartedAt, &usage.UpdatedAt)
	return usage, err
}

func NewPostgresRepository(ctx context.Context, databaseURL string, secrets SecretProvider) (*PostgresRepository, error) {
	if secrets == nil {
		return nil, ErrInvalidSecret
	}
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open token database: %w", err)
	}
	repository := &PostgresRepository{pool: pool, secrets: secrets, now: func() time.Time { return time.Now().UTC() }}
	if err := repository.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return repository, nil
}

func (r *PostgresRepository) Ping(ctx context.Context) error { return r.pool.Ping(ctx) }
func (r *PostgresRepository) Close()                         { r.pool.Close() }

func (r *PostgresRepository) CreateAccount(ctx context.Context, request CreateAccountRequest) (TokenAccount, error) {
	request.OwnerType = strings.ToUpper(strings.TrimSpace(request.OwnerType))
	request.ShareScope = strings.ToUpper(strings.TrimSpace(request.ShareScope))
	request.Name = strings.TrimSpace(request.Name)
	if !validOwner(request) || !validShareScope(request.ShareScope) || request.WalletID == uuid.Nil || request.CreatedBy == uuid.Nil || request.Name == "" || request.DefaultPriority < 0 || request.DefaultPriority > 10000 {
		return TokenAccount{}, ErrForbidden
	}
	now := r.now()
	account := TokenAccount{ID: uuid.Must(uuid.NewV7()), OwnerType: request.OwnerType, OwnerID: request.OwnerID, OwnerOrganization: request.OwnerOrganization, OwnerUnit: request.OwnerUnit, WalletID: request.WalletID, Name: request.Name, Status: "ACTIVE", ShareScope: request.ShareScope, DefaultPriority: request.DefaultPriority}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return TokenAccount{}, err
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,owner_organization_id,owner_unit_id,wallet_id,name,status,default_priority,created_by,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,'ACTIVE',$8,$9,$10,$10)`, account.ID, account.OwnerType, account.OwnerID, account.OwnerOrganization, account.OwnerUnit, account.WalletID, account.Name, account.DefaultPriority, request.CreatedBy, now)
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO token_share_policies(token_account_id,scope,owner_organization_id,owner_unit_id,updated_at) VALUES($1,$2,$3,$4,$5)`, account.ID, account.ShareScope, account.OwnerOrganization, account.OwnerUnit, now)
	}
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
	if err := tx.QueryRow(ctx, `SELECT id FROM token_accounts WHERE id=$1 AND status='ACTIVE' FOR UPDATE`, accountID).Scan(&exists); errors.Is(err, pgx.ErrNoRows) {
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
	_, err = tx.Exec(ctx, `INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'PENDING',$9,$10,$11,$10)`, credential.ID, accountID, provider, baseURL, ciphertext, r.secrets.KeyID(), credential.Fingerprint, nextVersion, models, now, actorID)
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE provider_credentials SET status='RETIRED',retired_at=$2 WHERE token_account_id=$1 AND status='ACTIVE'`, accountID, now)
	}
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE provider_credentials SET status='ACTIVE',activated_at=$2 WHERE id=$1`, credential.ID, now)
	}
	for _, model := range models {
		if err != nil {
			break
		}
		_, err = tx.Exec(ctx, `INSERT INTO routing_policies(id,token_account_id,model,priority,default_scope,health_status,updated_at) SELECT $1,id,$2,default_priority,'NONE','HEALTHY',$3 FROM token_accounts WHERE id=$4 ON CONFLICT(token_account_id,model) DO UPDATE SET health_status='HEALTHY',updated_at=EXCLUDED.updated_at`, uuid.Must(uuid.NewV7()), model, now, accountID)
	}
	if err != nil {
		return Credential{}, err
	}
	return credential, tx.Commit(ctx)
}

func (r *PostgresRepository) ListCandidates(ctx context.Context, model string) ([]Candidate, error) {
	rows, err := r.pool.Query(ctx, `
SELECT ta.id,ta.name,ta.owner_type,ta.owner_id,ta.owner_organization_id,ta.owner_unit_id,ta.wallet_id,ta.status,
       sp.scope,pc.allowed_models,
       ARRAY(SELECT em.user_id FROM token_explicit_members em WHERE em.token_account_id=ta.id ORDER BY em.user_id),
       rp.default_scope,rp.priority,rp.health_status,pc.id
FROM token_accounts ta
JOIN token_share_policies sp ON sp.token_account_id=ta.id
JOIN provider_credentials pc ON pc.token_account_id=ta.id AND pc.status='ACTIVE'
JOIN routing_policies rp ON rp.token_account_id=ta.id AND rp.model=$1
WHERE ta.status='ACTIVE'
ORDER BY rp.priority DESC,ta.id`, model)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Candidate, 0)
	for rows.Next() {
		var item Candidate
		if err := rows.Scan(&item.TokenAccountID, &item.Name, &item.OwnerType, &item.OwnerID, &item.OwnerOrganization, &item.OwnerUnit, &item.WalletID, &item.Status, &item.ShareScope, &item.AllowedModels, &item.ExplicitMembers, &item.DefaultScope, &item.Priority, &item.HealthStatus, &item.CredentialID); err != nil {
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
	if keyID != r.secrets.KeyID() {
		return Credential{}, nil, ErrInvalidSecret
	}
	plaintext, err := r.secrets.Decrypt(ciphertext, []byte(accountID.String()+":"+credentialID.String()))
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
