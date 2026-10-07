package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	if err := r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return TokenAccount{}, err
	}
	account, _, err := setAccountStatusEffect(ctx, tx, accountID, status, actorID, reason, r.now())
	if err != nil {
		return TokenAccount{}, err
	}
	return account, r.commitGovernedMutation(ctx, tx)
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

// UpsertRoute is the frozen v1 direct contract. APT-138 switches callers to
// the v2 governed route. Until then this path remains available and never
// creates a stable reference from untrusted text.
func (r *PostgresRepository) UpsertRoute(ctx context.Context, accountID uuid.UUID, model, protocolCode string, priority int, defaultScope, healthStatus string, actorID uuid.UUID) (RoutingPolicy, error) {
	model, protocolCode = strings.TrimSpace(model), strings.ToUpper(strings.TrimSpace(protocolCode))
	defaultScope, healthStatus = strings.ToUpper(strings.TrimSpace(defaultScope)), strings.ToUpper(strings.TrimSpace(healthStatus))
	if accountID == uuid.Nil || actorID == uuid.Nil || model == "" || protocolCode != "CHAT" || priority < 0 || priority > 10000 || !slices.Contains([]string{"NONE", "UNIT", "ORG"}, defaultScope) || !slices.Contains([]string{"HEALTHY", "DEGRADED", "UNHEALTHY"}, healthStatus) {
		return RoutingPolicy{}, ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return RoutingPolicy{}, err
	}
	defer tx.Rollback(ctx)
	if err = r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return RoutingPolicy{}, err
	}
	account, err := getTokenAccount(ctx, tx, accountID, true)
	if errors.Is(err, pgx.ErrNoRows) || account.ManagementMode != "PLATFORM" {
		return RoutingPolicy{}, ErrNotFound
	}
	if err != nil {
		return RoutingPolicy{}, err
	}
	if !defaultScopeAllowed(account.OwnerType, defaultScope) {
		return RoutingPolicy{}, ErrForbidden
	}
	var allowed bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM provider_credentials WHERE token_account_id=$1 AND status='ACTIVE' AND validation_status='VALID' AND $2=ANY(allowed_models))`, accountID, model).Scan(&allowed); err != nil {
		return RoutingPolicy{}, err
	}
	if !allowed {
		return RoutingPolicy{}, ErrNotFound
	}
	now := r.now().UTC()
	var routeID uuid.UUID
	var referenceStatus string
	var routeCount int
	if err = tx.QueryRow(ctx, `SELECT count(*) FROM routing_policies WHERE token_account_id=$1 AND model=$2 AND reference_status<>'DELETED'`, accountID, model).Scan(&routeCount); err != nil {
		return RoutingPolicy{}, err
	}
	if routeCount == 0 {
		routeID = uuid.Must(uuid.NewV7())
		_, err = tx.Exec(ctx, `INSERT INTO routing_policies(id,token_account_id,model,protocol_code,priority,default_scope,health_status,updated_at,updated_by,route_generation,reference_status,route_version,health_version) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,1,'LEGACY_UNRESOLVED',1,1)`, routeID, accountID, model, protocolCode, priority, defaultScope, healthStatus, now, actorID)
	} else if routeCount > 1 {
		// Multiple protocol-versioned routes deliberately make the text-only v1
		// mutation ambiguous; callers must wait for the v2 activation.
		return RoutingPolicy{}, ErrAmbiguousRoute
	} else if err = tx.QueryRow(ctx, `SELECT id,reference_status FROM routing_policies WHERE token_account_id=$1 AND model=$2 AND reference_status<>'DELETED' FOR UPDATE`, accountID, model).Scan(&routeID, &referenceStatus); err != nil {
		return RoutingPolicy{}, err
	} else if referenceStatus == "LEGACY_UNRESOLVED" {
		_, err = tx.Exec(ctx, `UPDATE routing_policies SET protocol_code=$2,priority=$3,default_scope=$4,health_status=$5,updated_at=$6,updated_by=$7,route_generation=route_generation+1,route_version=route_version+1 WHERE id=$1`, routeID, protocolCode, priority, defaultScope, healthStatus, now, actorID)
	} else {
		_, err = tx.Exec(ctx, `UPDATE routing_policies SET priority=$2,default_scope=$3,health_status='UNKNOWN',health_diagnostic_code='LEGACY_CONFIGURATION_CHANGED',health_observed_at=NULL,health_expires_at=NULL,next_probe_at=$4,updated_at=$4,updated_by=$5,route_generation=route_generation+1,route_version=route_version+1,health_version=health_version+1 WHERE id=$1`, routeID, priority, defaultScope, now, actorID)
	}
	if err != nil {
		return RoutingPolicy{}, err
	}
	if err = recordTokenAudit(ctx, tx, actorID, "TOKEN_ROUTE_UPSERTED", "TOKEN_ACCOUNT", accountID, `jsonb_build_object('model',$6,'protocol_code',$7,'priority',$8,'default_scope',$9,'health_status',$10)`, model, protocolCode, priority, defaultScope, healthStatus); err != nil {
		return RoutingPolicy{}, err
	}
	item, err := scanRoutingPolicy(tx.QueryRow(ctx, routingPolicyProjection+` WHERE id=$1`, routeID))
	if err != nil {
		return RoutingPolicy{}, err
	}
	if err = r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return RoutingPolicy{}, err
	}
	return item, tx.Commit(ctx)
}

func stableRouteDigest(command string, accountID uuid.UUID, input StableRouteInput) string {
	payload, _ := json.Marshal(map[string]any{"command_type": command, "token_account_id": accountID, "model_id": input.ModelID, "protocol_version_id": input.ProtocolVersionID, "priority": input.Priority, "default_scope": input.DefaultScope, "expected_account_version": input.ExpectedAccountVersion, "expected_route_version": input.ExpectedRouteVersion, "reason": input.Reason})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func validStableRouteInput(accountID, actorID uuid.UUID, input StableRouteInput) bool {
	input.DefaultScope = strings.ToUpper(strings.TrimSpace(input.DefaultScope))
	input.Reason = strings.TrimSpace(input.Reason)
	return accountID != uuid.Nil && actorID != uuid.Nil && input.ModelID != uuid.Nil && input.ProtocolVersionID != uuid.Nil && input.Priority >= 0 && input.Priority <= 10000 && slices.Contains([]string{"NONE", "UNIT", "ORG"}, input.DefaultScope) && input.ExpectedAccountVersion > 0 && input.ExpectedRouteVersion >= 0 && len(input.IdempotencyKey) >= 16 && len(input.IdempotencyKey) <= 128 && utf8.RuneCountInString(input.Reason) >= 10 && utf8.RuneCountInString(input.Reason) <= 2000
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

func (r *PostgresRepository) UpsertStableRoute(ctx context.Context, accountID uuid.UUID, input StableRouteInput, actorID uuid.UUID) (StableRouteResult, error) {
	input.DefaultScope, input.IdempotencyKey, input.Reason = strings.ToUpper(strings.TrimSpace(input.DefaultScope)), strings.TrimSpace(input.IdempotencyKey), strings.TrimSpace(input.Reason)
	if !validStableRouteInput(accountID, actorID, input) {
		return StableRouteResult{}, ErrForbidden
	}
	digest := stableRouteDigest("TOKEN_ROUTE_UPSERT", accountID, input)
	tx, release, err := r.beginTokenSerializedTransaction(ctx, "TOKEN_ROUTE_ACCOUNT:"+accountID.String())
	if err != nil {
		return StableRouteResult{}, err
	}
	defer release()
	if err = r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return StableRouteResult{}, err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "TOKEN_ROUTE:"+input.IdempotencyKey); err != nil {
		return StableRouteResult{}, err
	}
	var storedActor uuid.UUID
	var storedDigest string
	var snapshot []byte
	if err = tx.QueryRow(ctx, `SELECT actor_id,request_digest,result_snapshot FROM routing_policy_mutations WHERE idempotency_key=$1`, input.IdempotencyKey).Scan(&storedActor, &storedDigest, &snapshot); err == nil {
		if storedActor != actorID || storedDigest != digest {
			return StableRouteResult{}, ErrIdempotencyConflict
		}
		var replay StableRouteResult
		if json.Unmarshal(snapshot, &replay) != nil || replay.ID == uuid.Nil || replay.ModelID == uuid.Nil || replay.ProtocolVersionID == uuid.Nil {
			return StableRouteResult{}, ErrRouteVersionConflict
		}
		if err = r.recheckTokenRouteActivation(ctx, tx); err != nil {
			return StableRouteResult{}, err
		}
		return replay, tx.Commit(ctx)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return StableRouteResult{}, err
	}
	result, err := r.upsertStableRouteTx(ctx, tx, accountID, input, actorID, true)
	if err != nil {
		return StableRouteResult{}, err
	}
	snapshot, _ = json.Marshal(result)
	now := r.now().UTC()
	if _, err = tx.Exec(ctx, `INSERT INTO routing_policy_mutations(idempotency_key,actor_id,command_type,request_digest,route_id,result_snapshot,created_at,expires_at) VALUES($1,$2,'TOKEN_ROUTE_UPSERT',$3,$4,$5,$6::timestamptz,$6::timestamptz+interval '7 days')`, input.IdempotencyKey, actorID, digest, result.ID, snapshot, now); err != nil {
		return StableRouteResult{}, err
	}
	if err = r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return StableRouteResult{}, err
	}
	return result, tx.Commit(ctx)
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

func (r *PostgresRepository) DeleteStableRoute(ctx context.Context, accountID uuid.UUID, input StableRouteDeleteInput, actorID uuid.UUID) error {
	input.IdempotencyKey, input.Reason = strings.TrimSpace(input.IdempotencyKey), strings.TrimSpace(input.Reason)
	if accountID == uuid.Nil || input.RouteID == uuid.Nil || actorID == uuid.Nil || input.ExpectedAccountVersion <= 0 || input.ExpectedRouteVersion <= 0 || len(input.IdempotencyKey) < 16 || len(input.IdempotencyKey) > 128 || utf8.RuneCountInString(input.Reason) < 10 || utf8.RuneCountInString(input.Reason) > 2000 {
		return ErrForbidden
	}
	digestInput := StableRouteInput{ExpectedAccountVersion: input.ExpectedAccountVersion, ExpectedRouteVersion: input.ExpectedRouteVersion, IdempotencyKey: input.IdempotencyKey, Reason: input.Reason}
	digest := stableRouteDigest("TOKEN_ROUTE_DELETE:"+input.RouteID.String(), accountID, digestInput)
	tx, release, err := r.beginTokenSerializedTransaction(ctx, "TOKEN_ROUTE_ACCOUNT:"+accountID.String())
	if err != nil {
		return err
	}
	defer release()
	if err = r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "TOKEN_ROUTE:"+input.IdempotencyKey); err != nil {
		return err
	}
	var storedActor uuid.UUID
	var storedDigest string
	if err = tx.QueryRow(ctx, `SELECT actor_id,request_digest FROM routing_policy_mutations WHERE idempotency_key=$1`, input.IdempotencyKey).Scan(&storedActor, &storedDigest); err == nil {
		if storedActor != actorID || storedDigest != digest {
			return ErrIdempotencyConflict
		}
		if err = r.recheckTokenRouteActivation(ctx, tx); err != nil {
			return err
		}
		return tx.Commit(ctx)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err = r.deleteStableRouteTx(ctx, tx, accountID, input, actorID, true); err != nil {
		return err
	}
	snapshot, _ := json.Marshal(map[string]any{"route_id": input.RouteID, "deleted": true})
	now := r.now().UTC()
	if _, err = tx.Exec(ctx, `INSERT INTO routing_policy_mutations(idempotency_key,actor_id,command_type,request_digest,route_id,result_snapshot,created_at,expires_at) VALUES($1,$2,'TOKEN_ROUTE_DELETE',$3,$4,$5,$6::timestamptz,$6::timestamptz+interval '7 days')`, input.IdempotencyKey, actorID, digest, input.RouteID, snapshot, now); err != nil {
		return err
	}
	if err = r.recheckTokenRouteActivation(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
