package account

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const mobileSignupConsentVersion = "mobile-signup-v1"
const signupCreateNotDispatched = "SMS_SIGNUP_CREATE_NOT_DISPATCHED"

var (
	errMobileSignupConsent = errors.New("invalid mobile signup consent")
	errMobileSignupReview  = errors.New("mobile registration requires manual review")
)

type mobileSignupRepository interface {
	MobileSignupConsented(context.Context, uuid.UUID, string) (bool, error)
	CompleteConsentedSMS(context.Context, string, RegistrationOperation) (string, RegistrationOperation, error)
	CommittedMobileSignupSubject(context.Context, string, uuid.UUID, uuid.UUID) (string, error)
}

func (s *MobileAuthService) compensateKnownRegistrationDispatch(ctx context.Context, operationID uuid.UUID, code string, version int64) error {
	if code != signupCreateNotDispatched {
		return s.repository.CompensatePreIdentityRegistration(ctx, operationID, code, version)
	}
	retry := time.NewTicker(100 * time.Millisecond)
	defer retry.Stop()
	for {
		err := s.repository.CompensatePreIdentityRegistration(ctx, operationID, code, version)
		if err == nil || errors.Is(err, ErrRegistrationConflict) {
			return err
		}
		select {
		case <-ctx.Done():
			slog.WarnContext(ctx, "SMS registration dispatch proof persistence expired", "operation_id", operationID, "attempt_version", version)
			return err
		case <-retry.C:
		}
	}
}

func (s *MobileAuthService) verifyMobileSignup(ctx context.Context, repository mobileSignupRepository, mobile string, challenge uuid.UUID, code string) (string, error) {
	verified, err := s.checkSMSCode(ctx, challenge, mobile, SMSPurposeLogin, code)
	if err != nil {
		return "", err
	}
	if !verified {
		if err := s.repository.ConsumeSMSChallenge(ctx, challenge, mobile, SMSPurposeLogin, false); err != nil {
			return "", err
		}
		return "", ErrSMSInvalid
	}

	id := uuid.Must(uuid.NewV7())
	queryToken, _, err := s.tokens.Issue(id)
	if err != nil {
		return "", err
	}
	// The generated login name encodes neither the phone number nor the user ID.
	username := "user-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
	queryHash := registrationTokenHash(queryToken)
	policy := identity.MobileSignupCredentialMode
	fingerprint := registrationFingerprint(username, mobile, challenge, policy)
	candidate := RegistrationOperation{
		ID: id, CandidateUserID: uuid.Must(uuid.NewV7()), Username: username, Mobile: mobile,
		ChallengeID: challenge, PasswordPolicyVersion: policy,
		QueryTokenHash: queryHash[:], RequestFingerprint: fingerprint[:],
	}
	subject, operation, err := repository.CompleteConsentedSMS(ctx, s.issuer, candidate)
	if err != nil || subject != "" {
		return subject, err
	}
	// From this point the consumed challenge and registration are durable. A
	// lost client response must not make the recovery worker start another one.
	resumeCtx, cancel, err := withIdentityRecoveryClaim(context.WithoutCancel(ctx), "registration", operation.ID, operation.RecoveryGeneration, operation.RecoveryLeaseUntil)
	if err != nil {
		return "", ErrRegistrationRecovering
	}
	defer cancel()
	result, err := s.resumeRegistration(resumeCtx, operation)
	if result.UserID == nil && result.Status == "REPAIR_REQUIRED" {
		return "", errMobileSignupReview
	}
	if errors.Is(err, ErrRegistrationRestart) {
		return "", ErrRegistrationRestart
	}
	if err != nil || result.UserID == nil {
		return "", ErrRegistrationRecovering
	}
	return repository.CommittedMobileSignupSubject(resumeCtx, s.issuer, operation.ID, *result.UserID)
}

