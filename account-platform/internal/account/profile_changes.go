package account

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"sort"
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
	FenceVersion           int64      `json:"-"`
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

type PasswordResetOperation struct {
	ID                     uuid.UUID
	UserID                 uuid.UUID
	IdentitySubject        string
	Mobile                 string
	PasswordHandle         string
	PasswordPolicyVersion  string
	FenceVersion           int64
	Status                 string
	PasswordWriteStartedAt *time.Time
	PasswordWriteOwner     *uuid.UUID
	PasswordUpdatedAt      *time.Time
	SessionsRevokedAt      *time.Time
	Attempts               int
	LastErrorCode          string
}

type ProfileChangeStore interface {
	CreateProfileChange(context.Context, identity.Principal, string, string) (ProfileChange, error)
	ProfileChange(context.Context, uuid.UUID, *uuid.UUID) (ProfileChange, error)
	ReserveProfileSMSChallenge(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, int, SMSChallenge) (SMSChallenge, error)
	ClaimProfileSMSDelivery(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, bool) (uuid.UUID, bool, error)
	MarkProfileSMSDeliverySent(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, SMSDelivery) error
	MarkProfileSMSDeliveryFailed(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error
	ReleaseProfileSMSDeliveryLease(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error
	MarkProfileSMSDeliveryUncertain(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error
	CommitProfileSMSVerification(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, bool) error
	MarkProfileIdentityUpdated(context.Context, uuid.UUID) error
	CommitProfileChange(context.Context, uuid.UUID) error
	MarkProfileSessionsRevoked(context.Context, uuid.UUID) error
	CompleteProfileChange(context.Context, uuid.UUID) error
	ScheduleProfileChangeRetry(context.Context, uuid.UUID, string) error
	ClaimProfileChange(context.Context) (*ProfileChange, error)
	PasswordResetByChallenge(context.Context, uuid.UUID) (PasswordResetOperation, error)
	CreatePasswordReset(context.Context, string, uuid.UUID, string, bool, string, string) (PasswordResetOperation, error)
	CreateAuthenticatedPasswordReset(context.Context, identity.Principal, string, string) (PasswordResetOperation, error)
	MarkPasswordWriteStarted(context.Context, uuid.UUID, uuid.UUID) (bool, error)
	MarkPasswordUpdated(context.Context, uuid.UUID) error
	MarkPasswordSessionsRevoked(context.Context, uuid.UUID) error
	CompletePasswordReset(context.Context, uuid.UUID) error
	FailPasswordReset(context.Context, uuid.UUID, string) error
	SchedulePasswordResetRetry(context.Context, uuid.UUID, string) error
	RequirePasswordResetRepair(context.Context, uuid.UUID, string) error
	ClaimPasswordReset(context.Context) (*PasswordResetOperation, error)
	ListProfileRepairs(context.Context) ([]profileChangeRepairItem, error)
	RetryProfileRepair(context.Context, identity.Principal, uuid.UUID) (profileRepairRetryResult, error)
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
	if change.IdentityIssuer != principal.Issuer || change.IdentitySubject != principal.Subject {
		return SMSChallenge{}, ErrNotFound
	}
	if change.Field != "MOBILE" || change.Status != "PENDING" {
		return SMSChallenge{}, ErrProfileChangeConflict
	}
	provider, err := activeSMSProvider(ctx, s.auth.sms)
	if err != nil {
		return SMSChallenge{}, ErrSMSUnavailable
	}
	candidate := SMSChallenge{
		ID: uuid.Must(uuid.NewV7()), Mobile: change.NewValue, Purpose: SMSPurposeProfileMobile,
		ExpiresAt: s.now().Add(5 * time.Minute), Source: strings.TrimSpace(source), Device: strings.TrimSpace(device),
		Provider: provider.Descriptor(),
	}
	verificationData, err := provider.PrepareVerification(candidate.ID)
	if err != nil {
		return SMSChallenge{}, ErrSMSUnavailable
	}
	candidate.VerificationData = verificationData
	challenge, err := s.store.ReserveProfileSMSChallenge(ctx, requestID, change.UserID, captchaID, offset, candidate)
	if err != nil {
		return SMSChallenge{}, err
	}
	provider, err = smsProviderForSend(ctx, s.auth.sms, challenge.Provider)
	if err != nil {
		return SMSChallenge{}, ErrSMSUnavailable
	}
	if challenge.Status == "SENT" {
		return challenge, nil
	}
	policy := provider.DeliveryRetryPolicy()
	if challenge.Status == "UNCERTAIN" {
		return challenge, nil
	}
	if challenge.Status == "SENDING" && policy == SMSDeliveryAtMostOnce && challenge.DeliveryLeaseUntil != nil && !challenge.DeliveryLeaseUntil.After(s.now()) {
		if err := s.store.MarkProfileSMSDeliveryUncertain(context.WithoutCancel(ctx), requestID, challenge.ID, uuid.Nil); err != nil {
			return SMSChallenge{}, ErrSMSUnavailable
		}
		challenge.Status = "UNCERTAIN"
		return challenge, nil
	}
	leaseID, claimed, err := s.store.ClaimProfileSMSDelivery(ctx, requestID, change.UserID, challenge.ID, policy == SMSDeliveryIdempotent)
	if err != nil {
		return SMSChallenge{}, err
	}
	if !claimed {
		challenge.Status = "SENDING"
		return challenge, nil
	}
	delivery, err := provider.SendCode(ctx, challenge.Mobile, challenge.Purpose, challenge.ID)
	if err != nil {
		var rejected *SMSDeliveryRejectedError
		if errors.As(err, &rejected) {
			if markErr := s.store.MarkProfileSMSDeliveryFailed(context.WithoutCancel(ctx), requestID, challenge.ID, leaseID); markErr != nil {
				return SMSChallenge{}, ErrSMSUnavailable
			}
		} else {
			if policy == SMSDeliveryIdempotent {
				_ = s.store.ReleaseProfileSMSDeliveryLease(context.WithoutCancel(ctx), requestID, challenge.ID, leaseID)
			} else {
				if uncertainErr := s.store.MarkProfileSMSDeliveryUncertain(context.WithoutCancel(ctx), requestID, challenge.ID, leaseID); uncertainErr == nil {
					challenge.Status = "UNCERTAIN"
					return challenge, nil
				}
			}
		}
		return SMSChallenge{}, fmt.Errorf("%w: %v", ErrSMSUnavailable, err)
	}
	if err := s.store.MarkProfileSMSDeliverySent(context.WithoutCancel(ctx), requestID, challenge.ID, leaseID, delivery); err != nil {
		if policy == SMSDeliveryIdempotent {
			_ = s.store.ReleaseProfileSMSDeliveryLease(context.WithoutCancel(ctx), requestID, challenge.ID, leaseID)
		} else {
			if uncertainErr := s.store.MarkProfileSMSDeliveryUncertain(context.WithoutCancel(ctx), requestID, challenge.ID, leaseID); uncertainErr == nil {
				challenge.Status = "UNCERTAIN"
				return challenge, nil
			}
		}
		return SMSChallenge{}, ErrSMSUnavailable
	}
	challenge.Status = "SENT"
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
		verified, err := s.auth.VerifyProfileMobile(ctx, change.NewValue, *change.SMSChallengeID, code)
		if err != nil {
			return ProfileChange{}, err
		}
		if err := s.store.CommitProfileSMSVerification(ctx, change.ID, change.UserID, *change.SMSChallengeID, verified); err != nil {
			return ProfileChange{}, err
		}
		change.SMSVerifiedAt = ptrTime(s.now())
		change.Status = "VERIFIED"
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
			err = s.identities.UpdateUsername(ctx, change.IdentitySubject, change.ID, change.FenceVersion, change.NewValue)
		} else {
			err = s.identities.UpdateMobile(ctx, change.IdentitySubject, change.ID, change.FenceVersion, change.NewValue)
		}
		if err != nil {
			recordProfileRecoveryFailure(ctx, "PROFILE_RETRY_SCHEDULE_FAILED", s.store.ScheduleProfileChangeRetry(context.WithoutCancel(ctx), change.ID, "IDENTITY_UPDATE_UNAVAILABLE"))
			return change, err
		}
		if err := s.store.MarkProfileIdentityUpdated(ctx, change.ID); err != nil {
			return change, err
		}
		change.IdentityUpdatedAt = ptrTime(s.now())
	}
	if change.AccountCommittedAt == nil {
		if err := s.store.CommitProfileChange(ctx, change.ID); err != nil {
			recordProfileRecoveryFailure(ctx, "PROFILE_RETRY_SCHEDULE_FAILED", s.store.ScheduleProfileChangeRetry(context.WithoutCancel(ctx), change.ID, "ACCOUNT_COMMIT_FAILED"))
			return change, err
		}
		change.AccountCommittedAt = ptrTime(s.now())
	}
	if change.SessionsRevokedAt == nil {
		if err := s.identities.RevokeSessions(ctx, change.IdentitySubject); err != nil {
			recordProfileRecoveryFailure(ctx, "PROFILE_RETRY_SCHEDULE_FAILED", s.store.ScheduleProfileChangeRetry(context.WithoutCancel(ctx), change.ID, "SESSION_REVOCATION_FAILED"))
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
	if existing, loadErr := s.store.PasswordResetByChallenge(ctx, challengeID); loadErr == nil {
		if existing.Mobile != normalized {
			return ErrSMSInvalid
		}
		return s.resumePasswordReset(ctx, existing)
	} else if !errors.Is(loadErr, ErrNotFound) {
		return loadErr
	}
	verified, err := s.auth.VerifyPasswordReset(ctx, normalized, challengeID, code)
	if err != nil {
		return err
	}
	operation, err := s.store.CreatePasswordReset(ctx, s.auth.issuer, challengeID, normalized, verified, handle, policyVersion)
	if err != nil {
		return err
	}
	return s.resumePasswordReset(ctx, operation)
}

func (s *ProfileChangeService) ChangePassword(ctx context.Context, principal identity.Principal, handle, policyVersion string) error {
	if err := s.requireRecent(principal); err != nil {
		return err
	}
	operation, err := s.store.CreateAuthenticatedPasswordReset(ctx, principal, handle, policyVersion)
	if err != nil {
		return err
	}
	return s.resumePasswordReset(ctx, operation)
}

func (s *ProfileChangeService) resumePasswordReset(ctx context.Context, operation PasswordResetOperation) error {
	switch operation.Status {
	case "COMPLETED":
		return nil
	case "FAILED":
		return passwordResetStoredError(operation.LastErrorCode)
	case "REPAIR_REQUIRED":
		return ErrConflict
	}
	if operation.PasswordWriteStartedAt != nil && operation.PasswordUpdatedAt == nil && operation.LastErrorCode == "LEGACY_SESSION_REVOCATION_UNCERTAIN" {
		return s.recoverUncertainPasswordReset(ctx, operation, operation.LastErrorCode)
	}
	if operation.PasswordUpdatedAt == nil {
		if operation.PasswordWriteStartedAt == nil {
			owner := uuid.Must(uuid.NewV7())
			acquired, err := s.store.MarkPasswordWriteStarted(ctx, operation.ID, owner)
			if err != nil {
				return err
			}
			if !acquired {
				return ErrConflict
			}
			operation.PasswordWriteStartedAt = ptrTime(s.now())
		}
		if err := s.identities.ResetPassword(ctx, operation.IdentitySubject, operation.ID, operation.FenceVersion, operation.Mobile, operation.PasswordHandle, operation.PasswordPolicyVersion); err != nil {
			if errors.Is(err, ErrPasswordReferenceInvalid) || errors.Is(err, ErrPasswordReferenceConflict) || errors.Is(err, ErrPasswordInvalid) || errors.Is(err, ErrPasswordEdgeSpace) {
				recordProfileRecoveryFailure(ctx, "PASSWORD_RESET_FAILURE_CHECKPOINT_FAILED", s.store.FailPasswordReset(context.WithoutCancel(ctx), operation.ID, passwordResetErrorCode(err)))
			} else {
				if revokeErr := s.revokePasswordSessionsAfterUncertainWrite(ctx, operation); revokeErr != nil {
					recordProfileRecoveryFailure(ctx, "PROFILE_RETRY_SCHEDULE_FAILED", s.store.SchedulePasswordResetRetry(context.WithoutCancel(ctx), operation.ID, "IDENTITY_RESET_UNCERTAIN_SESSION_REVOCATION_FAILED"))
					return errors.Join(err, revokeErr)
				}
				recordProfileRecoveryFailure(ctx, "PROFILE_RETRY_SCHEDULE_FAILED", s.store.SchedulePasswordResetRetry(context.WithoutCancel(ctx), operation.ID, "IDENTITY_RESET_UNCERTAIN"))
			}
			return err
		}
		if err := s.store.MarkPasswordUpdated(ctx, operation.ID); err != nil {
			if revokeErr := s.revokePasswordSessionsAfterUncertainWrite(ctx, operation); revokeErr != nil {
				recordProfileRecoveryFailure(ctx, "PROFILE_RETRY_SCHEDULE_FAILED", s.store.SchedulePasswordResetRetry(context.WithoutCancel(ctx), operation.ID, "PASSWORD_UPDATE_CHECKPOINT_AND_SESSION_REVOCATION_FAILED"))
				return errors.Join(err, revokeErr)
			}
			recordProfileRecoveryFailure(ctx, "PROFILE_RETRY_SCHEDULE_FAILED", s.store.SchedulePasswordResetRetry(context.WithoutCancel(ctx), operation.ID, "PASSWORD_UPDATE_CHECKPOINT_FAILED"))
			return err
		}
		operation.PasswordUpdatedAt = ptrTime(s.now())
	}
	if operation.SessionsRevokedAt == nil {
		if err := s.identities.RevokeSessions(ctx, operation.IdentitySubject); err != nil {
			recordProfileRecoveryFailure(ctx, "PROFILE_RETRY_SCHEDULE_FAILED", s.store.SchedulePasswordResetRetry(context.WithoutCancel(ctx), operation.ID, "SESSION_REVOCATION_FAILED"))
			return err
		}
		if err := s.store.MarkPasswordSessionsRevoked(ctx, operation.ID); err != nil {
			recordProfileRecoveryFailure(ctx, "PROFILE_RETRY_SCHEDULE_FAILED", s.store.SchedulePasswordResetRetry(context.WithoutCancel(ctx), operation.ID, "SESSION_REVOCATION_CHECKPOINT_FAILED"))
			return err
		}
	}
	return s.store.CompletePasswordReset(ctx, operation.ID)
}

func (s *ProfileChangeService) revokePasswordSessionsAfterUncertainWrite(ctx context.Context, operation PasswordResetOperation) error {
	if operation.SessionsRevokedAt != nil {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if err := s.identities.RevokeSessions(cleanupCtx, operation.IdentitySubject); err != nil {
		return err
	}
	return s.store.MarkPasswordSessionsRevoked(cleanupCtx, operation.ID)
}

func (s *ProfileChangeService) recoverUncertainPasswordReset(ctx context.Context, operation PasswordResetOperation, code string) error {
	if strings.TrimSpace(code) == "" {
		code = "PASSWORD_WRITE_UNCERTAIN"
	}
	if err := s.revokePasswordSessionsAfterUncertainWrite(ctx, operation); err != nil {
		recordProfileRecoveryFailure(ctx, "PROFILE_RETRY_SCHEDULE_FAILED", s.store.SchedulePasswordResetRetry(context.WithoutCancel(ctx), operation.ID, code+"_SESSION_REVOCATION_FAILED"))
		return err
	}
	if err := s.store.RequirePasswordResetRepair(context.WithoutCancel(ctx), operation.ID, code); err != nil {
		return err
	}
	return passwordResetStoredError(code)
}

func passwordResetStoredError(code string) error {
	switch code {
	case "PASSWORD_REFERENCE_INVALID", "MIGRATION_PASSWORD_REFERENCE_UNAVAILABLE":
		return ErrPasswordReferenceInvalid
	case "PASSWORD_REFERENCE_CONFLICT":
		return ErrPasswordReferenceConflict
	case "PASSWORD_EDGE_SPACE_NOT_ALLOWED":
		return ErrPasswordEdgeSpace
	case "PASSWORD_INVALID":
		return ErrPasswordInvalid
	default:
		return ErrConflict
	}
}

func passwordResetErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrPasswordReferenceInvalid):
		return "PASSWORD_REFERENCE_INVALID"
	case errors.Is(err, ErrPasswordReferenceConflict):
		return "PASSWORD_REFERENCE_CONFLICT"
	case errors.Is(err, ErrPasswordEdgeSpace):
		return "PASSWORD_EDGE_SPACE_NOT_ALLOWED"
	case errors.Is(err, ErrPasswordInvalid):
		return "PASSWORD_INVALID"
	default:
		return "IDENTITY_RESET_UNCERTAIN"
	}
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

// RunOnce exposes background storage and resume failures to supervision without
// skipping the independent password-reset recovery when profile recovery fails.
func (w *ProfileChangeWorker) RunOnce(ctx context.Context) error {
	var failures []error
	change, err := w.service.store.ClaimProfileChange(ctx)
	if err != nil {
		failures = append(failures, err)
	} else if change != nil {
		if _, err := w.service.resume(ctx, *change); err != nil {
			failures = append(failures, err)
		}
	}
	reset, err := w.service.store.ClaimPasswordReset(ctx)
	if err != nil {
		failures = append(failures, err)
	} else if reset != nil {
		if err := w.service.resumePasswordReset(ctx, *reset); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func recordProfileRecoveryFailure(ctx context.Context, code string, err error) {
	if err != nil && ctx.Err() == nil {
		slog.ErrorContext(ctx, "profile recovery operation failed", "error_code", code)
	}
}

func (w *ProfileChangeWorker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		recordProfileRecoveryFailure(ctx, "PROFILE_RECOVERY_FAILED", w.RunOnce(ctx))
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func ptrTime(value time.Time) *time.Time { return &value }

func (r *PostgresRepository) CreateProfileChange(ctx context.Context, principal identity.Principal, field, value string) (ProfileChange, error) {
	// The user row is the cross-table identity-security mutex. READ COMMITTED
	// lets a waiter observe the winner after acquiring that row lock instead of
	// surfacing a serialization failure to the user.
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return ProfileChange{}, err
	}
	defer tx.Rollback(ctx)
	var change ProfileChange
	var username, mobile *string
	var securityVersion int64
	err = tx.QueryRow(ctx, `SELECT u.id,i.issuer,i.subject,u.username,u.mobile_e164,u.profile_version,u.identity_security_version FROM users u JOIN identities i ON i.user_id=u.id WHERE i.issuer=$1 AND i.subject=$2 AND u.status IN ('ACTIVE','PROVISIONING','REPAIR_REQUIRED') FOR UPDATE OF u`, principal.Issuer, principal.Subject).Scan(&change.UserID, &change.IdentityIssuer, &change.IdentitySubject, &username, &mobile, &change.ExpectedProfileVersion, &securityVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProfileChange{}, ErrNotFound
	}
	if err != nil {
		return ProfileChange{}, err
	}
	var passwordActive bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM password_reset_operations WHERE user_id=$1 AND status IN ('VERIFIED','PASSWORD_STARTED','PASSWORD_UPDATED','RETRY','REPAIR_REQUIRED'))`, change.UserID).Scan(&passwordActive); err != nil {
		return ProfileChange{}, err
	}
	if passwordActive {
		return ProfileChange{}, ErrProfileChangeConflict
	}
	active, activeErr := scanProfileChange(tx.QueryRow(ctx, profileChangeSelect+` WHERE user_id=$1 AND status IN ('PENDING','VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY','REPAIR_REQUIRED') ORDER BY created_at DESC LIMIT 1 FOR UPDATE`, change.UserID))
	if activeErr != nil && !errors.Is(activeErr, pgx.ErrNoRows) {
		// Preserve the actual database failure; another query on an aborted
		// transaction would replace it with the uninformative SQLSTATE 25P02.
		return ProfileChange{}, activeErr
	}
	var databaseNow time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&databaseNow); err != nil {
		return ProfileChange{}, err
	}
	if activeErr == nil {
		if active.Status != "PENDING" || active.SMSVerifiedAt != nil || active.IdentityUpdatedAt != nil || active.AccountCommittedAt != nil || active.SessionsRevokedAt != nil {
			return ProfileChange{}, ErrProfileChangeConflict
		}
		expired := !active.SLAExpiresAt.After(databaseNow)
		if !expired && active.Field != field {
			return ProfileChange{}, ErrProfileChangeConflict
		}
		if !expired && active.NewValue == value {
			if err := tx.Commit(ctx); err != nil {
				return ProfileChange{}, err
			}
			return active, nil
		}
		if !expired && active.SMSChallengeID != nil {
			return ProfileChange{}, ErrProfileChangeConflict
		}
		reason := "REPLACED_BEFORE_VERIFICATION"
		if expired {
			reason = "EXPIRED_BEFORE_VERIFICATION"
		}
		command, updateErr := tx.Exec(ctx, `UPDATE profile_change_requests SET status='SUPERSEDED',version=version+1,last_error_code=$3,updated_at=$2 WHERE id=$1 AND status='PENDING' AND (sms_challenge_id IS NULL OR sla_expires_at<=$2) AND sms_verified_at IS NULL AND identity_updated_at IS NULL AND account_committed_at IS NULL AND sessions_revoked_at IS NULL`, active.ID, databaseNow, reason)
		if updateErr != nil {
			return ProfileChange{}, updateErr
		}
		if command.RowsAffected() != 1 {
			return ProfileChange{}, ErrProfileChangeConflict
		}
		if err := appendAudit(ctx, tx, change.UserID, principal.SessionID, nil, "account.profile_change.superseded", "profile_change", active.ID.String(), "SUCCEEDED", map[string]any{"field": active.Field, "reason": reason}); err != nil {
			return ProfileChange{}, err
		}
	}
	change.ID = uuid.Must(uuid.NewV7())
	change.FenceVersion = securityVersion + 1
	change.Field = field
	change.NewValue = value
	change.Status = "PENDING"
	if field == "USERNAME" {
		change.Status = "VERIFIED"
	}
	change.NextAttemptAt = databaseNow.Add(30 * time.Second)
	change.SLAExpiresAt = databaseNow.Add(30 * time.Minute)
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
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE lower(username)=lower($1) AND status<>'DELETED')`, value).Scan(&exists); err != nil {
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
	command, err := tx.Exec(ctx, `UPDATE users SET identity_security_version=$2,version=version+1,updated_at=$3 WHERE id=$1 AND identity_security_version=$4`, change.UserID, change.FenceVersion, databaseNow, securityVersion)
	if err != nil {
		return ProfileChange{}, err
	}
	if command.RowsAffected() != 1 {
		return ProfileChange{}, ErrProfileChangeConflict
	}
	_, err = tx.Exec(ctx, `INSERT INTO profile_change_requests(id,user_id,identity_issuer,identity_subject,field,old_value,new_value,expected_profile_version,fence_version,status,next_attempt_at,sla_expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$13)`, change.ID, change.UserID, change.IdentityIssuer, change.IdentitySubject, field, change.OldValue, value, change.ExpectedProfileVersion, change.FenceVersion, change.Status, change.NextAttemptAt, change.SLAExpiresAt, databaseNow)
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
	err := row.Scan(&c.ID, &c.UserID, &c.IdentityIssuer, &c.IdentitySubject, &c.Field, &c.OldValue, &c.NewValue, &c.ExpectedProfileVersion, &c.FenceVersion, &c.SMSChallengeID, &c.SMSVerifiedAt, &c.IdentityUpdatedAt, &c.AccountCommittedAt, &c.SessionsRevokedAt, &c.Status, &c.NextAttemptAt, &c.SLAExpiresAt, &c.Attempts, &c.LastErrorCode)
	return c, err
}

const profileChangeSelect = `SELECT id,user_id,identity_issuer,identity_subject,field,old_value,new_value,expected_profile_version,fence_version,sms_challenge_id,sms_verified_at,identity_updated_at,account_committed_at,sessions_revoked_at,status,next_attempt_at,sla_expires_at,attempts,COALESCE(last_error_code,'') FROM profile_change_requests`

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

const profileSMSDeliveryLease = 30 * time.Second

// ReserveProfileSMSChallenge persists the request-to-challenge binding before
// any provider side effect. The profile row is the database-wide single-flight
// mutex: an active CREATED/SENT challenge is always observed by every caller,
// while only FAILED/EXPIRED delivery may be replaced by a fresh captcha-bound
// challenge.
func (r *PostgresRepository) ReserveProfileSMSChallenge(ctx context.Context, id, userID, captchaID uuid.UUID, offset int, candidate SMSChallenge) (SMSChallenge, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return SMSChallenge{}, err
	}
	defer tx.Rollback(ctx)
	var boundID *uuid.UUID
	var mobile string
	err = tx.QueryRow(ctx, `SELECT sms_challenge_id,new_value FROM profile_change_requests WHERE id=$1 AND user_id=$2 AND field='MOBILE' AND status='PENDING' FOR UPDATE`, id, userID).Scan(&boundID, &mobile)
	if errors.Is(err, pgx.ErrNoRows) {
		return SMSChallenge{}, ErrProfileChangeConflict
	}
	if err != nil {
		return SMSChallenge{}, err
	}
	if candidate.Mobile != mobile || candidate.Purpose != SMSPurposeProfileMobile || candidate.Provider.ID == "" || candidate.Provider.AdapterVersion == "" {
		return SMSChallenge{}, ErrProfileChangeConflict
	}
	now := r.now()
	if boundID != nil {
		var existing SMSChallenge
		err = tx.QueryRow(ctx, `SELECT id,mobile_e164,purpose,status,expires_at,provider_id,adapter_version,delivery_lease_until,delivery_started_at,delivery_uncertain_at FROM sms_challenges WHERE id=$1 FOR UPDATE`, *boundID).Scan(&existing.ID, &existing.Mobile, &existing.Purpose, &existing.Status, &existing.ExpiresAt, &existing.Provider.ID, &existing.Provider.AdapterVersion, &existing.DeliveryLeaseUntil, &existing.DeliveryStartedAt, &existing.DeliveryUncertainAt)
		if err != nil {
			return SMSChallenge{}, err
		}
		if (existing.Status == "CREATED" || existing.Status == "SENT") && existing.ExpiresAt.After(now) {
			if existing.Status == "CREATED" && existing.DeliveryUncertainAt != nil {
				existing.Status = "UNCERTAIN"
			} else if existing.Status == "CREATED" && existing.DeliveryStartedAt != nil {
				existing.Status = "SENDING"
			}
			if err := tx.Commit(ctx); err != nil {
				return SMSChallenge{}, err
			}
			return existing, nil
		}
		if (existing.Status == "CREATED" || existing.Status == "SENT") && !existing.ExpiresAt.After(now) {
			if _, err := tx.Exec(ctx, `UPDATE sms_challenges SET status='EXPIRED',delivery_lease_id=NULL,delivery_lease_until=NULL,updated_at=$2 WHERE id=$1 AND status IN ('CREATED','SENT')`, existing.ID, now); err != nil {
				return SMSChallenge{}, err
			}
			existing.Status = "EXPIRED"
		}
		if existing.Status != "FAILED" && existing.Status != "EXPIRED" {
			return SMSChallenge{}, ErrProfileChangeConflict
		}
	}
	if err := r.consumeSliderCaptchaTx(ctx, tx, captchaID, mobile, SMSPurposeProfileMobile, offset); err != nil {
		if errors.Is(err, ErrCaptchaInvalid) {
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return SMSChallenge{}, commitErr
			}
		}
		return SMSChallenge{}, err
	}
	if candidate.Source == "" {
		candidate.Source = "source:unknown"
	}
	if candidate.Device == "" {
		candidate.Device = "device:absent:" + candidate.Source
	}
	locks := []string{"mobile:" + candidate.Mobile, "source:" + candidate.Source, "device:" + candidate.Device}
	sort.Strings(locks)
	for _, lock := range locks {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lock); err != nil {
			return SMSChallenge{}, err
		}
	}
	if err := enforceSMSRateLimits(ctx, tx, candidate, now); err != nil {
		if errors.Is(err, ErrSMSRateLimited) {
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return SMSChallenge{}, commitErr
			}
		}
		return SMSChallenge{}, err
	}
	candidate.Status = "CREATED"
	_, err = tx.Exec(ctx, `INSERT INTO sms_challenges(id,mobile_e164,purpose,code_hash,status,expires_at,created_at,updated_at,provider_id,adapter_version) VALUES($1,$2,$3,$4,'CREATED',$5,$6,$6,$7,$8)`, candidate.ID, candidate.Mobile, candidate.Purpose, candidate.VerificationData, candidate.ExpiresAt, now, candidate.Provider.ID, candidate.Provider.AdapterVersion)
	if err != nil {
		return SMSChallenge{}, err
	}
	command, err := tx.Exec(ctx, `UPDATE profile_change_requests SET sms_challenge_id=$3,version=version+1,updated_at=$4 WHERE id=$1 AND user_id=$2 AND field='MOBILE' AND status='PENDING'`, id, userID, candidate.ID, now)
	if err != nil {
		return SMSChallenge{}, err
	}
	if command.RowsAffected() != 1 {
		return SMSChallenge{}, ErrProfileChangeConflict
	}
	mobileHash, sourceHash, deviceHash := sha256.Sum256([]byte(candidate.Mobile)), sha256.Sum256([]byte(candidate.Source)), sha256.Sum256([]byte(candidate.Device))
	if err := recordSMSAttempt(ctx, tx, candidate, mobileHash, sourceHash, deviceHash, "ACCEPTED", now); err != nil {
		return SMSChallenge{}, err
	}
	return candidate, tx.Commit(ctx)
}

