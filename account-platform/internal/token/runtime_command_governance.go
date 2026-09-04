package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ExecuteRuntimeTokenCommand keeps the generic result, final CAS, bounded
// domain queue write and both audit records in one Token transaction. A CAS
// failure therefore leaves neither a PENDING generic result nor a domain
// command behind. It never invokes the model Provider.
type runtimeTokenCommandBegin struct {
	tokenCommandBegin
	RequestDigest          string
	KeyDigest              string
	IdempotencyFingerprint string
}

func (r *PostgresRepository) ExecuteRuntimeTokenCommand(ctx context.Context, input runtimeTokenCommandBegin) (TokenCommandResult, bool, error) {
	canonical := input.Envelope
	requestDigest, keyDigest, fingerprint := input.RequestDigest, input.KeyDigest, input.IdempotencyFingerprint
	if input.ActorUserID == uuid.Nil || input.AuthorizationVersion < 1 || strings.TrimSpace(input.ActorPermission) == "" || strings.TrimSpace(input.AuditRequestID) == "" || !isRuntimeGovernedCommand(canonical.CommandType) || len(requestDigest) != 64 || len(keyDigest) != 64 || len(fingerprint) != 64 {
		return TokenCommandResult{}, false, ErrTokenCommandInvalid
	}
	policy, err := approvalpolicy.Lookup(canonical.CommandType)
	if err != nil || policy.Mode != approvalpolicy.AuditOnly {
		return TokenCommandResult{}, false, ErrTokenCommandInvalid
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	defer tx.Rollback(ctx)
	if existing, existingErr := runtimeExistingTokenCommand(ctx, tx, input.ActorUserID, canonical.CommandType, keyDigest, fingerprint); existingErr == nil {
		return existing, false, tx.Commit(ctx)
	} else if !errors.Is(existingErr, ErrNotFound) {
		return TokenCommandResult{}, false, existingErr
	}
	command, err := parseRuntimeGovernedCommand(canonical.CommandType, canonical.TargetResourceID, canonical.Command)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if err = lockRuntimeCommandResource(ctx, tx, canonical.CommandType, command.ResourceID); err != nil {
		return TokenCommandResult{}, false, err
	}
	// A same-key peer may have committed while this transaction waited for the
	// resource lock. Re-read before computing or writing any domain fact.
	if existing, existingErr := runtimeExistingTokenCommand(ctx, tx, input.ActorUserID, canonical.CommandType, keyDigest, fingerprint); existingErr == nil {
		return existing, false, tx.Commit(ctx)
	} else if !errors.Is(existingErr, ErrNotFound) {
		return TokenCommandResult{}, false, existingErr
	}
	authoritativeBefore, precheckFingerprint, err := runtimeCommandPrecheckInTx(ctx, tx, canonical)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if canonical.PrecheckFingerprint != precheckFingerprint || !tokenCommandExpectedVersionsMatch(canonical.ExpectedVersions, authoritativeBefore) {
		return TokenCommandResult{}, false, ErrCatalogVersionConflict
	}
	reference := uuid.Must(uuid.NewV7())
	result, err := tx.Exec(ctx, `INSERT INTO token_command_results(command_reference,command_type,policy_version,actor_user_id,actor_permission,authorization_version,idempotency_key_digest,idempotency_fingerprint,request_digest,target_resource_type,target_resource_id,reason,expected_versions,precheck_fingerprint,status,before_state,audit_request_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'PENDING',$15,$16) ON CONFLICT(actor_user_id,command_type,idempotency_key_digest) DO NOTHING`, reference, canonical.CommandType, policy.PolicyVersion, input.ActorUserID, input.ActorPermission, input.AuthorizationVersion, keyDigest, fingerprint, requestDigest, canonical.TargetResourceType, canonical.TargetResourceID, canonical.Reason, canonical.ExpectedVersions, canonical.PrecheckFingerprint, authoritativeBefore, input.AuditRequestID)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	if result.RowsAffected() == 0 {
		existing, existingErr := runtimeExistingTokenCommand(ctx, tx, input.ActorUserID, canonical.CommandType, keyDigest, fingerprint)
		if existingErr != nil {
			return TokenCommandResult{}, false, existingErr
		}
		return existing, false, tx.Commit(ctx)
	}
	var after, redacted json.RawMessage
	switch canonical.CommandType {
	case "TOKEN_ROUTE_PROVISIONING_REPAIR":
		_, after, redacted, err = r.executeGovernedProvisioningRepairTx(ctx, tx, reference, input.ActorUserID, canonical.Reason, command)
	case "TOKEN_USAGE_RECONCILIATION_RETRY":
		_, after, redacted, err = r.executeGovernedUsageReconciliationTx(ctx, tx, reference, input.ActorUserID, canonical.Reason, command)
	default:
		err = ErrTokenCommandInvalid
	}
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	item, err := completeRuntimeTokenCommandTx(ctx, tx, input.tokenCommandBegin, canonical, reference, fingerprint, authoritativeBefore, after, redacted)
	if err != nil {
		return TokenCommandResult{}, false, err
	}
	return item, true, tx.Commit(ctx)
}

func runtimeExistingTokenCommand(ctx context.Context, tx pgx.Tx, actor uuid.UUID, commandType, keyDigest, fingerprint string) (TokenCommandResult, error) {
	item, err := scanTokenCommandResult(tx.QueryRow(ctx, tokenCommandResultSelect+` WHERE actor_user_id=$1 AND command_type=$2 AND idempotency_key_digest=$3`, actor, commandType, keyDigest))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenCommandResult{}, ErrNotFound
	}
	if err != nil {
		return TokenCommandResult{}, err
	}
	if item.IdempotencyFingerprint != fingerprint {
		return TokenCommandResult{}, ErrTokenCommandResultMismatch
	}
	return item, nil
}

