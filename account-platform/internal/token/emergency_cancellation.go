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
}

type EmergencyModelCancellationWorker struct {
	repository *PostgresRepository
	service    *Service
	interval   time.Duration
	batchSize  int
}

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
		return ErrForbidden
	}
	if err := w.repository.syncEmergencyCancellationTargets(ctx); err != nil {
		return err
	}
	targets, err := w.repository.claimEmergencyCancellations(ctx, w.batchSize)
	if err != nil {
		return err
	}
	capability := w.service.cancellationAdapter.CancelCapability()
	for _, target := range targets {
		preDispatch, terminal, requestErr := w.repository.requestEmergencyCancellation(ctx, target.ID, capability, w.service.upstream.AdapterVersion())
		if requestErr != nil {
			_ = w.repository.retryEmergencyCancellation(ctx, target.ID, requestErr)
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
			_ = w.repository.retryEmergencyCancellation(ctx, target.ID, requestErr)
			continue
		}
		if err := w.repository.recordEmergencyCancellationAttempt(ctx, target.ID, capability, aborted, preDispatch, terminal); err != nil {
			_ = w.repository.retryEmergencyCancellation(ctx, target.ID, err)
		}
	}
	return w.repository.finalizeEmergencyCancellations(ctx, w.batchSize)
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
    AND NOT EXISTS (SELECT 1 FROM usage_requests request WHERE request.id=cancellation.usage_request_id
        AND request.cancellation_origin='USER' AND request.execution_status='REVIEW'
        AND request.cancel_status='CANCEL_REQUESTED' AND request.error_class='cancel_hold_recovery_unconfirmed')
    ORDER BY cancellation.requested_at,cancellation.id FOR UPDATE SKIP LOCKED LIMIT $2
), claimed AS (
    UPDATE model_emergency_cancellations cancellation SET status='LEASED',lease_until=$1::timestamptz+interval '30 seconds',attempts=attempts+1,updated_at=$1::timestamptz
    FROM candidates WHERE cancellation.id=candidates.id RETURNING cancellation.id,cancellation.usage_request_id
)
SELECT claimed.id,claimed.usage_request_id,request.execution_status FROM claimed JOIN usage_requests request ON request.id=claimed.usage_request_id`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]EmergencyCancellationTarget, 0)
	for rows.Next() {
		var item EmergencyCancellationTarget
		if err := rows.Scan(&item.ID, &item.UsageRequestID, &item.ExecutionStatus); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) requestEmergencyCancellation(ctx context.Context, cancellationID uuid.UUID, capability ProviderCancelCapability, adapterVersion string) (bool, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, false, err
	}
	defer tx.Rollback(ctx)
	var usage UsageRequest
	qualifiedUsageColumns := "request." + strings.ReplaceAll(usageSelectColumns, ",", ",request.")
	if err := scanUsage(tx.QueryRow(ctx, `SELECT `+qualifiedUsageColumns+` FROM usage_requests request JOIN model_emergency_cancellations cancellation ON cancellation.usage_request_id=request.id WHERE cancellation.id=$1 FOR UPDATE OF request`, cancellationID), &usage); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, false, ErrNotFound
		}
		return false, false, err
	}
	var cancellationOrigin string
	var requestAlreadyRecorded bool
	if err := tx.QueryRow(ctx, `SELECT request.cancellation_origin,EXISTS(SELECT 1 FROM model_emergency_cancellation_evidence evidence WHERE evidence.emergency_cancellation_id=$2 AND evidence.evidence_type='REQUESTED') FROM usage_requests request WHERE request.id=$1`, usage.ID, cancellationID).Scan(&cancellationOrigin, &requestAlreadyRecorded); err != nil {
		return false, false, err
	}
	// The original USER proof owns this unconfirmed pre-dispatch release.
	// Keep its identity and state intact so the existing recovery can finish;
	// the MODEL target retries and observes the confirmed terminal result.
	if cancellationOrigin == "USER" && usage.ExecutionStatus == "REVIEW" && usage.CancelStatus == "CANCEL_REQUESTED" && usage.ErrorClass != nil && *usage.ErrorClass == "cancel_hold_recovery_unconfirmed" {
		return false, false, ErrForbidden
	}
	if cancellationOrigin == "MODEL_EMERGENCY" && requestAlreadyRecorded {
		if usage.ExecutionStatus == "REVIEW" && usage.HoldID != nil {
			if err := r.restoreEmergencyPreDispatchReview(ctx, tx, usage, cancellationID); err != nil {
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
	// A confirmed USER cancellation remains the authority for expiry-proof
	// replay when its response was lost. MODEL only observes that terminal.
	keepUserConfirmation := cancellationOrigin == "USER" && usage.ExecutionStatus == "CANCELLED_BEFORE_DISPATCH" && usage.CancelStatus == "LOCAL_CONFIRMED" && usage.SettlementStatus == "RELEASED"
	if _, err := tx.Exec(ctx, `UPDATE usage_requests SET cancel_status=CASE WHEN $5 THEN cancel_status ELSE $2 END,delivery_status=CASE WHEN $3 THEN delivery_status ELSE 'USER_STOPPED' END,cancellation_origin=CASE WHEN $5 THEN cancellation_origin ELSE 'MODEL_EMERGENCY' END,cancel_requested_at=COALESCE(cancel_requested_at,$4),user_stopped_at=CASE WHEN $3 THEN user_stopped_at ELSE COALESCE(user_stopped_at,$4) END,cancel_reconciliation_deadline=CASE WHEN $3 THEN cancel_reconciliation_deadline ELSE $4+make_interval(secs=>cancel_reconciliation_seconds) END,updated_at=$4 WHERE id=$1`, usage.ID, cancelStatus, terminal, now, keepUserConfirmation); err != nil {
		return false, false, err
	}
	snapshot := map[string]any{"origin": "MODEL_EMERGENCY", "capability": capability, "pre_dispatch": preDispatch, "already_terminal": terminal, "provider_success_inferred": false}
	if _, err := tx.Exec(ctx, `INSERT INTO usage_cancellation_evidence(id,usage_request_id,evidence_type,adapter_version,sanitized_evidence,occurred_at) VALUES($1,$2,'MODEL_EMERGENCY_REQUESTED',$3,$4,$5)`, uuid.Must(uuid.NewV7()), usage.ID, strings.TrimSpace(adapterVersion), snapshot, now); err != nil {
		return false, false, err
	}
	if err := appendUsageConvergenceEvidenceTx(ctx, tx, usage.ID, "CANCELLATION", "GATEWAY", "RECORDED", "model-emergency-requested", snapshot, now); err != nil {
		return false, false, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO model_emergency_cancellation_evidence(id,emergency_cancellation_id,evidence_type,snapshot,created_at) VALUES($1,$2,'REQUESTED',$3,$4)`, uuid.Must(uuid.NewV7()), cancellationID, snapshot, now); err != nil {
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

func (r *PostgresRepository) recordEmergencyCancellationAttempt(ctx context.Context, cancellationID uuid.UUID, capability ProviderCancelCapability, aborted, preDispatch, terminal bool) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var usageID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT usage_request_id FROM model_emergency_cancellations WHERE id=$1 FOR UPDATE`, cancellationID).Scan(&usageID); err != nil {
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
	if _, err := tx.Exec(ctx, `UPDATE model_emergency_cancellations SET status='CONVERGING',lease_until=NULL,cancellation_capability=$2,local_transport_aborted=$3,provider_cancel_result=$4,attempted_at=$5,updated_at=$5 WHERE id=$1`, cancellationID, capability, aborted, providerResult, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO model_emergency_cancellation_evidence(id,emergency_cancellation_id,evidence_type,snapshot,created_at) VALUES($1,$2,'ATTEMPTED',$3,$4)`, uuid.Must(uuid.NewV7()), cancellationID, snapshot, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO usage_cancellation_evidence(id,usage_request_id,evidence_type,adapter_version,sanitized_evidence,occurred_at) VALUES($1,$2,'MODEL_EMERGENCY_ATTEMPTED','gateway',$3,$4)`, uuid.Must(uuid.NewV7()), usageID, snapshot, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) retryEmergencyCancellation(ctx context.Context, cancellationID uuid.UUID, cause error) error {
	_, err := r.pool.Exec(ctx, `UPDATE model_emergency_cancellations SET status='PENDING',lease_until=NULL,provider_cancel_result=$2,updated_at=$3 WHERE id=$1 AND status='LEASED'`, cancellationID, "ATTEMPT_ERROR:"+stableEmergencyError(cause), r.now())
	return err
}

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
		snapshot := map[string]any{"usage_request_id": item.usageID, "execution_status": item.execution, "cancel_status": item.cancel, "settlement_status": item.settlement, "hold_id": item.hold, "actual_microcredits": item.actual, "usage_snapshot": item.usage, "emergency_removal_implied_free": false}
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