func (r *PostgresRepository) ClaimProfileSMSDelivery(ctx context.Context, requestID, userID, challengeID uuid.UUID, allowUnknownRetry bool) (uuid.UUID, bool, error) {
	leaseID := uuid.Must(uuid.NewV7())
	now := r.now()
	command, err := r.pool.Exec(ctx, `UPDATE sms_challenges challenge SET delivery_lease_id=$4,delivery_lease_until=$5,delivery_started_at=COALESCE(delivery_started_at,$6),updated_at=$6 FROM profile_change_requests request WHERE request.id=$1 AND request.user_id=$2 AND request.status='PENDING' AND request.sms_challenge_id=challenge.id AND challenge.id=$3 AND challenge.status='CREATED' AND challenge.delivery_uncertain_at IS NULL AND challenge.expires_at>$6 AND (challenge.delivery_started_at IS NULL OR ($7 AND (challenge.delivery_lease_until IS NULL OR challenge.delivery_lease_until<=$6)))`, requestID, userID, challengeID, leaseID, now.Add(profileSMSDeliveryLease), now, allowUnknownRetry)
	if err != nil {
		return uuid.Nil, false, err
	}
	if command.RowsAffected() == 1 {
		return leaseID, true, nil
	}
	var status string
	err = r.pool.QueryRow(ctx, `SELECT challenge.status FROM profile_change_requests request JOIN sms_challenges challenge ON challenge.id=request.sms_challenge_id WHERE request.id=$1 AND request.user_id=$2 AND request.status='PENDING' AND challenge.id=$3`, requestID, userID, challengeID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, ErrProfileChangeConflict
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	if status == "CREATED" || status == "SENT" {
		return uuid.Nil, false, nil
	}
	return uuid.Nil, false, ErrSMSUnavailable
}

func (r *PostgresRepository) MarkProfileSMSDeliverySent(ctx context.Context, requestID, challengeID, leaseID uuid.UUID, delivery SMSDelivery) error {
	command, err := r.pool.Exec(ctx, `UPDATE sms_challenges challenge SET status='SENT',provider_message_id=NULLIF($4,''),code_hash=$5,sent_at=$6,delivery_lease_id=NULL,delivery_lease_until=NULL,updated_at=$6 FROM profile_change_requests request WHERE request.id=$1 AND request.status='PENDING' AND request.sms_challenge_id=challenge.id AND challenge.id=$2 AND challenge.status='CREATED' AND challenge.delivery_lease_id=$3`, requestID, challengeID, leaseID, delivery.MessageID, delivery.VerificationData, r.now())
	if err == nil && command.RowsAffected() != 1 {
		return ErrConflict
	}
	return err
}

func (r *PostgresRepository) MarkProfileSMSDeliveryFailed(ctx context.Context, requestID, challengeID, leaseID uuid.UUID) error {
	command, err := r.pool.Exec(ctx, `UPDATE sms_challenges challenge SET status='FAILED',delivery_lease_id=NULL,delivery_lease_until=NULL,updated_at=$4 FROM profile_change_requests request WHERE request.id=$1 AND request.status='PENDING' AND request.sms_challenge_id=challenge.id AND challenge.id=$2 AND challenge.status='CREATED' AND challenge.delivery_lease_id=$3`, requestID, challengeID, leaseID, r.now())
	if err == nil && command.RowsAffected() != 1 {
		return ErrConflict
	}
	return err
}

func (r *PostgresRepository) ReleaseProfileSMSDeliveryLease(ctx context.Context, requestID, challengeID, leaseID uuid.UUID) error {
	command, err := r.pool.Exec(ctx, `UPDATE sms_challenges challenge SET delivery_lease_id=NULL,delivery_lease_until=NULL,updated_at=$4 FROM profile_change_requests request WHERE request.id=$1 AND request.status='PENDING' AND request.sms_challenge_id=challenge.id AND challenge.id=$2 AND challenge.status='CREATED' AND challenge.delivery_lease_id=$3`, requestID, challengeID, leaseID, r.now())
	if err == nil && command.RowsAffected() != 1 {
		return ErrConflict
	}
	return err
}

func (r *PostgresRepository) MarkProfileSMSDeliveryUncertain(ctx context.Context, requestID, challengeID, leaseID uuid.UUID) error {
	now := r.now()
	query := `UPDATE sms_challenges challenge SET delivery_uncertain_at=COALESCE(delivery_uncertain_at,$4),delivery_lease_id=NULL,delivery_lease_until=NULL,updated_at=$4 FROM profile_change_requests request WHERE request.id=$1 AND request.status='PENDING' AND request.sms_challenge_id=challenge.id AND challenge.id=$2 AND challenge.status='CREATED' AND challenge.delivery_started_at IS NOT NULL AND challenge.delivery_lease_id=$3`
	args := []any{requestID, challengeID, leaseID, now}
	if leaseID == uuid.Nil {
		query = `UPDATE sms_challenges challenge SET delivery_uncertain_at=COALESCE(delivery_uncertain_at,$3),delivery_lease_id=NULL,delivery_lease_until=NULL,updated_at=$3 FROM profile_change_requests request WHERE request.id=$1 AND request.status='PENDING' AND request.sms_challenge_id=challenge.id AND challenge.id=$2 AND challenge.status='CREATED' AND challenge.delivery_started_at IS NOT NULL AND challenge.delivery_uncertain_at IS NULL AND challenge.delivery_lease_until<=$3`
		args = []any{requestID, challengeID, now}
	}
	command, err := r.pool.Exec(ctx, query, args...)
	if err == nil && command.RowsAffected() != 1 {
		return ErrConflict
	}
	return err
}

func (r *PostgresRepository) CommitProfileSMSVerification(ctx context.Context, id, userID, challengeID uuid.UUID, verified bool) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var profileStatus string
	var boundID *uuid.UUID
	var smsVerifiedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT status,sms_challenge_id,sms_verified_at FROM profile_change_requests WHERE id=$1 AND user_id=$2 AND field='MOBILE' FOR UPDATE`, id, userID).Scan(&profileStatus, &boundID, &smsVerifiedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrProfileChangeConflict
		}
		return err
	}
	if boundID == nil || *boundID != challengeID {
		return ErrProfileChangeConflict
	}
	if profileStatus == "VERIFIED" && smsVerifiedAt != nil {
		return tx.Commit(ctx)
	}
	if profileStatus != "PENDING" || smsVerifiedAt != nil {
		return ErrProfileChangeConflict
	}
	var challengeStatus string
	var attempts int
	var expiresAt time.Time
	var deliveryStartedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT status,attempts,expires_at,delivery_started_at FROM sms_challenges WHERE id=$1 AND mobile_e164=(SELECT new_value FROM profile_change_requests WHERE id=$2) AND purpose='PROFILE_MOBILE' FOR UPDATE`, challengeID, id).Scan(&challengeStatus, &attempts, &expiresAt, &deliveryStartedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrSMSInvalid
		}
		return err
	}
	now := r.now()
	ready := challengeStatus == "SENT" || challengeStatus == "CREATED" && deliveryStartedAt != nil
	if !ready || attempts >= 5 || !expiresAt.After(now) {
		return ErrSMSInvalid
	}
	if !verified {
		attempts++
		if _, err := tx.Exec(ctx, `UPDATE sms_challenges SET attempts=$2,status=CASE WHEN $2>=5 THEN 'FAILED' ELSE status END,delivery_lease_id=CASE WHEN $2>=5 THEN NULL ELSE delivery_lease_id END,delivery_lease_until=CASE WHEN $2>=5 THEN NULL ELSE delivery_lease_until END,updated_at=$3 WHERE id=$1`, challengeID, attempts, now); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return ErrSMSInvalid
	}
	if _, err := tx.Exec(ctx, `UPDATE sms_challenges SET status='CONSUMED',consumed_at=$2,delivery_lease_id=NULL,delivery_lease_until=NULL,updated_at=$2 WHERE id=$1`, challengeID, now); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE profile_change_requests SET sms_verified_at=$3,status='VERIFIED',version=version+1,updated_at=$3 WHERE id=$1 AND user_id=$2 AND status='PENDING' AND sms_challenge_id=$4`, id, userID, now, challengeID)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	return tx.Commit(ctx)
}
func (r *PostgresRepository) MarkProfileIdentityUpdated(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE profile_change_requests SET identity_updated_at=COALESCE(identity_updated_at,$2),status='IDENTITY_UPDATED',version=version+1,updated_at=$2 WHERE id=$1 AND status IN ('PENDING','VERIFIED','RETRY','IDENTITY_UPDATED')`, id, r.now())
	return err
}

