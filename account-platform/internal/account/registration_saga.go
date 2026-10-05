package account

import (
	"context"
	"crypto/sha256"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const (
	walletProvisioningPolicyVersion = "wallet-provisioning-v1"
	walletProvisioningSLA           = 15 * time.Minute
)

func (r *PostgresRepository) CreateRegistrationOperation(ctx context.Context, operation RegistrationOperation, challenge SMSChallenge) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if challenge.Source == "" {
		challenge.Source = "source:unknown"
	}
	if challenge.Device == "" {
		challenge.Device = "device:absent:" + challenge.Source
	}
	locks := []string{"device:" + challenge.Device, "mobile:" + challenge.Mobile, "registration-mobile:" + operation.Mobile, "registration-username:" + operation.Username, "source:" + challenge.Source}
	sort.Strings(locks)
	last := ""
	for _, lock := range locks {
		if lock == last {
			continue
		}
		last = lock
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lock); err != nil {
			return err
		}
	}
	now := r.now()
	_, err = tx.Exec(ctx, `
UPDATE registration_operations
SET status='EXPIRED',last_error_code='RESERVATION_EXPIRED',version=version+1,updated_at=$1
WHERE status='SMS_PENDING' AND lease_expires_at<=$1
  AND (username_normalized=$2 OR mobile_e164=$3)`, now, operation.Username, operation.Mobile)
	if err != nil {
		return err
	}
	var usernameExists, mobileExists bool
	if operation.LegacyIntent {
		var pending MobileRegistration
		if operation.ID != operation.CandidateUserID {
			return ErrRegistrationConflict
		}
		if err := tx.QueryRow(ctx, pendingMobileRegistrationSQL+" FOR UPDATE OF u", operation.Username, operation.Mobile, operation.IssuerDigest).Scan(&pending.UserID, &pending.Username, &pending.Mobile); err != nil || pending.UserID != operation.ID {
			return ErrRegistrationConflict
		}
	} else {
		if err := tx.QueryRow(ctx, `SELECT
EXISTS(SELECT 1 FROM users WHERE lower(username)=lower($1)),
EXISTS(SELECT 1 FROM users WHERE mobile_e164=$2 AND status<>'DELETED')`, operation.Username, operation.Mobile).Scan(&usernameExists, &mobileExists); err != nil {
			return err
		}
		if usernameExists {
			return ErrUsernameAlreadyRegistered
		}
		if mobileExists {
			return ErrMobileAlreadyRegistered
		}
	}
	if err := enforceSMSRateLimits(ctx, tx, challenge, now); err != nil {
		if !errors.Is(err, ErrSMSRateLimited) {
			return err
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return commitErr
		}
		return err
	}
	_, err = tx.Exec(ctx, `
INSERT INTO registration_operations(
 id,candidate_user_id,query_token_hash,request_fingerprint,username_normalized,mobile_e164,sms_challenge_id,
 password_policy_version,status,version,lease_expires_at,next_retry_at,created_at,updated_at,issuer_digest,legacy_intent)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,'SMS_PENDING',1,$9,$9,$10,$10,$11,$12)`,
		operation.ID, operation.CandidateUserID, operation.QueryTokenHash, operation.RequestFingerprint, operation.Username, operation.Mobile,
		challenge.ID, operation.PasswordPolicyVersion, challenge.ExpiresAt, now, operation.IssuerDigest, operation.LegacyIntent)
	if err != nil {
		return mapRegistrationConstraintError(err)
	}
	_, err = tx.Exec(ctx, `
INSERT INTO sms_challenges(id,mobile_e164,purpose,code_hash,status,expires_at,created_at,updated_at,provider_id,adapter_version,registration_operation_id)
VALUES($1,$2,$3,NULL,'CREATED',$4,$5,$5,$6,$7,$8)`, challenge.ID, challenge.Mobile, challenge.Purpose, challenge.ExpiresAt, now, challenge.Provider.ID, challenge.Provider.AdapterVersion, operation.ID)
	if err != nil {
		return err
	}
	mobileHash, sourceHash, deviceHash := sha256Bytes(challenge.Mobile), sha256Bytes(challenge.Source), sha256Bytes(challenge.Device)
	if err := recordSMSAttempt(ctx, tx, challenge, mobileHash, sourceHash, deviceHash, "ACCEPTED", now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func enforceSMSRateLimits(ctx context.Context, tx pgx.Tx, challenge SMSChallenge, now time.Time) error {
	var last, oldest *time.Time
	var hourly int
	if err := tx.QueryRow(ctx, `SELECT max(created_at) FILTER (WHERE status<>'FAILED'),min(created_at) FILTER (WHERE created_at>$2 AND status<>'FAILED'),count(*) FILTER (WHERE created_at>$2 AND status<>'FAILED') FROM sms_challenges WHERE mobile_e164=$1`, challenge.Mobile, now.Add(-time.Hour)).Scan(&last, &oldest, &hourly); err != nil {
		return err
	}
	mobileHash, sourceHash, deviceHash := sha256Bytes(challenge.Mobile), sha256Bytes(challenge.Source), sha256Bytes(challenge.Device)
	if err := smsRateLimit(now, last, oldest, hourly); err != nil {
		if recordErr := recordSMSAttempt(ctx, tx, challenge, mobileHash, sourceHash, deviceHash, "RATE_LIMITED", now); recordErr != nil {
			return recordErr
		}
		return err
	}
	var sourceHour, sourceDay, deviceHour int
	if err := tx.QueryRow(ctx, `SELECT
count(*) FILTER (WHERE source_hash=$1 AND occurred_at>$3::timestamptz-interval '1 hour'),
count(*) FILTER (WHERE source_hash=$1 AND occurred_at>$3::timestamptz-interval '1 day'),
count(*) FILTER (WHERE device_hash=$2 AND occurred_at>$3::timestamptz-interval '1 hour')
FROM sms_send_attempts WHERE occurred_at>$3::timestamptz-interval '1 day'`, sourceHash[:], deviceHash[:], now).Scan(&sourceHour, &sourceDay, &deviceHour); err != nil {
		return err
	}
	if sourceHour >= 20 || sourceDay >= 100 || deviceHour >= 10 {
		if err := recordSMSAttempt(ctx, tx, challenge, mobileHash, sourceHash, deviceHash, "RATE_LIMITED", now); err != nil {
			return err
		}
		return &SMSRateLimitError{RetryAfter: time.Hour}
	}
	return nil
}

func sha256Bytes(value string) [32]byte { return sha256.Sum256([]byte(value)) }

func (r *PostgresRepository) RegistrationOperation(ctx context.Context, operationID uuid.UUID) (RegistrationOperation, error) {
	return scanRegistrationOperation(r.pool.QueryRow(ctx, registrationOperationSelect+` WHERE ro.id=$1`, operationID))
}

const registrationOperationSelect = `
SELECT ro.id,ro.candidate_user_id,ro.query_token_hash,ro.request_fingerprint,ro.username_normalized,ro.mobile_e164,
ro.sms_challenge_id,ro.password_policy_version,COALESCE(ro.identity_subject,''),
ro.committed_user_id,COALESCE(u.user_code,''),ro.status,ro.version,ro.identity_created_at,ro.next_retry_at,
COALESCE(ro.last_error_code,''),ro.created_at,ro.updated_at,ro.issuer_digest,ro.legacy_intent,ro.password_preserved,ro.identity_create_attempted_at,ro.recovery_generation,ro.recovery_lease_until
FROM registration_operations ro LEFT JOIN users u ON u.id=ro.committed_user_id`

func scanRegistrationOperation(row pgx.Row) (RegistrationOperation, error) {
	var operation RegistrationOperation
	var rawUserCode string
	err := row.Scan(&operation.ID, &operation.CandidateUserID, &operation.QueryTokenHash, &operation.RequestFingerprint, &operation.Username, &operation.Mobile,
		&operation.ChallengeID, &operation.PasswordPolicyVersion, &operation.IdentitySubject,
		&operation.UserID, &rawUserCode, &operation.Status, &operation.Version, &operation.IdentityCreatedAt, &operation.NextRetryAt,
		&operation.LastErrorCode, &operation.CreatedAt, &operation.UpdatedAt, &operation.IssuerDigest, &operation.LegacyIntent, &operation.PasswordPreserved, &operation.IdentityCreateAttemptedAt, &operation.RecoveryGeneration, &operation.RecoveryLeaseUntil)
	if errors.Is(err, pgx.ErrNoRows) {
		return RegistrationOperation{}, ErrNotFound
	}
	operation.UserCode = FormatUserCode(rawUserCode)
	return operation, err
}

func (r *PostgresRepository) ConsumeRegistrationSMS(ctx context.Context, operationID, challengeID uuid.UUID, mobile string, verified bool) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var status string
	var attempts int
	var expires time.Time
	err = tx.QueryRow(ctx, `
SELECT sc.status,sc.attempts,sc.expires_at
FROM registration_operations ro JOIN sms_challenges sc ON sc.id=ro.sms_challenge_id
WHERE ro.id=$1 AND ro.status='SMS_PENDING' AND sc.id=$2 AND sc.mobile_e164=$3 AND sc.purpose='REGISTER'
FOR UPDATE OF ro,sc`, operationID, challengeID, mobile).Scan(&status, &attempts, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSMSInvalid
	}
	if err != nil {
		return err
	}
	if status != "SENT" || attempts >= 5 || !expires.After(r.now()) {
		return ErrSMSInvalid
	}
	if !verified {
		attempts++
		_, err = tx.Exec(ctx, `UPDATE sms_challenges SET attempts=$2,status=CASE WHEN $2>=5 THEN 'FAILED' ELSE status END,updated_at=$3 WHERE id=$1`, challengeID, attempts, r.now())
		if err != nil {
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		return ErrSMSInvalid
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `UPDATE sms_challenges SET status='CONSUMED',consumed_at=$2,updated_at=$2 WHERE id=$1`, challengeID, now); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `
UPDATE registration_operations SET status='REGISTRATION_IN_PROGRESS',lease_expires_at=$2::timestamptz+interval '30 minutes',next_retry_at=$2,
version=version+1,last_error_code=NULL,updated_at=$2 WHERE id=$1 AND status='SMS_PENDING'`, operationID, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrRegistrationConflict
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) MarkRegistrationIdentityCreating(ctx context.Context, operationID uuid.UUID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = r.lockRecoveryClaim(ctx, tx, "registration", operationID); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `
UPDATE registration_operations SET status='IDENTITY_CREATING',next_retry_at=$2::timestamptz+interval '5 seconds',version=version+1,updated_at=$2
WHERE id=$1 AND (status IN ('REGISTRATION_IN_PROGRESS','IDENTITY_CREATING') OR (status='REPAIR_REQUIRED' AND identity_subject IS NULL AND committed_user_id IS NULL))`, operationID, r.now())
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrRegistrationConflict
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) MarkRegistrationCreateAttempted(ctx context.Context, operationID uuid.UUID) (bool, int64, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return false, 0, err
	}
	defer tx.Rollback(ctx)
	if err = r.lockRecoveryClaim(ctx, tx, "registration", operationID); err != nil {
		return false, 0, err
	}
	var priorAttempt *time.Time
	var version int64
	err = tx.QueryRow(ctx, `SELECT identity_create_attempted_at,version FROM registration_operations WHERE id=$1 AND status='IDENTITY_CREATING' AND identity_subject IS NULL AND committed_user_id IS NULL FOR UPDATE`, operationID).Scan(&priorAttempt, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, 0, ErrRegistrationConflict
	}
	if err != nil {
		return false, 0, err
	}
	version++
	_, err = tx.Exec(ctx, `UPDATE registration_operations SET identity_create_attempted_at=COALESCE(identity_create_attempted_at,$2),version=$3,updated_at=$2 WHERE id=$1`, operationID, r.now(), version)
	if err != nil {
		return false, 0, err
	}
	if err = tx.Commit(ctx); err != nil {
		return false, 0, err
	}
	return priorAttempt == nil, version, nil
}