func lockRuntimeCommandResource(ctx context.Context, tx pgx.Tx, commandType string, resourceID uuid.UUID) error {
	var locked uuid.UUID
	var err error
	switch commandType {
	case "TOKEN_ROUTE_PROVISIONING_REPAIR":
		err = tx.QueryRow(ctx, `SELECT id FROM route_provisionings WHERE id=$1 FOR UPDATE`, resourceID).Scan(&locked)
	case "TOKEN_USAGE_RECONCILIATION_RETRY":
		err = tx.QueryRow(ctx, `SELECT id FROM usage_requests WHERE id=$1 FOR UPDATE`, resourceID).Scan(&locked)
	default:
		return ErrTokenCommandInvalid
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func runtimeCommandPrecheckInTx(ctx context.Context, tx pgx.Tx, envelope TokenCommandEnvelope) (json.RawMessage, string, error) {
	snapshot, err := loadCommandPrecheckSnapshot(ctx, tx, envelope.CommandType, envelope.TargetResourceID, envelope.Command)
	if err != nil {
		return nil, "", err
	}
	if len(snapshot.Blockers) > 0 {
		return nil, "", ErrCatalogVersionConflict
	}
	expected, err := json.Marshal(snapshot.ExpectedVersions)
	if err != nil {
		return nil, "", err
	}
	command, err := canonicalJSON(envelope.Command)
	if err != nil {
		return nil, "", ErrTokenCommandInvalid
	}
	values := []string{envelope.CommandType, envelope.TargetResourceType, envelope.TargetResourceID, string(expected), string(command)}
	values = append(values, snapshot.EvidenceValues...)
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return expected, hex.EncodeToString(digest[:]), nil
}

func completeRuntimeTokenCommandTx(ctx context.Context, tx pgx.Tx, input tokenCommandBegin, envelope TokenCommandEnvelope, reference uuid.UUID, idempotencyFingerprint string, beforeState, afterState, redactedResult json.RawMessage) (TokenCommandResult, error) {
	result, err := tx.Exec(ctx, `UPDATE token_command_results SET status='SUCCEEDED',after_state=$2,redacted_result=$3,stable_error_code=NULL,completed_at=transaction_timestamp() WHERE command_reference=$1 AND status='PENDING'`, reference, nullableJSONObject(afterState), nullableJSONObject(redactedResult))
	if err != nil {
		return TokenCommandResult{}, err
	}
	if result.RowsAffected() != 1 {
		return TokenCommandResult{}, ErrTokenCommandResultFinal
	}
	policy, err := approvalpolicy.Lookup(envelope.CommandType)
	if err != nil {
		return TokenCommandResult{}, err
	}
	resourceID := uuid.NewSHA1(uuid.NameSpaceURL, []byte(envelope.TargetResourceType+"\x00"+envelope.TargetResourceID))
	summary, _ := json.Marshal(map[string]any{
		"command_reference": reference, "command_type": envelope.CommandType, "policy_version": policy.PolicyVersion, "permission": input.ActorPermission,
		"authorization_version": input.AuthorizationVersion, "reason": envelope.Reason, "idempotency_fingerprint": idempotencyFingerprint,
		"precheck_fingerprint": envelope.PrecheckFingerprint, "before_state": beforeState, "after_state": nullableJSONObject(afterState),
		"result": nullableJSONObject(redactedResult), "status": "SUCCEEDED", "stable_error_code": "",
		"approval_request_id": nil, "request_id": input.AuditRequestID,
	})
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return TokenCommandResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO token_audit_logs(id,actor_user_id,action,resource_type,resource_id,summary,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, uuid.Must(uuid.NewV7()), input.ActorUserID, envelope.CommandType, envelope.TargetResourceType, resourceID, summary, now); err != nil {
		return TokenCommandResult{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at) VALUES($1,'token.management-command.executed.v1',$2,1,$3,$4,$5)`, uuid.Must(uuid.NewV7()), resourceID, summary, input.AuditRequestID, now); err != nil {
		return TokenCommandResult{}, err
	}
	return scanTokenCommandResult(tx.QueryRow(ctx, tokenCommandResultSelect+` WHERE command_reference=$1`, reference))
}

func (r *PostgresRepository) executeGovernedProvisioningRepairTx(ctx context.Context, tx pgx.Tx, reference, actor uuid.UUID, reason string, command runtimeGovernedCommand) (json.RawMessage, json.RawMessage, json.RawMessage, error) {
	var status string
	var desiredGeneration int64
	var repairRound int
	var updatedAt time.Time
	if err := tx.QueryRow(ctx, `SELECT status,desired_generation,repair_round,updated_at FROM route_provisionings WHERE id=$1 FOR UPDATE`, command.ResourceID).Scan(&status, &desiredGeneration, &repairRound, &updatedAt); errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil, ErrNotFound
	} else if err != nil {
		return nil, nil, nil, err
	}
	storageKey := "governed:" + reference.String()
	var storedResult json.RawMessage
	var storedProvisioning uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT provisioning_id,result_snapshot FROM route_provisioning_commands WHERE idempotency_key=$1 AND actor_id=$2 AND command_type='PROVISIONING_REPAIR_GOVERNED'`, storageKey, actor).Scan(&storedProvisioning, &storedResult); err == nil {
		if storedProvisioning != command.ResourceID {
			return nil, nil, nil, ErrTokenCommandResultMismatch
		}
		return storedResult, storedResult, storedResult, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil, err
	}
	if !updatedAt.Equal(command.ExpectedVersion) || desiredGeneration != command.TargetGeneration || status != "FAILED" && status != "REPAIR_REQUIRED" {
		return nil, nil, nil, ErrCatalogVersionConflict
	}
	before := runtimeProvisioningResult(command.ResourceID, status, repairRound, desiredGeneration)
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return nil, nil, nil, err
	}
	repairRound++
	result, err := tx.Exec(ctx, `UPDATE route_provisionings SET status='PENDING',stage='MANUAL_REPAIR',repair_round=$2,attempt_count=0,sla_deadline_at=$3,next_retry_at=$4,lease_owner=NULL,lease_until=NULL,last_error_code=NULL,last_error_detail=NULL,updated_at=$4 WHERE id=$1 AND updated_at=$5 AND desired_generation=$6 AND status IN ('FAILED','REPAIR_REQUIRED')`, command.ResourceID, repairRound, now.Add(30*time.Minute), now, command.ExpectedVersion, command.TargetGeneration)
	if err != nil {
		return nil, nil, nil, err
	}
	if result.RowsAffected() != 1 {
		return nil, nil, nil, ErrCatalogVersionConflict
	}
	after := runtimeProvisioningResult(command.ResourceID, "PENDING", repairRound, desiredGeneration)
	digest := runtimeCommandDigest(reference, envelopeRuntimeEvidence(command))
	if _, err = tx.Exec(ctx, `INSERT INTO route_provisioning_commands(idempotency_key,command_type,request_digest,provisioning_id,repair_round,actor_id,reason,result_snapshot,created_at) VALUES($1,'PROVISIONING_REPAIR_GOVERNED',$2,$3,$4,$5,'AUDIT_ONLY',$6,$7)`, storageKey, digest, command.ResourceID, repairRound, actor, after, now); err != nil {
		return nil, nil, nil, err
	}
	evidence, _ := json.Marshal(map[string]any{"command_reference": reference, "repair_round": repairRound, "replay_model_request": false})
	if _, err = tx.Exec(ctx, `INSERT INTO route_provisioning_audit(id,provisioning_id,action,before_status,after_status,desired_generation,actor_id,reason,evidence,created_at) VALUES($1,$2,'GOVERNED_REPAIR_REQUESTED',$3,'PENDING',$4,$5,$6,$7,$8)`, uuid.Must(uuid.NewV7()), command.ResourceID, status, desiredGeneration, actor, reason, evidence, now); err != nil {
		return nil, nil, nil, err
	}
	return before, after, after, nil
}

func (r *PostgresRepository) executeGovernedUsageReconciliationTx(ctx context.Context, tx pgx.Tx, reference, actor uuid.UUID, reason string, command runtimeGovernedCommand) (json.RawMessage, json.RawMessage, json.RawMessage, error) {
	var status string
	var highWater int64
	var updatedAt time.Time
	var debitProhibited bool
	if err := tx.QueryRow(ctx, `SELECT u.status,u.last_persisted_sequence,u.updated_at,EXISTS(SELECT 1 FROM usage_platform_loss_reviews loss WHERE loss.usage_request_id=u.id AND loss.customer_debit_prohibited) FROM usage_requests u WHERE u.id=$1 FOR UPDATE`, command.ResourceID).Scan(&status, &highWater, &updatedAt, &debitProhibited); errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil, ErrNotFound
	} else if err != nil {
		return nil, nil, nil, err
	}
	keyDigest := runtimeCommandDigest(reference, nil)
	var storedUsage uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT usage_request_id FROM usage_reconciliation_commands WHERE actor_user_id=$1 AND idempotency_key_digest=$2`, actor, keyDigest).Scan(&storedUsage); err == nil {
		if storedUsage != command.ResourceID {
			return nil, nil, nil, ErrTokenCommandResultMismatch
		}
		result := runtimeUsageResult(command.ResourceID, "QUEUED", command.EvidenceHighWatermark)
		return result, result, result, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil, err
	}
	if !updatedAt.Equal(command.ExpectedVersion) || highWater != command.EvidenceHighWatermark || status != "SETTLEMENT_PENDING" && status != "REVIEW" || debitProhibited {
		return nil, nil, nil, ErrCatalogVersionConflict
	}
	before := runtimeUsageResult(command.ResourceID, status, highWater)
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&now); err != nil {
		return nil, nil, nil, err
	}
	requestFingerprint := runtimeCommandDigest(reference, envelopeRuntimeEvidence(command))
	if _, err := tx.Exec(ctx, `INSERT INTO usage_reconciliation_commands(id,usage_request_id,actor_user_id,idempotency_key_digest,request_fingerprint,reason,created_at) VALUES($1,$2,$3,$4,$5,'AUDIT_ONLY',$6)`, uuid.Must(uuid.NewV7()), command.ResourceID, actor, keyDigest, requestFingerprint, now); err != nil {
		return nil, nil, nil, err
	}
	result, err := tx.Exec(ctx, `UPDATE usage_requests SET reconciliation_next_at=$2,reconciliation_lease_until=NULL,reconciliation_last_error='MANUAL_RECONCILIATION_REQUESTED',reconciliation_stopped_at=NULL,updated_at=$2 WHERE id=$1 AND updated_at=$3 AND last_persisted_sequence=$4 AND status IN ('SETTLEMENT_PENDING','REVIEW') AND NOT EXISTS(SELECT 1 FROM usage_platform_loss_reviews loss WHERE loss.usage_request_id=usage_requests.id AND loss.customer_debit_prohibited)`, command.ResourceID, now, command.ExpectedVersion, command.EvidenceHighWatermark)
	if err != nil {
		return nil, nil, nil, err
	}
	if result.RowsAffected() != 1 {
		return nil, nil, nil, ErrCatalogVersionConflict
	}
	if err = recordTokenAudit(ctx, tx, actor, "USAGE_RECONCILIATION_GOVERNED", "USAGE_REQUEST", command.ResourceID, `jsonb_build_object('reason',$6,'command_reference',$7,'replay_model_request',false)`, reason, reference); err != nil {
		return nil, nil, nil, err
	}
	after := runtimeUsageResult(command.ResourceID, "QUEUED", highWater)
	return before, after, after, nil
}

func runtimeProvisioningResult(id uuid.UUID, status string, repairRound int, targetGeneration int64) json.RawMessage {
	result, _ := json.Marshal(map[string]any{"provisioning_id": id, "status": status, "repair_round": repairRound, "target_generation": targetGeneration, "replay_model_request": false})
	return result
}

func runtimeUsageResult(id uuid.UUID, status string, evidenceHighWatermark int64) json.RawMessage {
	result, _ := json.Marshal(map[string]any{"usage_request_id": id, "status": status, "evidence_high_watermark": evidenceHighWatermark, "replay_model_request": false})
	return result
}

func runtimeCommandDigest(reference uuid.UUID, evidence []string) string {
	payload := reference.String()
	for _, item := range evidence {
		payload += "\x00" + item
	}
	digest := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(digest[:])
}

func envelopeRuntimeEvidence(command runtimeGovernedCommand) []string {
	return []string{command.ResourceID.String(), command.ExpectedVersion.Format(time.RFC3339Nano), strconv.FormatInt(command.TargetGeneration, 10), strconv.FormatInt(command.EvidenceHighWatermark, 10), "replay_model_request=false"}
}
