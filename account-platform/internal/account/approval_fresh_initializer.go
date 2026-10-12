package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	if expectedVersion <= 0 || !validFreshApprovalStep(step) || ((step == FreshApprovalResources || step == FreshApprovalActivate || step == FreshApprovalResume) && resources == nil) {
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
	tx, err := c.Begin(ctx)
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
	if err != nil || control.Version != expectedVersion {
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
	if _, err = tx.Exec(ctx, `SET LOCAL ROLE account_approval_authorization_owner`); err != nil {
		return AdminApprovalControl{}, ErrApprovalFreshInitialization
	}
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM account_approval_authorization.approval_authorization_event_bytes) OR EXISTS(SELECT 1 FROM account_approval_authorization.authorization_event_pending) OR EXISTS(SELECT 1 FROM account_approval_authorization.authorization_delivery_pending) OR EXISTS(SELECT 1 FROM account_approval_authorization.approval_delivery_states) OR EXISTS(SELECT 1 FROM account_approval_authorization.approval_authorization_delivery_exclusions)`).Scan(&history)
	if err != nil || history {
		return AdminApprovalControl{}, ErrApprovalFreshInitialization
	}
	var receipt json.RawMessage
	if resources != nil {
		receipt, err = resources(ctx, control)
		if err != nil || !json.Valid(receipt) {
			return AdminApprovalControl{}, ErrApprovalFreshInitialization
		}
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
	summary, _ := json.Marshal(map[string]any{"step": step, "control_version": control.Version, "approval_epoch": control.ApprovalEpoch, "authorization_relay_epoch": control.AuthorizationRelayEpoch, "resource_receipt_sha256": hex.EncodeToString(digest[:])})
	_, err = tx.Exec(ctx, `INSERT INTO public.audit_logs(id,action,resource_type,resource_id,result,request_id,trace_id,change_summary,occurred_at) VALUES($1,'APPROVAL_FRESH_INITIALIZATION','ADMIN_APPROVAL_CONTROL','ADMIN_APPROVAL','SUCCEEDED',$2,'operator',$3,transaction_timestamp())`, uuid.New(), uuid.NewString(), summary)
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