func (r *PostgresRepository) MarkRegistrationIdentityCreated(ctx context.Context, operationID uuid.UUID, subject string, preserved bool) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = r.lockRecoveryClaim(ctx, tx, "registration", operationID); err != nil {
		return err
	}
	if strings.TrimSpace(subject) == "" {
		return ErrRegistrationConflict
	}
	now := r.now()
	command, err := tx.Exec(ctx, `
UPDATE registration_operations SET status=CASE WHEN status='IDENTITY_CREATING' THEN 'IDENTITY_CREATED' ELSE status END,identity_subject=$2,password_preserved=password_preserved OR $4,lease_expires_at=NULL,
identity_created_at=COALESCE(identity_created_at,$3),next_retry_at=$3,version=version+1,last_error_code=NULL,updated_at=$3
WHERE id=$1 AND status IN ('IDENTITY_CREATING','IDENTITY_CREATED','ACCOUNT_COMMITTING','ACCOUNT_COMMITTED','PROVISIONING','ACTIVE','REPAIR_REQUIRED') AND (identity_subject IS NULL OR identity_subject=$2)`, operationID, subject, now, preserved)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrRegistrationConflict
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) CommitRegistration(ctx context.Context, operationID uuid.UUID, issuer string) (RegistrationOperation, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return RegistrationOperation{}, err
	}
	defer tx.Rollback(ctx)
	if err = r.lockRecoveryClaim(ctx, tx, "registration", operationID); err != nil {
		return RegistrationOperation{}, err
	}
	operation, err := scanRegistrationOperation(tx.QueryRow(ctx, registrationOperationSelect+` WHERE ro.id=$1 FOR UPDATE OF ro`, operationID))
	if err != nil {
		return RegistrationOperation{}, err
	}
	if operation.IssuerDigest != registrationIssuerDigest(issuer) {
		return RegistrationOperation{}, ErrRegistrationConflict
	}
	if operation.Status == "PROVISIONING" || operation.Status == "ACCOUNT_COMMITTED" || operation.Status == "ACTIVE" {
		return operation, tx.Commit(ctx)
	}
	if operation.IdentitySubject == "" || operation.Status != "IDENTITY_CREATED" && operation.Status != "ACCOUNT_COMMITTING" && operation.Status != "REPAIR_REQUIRED" {
		return RegistrationOperation{}, ErrRegistrationConflict
	}
	if operation.IssuerDigest != registrationIssuerDigest(issuer) {
		return RegistrationOperation{}, ErrRegistrationConflict
	}
	now := r.now()
	if _, err := tx.Exec(ctx, `UPDATE registration_operations SET status='ACCOUNT_COMMITTING',next_retry_at=$2::timestamptz+interval '5 seconds',version=version+1,updated_at=$2 WHERE id=$1`, operationID, now); err != nil {
		return RegistrationOperation{}, err
	}
	var usernameExists, mobileExists bool
	var rawUserCode string
	if operation.LegacyIntent {
		var pending MobileRegistration
		if operation.ID != operation.CandidateUserID {
			return RegistrationOperation{}, ErrRegistrationConflict
		}
		if err := tx.QueryRow(ctx, pendingMobileRegistrationSQL+" FOR UPDATE OF u", operation.Username, operation.Mobile, operation.IssuerDigest).Scan(&pending.UserID, &pending.Username, &pending.Mobile); err != nil || pending.UserID != operation.ID {
			return RegistrationOperation{}, ErrRegistrationConflict
		}
		if err := tx.QueryRow(ctx, `SELECT user_code FROM users WHERE id=$1`, pending.UserID).Scan(&rawUserCode); err != nil {
			return RegistrationOperation{}, err
		}
	} else {
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE lower(username)=lower($1)),EXISTS(SELECT 1 FROM users WHERE mobile_e164=$2 AND status<>'DELETED')`, operation.Username, operation.Mobile).Scan(&usernameExists, &mobileExists); err != nil {
			return RegistrationOperation{}, err
		}
		if usernameExists {
			return RegistrationOperation{}, ErrUsernameAlreadyRegistered
		}
		if mobileExists {
			return RegistrationOperation{}, ErrMobileAlreadyRegistered
		}
		rawUserCode, err = insertUserWithGeneratedCode(ctx, tx, func(database pgx.Tx, candidate string) error {
			_, insertErr := database.Exec(ctx, `INSERT INTO users(id,user_code,status,username,display_name,mobile_e164,mobile_verified_at,created_at,updated_at) VALUES($1,$2,'PROVISIONING',$3,$3,$4,$5,$5,$5)`, operation.CandidateUserID, candidate, operation.Username, operation.Mobile, now)
			return insertErr
		})
		if err != nil {
			return RegistrationOperation{}, mapRegistrationConstraintError(err)
		}
	}
	if _, err = tx.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES($1,$2,$3,$4,$4)`, issuer, operation.IdentitySubject, operation.CandidateUserID, now); err != nil {
		return RegistrationOperation{}, err
	}
	if err = enqueueCommerceCustomerSync(ctx, tx, "USER", operation.CandidateUserID, operation.Mobile, r.commerceProviderID, r.commerceAdapterVersion, "CUSTOMER_PROVISIONING", now); err != nil {
		return RegistrationOperation{}, err
	}
	if _, err = tx.Exec(ctx, `
INSERT INTO wallet_provisioning_jobs(user_id,registration_operation_id,status,policy_version,sla_expires_at,next_attempt_at,created_at,updated_at)
VALUES($1,$2,'PENDING',$3,$4,$5,$5,$5)`, operation.CandidateUserID, operation.ID, r.walletPolicyVersion, now.Add(r.walletProvisioningSLA), now); err != nil {
		return RegistrationOperation{}, err
	}
	if err := appendOutbox(ctx, tx, "account.wallet.provisioning.requested.v1", operation.CandidateUserID, 1, map[string]any{"user_id": operation.CandidateUserID, "registration_operation_id": operation.ID, "policy_version": r.walletPolicyVersion}); err != nil {
		return RegistrationOperation{}, err
	}
	if err := appendAudit(ctx, tx, operation.CandidateUserID, "registration", nil, "account.mobile.register", "registration_operation", operation.ID.String(), "SUCCEEDED", map[string]any{"mobile_verified": true, "status": "PROVISIONING"}); err != nil {
		return RegistrationOperation{}, err
	}
	if operation.LegacyIntent {
		if err := appendAudit(ctx, tx, operation.CandidateUserID, "registration", nil, "account.mobile.registration.complete", "mobile_registration", operation.CandidateUserID.String(), "SUCCEEDED", map[string]any{"mobile_verified": true}); err != nil {
			return RegistrationOperation{}, err
		}
	}
	_, err = tx.Exec(ctx, `
UPDATE registration_operations SET status='PROVISIONING',committed_user_id=$2,account_committed_at=$3,next_retry_at=$3,
version=version+1,last_error_code=NULL,updated_at=$3 WHERE id=$1`, operation.ID, operation.CandidateUserID, now)
	if err != nil {
		return RegistrationOperation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return RegistrationOperation{}, err
	}
	operation.UserID = &operation.CandidateUserID
	operation.UserCode = FormatUserCode(rawUserCode)
	operation.Status = "PROVISIONING"
	operation.NextRetryAt = now
	operation.UpdatedAt = now
	return operation, nil
}

