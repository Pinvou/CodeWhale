package token

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type EmergencyCancellationTarget struct {
	ID              uuid.UUID
	UsageRequestID  uuid.UUID
	ExecutionStatus string
	// ClaimAttempt is the monotonically increasing ownership token returned by claim.
	ClaimAttempt int
}

type EmergencyModelCancellationWorker struct {
	repository *PostgresRepository
	service    *Service
	interval   time.Duration
	batchSize  int
}

const emergencyCancellationFailure = "model emergency cancellation failed"

// Keep the attempt fence separate: installed targets may have historical claims
// but have not yet used any of the new, bounded automatic retry budget.
const emergencyCancellationRetryBudget = 8

func NewEmergencyModelCancellationWorker(repository *PostgresRepository, service *Service, interval time.Duration) *EmergencyModelCancellationWorker {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &EmergencyModelCancellationWorker{repository: repository, service: service, interval: interval, batchSize: 100}
}

func (w *EmergencyModelCancellationWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		_ = w.runOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (w *EmergencyModelCancellationWorker) runOnce(ctx context.Context) error {
	if w == nil || w.repository == nil || w.service == nil || w.service.cancellationStore == nil || w.service.cancellationBilling == nil || w.service.cancellationAdapter == nil {
		reportTokenWorkerFailure(ctx, emergencyCancellationFailure, "configure", ErrForbidden)
		return ErrForbidden
	}
	if err := w.repository.syncEmergencyCancellationTargets(ctx); err != nil {
		reportTokenWorkerFailure(ctx, emergencyCancellationFailure, "sync_targets", err)
		return err
	}
	if err := w.repository.reviewExpiredEmergencyCancellations(ctx, w.batchSize); err != nil {
		reportTokenWorkerFailure(ctx, emergencyCancellationFailure, "review_expired_claims", err)
		return err
	}
	capability := w.service.cancellationAdapter.CancelCapability()
	for range w.batchSize {
		// Claim only what this worker can start now. Leasing the whole batch
		// would let its unstarted tail expire and spend the retry budget.
		targets, err := w.repository.claimEmergencyCancellations(ctx, 1)
		if err != nil {
			reportTokenWorkerFailure(ctx, emergencyCancellationFailure, "claim_targets", err)
			return err
		}
		if len(targets) == 0 {
			break
		}
		target := targets[0]
		preDispatch, terminal, requestErr := w.repository.requestEmergencyCancellation(ctx, target, capability, w.service.upstream.AdapterVersion())
		if requestErr != nil {
			w.retryTarget(ctx, target, "request_cancellation", requestErr)
			continue
		}
		aborted := false
		if !terminal {
			if preDispatch {
				_, requestErr = w.service.finishPreDispatchCancellation(ctx, target.UsageRequestID)
			} else {
				aborted = w.service.abortActiveUpstream(target.UsageRequestID)
			}
		}
		if requestErr != nil {
			w.retryTarget(ctx, target, "finish_pre_dispatch", requestErr)
			continue
		}
		if err := w.repository.recordEmergencyCancellationAttempt(ctx, target, capability, aborted, preDispatch, terminal); err != nil {
			w.retryTarget(ctx, target, "record_attempt", err)
		}
	}
	err := w.repository.finalizeEmergencyCancellations(ctx, w.batchSize)
	if err != nil {
		reportTokenWorkerFailure(ctx, emergencyCancellationFailure, "finalize_targets", err)
	}
	return err
}

func (w *EmergencyModelCancellationWorker) retryTarget(ctx context.Context, target EmergencyCancellationTarget, stage string, cause error) {
	err := w.repository.retryEmergencyCancellation(ctx, target, cause)
	reportTokenWorkerFailure(ctx, emergencyCancellationFailure, stage, cause)
	if err != nil {
		reportTokenWorkerFailure(ctx, emergencyCancellationFailure, "persist_retry", err)
	}
}

func (r *PostgresRepository) syncEmergencyCancellationTargets(ctx context.Context) error {
	now := r.now()
	_, err := r.pool.Exec(ctx, `INSERT INTO model_emergency_cancellations(id,policy_id,usage_request_id,status,requested_at,updated_at)
SELECT gen_random_uuid(),policy.id,request.id,'PENDING',$1,$1
FROM model_emergency_cancellation_policies policy
JOIN usage_requests request ON request.model_id=policy.model_id AND request.started_at<policy.effective_at
WHERE policy.effective_at<=$1 AND request.execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING')
ON CONFLICT(usage_request_id) DO NOTHING`, now)
	return err
}

func (r *PostgresRepository) claimEmergencyCancellations(ctx context.Context, limit int) ([]EmergencyCancellationTarget, error) {
	if limit < 1 || limit > 1000 {
		return nil, ErrForbidden
	}
	now := r.now()
	rows, err := r.pool.Query(ctx, `WITH candidates AS (
    SELECT cancellation.id FROM model_emergency_cancellations cancellation
    WHERE cancellation.status IN ('PENDING','LEASED') AND (cancellation.lease_until IS NULL OR cancellation.lease_until<$1)
    AND (cancellation.next_attempt_at IS NULL OR cancellation.next_attempt_at<=$1)
    AND cancellation.retry_attempts<$3
    AND NOT EXISTS (SELECT 1 FROM usage_requests request WHERE request.id=cancellation.usage_request_id
        AND ((request.cancellation_origin='USER' AND request.execution_status='REVIEW'
        AND request.cancel_status='CANCEL_REQUESTED' AND request.error_class='cancel_hold_recovery_unconfirmed')
        OR (`+pendingUnusedExpiryFenceSQL+`) OR (`+pendingUserUnusedIntentSQL+`)))
    ORDER BY cancellation.requested_at,cancellation.id FOR UPDATE SKIP LOCKED LIMIT $2
), claimed AS (
    UPDATE model_emergency_cancellations cancellation SET status='LEASED',lease_until=$1::timestamptz+interval '30 seconds',attempts=attempts+1,
        retry_attempts=retry_attempts+1,
        next_attempt_at=$1::timestamptz+interval '30 seconds'+make_interval(secs=>LEAST(300,5*(1<<LEAST(retry_attempts,6)))),
        updated_at=$1::timestamptz
    FROM candidates WHERE cancellation.id=candidates.id RETURNING cancellation.id,cancellation.usage_request_id,cancellation.attempts
)
SELECT claimed.id,claimed.usage_request_id,request.execution_status,claimed.attempts FROM claimed JOIN usage_requests request ON request.id=claimed.usage_request_id`, now, limit, emergencyCancellationRetryBudget)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]EmergencyCancellationTarget, 0)
	for rows.Next() {
		var item EmergencyCancellationTarget
		if err := rows.Scan(&item.ID, &item.UsageRequestID, &item.ExecutionStatus, &item.ClaimAttempt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) requestEmergencyCancellation(ctx context.Context, target EmergencyCancellationTarget, capability ProviderCancelCapability, adapterVersion string) (bool, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback(ctx)
	// Lock the target before usage, matching record's lock order. Reclaim
	// skips this locked target, and an older claim cannot write evidence.
	var ownedID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT usage_request_id FROM model_emergency_cancellations WHERE id=$1 AND status='LEASED' AND attempts=$2 AND attempts>0 FOR UPDATE`, target.ID, target.ClaimAttempt).Scan(&ownedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, false, ErrForbidden
		}
		return false, false, err
	}
	var usage UsageRequest
	qualifiedUsageColumns := "request." + strings.ReplaceAll(usageSelectColumns, ",", ",request.")
	if err := scanUsage(tx.QueryRow(ctx, `SELECT `+qualifiedUsageColumns+` FROM usage_requests request WHERE request.id=$1 FOR UPDATE OF request`, ownedID), &usage); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, false, ErrNotFound
		}
		return false, false, err
	}
	var cancellationOrigin string
	var requestAlreadyRecorded, expiryOwnsPending, userOwnsUnusedIntent bool
	if err := tx.QueryRow(ctx, `SELECT request.cancellation_origin,EXISTS(SELECT 1 FROM model_emergency_cancellation_evidence evidence WHERE evidence.emergency_cancellation_id=$2 AND evidence.evidence_type='REQUESTED'),`+pendingUnusedExpiryFenceSQL+`,`+pendingUserUnusedIntentSQL+` FROM usage_requests request WHERE request.id=$1`, usage.ID, target.ID).Scan(&cancellationOrigin, &requestAlreadyRecorded, &expiryOwnsPending, &userOwnsUnusedIntent); err != nil {
		return false, false, err
	}
	// Claiming and cancellation are separate transactions. A USER stop or
	// expiry fence may commit after the claim; let that original owner finish.
	if expiryOwnsPending || userOwnsUnusedIntent {
		return false, false, ErrForbidden
	}
	// The original USER proof owns this unconfirmed pre-dispatch release.
	// Keep its identity and state intact so the existing recovery can finish;
	// the MODEL target retries and observes the confirmed terminal result.
	if cancellationOrigin == "USER" && usage.ExecutionStatus == "REVIEW" && usage.CancelStatus == "CANCEL_REQUESTED" && usage.ErrorClass != nil && *usage.ErrorClass == "cancel_hold_recovery_unconfirmed" {
		return false, false, ErrForbidden
	}
	if cancellationOrigin == "MODEL_EMERGENCY" && requestAlreadyRecorded {
		if usage.ExecutionStatus == "REVIEW" && usage.HoldID != nil {
			if err := r.restoreEmergencyPreDispatchReview(ctx, tx, usage, target.ID); err != nil {
				return false, false, err
			}
		}
		preDispatch := usage.CancelStatus == "CANCEL_REQUESTED" || usage.CancelStatus == "LOCAL_CONFIRMED" || usage.ExecutionStatus == "CANCELLED_BEFORE_DISPATCH"
		return preDispatch, false, tx.Commit(ctx)
	}
	now := r.now()
	terminal := usage.ExecutionStatus == "SUCCEEDED" || usage.ExecutionStatus == "FAILED" || usage.ExecutionStatus == "CANCELLED_BEFORE_DISPATCH"
	preDispatch := usage.ExecutionStatus == "RECEIVED" || usage.ExecutionStatus == "HOLD_RESERVING" || usage.ExecutionStatus == "HOLD_RESERVED"
	cancelStatus := "ALREADY_TERMINAL"
	if preDispatch {
		cancelStatus = "CANCEL_REQUESTED"
	} else if !terminal {
		cancelStatus = "RESULT_UNKNOWN"
		if capability == ProviderCancelUnsupported {
			cancelStatus = "UNSUPPORTED"
		}
	}
	// Any confirmed local cancellation remains authoritative for expiry-proof
	// replay when its response was lost. MODEL only observes that terminal.
	keepLocalConfirmation := usage.ExecutionStatus == "CANCELLED_BEFORE_DISPATCH" && usage.CancelStatus == "LOCAL_CONFIRMED" && usage.SettlementStatus == "RELEASED"
	if _, err := tx.Exec(ctx, `UPDATE usage_requests SET cancel_status=CASE WHEN $5 THEN cancel_status ELSE $2 END,delivery_status=CASE WHEN $3 THEN delivery_status ELSE 'USER_STOPPED' END,cancellation_origin=CASE WHEN $5 THEN cancellation_origin ELSE 'MODEL_EMERGENCY' END,cancel_requested_at=COALESCE(cancel_requested_at,$4),user_stopped_at=CASE WHEN $3 THEN user_stopped_at ELSE COALESCE(user_stopped_at,$4) END,cancel_reconciliation_deadline=CASE WHEN $3 THEN cancel_reconciliation_deadline ELSE $4+make_interval(secs=>cancel_reconciliation_seconds) END,updated_at=$4 WHERE id=$1`, usage.ID, cancelStatus, terminal, now, keepLocalConfirmation); err != nil {
		return false, false, err
	}
	snapshot := map[string]any{"origin": "MODEL_EMERGENCY", "capability": capability, "pre_dispatch": preDispatch, "already_terminal": terminal, "provider_success_inferred": false}
	if _, err := tx.Exec(ctx, `INSERT INTO usage_cancellation_evidence(id,usage_request_id,evidence_type,adapter_version,sanitized_evidence,occurred_at) VALUES($1,$2,'MODEL_EMERGENCY_REQUESTED',$3,$4,$5)`, uuid.Must(uuid.NewV7()), usage.ID, strings.TrimSpace(adapterVersion), snapshot, now); err != nil {
		return false, false, err
	}
	if err := appendUsageConvergenceEvidenceTx(ctx, tx, usage.ID, "CANCELLATION", "GATEWAY", "RECORDED", "model-emergency-requested", snapshot, now); err != nil {
		return false, false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO model_emergency_cancellation_evidence(id,emergency_cancellation_id,evidence_type,snapshot,created_at) VALUES($1,$2,'REQUESTED',$3,$4)`, uuid.Must(uuid.NewV7()), target.ID, snapshot, now); err != nil {
		return false, false, err
	}
	return preDispatch, terminal, tx.Commit(ctx)
}