func (r *PostgresRepository) CommitProfileChange(ctx context.Context, id uuid.UUID) error {
	// Discover the immutable subject before opening the transaction; closure
	// cleanup takes USER before profile, so this writer must use the same order.
	var userID uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT user_id FROM profile_change_requests WHERE id=$1`, id).Scan(&userID); err != nil {
		return err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var userStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&userStatus); err != nil {
		return err
	}
	c, err := scanProfileChange(tx.QueryRow(ctx, profileChangeSelect+` WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return err
	}
	if c.UserID != userID {
		return ErrConflict
	}
	if c.AccountCommittedAt != nil {
		return nil
	}
	// Preserve acknowledged identity facts for closure Prepare to reconcile.
	// A worker may retry this command after cleanup; committed receipts above
	// remain read-only, while cancelled or closed subjects cannot regain PII.
	if userStatus == "CLOSING" || userStatus == "DELETED" || userStatus == "ANONYMIZED" || c.Status == "CANCELLED_ACCOUNT_CLOSURE" {
		return ErrConflict
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
		if err := enqueueCommerceCustomerSync(ctx, tx, "USER", c.UserID, c.NewValue, r.commerceProviderID, r.commerceAdapterVersion, "PROFILE_SYNC", r.now()); err != nil {
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
	// Preserve acknowledged revocation facts using USER, profile, sessions order.
	var userID uuid.UUID
	if err := r.pool.QueryRow(ctx, `SELECT user_id FROM profile_change_requests WHERE id=$1`, id).Scan(&userID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var lockedUserID uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT id FROM users WHERE id=$1 FOR UPDATE`, userID).Scan(&lockedUserID); err != nil {
		return err
	}
	c, err := scanProfileChange(tx.QueryRow(ctx, profileChangeSelect+` WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return err
	}
	if c.UserID != lockedUserID {
		return ErrConflict
	}
	if c.SessionsRevokedAt != nil {
		return nil
	}
	if _, err = tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id=$1`, userID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE profile_change_requests SET sessions_revoked_at=COALESCE(sessions_revoked_at,$2),version=version+1,updated_at=$2 WHERE id=$1`, id, r.now()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) CompleteProfileChange(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE profile_change_requests SET status='COMPLETED',last_error_code=NULL,version=version+1,updated_at=$2 WHERE id=$1 AND account_committed_at IS NOT NULL AND sessions_revoked_at IS NOT NULL AND status NOT IN ('COMPLETED','CANCELLED_ACCOUNT_CLOSURE')`, id, r.now())
	return err
}
func (r *PostgresRepository) ScheduleProfileChangeRetry(ctx context.Context, id uuid.UUID, code string) error {
	now := r.now()
	_, err := r.pool.Exec(ctx, `UPDATE profile_change_requests SET status=CASE WHEN sla_expires_at<=$2 THEN 'REPAIR_REQUIRED' ELSE 'RETRY' END,last_error_code=$3,next_attempt_at=$2+make_interval(secs=>LEAST(300,power(2,LEAST(attempts,8))::int)),version=version+1,updated_at=$2 WHERE id=$1 AND status IN ('VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY','REPAIR_REQUIRED')`, id, now, code)
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
	_, err = tx.Exec(ctx, `UPDATE profile_change_requests SET attempts=attempts+1,next_attempt_at=$2::timestamptz+interval '30 seconds',updated_at=$2::timestamptz WHERE id=$1`, c.ID, r.now())
	if err != nil {
		return nil, err
	}
	c.Attempts++
	return &c, tx.Commit(ctx)
}

func (r *PostgresRepository) PasswordResetByChallenge(ctx context.Context, challengeID uuid.UUID) (PasswordResetOperation, error) {
	operation, err := scanPasswordReset(r.pool.QueryRow(ctx, passwordResetSelect+` WHERE sms_challenge_id=$1`, challengeID))
	if errors.Is(err, pgx.ErrNoRows) {
		return PasswordResetOperation{}, ErrNotFound
	}
	return operation, err
}

func (r *PostgresRepository) CreatePasswordReset(ctx context.Context, issuer string, challengeID uuid.UUID, mobile string, verified bool, handle, policyVersion string) (PasswordResetOperation, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return PasswordResetOperation{}, err
	}
	defer tx.Rollback(ctx)
	if existing, loadErr := scanPasswordReset(tx.QueryRow(ctx, passwordResetSelect+` WHERE sms_challenge_id=$1`, challengeID)); loadErr == nil {
		if existing.Mobile != mobile {
			return PasswordResetOperation{}, ErrSMSInvalid
		}
		return existing, tx.Commit(ctx)
	} else if !errors.Is(loadErr, pgx.ErrNoRows) {
		return PasswordResetOperation{}, loadErr
	}
	var challengeStatus string
	var challengeAttempts int
	var challengeExpiresAt time.Time
	var deliveryStartedAt *time.Time
	if err = tx.QueryRow(ctx, `SELECT status,attempts,expires_at,delivery_started_at FROM sms_challenges WHERE id=$1 AND mobile_e164=$2 AND purpose='PASSWORD_RESET' FOR UPDATE`, challengeID, mobile).Scan(&challengeStatus, &challengeAttempts, &challengeExpiresAt, &deliveryStartedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return PasswordResetOperation{}, ErrSMSInvalid
		}
		return PasswordResetOperation{}, err
	}
	now := r.now()
	ready := challengeStatus == "SENT" || challengeStatus == "CREATED" && deliveryStartedAt != nil
	if !ready || challengeAttempts >= 5 || !challengeExpiresAt.After(now) {
		return PasswordResetOperation{}, ErrSMSInvalid
	}
	if !verified {
		challengeAttempts++
		if _, err = tx.Exec(ctx, `UPDATE sms_challenges SET attempts=$2,status=CASE WHEN $2>=5 THEN 'FAILED' ELSE status END,updated_at=$3 WHERE id=$1`, challengeID, challengeAttempts, now); err != nil {
			return PasswordResetOperation{}, err
		}
		if err = tx.Commit(ctx); err != nil {
			return PasswordResetOperation{}, err
		}
		return PasswordResetOperation{}, ErrSMSInvalid
	}
	var userID uuid.UUID
	var subject string
	var currentFence int64
	err = tx.QueryRow(ctx, `
SELECT u.id,i.subject,u.identity_security_version
FROM identities i JOIN users u ON u.id=i.user_id
WHERE i.issuer=$1 AND u.mobile_e164=$2
  AND u.status IN ('ACTIVE','PROVISIONING','REPAIR_REQUIRED')
  AND EXISTS(
    SELECT 1 FROM role_assignments ra
    JOIN roles ro ON ro.code=ra.role_code AND ro.scope='PERSONAL'
    JOIN role_permissions rp ON rp.role_code=ra.role_code AND rp.permission_code=$3
    WHERE ra.user_id=u.id AND ra.organization_id IS NULL AND ra.unit_id IS NULL
      AND ra.revoked_at IS NULL AND ra.starts_at<=now() AND (ra.expires_at IS NULL OR ra.expires_at>now())
  )
FOR UPDATE OF u`, issuer, mobile, PermissionPersonalSecurityManage).Scan(&userID, &subject, &currentFence)
	if errors.Is(err, pgx.ErrNoRows) {
		return PasswordResetOperation{}, ErrNotFound
	}
	if err != nil {
		return PasswordResetOperation{}, err
	}
	// The user row is already locked. Retire only expired, unverified work;
	// proof-bearing or live profile changes still fence password operations.
	if err := supersedeExpiredPendingProfileChanges(ctx, tx, userID); err != nil {
		return PasswordResetOperation{}, err
	}
	var profileActive bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM profile_change_requests WHERE user_id=$1 AND status IN ('PENDING','VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY','REPAIR_REQUIRED'))`, userID).Scan(&profileActive); err != nil {
		return PasswordResetOperation{}, err
	}
	if profileActive {
		return PasswordResetOperation{}, ErrConflict
	}
	var active bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM password_reset_operations WHERE user_id=$1 AND status IN ('VERIFIED','PASSWORD_STARTED','PASSWORD_UPDATED','RETRY','REPAIR_REQUIRED'))`, userID).Scan(&active); err != nil {
		return PasswordResetOperation{}, err
	}
	if active {
		return PasswordResetOperation{}, ErrConflict
	}
	fenceVersion := currentFence + 1
	command, err := tx.Exec(ctx, `UPDATE users SET identity_security_version=$2,version=version+1,updated_at=$3 WHERE id=$1 AND identity_security_version=$4`, userID, fenceVersion, now, currentFence)
	if err != nil {
		return PasswordResetOperation{}, err
	}
	if command.RowsAffected() != 1 {
		return PasswordResetOperation{}, ErrConflict
	}
	if _, err = tx.Exec(ctx, `UPDATE sms_challenges SET status='CONSUMED',consumed_at=$2,updated_at=$2 WHERE id=$1`, challengeID, now); err != nil {
		return PasswordResetOperation{}, err
	}
	id := uuid.Must(uuid.NewV7())
	_, err = tx.Exec(ctx, `INSERT INTO password_reset_operations(id,user_id,identity_issuer,identity_subject,sms_challenge_id,method,status,mobile_e164,password_handle,password_policy_version,fence_version,next_attempt_at,sla_expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'SMS','VERIFIED',$6,$7,$8,$9,$10::timestamptz+interval '30 seconds',$10::timestamptz+interval '30 minutes',$10,$10)`, id, userID, issuer, subject, challengeID, mobile, handle, policyVersion, fenceVersion, now)
	if err != nil {
		return PasswordResetOperation{}, err
	}
	if err = appendAudit(ctx, tx, userID, "", nil, "account.password_reset.verified", "password_reset", id.String(), "SUCCEEDED", map[string]any{"method": "SMS"}); err != nil {
		return PasswordResetOperation{}, err
	}
	operation := PasswordResetOperation{ID: id, UserID: userID, IdentitySubject: subject, Mobile: mobile, PasswordHandle: handle, PasswordPolicyVersion: policyVersion, FenceVersion: fenceVersion, Status: "VERIFIED"}
	return operation, tx.Commit(ctx)
}

func (r *PostgresRepository) CreateAuthenticatedPasswordReset(ctx context.Context, principal identity.Principal, handle, policyVersion string) (PasswordResetOperation, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return PasswordResetOperation{}, err
	}
	defer tx.Rollback(ctx)
	var userID uuid.UUID
	var subject, mobile string
	var currentFence int64
	err = tx.QueryRow(ctx, `
