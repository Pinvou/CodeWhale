package account

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrRecentAuthenticationRequired      = errors.New("recent authentication is required")
	ErrProfileChangeConflict             = errors.New("profile change conflicts with current state")
	ErrProfileChangeVerificationRequired = errors.New("profile change verification is required")
)

type ProfileChange struct {
	ID                     uuid.UUID  `json:"profile_change_request_id"`
	UserID                 uuid.UUID  `json:"-"`
	IdentityIssuer         string     `json:"-"`
	IdentitySubject        string     `json:"-"`
	Field                  string     `json:"field"`
	OldValue               string     `json:"-"`
	NewValue               string     `json:"new_value"`
	ExpectedProfileVersion int64      `json:"-"`
	SMSChallengeID         *uuid.UUID `json:"sms_challenge_id,omitempty"`
	SMSVerifiedAt          *time.Time `json:"-"`
	IdentityUpdatedAt      *time.Time `json:"-"`
	AccountCommittedAt     *time.Time `json:"-"`
	SessionsRevokedAt      *time.Time `json:"-"`
	Status                 string     `json:"status"`
	NextAttemptAt          time.Time  `json:"next_retry_at"`
	SLAExpiresAt           time.Time  `json:"sla_expires_at"`
	Attempts               int        `json:"-"`
	LastErrorCode          string     `json:"reason,omitempty"`
}