// A failed release acknowledgement moves the usage into REVIEW. Only the
// original immutable local pre-dispatch request may resume that release; a
// review label or a missing dispatch row alone is never sufficient evidence.
// The caller holds the usage lock, which also fences dispatch persistence.
func (r *PostgresRepository) restoreEmergencyPreDispatchReview(ctx context.Context, tx pgx.Tx, usage UsageRequest, cancellationID uuid.UUID) error {
	if usage.ErrorClass == nil || *usage.ErrorClass != "cancel_hold_recovery_unconfirmed" || usage.CancelStatus != "CANCEL_REQUESTED" || usage.HoldID == nil || *usage.HoldID == uuid.Nil || usage.PriceVersionID == nil || *usage.PriceVersionID == uuid.Nil || usage.WalletID == uuid.Nil || usage.MaximumMicrocredits < 0 || usage.ProviderTerminalAt != nil || usage.FinalUsageObservedAt != nil || usage.ActualMicrocredits != nil || usage.LastPersistedSequence != 0 || usage.LastClientConfirmedSequence != 0 {
		return ErrForbidden
	}
	var fenced bool
	err := tx.QueryRow(ctx, `SELECT request.cancellation_origin='MODEL_EMERGENCY'
 AND request.delivery_status='USER_STOPPED'
 AND request.cancel_requested_at=request.user_stopped_at
 AND NOT EXISTS(SELECT 1 FROM usage_dispatch_intents intent WHERE intent.usage_request_id=request.id)
 AND NOT EXISTS(SELECT 1 FROM usage_execution_transitions dispatched WHERE dispatched.usage_request_id=request.id AND dispatched.to_status IN ('DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING','SUCCEEDED','FAILED'))
 AND EXISTS(SELECT 1 FROM usage_execution_transitions review
 JOIN usage_execution_transitions ack ON ack.usage_request_id=review.usage_request_id
 WHERE review.usage_request_id=request.id AND review.from_status='HOLD_RESERVED' AND review.to_status='REVIEW'
 AND review.reason='cancel_hold_recovery_unconfirmed' AND review.to_version=request.execution_version
 AND ack.from_status IN ('HOLD_RESERVING','REVIEW') AND ack.to_status='HOLD_RESERVED'
 AND ack.reason='billing ensured one hold' AND ack.to_version<=review.from_version)
 AND EXISTS(SELECT 1 FROM model_emergency_cancellations cancellation
 JOIN model_emergency_cancellation_policies policy ON policy.id=cancellation.policy_id
 JOIN model_emergency_cancellation_evidence original ON original.emergency_cancellation_id=cancellation.id AND original.evidence_type='REQUESTED'
 JOIN usage_cancellation_evidence evidence ON evidence.usage_request_id=request.id AND evidence.evidence_type='MODEL_EMERGENCY_REQUESTED'
 WHERE cancellation.id=$2 AND cancellation.usage_request_id=request.id AND policy.model_id=request.model_id
 AND policy.effective_at<=original.created_at AND request.started_at<policy.effective_at
 AND evidence.occurred_at=original.created_at
 AND (original.created_at=request.cancel_requested_at OR (original.created_at>request.cancel_requested_at
 AND EXISTS(SELECT 1 FROM usage_cancel_commands command
 JOIN usage_cancellation_evidence user_evidence ON user_evidence.usage_request_id=command.usage_request_id
 WHERE command.usage_request_id=request.id AND command.user_id=request.user_id
 AND command.cancellation_version=1 AND command.request_fingerprint=$3
 AND command.created_at=request.cancel_requested_at AND user_evidence.occurred_at=command.created_at
 AND user_evidence.evidence_type='CANCEL_REQUESTED' AND btrim(user_evidence.adapter_version)<>''
 AND user_evidence.provider_id IS NULL AND user_evidence.upstream_reference_digest IS NULL
 AND user_evidence.sanitized_evidence=jsonb_build_object('capability','LOCAL_PRE_DISPATCH','pre_dispatch',true))))
 AND evidence.provider_id IS NULL AND evidence.upstream_reference_digest IS NULL AND btrim(evidence.adapter_version)<>''
 AND original.snapshot=evidence.sanitized_evidence
 AND original.snapshot=jsonb_build_object('origin','MODEL_EMERGENCY','capability',original.snapshot->>'capability','pre_dispatch',true,'already_terminal',false,'provider_success_inferred',false))
 FROM usage_requests request WHERE request.id=$1`, usage.ID, cancellationID, cancellationFingerprint(usage.ID)).Scan(&fenced)
	if err != nil {
		return err
	}
	if !fenced {
		return ErrForbidden
	}
	now := r.now()
	if err := transitionExecutionTx(ctx, tx, usage.ID, []string{"REVIEW"}, "HOLD_RESERVED", "retry fenced model emergency release", now); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE usage_requests SET status='RESERVED',settlement_status='HOLD_ACTIVE',updated_at=$2 WHERE id=$1`, usage.ID, now)
	return err
}

func (r *PostgresRepository) recordEmergencyCancellationAttempt(ctx context.Context, target EmergencyCancellationTarget, capability ProviderCancelCapability, aborted, preDispatch, terminal bool) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var usageID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT usage_request_id FROM model_emergency_cancellations WHERE id=$1 AND status='LEASED' AND attempts=$2 AND attempts>0 FOR UPDATE`, target.ID, target.ClaimAttempt).Scan(&usageID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrForbidden
		}
		return err
	}
	if preDispatch && !terminal {
		var execution, cancellation, settlement string
		if err := tx.QueryRow(ctx, `SELECT execution_status,cancel_status,settlement_status FROM usage_requests WHERE id=$1 FOR UPDATE`, usageID).Scan(&execution, &cancellation, &settlement); err != nil {
			return err
		}
		// A pre-dispatch request or a nil service error is not proof of released
		// funds. Only the persisted confirmation may support this evidence.
		if execution != "CANCELLED_BEFORE_DISPATCH" || cancellation != "LOCAL_CONFIRMED" || settlement != "RELEASED" {
			return ErrForbidden
		}
	}
	now := r.now()
	providerResult := "NOT_CALLED"
	cancelStatus := "RESULT_UNKNOWN"
	if terminal {
		providerResult, cancelStatus = "ALREADY_TERMINAL", "ALREADY_TERMINAL"
	} else if preDispatch {
		providerResult, cancelStatus = "LOCAL_PRE_DISPATCH", "LOCAL_CONFIRMED"
	} else if capability == ProviderCancelUnsupported {
		providerResult, cancelStatus = "UNSUPPORTED", "UNSUPPORTED"
	} else if capability == ProviderCancelStreamAbortConfirmed && aborted {
		providerResult, cancelStatus = "STREAM_ABORT_CONFIRMED", "PROVIDER_CONFIRMED"
	}
	if _, err := tx.Exec(ctx, `UPDATE usage_requests SET cancel_status=CASE WHEN cancel_status='LOCAL_CONFIRMED' THEN cancel_status ELSE $2 END,provider_cancel_confirmed_at=CASE WHEN $2='PROVIDER_CONFIRMED' THEN $3 ELSE provider_cancel_confirmed_at END,updated_at=$3 WHERE id=$1`, usageID, cancelStatus, now); err != nil {
		return err
	}
	snapshot := map[string]any{"capability": capability, "local_transport_aborted": aborted, "provider_cancel_result": providerResult, "provider_success_inferred": cancelStatus == "PROVIDER_CONFIRMED", "hold_released": preDispatch}
	if _, err := tx.Exec(ctx, `UPDATE model_emergency_cancellations SET status='CONVERGING',lease_until=NULL,next_attempt_at=NULL,cancellation_capability=$2,local_transport_aborted=$3,provider_cancel_result=$4,attempted_at=$5,updated_at=$5 WHERE id=$1`, target.ID, capability, aborted, providerResult, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO model_emergency_cancellation_evidence(id,emergency_cancellation_id,evidence_type,snapshot,created_at) VALUES($1,$2,'ATTEMPTED',$3,$4)`, uuid.Must(uuid.NewV7()), target.ID, snapshot, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO usage_cancellation_evidence(id,usage_request_id,evidence_type,adapter_version,sanitized_evidence,occurred_at) VALUES($1,$2,'MODEL_EMERGENCY_ATTEMPTED','gateway',$3,$4)`, uuid.Must(uuid.NewV7()), usageID, snapshot, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) retryEmergencyCancellation(ctx context.Context, target EmergencyCancellationTarget, cause error) error {
	// A confirmed failure starts the same cooldown at failure time rather than
	// at lease expiry. Change only the matching claim, even on the final attempt.
	_, err := r.pool.Exec(ctx, `WITH changed AS (
    UPDATE model_emergency_cancellations SET status=CASE WHEN retry_attempts>=$5 THEN 'MANUAL_REVIEW' ELSE 'PENDING' END,
        next_attempt_at=CASE WHEN retry_attempts>=$5 THEN NULL ELSE $3::timestamptz+make_interval(secs=>LEAST(300,5*(1<<LEAST(retry_attempts-1,6)))) END,
        lease_until=NULL,provider_cancel_result=$2,updated_at=$3
    WHERE id=$1 AND status='LEASED' AND attempts=$4 AND attempts>0 AND retry_attempts>0
    RETURNING id,status,retry_attempts,provider_cancel_result,updated_at
)`+emergencyCancellationManualEvidenceSQL, target.ID, "ATTEMPT_ERROR:"+stableEmergencyError(cause), r.now(), target.ClaimAttempt, emergencyCancellationRetryBudget)
	return err
}