func (r *PostgresRepository) ScheduleRegistrationRetry(ctx context.Context, operationID uuid.UUID, code string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = r.lockRecoveryClaim(ctx, tx, "registration", operationID); err != nil {
		return err
	}
	now := r.now()
	_, err = tx.Exec(ctx, `
UPDATE registration_operations SET
 status=CASE WHEN (identity_created_at IS NOT NULL AND identity_created_at+interval '30 minutes'<=$2) OR (identity_created_at IS NULL AND lease_expires_at<=$2) THEN 'REPAIR_REQUIRED' ELSE status END,
 attempts=attempts+1,next_retry_at=$2::timestamptz+LEAST(interval '5 minutes',interval '5 seconds'*power(2,LEAST(attempts,6))),
 last_error_code=$3,version=version+1,updated_at=$2
WHERE id=$1 AND status IN ('REGISTRATION_IN_PROGRESS','IDENTITY_CREATING','IDENTITY_CREATED','ACCOUNT_COMMITTING','REPAIR_REQUIRED')`, operationID, now, code)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) CompensatePreIdentityRegistration(ctx context.Context, operationID uuid.UUID, code string, attemptVersion int64) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err = r.lockRecoveryClaim(ctx, tx, "registration", operationID); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `
UPDATE registration_operations SET status='COMPENSATED',lease_expires_at=NULL,next_retry_at=$2,
last_error_code=$3,compensation_evidence=jsonb_build_object('authority','IDENTITY_ADAPTER','creation_never_dispatched',true),version=version+1,updated_at=$2
WHERE id=$1 AND identity_subject IS NULL AND committed_user_id IS NULL AND legacy_intent=false
  AND $3='IDENTITY_CREATE_NOT_DISPATCHED' AND version=$4 AND $4>0
  AND status IN ('REGISTRATION_IN_PROGRESS','IDENTITY_CREATING','REPAIR_REQUIRED')`, operationID, r.now(), code, attemptVersion)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrRegistrationConflict
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) LeaseRegistrationRecoveries(ctx context.Context, limit int, lease time.Duration) ([]RegistrationOperation, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	now := r.now().Truncate(time.Microsecond)
	if _, err := tx.Exec(ctx, `
UPDATE registration_operations SET status='REPAIR_REQUIRED',last_error_code='FORWARD_RECOVERY_SLA_EXCEEDED',next_retry_at=$1,
version=version+1,updated_at=$1
WHERE (status IN ('IDENTITY_CREATED','ACCOUNT_COMMITTING') AND identity_created_at+interval '30 minutes'<=$1)
 OR (status IN ('REGISTRATION_IN_PROGRESS','IDENTITY_CREATING') AND identity_created_at IS NULL AND lease_expires_at<=$1)`, now); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, registrationOperationSelect+`
 WHERE ro.id IN (
   SELECT id FROM registration_operations
   WHERE status IN ('REGISTRATION_IN_PROGRESS','IDENTITY_CREATING','IDENTITY_CREATED','ACCOUNT_COMMITTING')
     AND next_retry_at<=$1 AND (recovery_lease_until IS NULL OR recovery_lease_until<=$1)
     AND (identity_created_at IS NULL OR identity_created_at+interval '30 minutes'>$1)
   ORDER BY next_retry_at,created_at FOR UPDATE SKIP LOCKED LIMIT $2
 ) ORDER BY ro.next_retry_at,ro.created_at`, now, limit)
	if err != nil {
		return nil, err
	}
	operations := make([]RegistrationOperation, 0, limit)
	ids := make([]uuid.UUID, 0, limit)
	for rows.Next() {
		operation, scanErr := scanRegistrationOperation(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		operations, ids = append(operations, operation), append(ids, operation.ID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(ids) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE registration_operations SET recovery_generation=recovery_generation+1,recovery_lease_until=$2::timestamptz+$3::interval,updated_at=$2 WHERE id=ANY($1)`, ids, now, lease.String()); err != nil {
			return nil, err
		}
	}
	for i := range operations {
		operations[i].RecoveryGeneration++
		until := now.Add(lease)
		operations[i].RecoveryLeaseUntil = &until
	}
	return operations, tx.Commit(ctx)
}

func mapRegistrationConstraintError(err error) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	if strings.Contains(message, "active_username") || strings.Contains(message, "users_username") {
		return ErrUsernameAlreadyRegistered
	}
	if strings.Contains(message, "active_mobile") || strings.Contains(message, "users_mobile") {
		return ErrMobileAlreadyRegistered
	}
	return err
}

func registrationRetryDelay(attempts int) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	return min(5*time.Minute, 5*time.Second*time.Duration(1<<min(attempts, 6)))
}
