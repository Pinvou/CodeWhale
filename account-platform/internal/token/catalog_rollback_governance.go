package token

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type governedCatalogRollbackCommand struct {
	SourceCatalogVersion int64 `json:"source_catalog_version"`
}

type governedCatalogRollbackCritical struct {
	CurrentCatalogVersion      int64  `json:"current_catalog_version"`
	SourceCatalogVersion       int64  `json:"source_catalog_version"`
	CatalogDiffFingerprint     string `json:"catalog_diff_fingerprint"`
	BillingPriceVersion        int64  `json:"billing_price_version"`
	RouteSetFingerprint        string `json:"route_set_fingerprint"`
	ProvisioningSetFingerprint string `json:"provisioning_set_fingerprint"`
	InflightRequestFingerprint string `json:"inflight_request_set_fingerprint"`
	PolicyVersion              int64  `json:"policy_version"`
}

func parseGovernedCatalogRollback(envelope TokenCommandEnvelope) (governedCatalogRollbackCommand, governedCatalogRollbackCritical, error) {
	var command governedCatalogRollbackCommand
	var critical governedCatalogRollbackCritical
	if envelope.CommandType != "TOKEN_MODEL_CATALOG_ROLLBACK" || envelope.TargetResourceType != "MODEL_CATALOG" ||
		decodeCommand(envelope.Command, &command) != nil || decodeCommand(envelope.ExpectedVersions, &critical) != nil ||
		command.SourceCatalogVersion < 1 || envelope.TargetResourceID != strconv.FormatInt(command.SourceCatalogVersion, 10) ||
		critical.CurrentCatalogVersion < 1 || critical.SourceCatalogVersion != command.SourceCatalogVersion || critical.BillingPriceVersion < 1 || critical.PolicyVersion != 2 ||
		!validImpactFingerprint(critical.CatalogDiffFingerprint) || !validImpactFingerprint(critical.RouteSetFingerprint) ||
		!validImpactFingerprint(critical.ProvisioningSetFingerprint) || !validImpactFingerprint(critical.InflightRequestFingerprint) {
		return governedCatalogRollbackCommand{}, governedCatalogRollbackCritical{}, ErrTokenCommandInvalid
	}
	return command, critical, nil
}

func sameGovernedCatalogRollbackCritical(left, right governedCatalogRollbackCritical) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	return leftErr == nil && rightErr == nil && string(leftJSON) == string(rightJSON)
}

func rollbackCriticalFromEvidence(sourceVersion int64, evidence ImpactLocalEvidence, billingVersion int64) governedCatalogRollbackCritical {
	current := int64(0)
	if evidence.CurrentVersion.CatalogVersion != nil {
		current = *evidence.CurrentVersion.CatalogVersion
	}
	return governedCatalogRollbackCritical{
		CurrentCatalogVersion: current, SourceCatalogVersion: sourceVersion,
		CatalogDiffFingerprint: evidence.CatalogDiffFingerprint, BillingPriceVersion: billingVersion,
		RouteSetFingerprint: evidence.RouteSetFingerprint, ProvisioningSetFingerprint: evidence.ProvisioningSetFingerprint,
		InflightRequestFingerprint: evidence.InflightSetFingerprint, PolicyVersion: 2,
	}
}

func (e *PostgresApprovedCredentialExecutor) precheckApprovedCatalogRollback(ctx context.Context, envelope TokenCommandEnvelope) (json.RawMessage, error) {
	command, expected, err := parseGovernedCatalogRollback(envelope)
	if err != nil {
		return nil, err
	}
	evidence, err := e.repository.CatalogRollbackImpact(ctx, command.SourceCatalogVersion)
	if err != nil {
		return nil, err
	}
	current := rollbackCriticalFromEvidence(command.SourceCatalogVersion, evidence, expected.BillingPriceVersion)
	if evidence.Dependencies.InflightRequests != 0 || !sameGovernedCatalogRollbackCritical(expected, current) {
		return nil, ErrCatalogVersionConflict
	}
	return json.Marshal(current)
}

