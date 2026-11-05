package token

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type StableRouteInput struct {
	ModelID                uuid.UUID
	ProtocolVersionID      uuid.UUID
	Priority               int
	DefaultScope           string
	ExpectedAccountVersion int64
	ExpectedRouteVersion   int64
	IdempotencyKey         string
	Reason                 string
}

func scanRoutingPolicy(row interface{ Scan(...any) error }) (RoutingPolicy, error) {
	var item RoutingPolicy
	err := row.Scan(&item.ID, &item.TokenAccountID, &item.Model, &item.ModelID, &item.ProtocolVersionID, &item.ProtocolCode, &item.Priority, &item.DefaultScope, &item.ReferenceStatus, &item.HealthStatus, &item.HealthDiagnostic, &item.HealthObservedAt, &item.HealthExpiresAt, &item.RouteVersion, &item.RouteGeneration, &item.UpdatedAt)
	return item, err
}

const routingPolicyProjection = `SELECT id,token_account_id,model,model_id,protocol_version_id,protocol_code,priority,default_scope,reference_status,health_status,COALESCE(health_diagnostic_code,''),health_observed_at,health_expires_at,route_version,route_generation,updated_at FROM routing_policies`

func stableRouteResult(item RoutingPolicy, accountVersion int64) (StableRouteResult, error) {
	if item.ReferenceStatus != "RESOLVED" || item.ModelID == nil || item.ProtocolVersionID == nil || accountVersion < 1 {
		return StableRouteResult{}, ErrRouteReferenceInvalid
	}
	return StableRouteResult{ID: item.ID, TokenAccountID: item.TokenAccountID, ModelID: *item.ModelID, ProtocolVersionID: *item.ProtocolVersionID, ProtocolCode: item.ProtocolCode, Priority: item.Priority, DefaultScope: item.DefaultScope, HealthStatus: item.HealthStatus, HealthDiagnostic: item.HealthDiagnostic, HealthObservedAt: item.HealthObservedAt, HealthExpiresAt: item.HealthExpiresAt, RouteVersion: item.RouteVersion, RouteGeneration: item.RouteGeneration, AccountVersion: accountVersion, UpdatedAt: item.UpdatedAt}, nil
}

type stableRouteReference struct {
	ModelID             uuid.UUID
	ProtocolVersionID   uuid.UUID
	CatalogVersion      int64
	CapabilityVersionID uuid.UUID
	UpstreamModel       string
	ProtocolCode        string
	Provider            string
	AdapterVersion      string
}

func resolveStableRouteReference(ctx context.Context, database interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, modelID, protocolVersionID uuid.UUID) (stableRouteReference, error) {
	var result stableRouteReference
	result.ModelID, result.ProtocolVersionID = modelID, protocolVersionID
	err := database.QueryRow(ctx, `SELECT current.version,entry.model_capability_version_id,identity.upstream_model_ref,protocol.protocol_code,identity.provider_id,identity.adapter_version
FROM model_catalog_versions current
JOIN model_catalog_entries entry ON entry.catalog_version=current.version AND entry.model_id=$1 AND entry.lifecycle_status='PUBLISHED' AND entry.source_available=true
JOIN model_identities identity ON identity.id=entry.model_id
JOIN model_capability_versions capability ON capability.id=entry.model_capability_version_id AND capability.model_id=entry.model_id AND capability.protocol_definition_id=$2
JOIN protocol_definitions protocol ON protocol.id=$2 AND protocol.id=entry.protocol_definition_id AND protocol.status='PUBLISHED'
WHERE current.version=(SELECT max(version) FROM model_catalog_versions)`, modelID, protocolVersionID).Scan(&result.CatalogVersion, &result.CapabilityVersionID, &result.UpstreamModel, &result.ProtocolCode, &result.Provider, &result.AdapterVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return stableRouteReference{}, ErrRouteReferenceInvalid
	}
	return result, err
}

