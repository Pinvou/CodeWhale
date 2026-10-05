package account

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrRegistrationCompensationForbidden = errors.New("registration compensation proof is incomplete")

type RegistrationRepairItem struct {
	OperationID         uuid.UUID  `json:"registration_operation_id"`
	Status              string     `json:"status"`
	Stage               string     `json:"stage"`
	Responsibility      string     `json:"responsibility"`
	AccountCreated      bool       `json:"account_created"`
	CompensationAllowed bool       `json:"compensation_allowed"`
	Attempts            int        `json:"attempts"`
	Reason              string     `json:"reason,omitempty"`
	NextRetryAt         time.Time  `json:"next_retry_at"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	SLAExpiresAt        *time.Time `json:"sla_expires_at,omitempty"`
}

type RegistrationRepairFilter struct {
	Status           string
	Stage            string
	Responsibility   string
	OlderThanMinutes int
	SLA              string
	Limit            int
}

type RegistrationAdminRepository interface {
	RegistrationRepairQueue(context.Context, RegistrationRepairFilter) ([]RegistrationRepairItem, error)
	RegistrationOperation(context.Context, uuid.UUID) (RegistrationOperation, error)
	RetryWalletProvisioning(context.Context, identity.Principal, uuid.UUID) error
	PrepareRegistrationCompensation(context.Context, identity.Principal, uuid.UUID) (RegistrationOperation, error)
	CompleteRegistrationCompensation(context.Context, identity.Principal, uuid.UUID) error
	AuditRegistrationRepair(context.Context, identity.Principal, uuid.UUID, string, string) error
}

func (r *PostgresRepository) AuditRegistrationRepair(ctx context.Context, principal identity.Principal, operationID uuid.UUID, action, result string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return err
	}
	if err := appendAudit(ctx, tx, actorID, principal.SessionID, nil, action, "registration_operation", operationID.String(), result, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) RegistrationRepairQueue(ctx context.Context, filter RegistrationRepairFilter) ([]RegistrationRepairItem, error) {
	filter.Status = strings.ToUpper(strings.TrimSpace(filter.Status))
	filter.Stage = strings.ToUpper(strings.TrimSpace(filter.Stage))
	filter.Responsibility = strings.ToUpper(strings.TrimSpace(filter.Responsibility))
	filter.SLA = strings.ToUpper(strings.TrimSpace(filter.SLA))
	if filter.Limit < 1 || filter.Limit > 100 {
		filter.Limit = 50
	}
	if filter.OlderThanMinutes < 0 || filter.OlderThanMinutes > 43200 {
		filter.OlderThanMinutes = 0
	}
	rows, err := r.pool.Query(ctx, `
WITH queue AS (
 SELECT ro.id,ro.status,
 CASE WHEN ro.committed_user_id IS NOT NULL THEN 'WALLET_PROVISIONING'
      WHEN ro.status='COMPENSATION_PENDING' THEN 'COMPENSATION'
      WHEN ro.identity_subject IS NOT NULL THEN 'ACCOUNT_COMMITTING'
      ELSE 'IDENTITY_CREATING' END AS stage,
 CASE WHEN ro.committed_user_id IS NOT NULL THEN 'BILLING'
      WHEN ro.status='COMPENSATION_PENDING' THEN 'IDENTITY'
      WHEN ro.identity_subject IS NOT NULL THEN 'ACCOUNT'
      ELSE 'IDENTITY' END AS responsibility,
 ro.committed_user_id IS NOT NULL AS account_created,
 ro.committed_user_id IS NULL AND ro.identity_subject IS NOT NULL
   AND (ro.status='COMPENSATION_PENDING' OR ro.identity_created_at+interval '24 hours'<=now() AND ro.status='REPAIR_REQUIRED') AS compensation_allowed,
 COALESCE(wj.attempts,ro.attempts) AS attempts,
 COALESCE(wj.last_error_code,ro.last_error_code,'') AS reason,
 CASE WHEN wj.user_id IS NOT NULL THEN wj.next_attempt_at ELSE ro.next_retry_at END AS next_retry_at,
 ro.created_at,ro.updated_at,
 COALESCE(wj.sla_expires_at,ro.identity_created_at+interval '30 minutes',ro.lease_expires_at) AS sla_expires_at
 FROM registration_operations ro LEFT JOIN wallet_provisioning_jobs wj ON wj.registration_operation_id=ro.id
 WHERE ro.status IN ('REPAIR_REQUIRED','COMPENSATION_PENDING')
)
SELECT id,status,stage,responsibility,account_created,compensation_allowed,attempts,reason,next_retry_at,created_at,updated_at,sla_expires_at
FROM queue
WHERE ($1='' OR status=$1) AND ($2='' OR stage=$2) AND ($3='' OR responsibility=$3)
  AND ($4=0 OR updated_at <= now()-make_interval(mins => $4))
  AND ($5='' OR $5='OVERDUE' AND sla_expires_at <= now() OR $5='WITHIN' AND sla_expires_at > now())
ORDER BY updated_at,created_at LIMIT $6`, filter.Status, filter.Stage, filter.Responsibility, filter.OlderThanMinutes, filter.SLA, filter.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]RegistrationRepairItem, 0, filter.Limit)
	for rows.Next() {
		var item RegistrationRepairItem
		if err := rows.Scan(&item.OperationID, &item.Status, &item.Stage, &item.Responsibility, &item.AccountCreated, &item.CompensationAllowed, &item.Attempts, &item.Reason, &item.NextRetryAt, &item.CreatedAt, &item.UpdatedAt, &item.SLAExpiresAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) RetryWalletProvisioning(ctx context.Context, principal identity.Principal, userID uuid.UUID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return err
	}
	now := r.now()
	command, err := tx.Exec(ctx, `
UPDATE wallet_provisioning_jobs SET status='RETRY',next_attempt_at=$2,lease_until=NULL,last_error_code=NULL,updated_at=$2
WHERE user_id=$1 AND status='REPAIR_REQUIRED'`, userID, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrRegistrationConflict
	}
	if _, err := tx.Exec(ctx, `UPDATE users SET status='PROVISIONING',version=version+1,updated_at=$2 WHERE id=$1 AND status='REPAIR_REQUIRED'`, userID, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE registration_operations SET status='PROVISIONING',next_retry_at=$2,last_error_code=NULL,version=version+1,updated_at=$2 WHERE committed_user_id=$1 AND status='REPAIR_REQUIRED'`, userID, now); err != nil {
		return err
	}
	if err := appendAudit(ctx, tx, actorID, principal.SessionID, nil, "registration.wallet.retry", "user", userID.String(), "SUCCEEDED", nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) PrepareRegistrationCompensation(ctx context.Context, principal identity.Principal, operationID uuid.UUID) (RegistrationOperation, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return RegistrationOperation{}, err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return RegistrationOperation{}, err
	}
	operation, err := compensationProof(ctx, tx, operationID, r.now())
	if err != nil {
		return RegistrationOperation{}, err
	}
	evidence, _ := json.Marshal(map[string]any{
		"checked_at": r.now(), "account_user_absent": true, "formal_mapping_absent": true,
		"wallet_job_absent": true, "account_outbox_absent": true, "downstream_business_facts_absent": true,
		"downstream_proof_basis":   "candidate user was never committed or published by Account",
		"identity_disable_pending": true,
	})
	_, err = tx.Exec(ctx, `UPDATE registration_operations SET status='COMPENSATION_PENDING',compensation_evidence=$2,next_retry_at=$3,version=version+1,updated_at=$3 WHERE id=$1`, operationID, evidence, r.now())
	if err != nil {
		return RegistrationOperation{}, err
	}
	if err := appendAudit(ctx, tx, actorID, principal.SessionID, nil, "registration.compensation.prepare", "registration_operation", operationID.String(), "SUCCEEDED", map[string]any{"proof_complete": true}); err != nil {
		return RegistrationOperation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RegistrationOperation{}, err
	}
	operation.Status = "COMPENSATION_PENDING"
	return operation, nil
}

