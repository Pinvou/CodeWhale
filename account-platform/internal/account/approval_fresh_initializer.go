package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrApprovalFreshInitialization = errors.New("APPROVAL_FRESH_INITIALIZATION_BLOCKED")

type FreshApprovalStep string

const (
	FreshApprovalStatus    FreshApprovalStep = "status"
	FreshApprovalQuiesce   FreshApprovalStep = "quiesce"
	FreshApprovalFence     FreshApprovalStep = "fence"
	FreshApprovalResources FreshApprovalStep = "initialize-consumers"
	FreshApprovalActivate  FreshApprovalStep = "activate"
	FreshApprovalResume    FreshApprovalStep = "resume"
)

// RunFreshApprovalStep is a bounded, explicit operator operation, not startup
// auto-provisioning. Each mode/relay transition is its own database transaction,
// as required by the immutable control trigger. It never reconciles, discards
// or rewrites historical execution, delivery, exclusion or financial evidence.
// The callback retains the exclusive control lock around broker verification.
func RunFreshApprovalStep(ctx context.Context, dsn string, expectedVersion int64, step FreshApprovalStep, resources func(context.Context, AdminApprovalControl) (json.RawMessage, error)) (AdminApprovalControl, error) {
	return runEmptyHistoryApprovalStep(ctx, dsn, expectedVersion, step, resources, false, "")
}

// ExistingBrokerPrepare captures a hash-bound plan while the same exclusive
// Account fence and empty-history checks used by initialization are held.
const ExistingBrokerPrepare FreshApprovalStep = "prepare-existing-consumers"
const ExistingBrokerPrepareRefundUpgrade FreshApprovalStep = "prepare-refund-upgrade"
const ExistingBrokerUpgradeRefund FreshApprovalStep = "upgrade-refund-consumer"
const ExistingBrokerCheckRefundUpgrade FreshApprovalStep = "check-refund-upgrade"
const ExistingBrokerInspectRefundUpgrade FreshApprovalStep = "inspect-refund-upgrade"
const ExistingBrokerPrepareTokenUpgrade FreshApprovalStep = "prepare-token-upgrade"
const ExistingBrokerUpgradeToken FreshApprovalStep = "upgrade-token-consumers"
const ExistingBrokerCheckTokenUpgrade FreshApprovalStep = "check-token-upgrade"
const ExistingBrokerVerify FreshApprovalStep = "verify-consumers"
const ExistingBrokerInspectTokenUpgrade FreshApprovalStep = "inspect-token-upgrade"
const ExistingBrokerInspect FreshApprovalStep = "inspect-consumers"

// RunExistingBrokerApprovalStep preserves the fresh SQL-history boundary, but
// records the distinct existing-broker operation rather than claiming a fresh
// broker initialization. The original entry point cannot select this mode.
func RunExistingBrokerApprovalStep(ctx context.Context, dsn string, expectedVersion int64, step FreshApprovalStep, planSHA256 string, resources func(context.Context, AdminApprovalControl) (json.RawMessage, error)) (AdminApprovalControl, error) {
	return runEmptyHistoryApprovalStep(ctx, dsn, expectedVersion, step, resources, true, planSHA256)
}