// upsertStableRouteTx shares only the domain effect. Its caller owns the
// transaction, idempotency result, and audit/outbox boundary.
func (r *PostgresRepository) upsertStableRouteTx(ctx context.Context, tx pgx.Tx, accountID uuid.UUID, input StableRouteInput, actorID uuid.UUID, legacyAudit bool) (StableRouteResult, error) {
	account, err := getTokenAccount(ctx, tx, accountID, true)
	if errors.Is(err, pgx.ErrNoRows) || account.ManagementMode != "PLATFORM" {
		return StableRouteResult{}, ErrNotFound
	}
	if err != nil {
		return StableRouteResult{}, err
	}
	var accountVersion int64
	if err = tx.QueryRow(ctx, `SELECT management_version FROM token_accounts WHERE id=$1`, accountID).Scan(&accountVersion); err != nil {
		return StableRouteResult{}, err
	}
	if accountVersion != input.ExpectedAccountVersion {
		return StableRouteResult{}, ErrRouteVersionConflict
	}
	if !defaultScopeAllowed(account.OwnerType, input.DefaultScope) {
		return StableRouteResult{}, ErrForbidden
	}
	reference, err := resolveStableRouteReference(ctx, tx, input.ModelID, input.ProtocolVersionID)
	if err != nil {
		return StableRouteResult{}, err
	}
	var allowed bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM provider_credentials WHERE token_account_id=$1 AND provider=$2 AND status='ACTIVE' AND validation_status='VALID' AND $3=ANY(allowed_models))`, accountID, reference.Provider, reference.UpstreamModel).Scan(&allowed); err != nil {
		return StableRouteResult{}, err
	}
	if !allowed {
		return StableRouteResult{}, ErrRouteReferenceInvalid
	}
	now := r.now().UTC()
	existing, loadErr := scanRoutingPolicy(tx.QueryRow(ctx, routingPolicyProjection+` WHERE token_account_id=$1 AND ((reference_status='RESOLVED' AND model_id=$2 AND protocol_version_id=$3) OR (reference_status='LEGACY_UNRESOLVED' AND model=$4)) ORDER BY CASE reference_status WHEN 'RESOLVED' THEN 0 ELSE 1 END,id LIMIT 1 FOR UPDATE`, accountID, input.ModelID, input.ProtocolVersionID, reference.UpstreamModel))
	var routeID uuid.UUID
	if errors.Is(loadErr, pgx.ErrNoRows) {
		if input.ExpectedRouteVersion != 0 {
			return StableRouteResult{}, ErrRouteVersionConflict
		}
		routeID = uuid.Must(uuid.NewV7())
		_, err = tx.Exec(ctx, `INSERT INTO routing_policies(id,token_account_id,model,protocol_code,priority,default_scope,health_status,updated_at,updated_by,route_generation,model_id,protocol_version_id,reference_status,route_version,health_version,health_diagnostic_code,next_probe_at)