type ProfileChangeStore interface {
	CreateProfileChange(context.Context, identity.Principal, string, string) (ProfileChange, error)
	ProfileChange(context.Context, uuid.UUID, *uuid.UUID) (ProfileChange, error)
	ActiveProfileSMSChallenge(context.Context, uuid.UUID) (*SMSChallenge, error)
	BindProfileSMSChallenge(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error
	MarkProfileSMSVerified(context.Context, uuid.UUID, uuid.UUID) error
	MarkProfileIdentityUpdated(context.Context, uuid.UUID) error
	CommitProfileChange(context.Context, uuid.UUID) error
	MarkProfileSessionsRevoked(context.Context, uuid.UUID) error
	CompleteProfileChange(context.Context, uuid.UUID) error
	ScheduleProfileChangeRetry(context.Context, uuid.UUID, string) error
	ClaimProfileChange(context.Context) (*ProfileChange, error)
	CreatePasswordReset(context.Context, string, string, uuid.UUID) (uuid.UUID, uuid.UUID, error)
	CreateAuthenticatedPasswordReset(context.Context, identity.Principal) (uuid.UUID, string, string, error)
	CompletePasswordReset(context.Context, uuid.UUID, string) error
	ListProfileRepairs(context.Context) ([]profileChangeRepairItem, error)
	RetryProfileRepair(context.Context, identity.Principal, uuid.UUID) error
}

type ProfileChangeService struct {
	store      ProfileChangeStore
	auth       *MobileAuthService
	identities IdentityProvisioner
	now        func() time.Time
}

func NewProfileChangeService(store ProfileChangeStore, auth *MobileAuthService, identities IdentityProvisioner) (*ProfileChangeService, error) {
	if store == nil || auth == nil || identities == nil {
		return nil, errors.New("profile changes are not fully configured")
	}
	return &ProfileChangeService{store: store, auth: auth, identities: identities, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (s *ProfileChangeService) requireRecent(principal identity.Principal) error {
	if principal.AuthenticatedAt.IsZero() || s.now().Sub(principal.AuthenticatedAt) < 0 || s.now().Sub(principal.AuthenticatedAt) > 5*time.Minute {
		return ErrRecentAuthenticationRequired
	}
	return nil
}

func (s *ProfileChangeService) Create(ctx context.Context, principal identity.Principal, field, value string) (ProfileChange, error) {
	if err := s.requireRecent(principal); err != nil {
		return ProfileChange{}, err
	}
	field = strings.ToUpper(strings.TrimSpace(field))
	var normalized string
	var err error
	switch field {
	case "USERNAME":
		normalized, err = NormalizeRegistrationUsername(value)
	case "MOBILE":
		normalized, err = NormalizeMainlandMobile(value)
	default:
		err = ErrProfileChangeConflict
	}
	if err != nil {
		return ProfileChange{}, err
	}
	return s.store.CreateProfileChange(ctx, principal, field, normalized)
}

func (s *ProfileChangeService) SendMobileCode(ctx context.Context, principal identity.Principal, requestID uuid.UUID, captchaID uuid.UUID, offset int, source, device string) (SMSChallenge, error) {
	if err := s.requireRecent(principal); err != nil {
		return SMSChallenge{}, err
	}
	change, err := s.store.ProfileChange(ctx, requestID, nil)
	if err != nil {
		return SMSChallenge{}, err
	}
	if change.IdentityIssuer != principal.Issuer || change.IdentitySubject != principal.Subject || change.Field != "MOBILE" || change.Status != "PENDING" {
		return SMSChallenge{}, ErrProfileChangeConflict
	}
	if existing, err := s.store.ActiveProfileSMSChallenge(ctx, requestID); err != nil {
		return SMSChallenge{}, err
	} else if existing != nil {
		return *existing, nil
	}
	challenge, err := s.auth.Send(ctx, "", change.NewValue, "", "", SMSPurposeProfileMobile, captchaID, offset, source, device)
	if err != nil {
		return SMSChallenge{}, err
	}
	if err := s.store.BindProfileSMSChallenge(ctx, requestID, change.UserID, challenge.ID); err != nil {
		return SMSChallenge{}, err
	}
	return challenge, nil
}

func (s *ProfileChangeService) Complete(ctx context.Context, principal identity.Principal, requestID uuid.UUID, code string) (ProfileChange, error) {
	if err := s.requireRecent(principal); err != nil {
		return ProfileChange{}, err
	}
	change, err := s.store.ProfileChange(ctx, requestID, nil)
	if err != nil {
		return ProfileChange{}, err
	}
	if change.IdentityIssuer != principal.Issuer || change.IdentitySubject != principal.Subject {
		return ProfileChange{}, ErrNotFound
	}
	if change.Field == "MOBILE" && change.SMSVerifiedAt == nil {
		if change.SMSChallengeID == nil {
			return ProfileChange{}, ErrProfileChangeVerificationRequired
		}
		if err := s.auth.VerifyProfileMobile(ctx, change.NewValue, *change.SMSChallengeID, code); err != nil {
			return ProfileChange{}, err
		}
		if err := s.store.MarkProfileSMSVerified(ctx, change.ID, change.UserID); err != nil {
			return ProfileChange{}, err
		}
		change.SMSVerifiedAt = ptrTime(s.now())
	}
	if change.Field == "USERNAME" && change.Status == "PENDING" {
		change.Status = "VERIFIED"
	}
	return s.resume(ctx, change)
}

func (s *ProfileChangeService) resume(ctx context.Context, change ProfileChange) (ProfileChange, error) {
	if change.Status == "COMPLETED" {
		return change, nil
	}
	if change.IdentityUpdatedAt == nil {
		var err error
		if change.Field == "USERNAME" {
			err = s.identities.UpdateUsername(ctx, change.IdentitySubject, change.ID, change.NewValue)
		} else {
			err = s.identities.UpdateMobile(ctx, change.IdentitySubject, change.ID, change.NewValue)
		}
		if err != nil {
			_ = s.store.ScheduleProfileChangeRetry(context.WithoutCancel(ctx), change.ID, "IDENTITY_UPDATE_UNAVAILABLE")
			return change, err
		}
		if err := s.store.MarkProfileIdentityUpdated(ctx, change.ID); err != nil {
			return change, err
		}
		change.IdentityUpdatedAt = ptrTime(s.now())
	}
	if change.AccountCommittedAt == nil {
		if err := s.store.CommitProfileChange(ctx, change.ID); err != nil {
			_ = s.store.ScheduleProfileChangeRetry(context.WithoutCancel(ctx), change.ID, "ACCOUNT_COMMIT_FAILED")
			return change, err
		}
		change.AccountCommittedAt = ptrTime(s.now())
	}
	if change.SessionsRevokedAt == nil {
		if err := s.identities.RevokeSessions(ctx, change.IdentitySubject); err != nil {
			_ = s.store.ScheduleProfileChangeRetry(context.WithoutCancel(ctx), change.ID, "SESSION_REVOCATION_FAILED")
			return change, err
		}
		if err := s.store.MarkProfileSessionsRevoked(ctx, change.ID); err != nil {
			return change, err
		}
	}
	if err := s.store.CompleteProfileChange(ctx, change.ID); err != nil {
		return change, err
	}
	return s.store.ProfileChange(ctx, change.ID, nil)
}

func (s *ProfileChangeService) ResetPassword(ctx context.Context, mobile string, challengeID uuid.UUID, code, handle, policyVersion string) error {
	normalized, err := NormalizeMainlandMobile(mobile)
	if err != nil {
		return err
	}
	subject, err := s.auth.VerifyPasswordReset(ctx, normalized, challengeID, code)
	if err != nil {
		return err
	}
	operationID, _, err := s.store.CreatePasswordReset(ctx, s.auth.issuer, subject, challengeID)
	if err != nil {
		return err
	}
	if err := s.identities.ResetPassword(ctx, subject, operationID, normalized, handle, policyVersion); err != nil {
		_ = s.store.CompletePasswordReset(context.WithoutCancel(ctx), operationID, "IDENTITY_RESET_FAILED")
		return err
	}
	return s.store.CompletePasswordReset(ctx, operationID, "")
}

func (s *ProfileChangeService) ChangePassword(ctx context.Context, principal identity.Principal, handle, policyVersion string) error {
	if err := s.requireRecent(principal); err != nil {
		return err
	}
	operationID, subject, mobile, err := s.store.CreateAuthenticatedPasswordReset(ctx, principal)
	if err != nil {
		return err
	}
	if err := s.identities.ResetPassword(ctx, subject, operationID, mobile, handle, policyVersion); err != nil {
		_ = s.store.CompletePasswordReset(context.WithoutCancel(ctx), operationID, "IDENTITY_RESET_FAILED")
		return err
	}
	return s.store.CompletePasswordReset(ctx, operationID, "")
}

type ProfileChangeWorker struct {
	service  *ProfileChangeService
	interval time.Duration
}

func NewProfileChangeWorker(service *ProfileChangeService, interval time.Duration) *ProfileChangeWorker {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	return &ProfileChangeWorker{service: service, interval: interval}
}
func (w *ProfileChangeWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		change, _ := w.service.store.ClaimProfileChange(ctx)
		if change != nil {
			_, _ = w.service.resume(ctx, *change)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func ptrTime(value time.Time) *time.Time { return &value }

func (r *PostgresRepository) CreateProfileChange(ctx context.Context, principal identity.Principal, field, value string) (ProfileChange, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ProfileChange{}, err
	}
	defer tx.Rollback(ctx)
	var change ProfileChange
	var username, mobile *string
	err = tx.QueryRow(ctx, `SELECT u.id,i.issuer,i.subject,u.username,u.mobile_e164,u.profile_version FROM users u JOIN identities i ON i.user_id=u.id WHERE i.issuer=$1 AND i.subject=$2 AND u.status IN ('ACTIVE','PROVISIONING','REPAIR_REQUIRED') FOR UPDATE OF u`, principal.Issuer, principal.Subject).Scan(&change.UserID, &change.IdentityIssuer, &change.IdentitySubject, &username, &mobile, &change.ExpectedProfileVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProfileChange{}, ErrNotFound
	}
	if err != nil {
		return ProfileChange{}, err
	}
	change.ID = uuid.Must(uuid.NewV7())
	change.Field = field
	change.NewValue = value
	change.Status = "PENDING"
	if field == "USERNAME" {
		change.Status = "VERIFIED"
	}
	change.NextAttemptAt = r.now()
	change.SLAExpiresAt = r.now().Add(30 * time.Minute)
	if field == "USERNAME" {
		if username != nil && *username == value {
			return ProfileChange{}, ErrProfileChangeConflict
		}
		if username != nil {
			change.OldValue = *username
		}
	} else {
		if mobile != nil && *mobile == value {
			return ProfileChange{}, ErrProfileChangeConflict
		}
		if mobile != nil {
			change.OldValue = *mobile
		}
	}
	if field == "USERNAME" {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE username=$1 AND status<>'DELETED')`, value).Scan(&exists); err != nil {
			return ProfileChange{}, err
		}
		if exists {
			return ProfileChange{}, ErrUsernameAlreadyRegistered
		}
	} else {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE mobile_e164=$1 AND status<>'DELETED')`, value).Scan(&exists); err != nil {
			return ProfileChange{}, err
		}
		if exists {
			return ProfileChange{}, ErrMobileAlreadyRegistered
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO profile_change_requests(id,user_id,identity_issuer,identity_subject,field,old_value,new_value,expected_profile_version,status,next_attempt_at,sla_expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$10,$10)`, change.ID, change.UserID, change.IdentityIssuer, change.IdentitySubject, field, change.OldValue, value, change.ExpectedProfileVersion, change.Status, change.NextAttemptAt, change.SLAExpiresAt)
	if err != nil {
		if strings.Contains(err.Error(), "profile_change_requests_active") {
			return ProfileChange{}, ErrProfileChangeConflict
		}
		return ProfileChange{}, err
	}
	if err := appendAudit(ctx, tx, change.UserID, principal.SessionID, nil, "account.profile_change.requested", "profile_change", change.ID.String(), "SUCCEEDED", map[string]any{"field": field}); err != nil {
		return ProfileChange{}, err
	}
	return change, tx.Commit(ctx)
}

func scanProfileChange(row pgx.Row) (ProfileChange, error) {
	var c ProfileChange
	err := row.Scan(&c.ID, &c.UserID, &c.IdentityIssuer, &c.IdentitySubject, &c.Field, &c.OldValue, &c.NewValue, &c.ExpectedProfileVersion, &c.SMSChallengeID, &c.SMSVerifiedAt, &c.IdentityUpdatedAt, &c.AccountCommittedAt, &c.SessionsRevokedAt, &c.Status, &c.NextAttemptAt, &c.SLAExpiresAt, &c.Attempts, &c.LastErrorCode)
	return c, err
}

const profileChangeSelect = `SELECT id,user_id,identity_issuer,identity_subject,field,old_value,new_value,expected_profile_version,sms_challenge_id,sms_verified_at,identity_updated_at,account_committed_at,sessions_revoked_at,status,next_attempt_at,sla_expires_at,attempts,COALESCE(last_error_code,'') FROM profile_change_requests`

func (r *PostgresRepository) ProfileChange(ctx context.Context, id uuid.UUID, userID *uuid.UUID) (ProfileChange, error) {
	query := profileChangeSelect + ` WHERE id=$1`
	args := []any{id}
	if userID != nil {
		query += ` AND user_id=$2`
		args = append(args, *userID)
	}
	c, err := scanProfileChange(r.pool.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}
func (r *PostgresRepository) BindProfileSMSChallenge(ctx context.Context, id, userID, challengeID uuid.UUID) error {
	command, err := r.pool.Exec(ctx, `UPDATE profile_change_requests p SET sms_challenge_id=$3,version=version+1,updated_at=$4 WHERE id=$1 AND user_id=$2 AND field='MOBILE' AND status='PENDING' AND (sms_challenge_id IS NULL OR NOT EXISTS(SELECT 1 FROM sms_challenges s WHERE s.id=p.sms_challenge_id AND s.status IN ('CREATED','SENT') AND s.expires_at>$4))`, id, userID, challengeID, r.now())
	if err == nil && command.RowsAffected() == 0 {
		return ErrConflict
	}
	return err
}

func (r *PostgresRepository) ActiveProfileSMSChallenge(ctx context.Context, id uuid.UUID) (*SMSChallenge, error) {
	var c SMSChallenge
	err := r.pool.QueryRow(ctx, `SELECT s.id,s.expires_at FROM profile_change_requests p JOIN sms_challenges s ON s.id=p.sms_challenge_id WHERE p.id=$1 AND p.status='PENDING' AND s.status IN ('CREATED','SENT') AND s.expires_at>$2`, id, r.now()).Scan(&c.ID, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	c.Purpose = SMSPurposeProfileMobile
	return &c, nil
}
func (r *PostgresRepository) MarkProfileSMSVerified(ctx context.Context, id, userID uuid.UUID) error {
	command, err := r.pool.Exec(ctx, `UPDATE profile_change_requests SET sms_verified_at=$3,status='VERIFIED',version=version+1,updated_at=$3 WHERE id=$1 AND user_id=$2 AND status='PENDING' AND sms_challenge_id IS NOT NULL`, id, userID, r.now())
	if err == nil && command.RowsAffected() == 0 {
		return ErrConflict
	}
	return err
}
func (r *PostgresRepository) MarkProfileIdentityUpdated(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE profile_change_requests SET identity_updated_at=COALESCE(identity_updated_at,$2),status='IDENTITY_UPDATED',version=version+1,updated_at=$2 WHERE id=$1 AND status IN ('PENDING','VERIFIED','RETRY','IDENTITY_UPDATED')`, id, r.now())
	return err
}

func (r *PostgresRepository) CommitProfileChange(ctx context.Context, id uuid.UUID) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	c, err := scanProfileChange(tx.QueryRow(ctx, profileChangeSelect+` WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return err
	}
	if c.AccountCommittedAt != nil {
		return nil
	}
	if c.IdentityUpdatedAt == nil {
		return ErrConflict
	}
	var rowsAffected int64
	if c.Field == "USERNAME" {
		command, execErr := tx.Exec(ctx, `UPDATE users SET username=$3,profile_version=profile_version+1,version=version+1,updated_at=$4 WHERE id=$1 AND profile_version=$2 AND username IS NOT DISTINCT FROM NULLIF($5,'')`, c.UserID, c.ExpectedProfileVersion, c.NewValue, r.now(), c.OldValue)
		err = execErr
		rowsAffected = command.RowsAffected()
	} else {
		command, execErr := tx.Exec(ctx, `UPDATE users SET mobile_e164=$3,mobile_verified_at=$4,profile_version=profile_version+1,version=version+1,updated_at=$4 WHERE id=$1 AND profile_version=$2 AND mobile_e164 IS NOT DISTINCT FROM NULLIF($5,'')`, c.UserID, c.ExpectedProfileVersion, c.NewValue, r.now(), c.OldValue)
		err = execErr
		rowsAffected = command.RowsAffected()
	}
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return ErrProfileChangeConflict
	}
	if c.Field == "MOBILE" {
		err = enqueueCommerceCustomerSync(ctx, tx, "USER", c.UserID, c.NewValue, r.commerceProviderID, r.commerceAdapterVersion, "PROFILE_SYNC", r.now())
		if err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE profile_change_requests SET account_committed_at=$2,status='ACCOUNT_COMMITTED',version=version+1,updated_at=$2 WHERE id=$1`, id, r.now())
	if err != nil {
		return err
	}
	if err = appendOutbox(ctx, tx, "account.profile.changed.v1", c.UserID, c.ExpectedProfileVersion+1, map[string]any{"user_id": c.UserID, "field": c.Field, "profile_version": c.ExpectedProfileVersion + 1}); err != nil {
		return err
	}
	if err = appendAudit(ctx, tx, c.UserID, "", nil, "account.profile_change.committed", "profile_change", id.String(), "SUCCEEDED", map[string]any{"field": c.Field}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (r *PostgresRepository) MarkProfileSessionsRevoked(ctx context.Context, id uuid.UUID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id=(SELECT user_id FROM profile_change_requests WHERE id=$1)`, id); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE profile_change_requests SET sessions_revoked_at=COALESCE(sessions_revoked_at,$2),version=version+1,updated_at=$2 WHERE id=$1`, id, r.now()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
func (r *PostgresRepository) CompleteProfileChange(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE profile_change_requests SET status='COMPLETED',last_error_code=NULL,version=version+1,updated_at=$2 WHERE id=$1 AND account_committed_at IS NOT NULL AND sessions_revoked_at IS NOT NULL`, id, r.now())
	return err
}
func (r *PostgresRepository) ScheduleProfileChangeRetry(ctx context.Context, id uuid.UUID, code string) error {
	now := r.now()
	_, err := r.pool.Exec(ctx, `UPDATE profile_change_requests SET status=CASE WHEN sla_expires_at<=$2 THEN 'REPAIR_REQUIRED' ELSE 'RETRY' END,last_error_code=$3,next_attempt_at=$2+make_interval(secs=>LEAST(300,power(2,LEAST(attempts,8))::int)),version=version+1,updated_at=$2 WHERE id=$1 AND status<>'COMPLETED'`, id, now, code)
	return err
}
func (r *PostgresRepository) ClaimProfileChange(ctx context.Context) (*ProfileChange, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	c, err := scanProfileChange(tx.QueryRow(ctx, profileChangeSelect+` WHERE status IN ('VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY') AND next_attempt_at<=$1 ORDER BY next_attempt_at FOR UPDATE SKIP LOCKED LIMIT 1`, r.now()))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `UPDATE profile_change_requests SET attempts=attempts+1,next_attempt_at=$2+interval '30 seconds',updated_at=$2 WHERE id=$1`, c.ID, r.now())
	if err != nil {
		return nil, err
	}
	c.Attempts++
	return &c, tx.Commit(ctx)
}

func (r *PostgresRepository) CreatePasswordReset(ctx context.Context, issuer, subject string, challengeID uuid.UUID) (uuid.UUID, uuid.UUID, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	defer tx.Rollback(ctx)
	var userID uuid.UUID
	err = tx.QueryRow(ctx, `
SELECT u.id
FROM identities i JOIN users u ON u.id=i.user_id
WHERE i.issuer=$1 AND i.subject=$2
  AND u.status IN ('ACTIVE','PROVISIONING','REPAIR_REQUIRED')
  AND EXISTS(
    SELECT 1 FROM role_assignments ra
    JOIN roles ro ON ro.code=ra.role_code AND ro.scope='PERSONAL'
    JOIN role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=$3
    WHERE ra.user_id=u.id AND ra.organization_id IS NULL AND ra.unit_id IS NULL
      AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())
  )
FOR UPDATE OF u`, issuer, subject, PermissionPersonalSecurityManage).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	id := uuid.Must(uuid.NewV7())
	_, err = tx.Exec(ctx, `INSERT INTO password_reset_operations(id,user_id,identity_issuer,identity_subject,sms_challenge_id,method,status,created_at) VALUES($1,$2,$3,$4,$5,'SMS','VERIFIED',$6)`, id, userID, issuer, subject, challengeID, r.now())
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if err = appendAudit(ctx, tx, userID, "", nil, "account.password_reset.verified", "password_reset", id.String(), "SUCCEEDED", map[string]any{"method": "SMS"}); err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	return id, userID, tx.Commit(ctx)
}

func (r *PostgresRepository) CreateAuthenticatedPasswordReset(ctx context.Context, principal identity.Principal) (uuid.UUID, string, string, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return uuid.Nil, "", "", err
	}
	defer tx.Rollback(ctx)
	var userID uuid.UUID
	var subject, mobile string
	err = tx.QueryRow(ctx, `
SELECT u.id,i.subject,COALESCE(u.mobile_e164,'')
FROM identities i JOIN users u ON u.id=i.user_id
WHERE i.issuer=$1 AND i.subject=$2
  AND u.status IN ('ACTIVE','PROVISIONING','REPAIR_REQUIRED')
  AND EXISTS(
    SELECT 1 FROM role_assignments ra
    JOIN roles ro ON ro.code=ra.role_code AND ro.scope='PERSONAL'
    JOIN role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=$3
    WHERE ra.user_id=u.id AND ra.organization_id IS NULL AND ra.unit_id IS NULL
      AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())
  )
FOR UPDATE OF u`, principal.Issuer, principal.Subject, PermissionPersonalSecurityManage).Scan(&userID, &subject, &mobile)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, "", "", ErrForbidden
	}
	if err != nil {
		return uuid.Nil, "", "", err
	}
	id := uuid.Must(uuid.NewV7())
	if _, err = tx.Exec(ctx, `INSERT INTO password_reset_operations(id,user_id,identity_issuer,identity_subject,method,status,created_at) VALUES($1,$2,$3,$4,'AUTHENTICATED','VERIFIED',$5)`, id, userID, principal.Issuer, subject, r.now()); err != nil {
		return uuid.Nil, "", "", err
	}
	if err = appendAudit(ctx, tx, userID, principal.Subject, nil, "account.password_change.verified", "password_reset", id.String(), "SUCCEEDED", map[string]any{"method": "RECENT_AUTHENTICATION"}); err != nil {
		return uuid.Nil, "", "", err
	}
	return id, subject, mobile, tx.Commit(ctx)
}
func (r *PostgresRepository) CompletePasswordReset(ctx context.Context, id uuid.UUID, code string) error {
	status := "COMPLETED"
	if code != "" {
		status = "FAILED"
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `UPDATE password_reset_operations SET status=$2,last_error_code=NULLIF($3,''),completed_at=$4 WHERE id=$1 AND status='VERIFIED'`, id, status, code, r.now()); err != nil {
		return err
	}
	if status == "COMPLETED" {
		if _, err = tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id=(SELECT user_id FROM password_reset_operations WHERE id=$1)`, id); err != nil {
			return err
		}
	}
	var userID uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT user_id FROM password_reset_operations WHERE id=$1`, id).Scan(&userID); err != nil {
		return err
	}
	result := "SUCCEEDED"
	if status != "COMPLETED" {
		result = "FAILED"
	}
	if err = appendAudit(ctx, tx, userID, "", nil, "account.password_reset.completed", "password_reset", id.String(), result, map[string]any{"reason": code}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