func (r *PostgresRepository) MobileSignupConsented(ctx context.Context, id uuid.UUID, mobile string) (bool, error) {
	var consented bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM sms_challenges WHERE id=$1 AND mobile_e164=$2 AND purpose='LOGIN' AND signup_consent_version=$3
	)`, id, mobile, mobileSignupConsentVersion).Scan(&consented)
	return consented, err
}

func (r *PostgresRepository) CompleteConsentedSMS(ctx context.Context, issuer string, operation RegistrationOperation) (string, RegistrationOperation, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return "", RegistrationOperation{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockRegistrationMobile(ctx, tx, operation.Mobile); err != nil {
		return "", RegistrationOperation{}, err
	}
	// Recheck expiry under the challenge lock, including time spent waiting for
	// another submitter to record a verification attempt.
	var issuedBinding []byte
	err = tx.QueryRow(ctx, `
SELECT signup_login_binding FROM sms_challenges
WHERE id=$1 AND mobile_e164=$2 AND purpose='LOGIN' AND signup_consent_version=$3
FOR UPDATE`, operation.ChallengeID, operation.Mobile, mobileSignupConsentVersion).Scan(&issuedBinding)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", RegistrationOperation{}, ErrSMSInvalid
	}
	if err != nil {
		return "", RegistrationOperation{}, err
	}
	now := r.now().Truncate(time.Microsecond)
	command, err := tx.Exec(ctx, `
UPDATE sms_challenges SET status='CONSUMED',consumed_at=$2,updated_at=$2
WHERE id=$1
  AND (status='SENT' OR (status='CREATED' AND delivery_started_at IS NOT NULL))
  AND attempts<5 AND expires_at>$2`, operation.ChallengeID, now)
	if err != nil {
		return "", RegistrationOperation{}, err
	}
	if command.RowsAffected() != 1 {
		return "", RegistrationOperation{}, ErrSMSInvalid
	}
	snapshot, err := readSMSLoginSnapshot(ctx, tx, issuer, operation.Mobile, now)
	if err != nil {
		return "", RegistrationOperation{}, err
	}
	if subtle.ConstantTimeCompare(issuedBinding, snapshot.binding[:]) != 1 {
		if err := tx.Commit(ctx); err != nil {
			return "", RegistrationOperation{}, err
		}
		return "", RegistrationOperation{}, ErrSMSInvalid
	}
	if snapshot.subject != "" {
		if err := storeSMSAuthorizationBinding(ctx, tx, operation.ChallengeID, snapshot.binding); err != nil {
			return "", RegistrationOperation{}, err
		}
		return snapshot.subject, RegistrationOperation{}, tx.Commit(ctx)
	}

	if snapshot.occupied && snapshot.pendingStatus == "" {
		retired, err := supersedeExpiredMobileSignupReservation(ctx, tx, operation.Mobile)
		if err != nil {
			return "", RegistrationOperation{}, err
		}
		snapshot, err = readSMSLoginSnapshot(ctx, tx, issuer, operation.Mobile, now)
		if err != nil {
			return "", RegistrationOperation{}, err
		}
		if retired && (snapshot.occupied || snapshot.pendingStatus != "") {
			// Do not commit a retirement without accepting the corresponding signup.
			return "", RegistrationOperation{}, ErrRegistrationRecovering
		}
	}
	if snapshot.occupied || snapshot.pendingStatus != "" {
		if err := tx.Commit(ctx); err != nil {
			return "", RegistrationOperation{}, err
		}
		if snapshot.occupied {
			return "", RegistrationOperation{}, ErrSMSInvalid
		}
		if snapshot.pendingStatus == "REPAIR_REQUIRED" {
			return "", RegistrationOperation{}, errMobileSignupReview
		}
		return "", RegistrationOperation{}, ErrRegistrationRecovering
	}
	if _, err := tx.Exec(ctx, `
UPDATE registration_operations SET status='EXPIRED',last_error_code='RESERVATION_EXPIRED',version=version+1,updated_at=$2
WHERE mobile_e164=$1 AND committed_user_id IS NULL AND status='SMS_PENDING' AND lease_expires_at<=$2`, operation.Mobile, now); err != nil {
		return "", RegistrationOperation{}, err
	}
	operation.Status = "REGISTRATION_IN_PROGRESS"
	operation.RecoveryGeneration = 1
	until := now.Add(10 * time.Second)
	operation.RecoveryLeaseUntil = &until
	_, err = tx.Exec(ctx, `