// ExecuteApprovedCatalogRollbackCommand claims the approved command and applies
// the catalog effect in the same serializable Token transaction. This is the
// APT-138 adapter seam; PRE_ACTIVATION does not register a consumer for it.
func (e *PostgresApprovedCredentialExecutor) ExecuteApprovedCatalogRollbackCommand(ctx context.Context, input runtimeTokenCommandBegin, capabilities []GatewayProtocolCapability) (TokenCommandResult, bool, error) {
	envelope := input.Envelope
	command, expected, err := parseGovernedCatalogRollback(envelope)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	policy, err := approvalpolicy.Lookup(envelope.CommandType)
	if err != nil || policy.Mode != approvalpolicy.ApprovalRequired || input.ApprovalRequestID == uuid.Nil || strings.TrimSpace(input.ApprovalFingerprint) == "" || input.ActorUserID == uuid.Nil || input.AuthorizationVersion < 1 || len(input.RequestDigest) != 64 || len(input.KeyDigest) != 64 || len(input.IdempotencyFingerprint) != 64 {
		return TokenCommandResult{}, false, ErrTokenCommandInvalid
	}
	tx, err := e.repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	defer tx.Rollback(ctx)
	if existing, existingErr := runtimeExistingTokenCommand(ctx, tx, input.ActorUserID, envelope.CommandType, input.KeyDigest, input.IdempotencyFingerprint); existingErr == nil {
		return existing, false, tx.Commit(ctx)
	} else if !errors.Is(existingErr, ErrNotFound) {
		return TokenCommandResult{}, false, existingErr
	}
	if err = lockCatalogPublication(ctx, tx); err != nil {
		return TokenCommandResult{}, false, err
	}
	// A same-key peer may have completed while this transaction waited for the
	// publication lock. Re-read before checking or writing any fact.
	if existing, existingErr := runtimeExistingTokenCommand(ctx, tx, input.ActorUserID, envelope.CommandType, input.KeyDigest, input.IdempotencyFingerprint); existingErr == nil {
		return existing, false, tx.Commit(ctx)
	} else if !errors.Is(existingErr, ErrNotFound) {
		return TokenCommandResult{}, false, existingErr
	}
	current, inflight, err := catalogRollbackCriticalTx(ctx, tx, command.SourceCatalogVersion, expected.BillingPriceVersion)
	if err != nil || inflight != 0 || !sameGovernedCatalogRollbackCritical(expected, current) {
		if err == nil {
			err = ErrCatalogVersionConflict
		}
		return TokenCommandResult{}, false, err
	}
	if err = e.repository.recheckTokenRouteActivation(ctx, tx); err != nil {
		return TokenCommandResult{}, false, err
	}
	reference := uuid.Must(uuid.NewV7())
	result, err := tx.Exec(ctx, `INSERT INTO token_command_results(command_reference,command_type,policy_version,actor_user_id,actor_permission,authorization_version,idempotency_key_digest,idempotency_fingerprint,request_digest,target_resource_type,target_resource_id,reason,expected_versions,precheck_fingerprint,approval_request_id,approval_command_fingerprint,status,before_state,audit_request_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'PENDING',$17,$18) ON CONFLICT(actor_user_id,command_type,idempotency_key_digest) DO NOTHING`, reference, envelope.CommandType, policy.PolicyVersion, input.ActorUserID, input.ActorPermission, input.AuthorizationVersion, input.KeyDigest, input.IdempotencyFingerprint, input.RequestDigest, envelope.TargetResourceType, envelope.TargetResourceID, envelope.Reason, envelope.ExpectedVersions, envelope.PrecheckFingerprint, input.ApprovalRequestID, strings.ToLower(strings.TrimSpace(input.ApprovalFingerprint)), input.BeforeState, input.AuditRequestID)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if result.RowsAffected() == 0 {
		existing, existingErr := runtimeExistingTokenCommand(ctx, tx, input.ActorUserID, envelope.CommandType, input.KeyDigest, input.IdempotencyFingerprint)
		if existingErr != nil {
			return TokenCommandResult{}, false, existingErr
		}
		return existing, false, tx.Commit(ctx)
	}
	version, err := rollbackCatalogEffect(ctx, tx, command.SourceCatalogVersion, expected.CurrentCatalogVersion, envelope.Reason, input.ActorUserID, capabilities, e.repository.now())
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	after, _ := json.Marshal(map[string]any{"catalog_version": version, "source_catalog_version": command.SourceCatalogVersion})
	completed, err := completeRuntimeTokenCommandTx(ctx, tx, input.tokenCommandBegin, envelope, reference, input.IdempotencyFingerprint, input.BeforeState, after, after)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	return completed, true, tx.Commit(ctx)
}