func (r *PostgresRepository) reviewExpiredEmergencyCancellations(ctx context.Context, limit int) error {
	if limit < 1 || limit > 1000 {
		return ErrForbidden
	}
	// A crash on the last claim has no acknowledgement. Expiry isolates only
	// that task; it is not evidence of a supplier outcome or a funds release.
	_, err := r.pool.Exec(ctx, `WITH expired AS (
    SELECT id FROM model_emergency_cancellations
    WHERE status='LEASED' AND retry_attempts>=$2 AND lease_until<$1
    ORDER BY lease_until,id FOR UPDATE SKIP LOCKED LIMIT $3
), changed AS (
    UPDATE model_emergency_cancellations cancellation SET status='MANUAL_REVIEW',lease_until=NULL,next_attempt_at=NULL,
        provider_cancel_result='ATTEMPT_ERROR:LEASE_EXPIRED',updated_at=$1
    FROM expired WHERE cancellation.id=expired.id
    RETURNING cancellation.id,cancellation.status,cancellation.retry_attempts,cancellation.provider_cancel_result,cancellation.updated_at
)`+emergencyCancellationManualEvidenceSQL, r.now(), emergencyCancellationRetryBudget, limit)
	return err
}

// This records task evidence only. Adding a usage-cancellation or convergence
// fact here could invalidate independent, immutable unused-hold release proofs.
const emergencyCancellationManualEvidenceSQL = `
INSERT INTO model_emergency_cancellation_evidence(id,emergency_cancellation_id,evidence_type,snapshot,created_at)
SELECT gen_random_uuid(),id,'MANUAL_REVIEW',jsonb_build_object('retry_attempts',retry_attempts,'diagnostic',provider_cancel_result),updated_at
FROM changed WHERE status='MANUAL_REVIEW'`

