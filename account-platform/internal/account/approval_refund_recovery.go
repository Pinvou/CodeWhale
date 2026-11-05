package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	ExistingBrokerPrepareRefundRecovery FreshApprovalStep = "prepare-refund-recovery"
	ExistingBrokerUpgradeRefundRecovery FreshApprovalStep = "upgrade-refund-recovery"
	ExistingBrokerCheckRefundRecovery   FreshApprovalStep = "check-refund-recovery"
)

func refundRecoveryStep(step FreshApprovalStep) bool {
	return step == ExistingBrokerPrepareRefundRecovery || step == ExistingBrokerUpgradeRefundRecovery || step == ExistingBrokerCheckRefundRecovery
}

func recoveryDigest(value string) bool {
	raw, err := hex.DecodeString(value)
	return err == nil && len(raw) == sha256.Size && hex.EncodeToString(raw) == value
}

// RunRefundRecoveryApprovalStep is a distinct, explicitly selected operation.
// It preserves the original attempt and permits at most one recovery send in
// the same control epoch. It does not make the original operation retryable.
func RunRefundRecoveryApprovalStep(ctx context.Context, dsn string, expectedVersion int64, step FreshApprovalStep, originalSHA256, planSHA256 string, resources func(context.Context, AdminApprovalControl) (json.RawMessage, error)) (AdminApprovalControl, error) {
	if !refundRecoveryStep(step) || !recoveryDigest(originalSHA256) || resources == nil ||
		(step == ExistingBrokerPrepareRefundRecovery && planSHA256 != "") ||
		(step != ExistingBrokerPrepareRefundRecovery && (!recoveryDigest(planSHA256) || planSHA256 == originalSHA256)) {
		return AdminApprovalControl{}, ErrApprovalFreshInitialization
	}
	return runEmptyHistoryApprovalStep(ctx, dsn, expectedVersion, step, resources, true, planSHA256, originalSHA256)
}

// The caller retains the exclusive Account control lock. All preparation and
// attempt evidence is append-only and bound to this exact control epoch.
func verifyRefundRecoveryContext(ctx context.Context, tx pgx.Tx, control AdminApprovalControl, step FreshApprovalStep, originalSHA256, planSHA256 string) error {
	if _, err := tx.Exec(ctx, `SET LOCAL ROLE account_service`); err != nil {
		return err
	}
	var attempts, originalAttempts, originalPlans, originalApplied, recoveryPlans, matchingPlans, recoveryAttempts, matchingAttempts int64
	err := tx.QueryRow(ctx, `SELECT
count(*) FILTER (WHERE action='security.approval_refund_upgrade_attempt'),
count(*) FILTER (WHERE action='security.approval_refund_upgrade_attempt' AND change_summary->>'resource_receipt_sha256'=$4),
count(*) FILTER (WHERE action='APPROVAL_EXISTING_BROKER_INITIALIZATION' AND change_summary->>'step'='prepare-refund-upgrade' AND change_summary->>'resource_receipt_sha256'=$4),
count(*) FILTER (WHERE action='APPROVAL_EXISTING_BROKER_INITIALIZATION' AND change_summary->>'step'='upgrade-refund-consumer'),
count(*) FILTER (WHERE action='APPROVAL_EXISTING_BROKER_INITIALIZATION' AND change_summary->>'step'='prepare-refund-recovery'),
count(*) FILTER (WHERE action='APPROVAL_EXISTING_BROKER_INITIALIZATION' AND change_summary->>'step'='prepare-refund-recovery' AND change_summary->>'resource_receipt_sha256'=$5 AND change_summary->>'recovery_original_sha256'=$4),
count(*) FILTER (WHERE action='security.approval_refund_recovery_attempt'),
count(*) FILTER (WHERE action='security.approval_refund_recovery_attempt' AND change_summary->>'resource_receipt_sha256'=$5 AND change_summary->>'recovery_original_sha256'=$4)
FROM public.audit_logs WHERE resource_type='ADMIN_APPROVAL_CONTROL' AND resource_id='ADMIN_APPROVAL' AND result='SUCCEEDED'
AND change_summary->>'control_version'=$1 AND change_summary->>'approval_epoch'=$2 AND change_summary->>'authorization_relay_epoch'=$3`,
		fmt.Sprint(control.Version), fmt.Sprint(control.ApprovalEpoch), fmt.Sprint(control.AuthorizationRelayEpoch), originalSHA256, planSHA256).
		Scan(&attempts, &originalAttempts, &originalPlans, &originalApplied, &recoveryPlans, &matchingPlans, &recoveryAttempts, &matchingAttempts)
	if err != nil || attempts != 1 || originalAttempts != 1 || originalPlans != 1 || originalApplied != 0 {
		return ErrApprovalFreshInitialization
	}
	if step == ExistingBrokerPrepareRefundRecovery {
		if recoveryPlans != 0 || recoveryAttempts != 0 {
			return ErrApprovalFreshInitialization
		}
		return nil
	}
	if recoveryPlans != 1 || matchingPlans != 1 {
		return ErrApprovalFreshInitialization
	}
	if step == ExistingBrokerUpgradeRefundRecovery && recoveryAttempts == 0 {
		return nil
	}
	if step == ExistingBrokerCheckRefundRecovery && recoveryAttempts == 1 && matchingAttempts == 1 {
		return nil
	}
	return ErrApprovalFreshInitialization
}

func recordRefundRecoveryAttempt(ctx context.Context, dsn string, control AdminApprovalControl, originalSHA256, planSHA256 string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = conn.Close(cleanup)
	}()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = tx.Rollback(cleanup)
	}()
	if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout='5s'; SET LOCAL statement_timeout='15s'; SET LOCAL ROLE account_service`); err != nil {
		return err
	}
	summary, err := json.Marshal(map[string]any{"control_version": control.Version, "approval_epoch": control.ApprovalEpoch, "authorization_relay_epoch": control.AuthorizationRelayEpoch, "resource_receipt_sha256": planSHA256, "recovery_original_sha256": originalSHA256})
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO public.audit_logs(id,action,resource_type,resource_id,result,request_id,trace_id,change_summary,occurred_at) VALUES($1,'security.approval_refund_recovery_attempt','ADMIN_APPROVAL_CONTROL','ADMIN_APPROVAL','SUCCEEDED',$2,'operator',$3,transaction_timestamp())`, uuid.New(), uuid.NewString(), summary); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