func catalogRollbackCriticalTx(ctx context.Context, tx pgx.Tx, sourceVersion, billingVersion int64) (governedCatalogRollbackCritical, int64, error) {
	var current int64
	if err := tx.QueryRow(ctx, `SELECT max(version) FROM model_catalog_versions`).Scan(&current); err != nil || current < 1 {
		return governedCatalogRollbackCritical{}, 0, ErrCatalogVersionConflict
	}
	ids, err := catalogRollbackAffectedModelIDs(ctx, tx, sourceVersion, current)
	if err != nil {
		return governedCatalogRollbackCritical{}, 0, err
	}
	critical := governedCatalogRollbackCritical{CurrentCatalogVersion: current, SourceCatalogVersion: sourceVersion, BillingPriceVersion: billingVersion, PolicyVersion: 2}
	critical.CatalogDiffFingerprint, err = impactRowsFingerprint(ctx, tx, `SELECT jsonb_build_array(catalog_version,model_id,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available,replacement_model_id,new_session_cutoff_at,continuation_cutoff_at,force_cancel_inflight)::text FROM model_catalog_entries WHERE catalog_version=ANY($1::bigint[]) ORDER BY catalog_version,model_id`, []int64{sourceVersion, current})
	if err == nil {
		critical.RouteSetFingerprint, err = impactRowsFingerprint(ctx, tx, `SELECT jsonb_build_array(route.id,route.model_id,route.protocol_version_id,route.route_version,route.route_generation,route.reference_status,route.health_status)::text FROM routing_policies route WHERE route.model_id=ANY($1::uuid[]) ORDER BY route.id`, ids)
	}
	if err == nil {
		critical.ProvisioningSetFingerprint, err = impactRowsFingerprint(ctx, tx, `SELECT jsonb_build_array(id,status,desired_generation,actual_generation,lease_until,active_token_account_id,active_credential_id,protocol_version_id)::text FROM route_provisionings WHERE model_id=ANY($1::uuid[]) ORDER BY id`, ids)
	}
	if err == nil {
		critical.InflightRequestFingerprint, err = impactRowsFingerprint(ctx, tx, `SELECT jsonb_build_array(id,session_id,status,execution_status,execution_version)::text FROM usage_requests WHERE model_id=ANY($1::uuid[]) AND execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING') ORDER BY id`, ids)
	}
	var inflight int64
	if err == nil {
		err = tx.QueryRow(ctx, `SELECT count(*) FROM usage_requests WHERE model_id=ANY($1::uuid[]) AND execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING')`, ids).Scan(&inflight)
	}
	if err != nil || strings.TrimSpace(critical.CatalogDiffFingerprint) == "" {
		return governedCatalogRollbackCritical{}, 0, err
	}
	return critical, inflight, nil
}

// Rollback can remove models that exist only in the current catalog. Both the
// preview and execution must bind their routes, tasks and in-flight requests.
// Price coverage remains scoped separately to the target catalog.
func catalogRollbackAffectedModelIDs(ctx context.Context, tx pgx.Tx, sourceVersion, currentVersion int64) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT model_id FROM model_catalog_entries WHERE catalog_version=ANY($1::bigint[]) AND lifecycle_status<>'REMOVED' ORDER BY model_id LIMIT $2`, []int64{sourceVersion, currentVersion}, maximumImpactModels+1)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 || len(ids) > maximumImpactModels {
		return nil, ErrImpactScopeTooLarge
	}
	return ids, nil
}