func stableEmergencyError(err error) string {
	if err == nil {
		return "UNKNOWN"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "DEADLINE_EXCEEDED"
	}
	if errors.Is(err, context.Canceled) {
		return "CONTEXT_CANCELLED"
	}
	return "DEPENDENCY_ERROR"
}

func (r *PostgresRepository) finalizeEmergencyCancellations(ctx context.Context, limit int) error {
	now := r.now()
	rows, err := r.pool.Query(ctx, `SELECT cancellation.id,request.id,request.execution_status,request.cancel_status,request.settlement_status,request.hold_id,request.actual_microcredits,request.usage_snapshot
FROM model_emergency_cancellations cancellation JOIN usage_requests request ON request.id=cancellation.usage_request_id
WHERE cancellation.status='CONVERGING' AND (request.settlement_status IN ('SETTLED','RELEASED') OR (request.settlement_status='REVIEW' AND request.reconciliation_stopped_at IS NOT NULL))
ORDER BY cancellation.updated_at,cancellation.id LIMIT $1`, limit)
	if err != nil {
		return err
	}
	type finalItem struct {
		id, usageID                   uuid.UUID
		execution, cancel, settlement string
		hold                          *uuid.UUID
		actual                        *int64
		usage                         []byte
	}
	items := make([]finalItem, 0)
	for rows.Next() {
		var item finalItem
		if err := rows.Scan(&item.id, &item.usageID, &item.execution, &item.cancel, &item.settlement, &item.hold, &item.actual, &item.usage); err != nil {
			rows.Close()
			return err
		}
		items = append(items, item)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range items {
		snapshot := emergencyConvergedSnapshot(item.usageID, item.execution, item.cancel, item.settlement, item.hold, item.actual, item.usage)
		tx, err := r.pool.Begin(ctx)
		if err != nil {
			return err
		}
		command, err := tx.Exec(ctx, `UPDATE model_emergency_cancellations SET status='CONVERGED',hold_id=$2,actual_microcredits=$3,final_execution_status=$4,final_cancel_status=$5,final_settlement_status=$6,final_usage_snapshot=$7,converged_at=$8,updated_at=$8 WHERE id=$1 AND status='CONVERGING'`, item.id, item.hold, item.actual, item.execution, item.cancel, item.settlement, item.usage, now)
		if err == nil && command.RowsAffected() == 1 {
			_, err = tx.Exec(ctx, `INSERT INTO model_emergency_cancellation_evidence(id,emergency_cancellation_id,evidence_type,snapshot,created_at) VALUES($1,$2,'CONVERGED',$3,$4)`, uuid.Must(uuid.NewV7()), item.id, snapshot, now)
			if err == nil {
				err = appendUsageConvergenceEvidenceTx(ctx, tx, item.usageID, "CANCELLATION", "RECONCILER", "RECORDED", "model-emergency-converged", snapshot, now)
			}
		}
		if err == nil {
			err = tx.Commit(ctx)
		} else {
			_ = tx.Rollback(ctx)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Keep the existing audit wire format shared by its writer and the expiry
// observer classifier. In particular, usage is the original []byte payload.
func emergencyConvergedSnapshot(id uuid.UUID, execution, cancellation, settlement string, hold *uuid.UUID, actual *int64, usage []byte) map[string]any {
	return map[string]any{"usage_request_id": id, "execution_status": execution, "cancel_status": cancellation, "settlement_status": settlement, "hold_id": hold, "actual_microcredits": actual, "usage_snapshot": usage, "emergency_removal_implied_free": false}
}