SELECT u.id,i.subject,COALESCE(u.mobile_e164,''),u.identity_security_version
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
FOR UPDATE OF u`, principal.Issuer, principal.Subject, PermissionPersonalSecurityManage).Scan(&userID, &subject, &mobile, &currentFence)
	if errors.Is(err, pgx.ErrNoRows) {
		return PasswordResetOperation{}, ErrForbidden
	}
	if err != nil {
		return PasswordResetOperation{}, err
	}
	now := r.now()
	// The user row is already locked. Retire only expired, unverified work;
	// proof-bearing or live profile changes still fence password operations.
	if err := supersedeExpiredPendingProfileChanges(ctx, tx, userID); err != nil {
		return PasswordResetOperation{}, err
	}
	var profileActive bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM profile_change_requests WHERE user_id=$1 AND status IN ('PENDING','VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY','REPAIR_REQUIRED'))`, userID).Scan(&profileActive); err != nil {
		return PasswordResetOperation{}, err
	}
	if profileActive {
		return PasswordResetOperation{}, ErrConflict
	}
	var active bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM password_reset_operations WHERE user_id=$1 AND status IN ('VERIFIED','PASSWORD_STARTED','PASSWORD_UPDATED','RETRY','REPAIR_REQUIRED'))`, userID).Scan(&active); err != nil {
		return PasswordResetOperation{}, err
	}
	if active {
		return PasswordResetOperation{}, ErrConflict
	}
	fenceVersion := currentFence + 1
	command, err := tx.Exec(ctx, `UPDATE users SET identity_security_version=$2,version=version+1,updated_at=$3 WHERE id=$1 AND identity_security_version=$4`, userID, fenceVersion, now, currentFence)
	if err != nil {
		return PasswordResetOperation{}, err
	}
	if command.RowsAffected() != 1 {
		return PasswordResetOperation{}, ErrConflict
	}
	id := uuid.Must(uuid.NewV7())
	if _, err = tx.Exec(ctx, `INSERT INTO password_reset_operations(id,user_id,identity_issuer,identity_subject,method,status,mobile_e164,password_handle,password_policy_version,fence_version,next_attempt_at,sla_expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,'AUTHENTICATED','VERIFIED',$5,$6,$7,$8,$9::timestamptz+interval '30 seconds',$9::timestamptz+interval '30 minutes',$9,$9)`, id, userID, principal.Issuer, subject, mobile, handle, policyVersion, fenceVersion, now); err != nil {
		return PasswordResetOperation{}, err
	}
	if err = appendAudit(ctx, tx, userID, principal.SessionID, nil, "account.password_change.verified", "password_reset", id.String(), "SUCCEEDED", map[string]any{"method": "RECENT_AUTHENTICATION"}); err != nil {
		return PasswordResetOperation{}, err
	}
	operation := PasswordResetOperation{ID: id, UserID: userID, IdentitySubject: subject, Mobile: mobile, PasswordHandle: handle, PasswordPolicyVersion: policyVersion, FenceVersion: fenceVersion, Status: "VERIFIED"}
	return operation, tx.Commit(ctx)
}

func (r *PostgresRepository) MarkPasswordUpdated(ctx context.Context, id uuid.UUID) error {
	command, err := r.pool.Exec(ctx, `UPDATE password_reset_operations SET password_updated_at=COALESCE(password_updated_at,$2),password_handle='',password_policy_version='',status='PASSWORD_UPDATED',last_error_code=NULL,next_attempt_at=$2,updated_at=$2 WHERE id=$1 AND password_write_started_at IS NOT NULL AND status IN ('PASSWORD_STARTED','PASSWORD_UPDATED','RETRY')`, id, r.now())
	if err == nil && command.RowsAffected() != 1 {
		return r.passwordResetStageSatisfied(ctx, id, `password_updated_at IS NOT NULL`)
	}
	return err
}

func (r *PostgresRepository) MarkPasswordWriteStarted(ctx context.Context, id, owner uuid.UUID) (bool, error) {
	if owner == uuid.Nil {
		return false, ErrConflict
	}
	now := r.now()
	command, err := r.pool.Exec(ctx, `UPDATE password_reset_operations SET password_write_started_at=$3,password_write_owner=$2,status='PASSWORD_STARTED',next_attempt_at=$3+interval '30 seconds',updated_at=$3 WHERE id=$1 AND status='VERIFIED' AND password_write_started_at IS NULL`, id, owner, now)
	if err != nil {
		return false, err
	}
	return command.RowsAffected() == 1, nil
}

func (r *PostgresRepository) MarkPasswordSessionsRevoked(ctx context.Context, id uuid.UUID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `DELETE FROM user_sessions WHERE user_id=(SELECT user_id FROM password_reset_operations WHERE id=$1)`, id); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE password_reset_operations SET sessions_revoked_at=COALESCE(sessions_revoked_at,$2),next_attempt_at=$2,updated_at=$2 WHERE id=$1 AND password_write_started_at IS NOT NULL AND status IN ('PASSWORD_STARTED','PASSWORD_UPDATED','RETRY','REPAIR_REQUIRED')`, id, r.now())
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		var satisfied bool
		if err = tx.QueryRow(ctx, `SELECT sessions_revoked_at IS NOT NULL FROM password_reset_operations WHERE id=$1`, id).Scan(&satisfied); err != nil {
			return err
		}
		if !satisfied {
			return ErrConflict
		}
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) CompletePasswordReset(ctx context.Context, id uuid.UUID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now := r.now()
	command, err := tx.Exec(ctx, `UPDATE password_reset_operations SET status='COMPLETED',last_error_code=NULL,completed_at=$2,updated_at=$2 WHERE id=$1 AND password_updated_at IS NOT NULL AND sessions_revoked_at IS NOT NULL AND status IN ('PASSWORD_UPDATED','RETRY')`, id, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		var completed bool
		if err = tx.QueryRow(ctx, `SELECT status='COMPLETED' FROM password_reset_operations WHERE id=$1`, id).Scan(&completed); err != nil {
			return err
		}
		if !completed {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	var userID uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT user_id FROM password_reset_operations WHERE id=$1`, id).Scan(&userID); err != nil {
		return err
	}
	if err = appendAudit(ctx, tx, userID, "", nil, "account.password_reset.completed", "password_reset", id.String(), "SUCCEEDED", map[string]any{}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) FailPasswordReset(ctx context.Context, id uuid.UUID, code string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now := r.now()
	command, err := tx.Exec(ctx, `UPDATE password_reset_operations SET password_handle='',password_policy_version='',status='FAILED',last_error_code=$2,completed_at=$3,updated_at=$3 WHERE id=$1 AND password_updated_at IS NULL AND status IN ('VERIFIED','PASSWORD_STARTED','RETRY')`, id, code, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	var userID uuid.UUID
	if err = tx.QueryRow(ctx, `SELECT user_id FROM password_reset_operations WHERE id=$1`, id).Scan(&userID); err != nil {
		return err
	}
	if err = appendAudit(ctx, tx, userID, "", nil, "account.password_reset.completed", "password_reset", id.String(), "FAILED", map[string]any{"reason": code}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) SchedulePasswordResetRetry(ctx context.Context, id uuid.UUID, code string) error {
	now := r.now()
	_, err := r.pool.Exec(ctx, `UPDATE password_reset_operations SET attempts=attempts+1,status=CASE WHEN attempts+1>=10 OR sla_expires_at<=$2 THEN 'REPAIR_REQUIRED' ELSE 'RETRY' END,last_error_code=$3,next_attempt_at=$2+make_interval(secs=>LEAST(300,power(2,LEAST(attempts+1,8))::int)),updated_at=$2 WHERE id=$1 AND status IN ('VERIFIED','PASSWORD_STARTED','PASSWORD_UPDATED','RETRY')`, id, now, code)
	return err
}

func (r *PostgresRepository) RequirePasswordResetRepair(ctx context.Context, id uuid.UUID, code string) error {
	command, err := r.pool.Exec(ctx, `UPDATE password_reset_operations SET password_handle='',password_policy_version='',status='REPAIR_REQUIRED',last_error_code=$2,next_attempt_at=$3,updated_at=$3 WHERE id=$1 AND password_write_started_at IS NOT NULL AND password_updated_at IS NULL AND sessions_revoked_at IS NOT NULL AND status IN ('PASSWORD_STARTED','RETRY','REPAIR_REQUIRED')`, id, code, r.now())
	if err == nil && command.RowsAffected() != 1 {
		return r.passwordResetStageSatisfied(ctx, id, `status='REPAIR_REQUIRED' AND sessions_revoked_at IS NOT NULL`)
	}
	return err
}

func (r *PostgresRepository) passwordResetStageSatisfied(ctx context.Context, id uuid.UUID, predicate string) error {
	var satisfied bool
	if err := r.pool.QueryRow(ctx, `SELECT `+predicate+` FROM password_reset_operations WHERE id=$1`, id).Scan(&satisfied); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if !satisfied {
		return ErrConflict
	}
	return nil
}

const passwordResetSelect = `SELECT id,user_id,identity_subject,mobile_e164,password_handle,password_policy_version,fence_version,status,password_write_started_at,password_write_owner,password_updated_at,sessions_revoked_at,attempts,COALESCE(last_error_code,'') FROM password_reset_operations`

func scanPasswordReset(row pgx.Row) (PasswordResetOperation, error) {
	var operation PasswordResetOperation
	err := row.Scan(&operation.ID, &operation.UserID, &operation.IdentitySubject, &operation.Mobile, &operation.PasswordHandle, &operation.PasswordPolicyVersion, &operation.FenceVersion, &operation.Status, &operation.PasswordWriteStartedAt, &operation.PasswordWriteOwner, &operation.PasswordUpdatedAt, &operation.SessionsRevokedAt, &operation.Attempts, &operation.LastErrorCode)
	return operation, err
}

func (r *PostgresRepository) ClaimPasswordReset(ctx context.Context) (*PasswordResetOperation, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	operation, err := scanPasswordReset(tx.QueryRow(ctx, passwordResetSelect+` WHERE status IN ('VERIFIED','PASSWORD_STARTED','PASSWORD_UPDATED','RETRY') AND next_attempt_at<=$1 ORDER BY next_attempt_at FOR UPDATE SKIP LOCKED LIMIT 1`, r.now()))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, err
	}
	if _, err = tx.Exec(ctx, `UPDATE password_reset_operations SET next_attempt_at=$2+interval '30 seconds',updated_at=$2 WHERE id=$1`, operation.ID, r.now()); err != nil {
		return nil, err
	}
	return &operation, tx.Commit(ctx)
}