func (r *PostgresRepository) CompleteRegistrationCompensation(ctx context.Context, principal identity.Principal, operationID uuid.UUID) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actorID, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return err
	}
	operation, err := compensationProof(ctx, tx, operationID, r.now())
	if err != nil || operation.Status != "COMPENSATION_PENDING" {
		return ErrRegistrationCompensationForbidden
	}
	now := r.now()
	command, err := tx.Exec(ctx, `
UPDATE registration_operations SET status='COMPENSATED',lease_expires_at=NULL,next_retry_at=$2,
last_error_code='IDENTITY_DISABLED_AND_PROOF_CONFIRMED',version=version+1,updated_at=$2
WHERE id=$1 AND status='COMPENSATION_PENDING'`, operationID, now)
	if err != nil || command.RowsAffected() != 1 {
		return ErrRegistrationCompensationForbidden
	}
	if err := appendAudit(ctx, tx, actorID, principal.SessionID, nil, "registration.compensation.complete", "registration_operation", operationID.String(), "SUCCEEDED", map[string]any{"identity_disabled": true, "reservation_released": true}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func compensationProof(ctx context.Context, tx pgx.Tx, operationID uuid.UUID, now time.Time) (RegistrationOperation, error) {
	operation, err := scanRegistrationOperation(tx.QueryRow(ctx, registrationOperationSelect+` WHERE ro.id=$1 FOR UPDATE OF ro`, operationID))
	if err != nil {
		return RegistrationOperation{}, err
	}
	if operation.UserID != nil || operation.IdentitySubject == "" || operation.IdentityCreatedAt == nil || operation.IdentityCreatedAt.Add(24*time.Hour).After(now) || operation.Status != "REPAIR_REQUIRED" && operation.Status != "COMPENSATION_PENDING" {
		return RegistrationOperation{}, ErrRegistrationCompensationForbidden
	}
	var factsExist bool
	err = tx.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM users WHERE id=$1 OR (status<>'DELETED' AND (lower(username)=lower($2) OR mobile_e164=$3)))
 OR EXISTS(SELECT 1 FROM identities WHERE subject=$4)
 OR EXISTS(SELECT 1 FROM wallet_provisioning_jobs WHERE user_id=$1)
 OR EXISTS(SELECT 1 FROM outbox_events WHERE aggregate_id=$1)`, operation.CandidateUserID, operation.Username, operation.Mobile, operation.IdentitySubject).Scan(&factsExist)
	if err != nil || factsExist {
		return RegistrationOperation{}, ErrRegistrationCompensationForbidden
	}
	return operation, nil
}