VALUES($1,$2,$3,$4,$5,$6,'UNKNOWN',$7,$8,1,$9,$10,'RESOLVED',1,1,'AWAITING_INITIAL_PROBE',$7)`, routeID, accountID, reference.UpstreamModel, reference.ProtocolCode, input.Priority, input.DefaultScope, now, actorID, input.ModelID, input.ProtocolVersionID)
	} else if loadErr != nil {
		return StableRouteResult{}, loadErr
	} else {
		if existing.RouteVersion != input.ExpectedRouteVersion {
			return StableRouteResult{}, ErrRouteVersionConflict
		}
		routeID = existing.ID
		var command pgconn.CommandTag
		command, err = tx.Exec(ctx, `UPDATE routing_policies SET model=$2,protocol_code=$3,model_id=$9,protocol_version_id=$10,reference_status='RESOLVED',priority=$4,default_scope=$5,health_status='UNKNOWN',health_diagnostic_code='ROUTE_CONFIGURATION_CHANGED',health_observed_at=NULL,health_expires_at=NULL,next_probe_at=$6,probe_lease_owner=NULL,probe_lease_until=NULL,route_version=route_version+1,route_generation=route_generation+1,health_version=health_version+1,updated_at=$6,updated_by=$7 WHERE id=$1 AND route_version=$8`, routeID, reference.UpstreamModel, reference.ProtocolCode, input.Priority, input.DefaultScope, now, actorID, input.ExpectedRouteVersion, input.ModelID, input.ProtocolVersionID)
		if err == nil && command.RowsAffected() != 1 {
			return StableRouteResult{}, ErrRouteVersionConflict
		}
	}
	if err != nil {
		return StableRouteResult{}, err
	}
	accountCommand, err := tx.Exec(ctx, `UPDATE token_accounts SET management_version=management_version+1,updated_at=$2 WHERE id=$1 AND management_version=$3`, accountID, now, input.ExpectedAccountVersion)
	if err != nil {
		return StableRouteResult{}, err
	}
	if accountCommand.RowsAffected() != 1 {
		return StableRouteResult{}, ErrRouteVersionConflict
	}
	if legacyAudit {
		if err = recordTokenAudit(ctx, tx, actorID, "TOKEN_ROUTE_UPSERTED", "TOKEN_ACCOUNT", accountID, `jsonb_build_object('route_id',$6,'model_id',$7,'protocol_version_id',$8,'priority',$9,'default_scope',$10,'route_version',$11,'reason',$12)`, routeID, input.ModelID, input.ProtocolVersionID, input.Priority, input.DefaultScope, input.ExpectedRouteVersion+1, input.Reason); err != nil {
			return StableRouteResult{}, err
		}
	}
	item, err := scanRoutingPolicy(tx.QueryRow(ctx, routingPolicyProjection+` WHERE id=$1`, routeID))
	if err != nil {
		return StableRouteResult{}, err
	}
	result, err := stableRouteResult(item, input.ExpectedAccountVersion+1)
	if err != nil {
		return StableRouteResult{}, err
	}
	return result, nil
}

type StableRouteDeleteInput struct {
	RouteID                uuid.UUID
	ExpectedAccountVersion int64
	ExpectedRouteVersion   int64
	IdempotencyKey         string
	Reason                 string
}

func (r *PostgresRepository) deleteStableRouteTx(ctx context.Context, tx pgx.Tx, accountID uuid.UUID, input StableRouteDeleteInput, actorID uuid.UUID, legacyAudit bool) error {
	var err error
	var accountVersion int64
	if err = tx.QueryRow(ctx, `SELECT management_version FROM token_accounts WHERE id=$1 AND management_mode='PLATFORM' FOR UPDATE`, accountID).Scan(&accountVersion); errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if accountVersion != input.ExpectedAccountVersion {
		return ErrRouteVersionConflict
	}
	item, err := scanRoutingPolicy(tx.QueryRow(ctx, routingPolicyProjection+` WHERE id=$1 AND token_account_id=$2 FOR UPDATE`, input.RouteID, accountID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if item.ReferenceStatus != "RESOLVED" || item.ModelID == nil || item.ProtocolVersionID == nil || item.RouteVersion != input.ExpectedRouteVersion {
		return ErrRouteVersionConflict
	}
	now := r.now().UTC()
	if _, err = tx.Exec(ctx, `INSERT INTO routing_policy_tombstones(route_id,token_account_id,model_id,protocol_version_id,route_version,route_generation,deleted_by,reason,deleted_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, item.ID, accountID, *item.ModelID, *item.ProtocolVersionID, item.RouteVersion, item.RouteGeneration, actorID, input.Reason, now); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE routing_policies SET reference_status='DELETED',health_status='STALE',health_diagnostic_code='ROUTE_DELETED',health_observed_at=$3,health_expires_at=NULL,next_probe_at=NULL,probe_lease_owner=NULL,probe_lease_until=NULL,route_version=route_version+1,health_version=health_version+1,updated_at=$3,updated_by=$4 WHERE id=$1 AND route_version=$2`, item.ID, input.ExpectedRouteVersion, now, actorID)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrRouteVersionConflict
	}
	accountCommand, err := tx.Exec(ctx, `UPDATE token_accounts SET management_version=management_version+1,updated_at=$2 WHERE id=$1 AND management_version=$3`, accountID, now, input.ExpectedAccountVersion)
	if err != nil {
		return err
	}
	if accountCommand.RowsAffected() != 1 {
		return ErrRouteVersionConflict
	}
	if legacyAudit {
		if err = recordTokenAudit(ctx, tx, actorID, "TOKEN_ROUTE_DELETED", "TOKEN_ACCOUNT", accountID, `jsonb_build_object('route_id',$6,'model_id',$7,'protocol_version_id',$8,'route_version',$9,'reason',$10)`, item.ID, *item.ModelID, *item.ProtocolVersionID, item.RouteVersion, input.Reason); err != nil {
			return err
		}
	}
	return nil
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