INSERT INTO registration_operations(
 id,candidate_user_id,query_token_hash,request_fingerprint,username_normalized,mobile_e164,sms_challenge_id,
 password_policy_version,signup_consent_version,status,lease_expires_at,next_retry_at,
 recovery_generation,recovery_lease_until,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,'REGISTRATION_IN_PROGRESS',$10,$11,1,$11,$12,$12)`,
		operation.ID, operation.CandidateUserID, operation.QueryTokenHash, operation.RequestFingerprint,
		operation.Username, operation.Mobile, operation.ChallengeID, operation.PasswordPolicyVersion, mobileSignupConsentVersion,
		now.Add(30*time.Minute), until, now)
	if err != nil {
		return "", RegistrationOperation{}, mapRegistrationConstraintError(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE sms_challenges SET registration_operation_id=$2 WHERE id=$1`, operation.ChallengeID, operation.ID); err != nil {
		return "", RegistrationOperation{}, err
	}
	return "", operation, tx.Commit(ctx)
}

func (r *PostgresRepository) CommittedMobileSignupSubject(ctx context.Context, issuer string, operationID, userID uuid.UUID) (string, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)
	var subject, mobile string
	var challenge uuid.UUID
	err = tx.QueryRow(ctx, `
SELECT i.subject,ro.mobile_e164,ro.sms_challenge_id FROM registration_operations ro
JOIN users u ON u.id=ro.committed_user_id AND u.mobile_e164=ro.mobile_e164
JOIN identities i ON i.user_id=u.id AND i.subject=ro.identity_subject AND i.issuer=$3
WHERE ro.id=$1 AND u.id=$2 AND u.status IN ('PROVISIONING','ACTIVE','REPAIR_REQUIRED')`, operationID, userID, issuer).Scan(&subject, &mobile, &challenge)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrSMSInvalid
	}
	if err != nil {
		return "", err
	}
	snapshot, err := readSMSLoginSnapshot(ctx, tx, issuer, mobile, r.now())
	if err != nil {
		return "", err
	}
	if snapshot.subject != subject {
		return "", ErrSMSInvalid
	}
	if err := storeSMSAuthorizationBinding(ctx, tx, challenge, snapshot.binding); err != nil {
		return "", err
	}
	return subject, tx.Commit(ctx)
}

func storeSMSAuthorizationBinding(ctx context.Context, tx pgx.Tx, challenge uuid.UUID, binding [32]byte) error {
	result, err := tx.Exec(ctx, `
UPDATE sms_challenges SET signup_authorized_binding=$2
WHERE id=$1 AND status='CONSUMED' AND signup_consent_version=$3 AND signup_authorized_binding IS NULL`,
		challenge, binding[:], mobileSignupConsentVersion)
	if err != nil {
		return err
	}
	if result.RowsAffected() != 1 {
		return ErrSMSInvalid
	}
	return nil
}

func (r *PostgresRepository) AuthorizeBoundSMSLogin(ctx context.Context, principal identity.Principal, challenge uuid.UUID) (AuthorizationDecision, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return AuthorizationDecision{}, err
	}
	defer tx.Rollback(ctx)
	var mobile string
	var binding []byte
	now := r.now()
	err = tx.QueryRow(ctx, `
SELECT mobile_e164,signup_authorized_binding FROM sms_challenges
WHERE id=$1 AND purpose='LOGIN' AND status='CONSUMED' AND signup_consent_version=$2
  AND expires_at>$3 AND signup_authorized_binding IS NOT NULL`, challenge, mobileSignupConsentVersion, now).Scan(&mobile, &binding)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthorizationDecision{}, ErrForbidden
	}
	if err != nil {
		return AuthorizationDecision{}, err
	}
	snapshot, err := readSMSLoginSnapshot(ctx, tx, principal.Issuer, mobile, now)
	if err != nil {
		return AuthorizationDecision{}, err
	}
	if snapshot.subject != principal.Subject || subtle.ConstantTimeCompare(binding, snapshot.binding[:]) != 1 {
		return AuthorizationDecision{}, ErrForbidden
	}
	decision, err := authorizePermission(ctx, tx, principal, PermissionPersonalSessionLogin, nil, nil, false)
	if err != nil {
		return AuthorizationDecision{}, err
	}
	return decision, tx.Commit(ctx)
}
func lockRegistrationMobile(ctx context.Context, tx pgx.Tx, mobile string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "registration-mobile:"+mobile)
	return err
}