func runEmptyHistoryApprovalStep(ctx context.Context, dsn string, expectedVersion int64, step FreshApprovalStep, resources func(context.Context, AdminApprovalControl) (json.RawMessage, error), existingBroker bool, planSHA256 string) (AdminApprovalControl, error) {
	auditStep := step
	if existingBroker && (step == ExistingBrokerPrepare || step == ExistingBrokerPrepareRefundUpgrade || step == ExistingBrokerUpgradeRefund || step == ExistingBrokerCheckRefundUpgrade || step == ExistingBrokerInspectRefundUpgrade || step == ExistingBrokerPrepareTokenUpgrade || step == ExistingBrokerUpgradeToken || step == ExistingBrokerCheckTokenUpgrade || step == ExistingBrokerVerify || step == ExistingBrokerInspectTokenUpgrade || step == ExistingBrokerInspect) {
		step = FreshApprovalResources
	}
	queryCurrent := existingBroker && step == FreshApprovalStatus && expectedVersion == 0
	if (expectedVersion <= 0 && !queryCurrent) || !validFreshApprovalStep(step) || ((step == FreshApprovalResources || step == FreshApprovalActivate || step == FreshApprovalResume) && resources == nil) {
		return AdminApprovalControl{}, ErrApprovalFreshInitialization
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return AdminApprovalControl{}, ErrApprovalFreshInitialization
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = c.Close(cleanup)
	}()
	var valid bool
	err = c.QueryRow(ctx, `SELECT session_user='account_migrator' AND current_user=session_user AND rolcanlogin AND NOT rolinherit AND NOT rolsuper AND NOT rolcreatedb AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls FROM pg_catalog.pg_roles WHERE rolname=session_user`).Scan(&valid)
	if err != nil || !valid {
		return AdminApprovalControl{}, ErrApprovalFreshInitialization
	}
	// Each post-lock statement must see a concurrent attempt committed while
	// this transaction waited for the control row, regardless of server defaults.
	tx, err := c.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return AdminApprovalControl{}, ErrApprovalFreshInitialization
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = tx.Rollback(cleanup)
	}()
	if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout='5s'; SET LOCAL statement_timeout='85s'; SET LOCAL ROLE account_approval_control_owner`); err != nil {
		return AdminApprovalControl{}, ErrApprovalFreshInitialization
	}
	control, err := LockAdminApprovalControlExclusive(ctx, tx)
	if err != nil || (!queryCurrent && control.Version != expectedVersion) {
		return AdminApprovalControl{}, ErrApprovalFreshInitialization
	}
	if step == FreshApprovalStatus {
		if tx.Commit(ctx) != nil {
			return AdminApprovalControl{}, ErrApprovalFreshInitialization
		}
		return control, nil
	}
	if !freshApprovalStepMatches(control, step) {
		return AdminApprovalControl{}, ErrApprovalFreshInitialization
	}
	if _, err = tx.Exec(ctx, `SET LOCAL ROLE account_service`); err != nil {
		return AdminApprovalControl{}, ErrApprovalFreshInitialization
	}
	// Even completed historical requests block this fresh-only initializer. A
	// historical drain requires its own controlled process; no terminal state
	// here authorizes deletion or a weaker "pending only" check.
	var history bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.approval_requests) OR EXISTS(SELECT 1 FROM public.approval_executions)`).Scan(&history)
	if err != nil || history {
		return AdminApprovalControl{}, ErrApprovalFreshInitialization
	}
	if _, err = approvalpolicy.VerifyPolicySet(ctx, tx); err != nil {
		return AdminApprovalControl{}, ErrApprovalFreshInitialization
	}
	if existingBroker {
		prepare := ExistingBrokerPrepare
		needsPlan := step == FreshApprovalResources || step == FreshApprovalActivate || step == FreshApprovalResume
		if auditStep == ExistingBrokerPrepare || auditStep == ExistingBrokerPrepareRefundUpgrade || auditStep == ExistingBrokerPrepareTokenUpgrade {
			needsPlan = false
		}
		if auditStep == ExistingBrokerUpgradeToken || auditStep == ExistingBrokerCheckTokenUpgrade || auditStep == ExistingBrokerInspectTokenUpgrade {
			prepare = ExistingBrokerPrepareTokenUpgrade
		}
		if auditStep == ExistingBrokerUpgradeRefund || auditStep == ExistingBrokerCheckRefundUpgrade || auditStep == ExistingBrokerInspectRefundUpgrade {
			prepare = ExistingBrokerPrepareRefundUpgrade
		}
		if needsPlan {
			decoded, e := hex.DecodeString(planSHA256)
			var prepared bool
			if e != nil || len(decoded) != sha256.Size || hex.EncodeToString(decoded) != planSHA256 {
				return AdminApprovalControl{}, ErrApprovalFreshInitialization
			}
			e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.audit_logs WHERE action='APPROVAL_EXISTING_BROKER_INITIALIZATION' AND resource_type='ADMIN_APPROVAL_CONTROL' AND resource_id='ADMIN_APPROVAL' AND result='SUCCEEDED' AND change_summary->>'step'=$1 AND change_summary->>'resource_receipt_sha256'=$2)`, string(prepare), planSHA256).Scan(&prepared)
			if e != nil || !prepared {
				return AdminApprovalControl{}, ErrApprovalFreshInitialization
			}
		} else if planSHA256 != "" {
			return AdminApprovalControl{}, ErrApprovalFreshInitialization
		}
	}
	if _, err = tx.Exec(ctx, `SET LOCAL ROLE account_approval_authorization_owner`); err != nil {
		return AdminApprovalControl{}, ErrApprovalFreshInitialization
	}
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM account_approval_authorization.approval_authorization_event_bytes) OR EXISTS(SELECT 1 FROM account_approval_authorization.authorization_event_pending) OR EXISTS(SELECT 1 FROM account_approval_authorization.authorization_delivery_pending) OR EXISTS(SELECT 1 FROM account_approval_authorization.approval_delivery_states) OR EXISTS(SELECT 1 FROM account_approval_authorization.approval_authorization_delivery_exclusions)`).Scan(&history)
	if err != nil || history {
		return AdminApprovalControl{}, ErrApprovalFreshInitialization
	}
	// A separate committed audit record is the one-shot send barrier. Account's
	// exclusive control lock serializes competing attempts; the independent
	// commit survives rollback or loss of the later broker response.
	if existingBroker && (auditStep == ExistingBrokerPrepareRefundUpgrade || auditStep == ExistingBrokerUpgradeRefund) {
		if _, err = tx.Exec(ctx, `SET LOCAL ROLE account_service`); err != nil {
			return AdminApprovalControl{}, ErrApprovalFreshInitialization
		}
		var attempted bool
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.audit_logs WHERE action='security.approval_refund_upgrade_attempt' AND resource_type='ADMIN_APPROVAL_CONTROL' AND resource_id='ADMIN_APPROVAL' AND change_summary->>'control_version'=$1 AND change_summary->>'approval_epoch'=$2 AND change_summary->>'authorization_relay_epoch'=$3)`, fmt.Sprint(control.Version), fmt.Sprint(control.ApprovalEpoch), fmt.Sprint(control.AuthorizationRelayEpoch)).Scan(&attempted)
		if err != nil || attempted {
			return AdminApprovalControl{}, ErrApprovalFreshInitialization
		}
		if auditStep == ExistingBrokerUpgradeRefund && recordRefundUpgradeAttempt(ctx, dsn, control, planSHA256) != nil {
			return AdminApprovalControl{}, ErrApprovalFreshInitialization
		}
	}
	var receipt json.RawMessage
	if resources != nil {
		receipt, err = resources(ctx, control)
		if err != nil || !json.Valid(receipt) {
			return AdminApprovalControl{}, ErrApprovalFreshInitialization
		}
	}
	if existingBroker && (auditStep == ExistingBrokerInspectRefundUpgrade || auditStep == ExistingBrokerCheckRefundUpgrade || auditStep == ExistingBrokerInspectTokenUpgrade || auditStep == ExistingBrokerInspect || auditStep == ExistingBrokerCheckTokenUpgrade || auditStep == ExistingBrokerVerify) {
		if tx.Commit(ctx) != nil {
			return AdminApprovalControl{}, ErrApprovalFreshInitialization
		}
		return control, nil
	}
	if _, err = tx.Exec(ctx, `SET LOCAL ROLE account_approval_control_owner`); err != nil {
		return AdminApprovalControl{}, ErrApprovalFreshInitialization
	}
	query := ""
	switch step {
	case FreshApprovalQuiesce:
		query = `UPDATE account_approval_control.admin_approval_control SET mode='QUIESCING',approval_epoch=approval_epoch+1,version=version+1 WHERE control_id='ADMIN_APPROVAL' AND version=$1 RETURNING ` + adminApprovalControlColumns
	case FreshApprovalFence:
		query = `UPDATE account_approval_control.admin_approval_control SET authorization_relay_state='FENCED',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1 WHERE control_id='ADMIN_APPROVAL' AND version=$1 RETURNING ` + adminApprovalControlColumns
	case FreshApprovalActivate:
		query = `UPDATE account_approval_control.admin_approval_control SET mode='PROOF_REQUIRED',approval_wire_version=2,proof_required=true,approval_epoch=approval_epoch+1,version=version+1 WHERE control_id='ADMIN_APPROVAL' AND version=$1 RETURNING ` + adminApprovalControlColumns
	case FreshApprovalResume:
		query = `UPDATE account_approval_control.admin_approval_control SET authorization_relay_state='RUNNING',authorization_relay_epoch=authorization_relay_epoch+1,version=version+1 WHERE control_id='ADMIN_APPROVAL' AND version=$1 RETURNING ` + adminApprovalControlColumns
	}
	if query != "" {
		control, err = scanAdminApprovalControl(tx.QueryRow(ctx, query, expectedVersion))
		if err != nil {
			return AdminApprovalControl{}, ErrApprovalFreshInitialization
		}
	}
	if _, err = tx.Exec(ctx, `SET LOCAL ROLE account_service`); err != nil {
		return AdminApprovalControl{}, ErrApprovalFreshInitialization
	}
	digest := sha256.Sum256(receipt)
	summary, _ := json.Marshal(map[string]any{"step": auditStep, "control_version": control.Version, "approval_epoch": control.ApprovalEpoch, "authorization_relay_epoch": control.AuthorizationRelayEpoch, "resource_receipt_sha256": hex.EncodeToString(digest[:])})
	action := "APPROVAL_FRESH_INITIALIZATION"
	if existingBroker {
		action = "APPROVAL_EXISTING_BROKER_INITIALIZATION"
	}
	_, err = tx.Exec(ctx, `INSERT INTO public.audit_logs(id,action,resource_type,resource_id,result,request_id,trace_id,change_summary,occurred_at) VALUES($1,$2,'ADMIN_APPROVAL_CONTROL','ADMIN_APPROVAL','SUCCEEDED',$3,'operator',$4,transaction_timestamp())`, uuid.New(), action, uuid.NewString(), summary)
	if err != nil || tx.Commit(ctx) != nil {
		return AdminApprovalControl{}, ErrApprovalFreshInitialization
	}
	return control, nil
}

func validFreshApprovalStep(step FreshApprovalStep) bool {
	switch step {
	case FreshApprovalStatus, FreshApprovalQuiesce, FreshApprovalFence, FreshApprovalResources, FreshApprovalActivate, FreshApprovalResume:
		return true
	}
	return false
}

func freshApprovalStepMatches(control AdminApprovalControl, step FreshApprovalStep) bool {
	switch step {
	case FreshApprovalQuiesce:
		return control.Mode == AdminApprovalModeLegacy && control.AuthorizationRelayState == AdminApprovalRelayRunning
	case FreshApprovalFence:
		return control.Mode == AdminApprovalModeQuiescing && control.AuthorizationRelayState == AdminApprovalRelayRunning
	case FreshApprovalResources, FreshApprovalActivate:
		return control.Mode == AdminApprovalModeQuiescing && control.AuthorizationRelayState == AdminApprovalRelayFenced
	case FreshApprovalResume:
		return control.Mode == AdminApprovalModeProofRequired && control.AuthorizationRelayState == AdminApprovalRelayFenced
	}
	return false
}

// recordRefundUpgradeAttempt commits before any UPDATE. Even a failed or
// ambiguous commit blocks the broker operation; a committed marker cannot be
// reused to send another update in this control epoch.
func recordRefundUpgradeAttempt(ctx context.Context, dsn string, control AdminApprovalControl, planSHA256 string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = conn.Close(c)
	}()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(c)
	}()
	if _, err = tx.Exec(ctx, `SET LOCAL lock_timeout='5s'; SET LOCAL statement_timeout='15s'; SET LOCAL ROLE account_service`); err != nil {
		return err
	}
	summary, err := json.Marshal(map[string]any{"control_version": control.Version, "approval_epoch": control.ApprovalEpoch, "authorization_relay_epoch": control.AuthorizationRelayEpoch, "resource_receipt_sha256": planSHA256})
	if err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO public.audit_logs(id,action,resource_type,resource_id,result,request_id,trace_id,change_summary,occurred_at) VALUES($1,'security.approval_refund_upgrade_attempt','ADMIN_APPROVAL_CONTROL','ADMIN_APPROVAL','SUCCEEDED',$2,'operator',$3,transaction_timestamp())`, uuid.New(), uuid.NewString(), summary); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
