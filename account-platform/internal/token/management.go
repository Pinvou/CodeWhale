package token

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (r *PostgresRepository) SetAccountStatus(ctx context.Context, accountID uuid.UUID, status string, actorID uuid.UUID, reason string) (TokenAccount, error) {
	status, reason = strings.ToUpper(strings.TrimSpace(status)), strings.TrimSpace(reason)
	if accountID == uuid.Nil || actorID == uuid.Nil || !slices.Contains([]string{"ACTIVE", "DISABLED"}, status) || reason == "" {
		return TokenAccount{}, ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return TokenAccount{}, err
	}
	defer tx.Rollback(ctx)
	account, err := getTokenAccount(ctx, tx, accountID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenAccount{}, ErrNotFound
	}
	if err != nil {
		return TokenAccount{}, err
	}
	if account.ManagementMode != "PLATFORM" || account.Status == "FROZEN" {
		return TokenAccount{}, ErrForbidden
	}
	if account.Status != status {
		now := r.now()
		command, err := tx.Exec(ctx, `UPDATE token_accounts SET status=$2,status_changed_by=$3,status_change_reason=$4,updated_at=$5 WHERE id=$1 AND status<>'FROZEN'`, accountID, status, actorID, reason, now)
		if err != nil {
			return TokenAccount{}, err
		}
		if command.RowsAffected() != 1 {
			return TokenAccount{}, ErrForbidden
		}
		account.Status = status
		if err := recordTokenAudit(ctx, tx, actorID, "TOKEN_ACCOUNT_STATUS_CHANGED", "TOKEN_ACCOUNT", accountID, `jsonb_build_object('status',$6,'reason',$7)`, status, reason); err != nil {
			return TokenAccount{}, err
		}
	}
	return account, tx.Commit(ctx)
}

func (r *PostgresRepository) RevokeCredential(ctx context.Context, accountID, credentialID, actorID uuid.UUID, reason string) (Credential, error) {
	reason = strings.TrimSpace(reason)
	if accountID == uuid.Nil || credentialID == uuid.Nil || actorID == uuid.Nil || reason == "" {
		return Credential{}, ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return Credential{}, err
	}
	defer tx.Rollback(ctx)
	var platformManaged bool
	if err := tx.QueryRow(ctx, `SELECT management_mode='PLATFORM' FROM token_accounts WHERE id=$1`, accountID).Scan(&platformManaged); err != nil || !platformManaged {
		return Credential{}, ErrNotFound
	}
	credential, err := getCredential(ctx, tx, accountID, credentialID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return Credential{}, ErrNotFound
	}
	if err != nil {
		return Credential{}, err
	}
	if credential.Status == "REVOKED" {
		return credential, tx.Commit(ctx)
	}
	if credential.Status != "ACTIVE" && credential.Status != "RETIRED" {
		return Credential{}, ErrForbidden
	}
	now := r.now()
	command, err := tx.Exec(ctx, `UPDATE provider_credentials SET status='REVOKED',revoked_at=$3,revoked_by=$4,revocation_reason=$5 WHERE id=$1 AND token_account_id=$2 AND status IN ('ACTIVE','RETIRED')`, credentialID, accountID, now, actorID, reason)
	if err != nil {
		return Credential{}, err
	}
	if command.RowsAffected() != 1 {
		return Credential{}, ErrForbidden
	}
	credential.Status = "REVOKED"
	return credential, tx.Commit(ctx)
}