// Keep tentative Identity updates and current Account mappings ordered exactly
// as ordinary SMS sign-in. The consented flow also binds the challenge to the
// observed profile generation, including the unbound state.
const smsLoginBindingQuery = `
SELECT subject,id,identity_security_version,COALESCE(is_current,FALSE) AS current_mobile
FROM (
  SELECT i.subject,u.id,u.identity_security_version,TRUE AS is_current,i.last_seen_at
  FROM users u JOIN identities i ON i.user_id=u.id AND i.issuer=$2
  WHERE u.mobile_e164=$1 AND u.status<>'DELETED'
  UNION ALL
  SELECT i.subject,u.id,u.identity_security_version,u.mobile_e164=$1,i.last_seen_at
  FROM profile_change_requests p
  JOIN users u ON u.id=p.user_id
  JOIN identities i ON i.issuer=p.identity_issuer AND i.subject=p.identity_subject AND i.user_id=u.id
  WHERE p.field='MOBILE' AND p.new_value=$1 AND p.identity_updated_at IS NOT NULL
    AND p.status IN ('IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY','REPAIR_REQUIRED')
    AND p.identity_issuer=$2 AND u.status<>'DELETED' AND u.mobile_e164 IS DISTINCT FROM $1
) candidates
ORDER BY candidates.is_current DESC NULLS FIRST,last_seen_at DESC LIMIT 1`

type smsLoginSnapshot struct {
	subject       string
	binding       [32]byte
	occupied      bool
	pendingStatus string
}

func readSMSLoginSnapshot(ctx context.Context, tx pgx.Tx, issuer, mobile string, now time.Time) (smsLoginSnapshot, error) {
	var snapshot smsLoginSnapshot
	var userID uuid.UUID
	var version int64
	var current bool
	// Registration completion can change the subject and consume the
	// reservation in one commit. Read them in the same SQL snapshot.
	err := tx.QueryRow(ctx, `WITH binding AS (`+smsLoginBindingQuery+`)
SELECT COALESCE(b.subject,''),COALESCE(b.id,'00000000-0000-0000-0000-000000000000'::uuid),
       COALESCE(b.identity_security_version,0),COALESCE(b.current_mobile,FALSE),
       EXISTS (SELECT 1 FROM users WHERE mobile_e164=$1 AND status<>'DELETED') OR
       EXISTS (SELECT 1 FROM profile_change_requests WHERE field='MOBILE' AND new_value=$1
         AND status IN ('PENDING','VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY','REPAIR_REQUIRED')),
       COALESCE((SELECT status FROM registration_operations WHERE mobile_e164=$1 AND committed_user_id IS NULL
         AND ((status='SMS_PENDING' AND lease_expires_at>$3)
           OR status IN ('REGISTRATION_IN_PROGRESS','IDENTITY_CREATING','IDENTITY_CREATED','ACCOUNT_COMMITTING','REPAIR_REQUIRED','COMPENSATION_PENDING'))
         LIMIT 1),'')
FROM (VALUES (1)) AS singleton(n) LEFT JOIN binding b ON TRUE`, mobile, issuer, now).
		Scan(&snapshot.subject, &userID, &version, &current, &snapshot.occupied, &snapshot.pendingStatus)
	if err != nil {
		return smsLoginSnapshot{}, err
	}
	if snapshot.subject == "" {
		snapshot.binding = sha256Bytes("unbound\n" + issuer)
	} else {
		snapshot.binding = sha256.Sum256(fmt.Appendf(nil, "%s\n%s\n%s\n%d\n%t", issuer, snapshot.subject, userID, version, current))
	}
	return snapshot, nil
}