func (r *PostgresRepository) UpdateSharePolicy(ctx context.Context, accountID uuid.UUID, scope string, members []uuid.UUID, actorID uuid.UUID) (TokenAccount, error) {
	scope = strings.ToUpper(strings.TrimSpace(scope))
	if accountID == uuid.Nil || actorID == uuid.Nil || !validShareScope(scope) {
		return TokenAccount{}, ErrForbidden
	}
	members = normalizeMemberIDs(members)
	if scope != "EXPLICIT_MEMBERS" && len(members) != 0 {
		return TokenAccount{}, ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return TokenAccount{}, err
	}
	defer tx.Rollback(ctx)
	account, err := getTokenAccount(ctx, tx, accountID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenAccount{}, ErrNotFound
	}
	if err != nil {
		return TokenAccount{}, err
	}
	if account.ManagementMode != "PLATFORM" {
		return TokenAccount{}, ErrNotFound
	}
	if !shareScopeAllowed(account.OwnerType, scope) {
		return TokenAccount{}, ErrForbidden
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `UPDATE token_share_policies SET scope=$2,updated_at=$3,updated_by=$4 WHERE token_account_id=$1`, accountID, scope, now, actorID); err != nil {
		return TokenAccount{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM token_explicit_members WHERE token_account_id=$1`, accountID); err != nil {
		return TokenAccount{}, err
	}
	for _, memberID := range members {
		if _, err := tx.Exec(ctx, `INSERT INTO token_explicit_members(token_account_id,user_id,created_at,created_by) VALUES($1,$2,$3,$4)`, accountID, memberID, now, actorID); err != nil {
			return TokenAccount{}, err
		}
	}
	account.ShareScope = scope
	return account, tx.Commit(ctx)
}

func (r *PostgresRepository) UpsertRoute(ctx context.Context, accountID uuid.UUID, model, protocolCode string, priority int, defaultScope, healthStatus string, actorID uuid.UUID) (RoutingPolicy, error) {
	model = strings.TrimSpace(model)
	protocolCode = strings.ToUpper(strings.TrimSpace(protocolCode))
	defaultScope, healthStatus = strings.ToUpper(strings.TrimSpace(defaultScope)), strings.ToUpper(strings.TrimSpace(healthStatus))
	if accountID == uuid.Nil || actorID == uuid.Nil || model == "" || protocolCode != "CHAT" || priority < 0 || priority > 10000 || !slices.Contains([]string{"NONE", "UNIT", "ORG"}, defaultScope) || !slices.Contains([]string{"HEALTHY", "DEGRADED", "UNHEALTHY"}, healthStatus) {
		return RoutingPolicy{}, ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return RoutingPolicy{}, err
	}
	defer tx.Rollback(ctx)
	account, err := getTokenAccount(ctx, tx, accountID, true)
	if errors.Is(err, pgx.ErrNoRows) {
		return RoutingPolicy{}, ErrNotFound
	}
	if err != nil {
		return RoutingPolicy{}, err
	}
	if account.ManagementMode != "PLATFORM" {
		return RoutingPolicy{}, ErrNotFound
	}
	if !defaultScopeAllowed(account.OwnerType, defaultScope) {
		return RoutingPolicy{}, ErrForbidden
	}
	var allowed bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM provider_credentials WHERE token_account_id=$1 AND status='ACTIVE' AND validation_status='VALID' AND $2=ANY(allowed_models))`, accountID, model).Scan(&allowed); err != nil {
		return RoutingPolicy{}, err
	}
	if !allowed {
		return RoutingPolicy{}, ErrNotFound
	}
	now := r.now()
	policy := RoutingPolicy{TokenAccountID: accountID, Model: model, ProtocolCode: protocolCode, Priority: priority, DefaultScope: defaultScope, HealthStatus: healthStatus, UpdatedAt: now}
	err = tx.QueryRow(ctx, `INSERT INTO routing_policies(id,token_account_id,model,protocol_code,priority,default_scope,health_status,updated_at,updated_by,route_generation) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,1) ON CONFLICT(token_account_id,model) DO UPDATE SET protocol_code=EXCLUDED.protocol_code,priority=EXCLUDED.priority,default_scope=EXCLUDED.default_scope,health_status=EXCLUDED.health_status,updated_at=EXCLUDED.updated_at,updated_by=EXCLUDED.updated_by,route_generation=routing_policies.route_generation+1 RETURNING route_generation`, uuid.Must(uuid.NewV7()), accountID, model, protocolCode, priority, defaultScope, healthStatus, now, actorID).Scan(&policy.RouteGeneration)
	if err != nil {
		return RoutingPolicy{}, err
	}
	if err := recordTokenAudit(ctx, tx, actorID, "TOKEN_ROUTE_UPSERTED", "TOKEN_ACCOUNT", accountID, `jsonb_build_object('model',$6,'protocol_code',$7,'priority',$8,'default_scope',$9,'health_status',$10)`, model, protocolCode, priority, defaultScope, healthStatus); err != nil {
		return RoutingPolicy{}, err
	}
	return policy, tx.Commit(ctx)
}

func getTokenAccount(ctx context.Context, database interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, accountID uuid.UUID, lock bool) (TokenAccount, error) {
	query := `SELECT ta.id,ta.owner_type,ta.owner_id,ta.owner_organization_id,ta.owner_unit_id,ta.wallet_id,ta.name,ta.status,sp.scope,ta.default_priority,ta.management_mode FROM token_accounts ta JOIN token_share_policies sp ON sp.token_account_id=ta.id WHERE ta.id=$1`
	if lock {
		query += ` FOR UPDATE OF ta,sp`
	}
	var account TokenAccount
	err := database.QueryRow(ctx, query, accountID).Scan(&account.ID, &account.OwnerType, &account.OwnerID, &account.OwnerOrganization, &account.OwnerUnit, &account.WalletID, &account.Name, &account.Status, &account.ShareScope, &account.DefaultPriority, &account.ManagementMode)
	return account, err
}

func getCredential(ctx context.Context, database interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, accountID, credentialID uuid.UUID, lock bool) (Credential, error) {
	query := `SELECT id,token_account_id,provider,base_url,fingerprint,version,status,allowed_models,validated_at FROM provider_credentials WHERE id=$1 AND token_account_id=$2`
	if lock {
		query += ` FOR UPDATE`
	}
	var credential Credential
	err := database.QueryRow(ctx, query, credentialID, accountID).Scan(&credential.ID, &credential.TokenAccountID, &credential.Provider, &credential.BaseURL, &credential.Fingerprint, &credential.Version, &credential.Status, &credential.AllowedModels, &credential.ValidatedAt)
	return credential, err
}

func normalizeMemberIDs(members []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]bool, len(members))
	result := make([]uuid.UUID, 0, len(members))
	for _, memberID := range members {
		if memberID != uuid.Nil && !seen[memberID] {
			seen[memberID] = true
			result = append(result, memberID)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].String() < result[j].String() })
	return result
}

func shareScopeAllowed(ownerType, scope string) bool {
	switch ownerType {
	case "USER":
		return scope == "OWNER_UNIT_ONLY"
	case "ORG":
		return scope == "ORG_ALL" || scope == "EXPLICIT_MEMBERS"
	case "ORG_UNIT":
		return validShareScope(scope)
	default:
		return false
	}
}

func defaultScopeAllowed(ownerType, scope string) bool {
	switch ownerType {
	case "USER":
		return scope == "NONE"
	case "ORG":
		return scope == "NONE" || scope == "ORG"
	case "ORG_UNIT":
		return scope == "NONE" || scope == "UNIT" || scope == "ORG"
	default:
		return false
	}
}
