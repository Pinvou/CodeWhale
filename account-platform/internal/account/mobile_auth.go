package account

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrMobileInvalid                 = errors.New("mobile number is invalid")
	ErrMobileAlreadyRegistered       = errors.New("mobile number is already registered")
	ErrUsernameInvalid               = errors.New("username is invalid")
	ErrUsernameAlreadyRegistered     = errors.New("username is already registered")
	ErrPasswordInvalid               = errors.New("password is invalid")
	ErrSMSRejected                   = errors.New("SMS provider did not accept the request")
	ErrPasswordReferenceInvalid      = errors.New("password reference is invalid or expired")
	ErrPasswordReferenceConflict     = errors.New("password reference conflicts with registration data")
	ErrCaptchaInvalid                = errors.New("slider captcha is invalid")
	ErrSMSRateLimited                = errors.New("SMS requests are rate limited")
	ErrSMSInvalid                    = errors.New("SMS challenge is invalid")
	ErrSMSUnavailable                = errors.New("SMS service is unavailable")
	ErrRegistrationConflict          = errors.New("registration operation conflicts with the request")
	ErrRegistrationQueryUnauthorized = errors.New("registration operation query is unauthorized")
	ErrRegistrationRecovering        = errors.New("registration is recovering asynchronously")
	ErrRegistrationRestart           = errors.New("registration must be restarted")
)

var mainlandMobile = regexp.MustCompile(`^1[3-9][0-9]{9}$`)
var registrationUsername = regexp.MustCompile(`^[a-z][a-z0-9._-]{2,31}$`)

type SMSChallenge struct {
	ID                              uuid.UUID             `json:"challenge_id"`
	Mobile                          string                `json:"-"`
	Purpose                         SMSPurpose            `json:"-"`
	ExpiresAt                       time.Time             `json:"expires_at"`
	Source                          string                `json:"-"`
	Device                          string                `json:"-"`
	Provider                        SMSProviderDescriptor `json:"-"`
	RegistrationOperationID         uuid.UUID             `json:"registration_operation_id,omitempty"`
	RegistrationQueryToken          string                `json:"registration_query_token,omitempty"`
	RegistrationQueryTokenExpiresAt time.Time             `json:"registration_query_token_expires_at,omitempty"`
}

type MobileRegistration struct {
	UserID   uuid.UUID
	Username string
	Mobile   string
}

type RegistrationOperation struct {
	RecoveryGeneration        int64      `json:"-"`
	RecoveryLeaseUntil        *time.Time `json:"-"`
	LegacyIntent              bool       `json:"-"`
	IssuerDigest              string     `json:"-"`
	PasswordPreserved         bool       `json:"-"`
	IdentityCreateAttemptedAt *time.Time `json:"-"`
	ID                        uuid.UUID  `json:"registration_operation_id"`
	CandidateUserID           uuid.UUID  `json:"-"`
	QueryTokenHash            []byte     `json:"-"`
	RequestFingerprint        []byte     `json:"-"`
	Username                  string     `json:"-"`
	Mobile                    string     `json:"-"`
	ChallengeID               uuid.UUID  `json:"-"`
	PasswordPolicyVersion     string     `json:"-"`
	IdentitySubject           string     `json:"-"`
	UserID                    *uuid.UUID `json:"user_id,omitempty"`
	UserCode                  string     `json:"user_code,omitempty"`
	Status                    string     `json:"status"`
	Version                   int64      `json:"-"`
	IdentityCreatedAt         *time.Time `json:"-"`
	NextRetryAt               time.Time  `json:"next_retry_at"`
	LastErrorCode             string     `json:"reason,omitempty"`
	CreatedAt                 time.Time  `json:"-"`
	UpdatedAt                 time.Time  `json:"updated_at"`
}

type MobileRegistrationResult struct {
	UserID            uuid.UUID `json:"user_id"`
	Username          string    `json:"username"`
	Status            string    `json:"status"`
	PasswordPreserved bool      `json:"password_preserved"`
}

type RegistrationResult struct {
	PasswordPreserved bool       `json:"password_preserved"`
	OperationID       uuid.UUID  `json:"registration_operation_id"`
	UserID            *uuid.UUID `json:"user_id,omitempty"`
	UserCode          string     `json:"user_code,omitempty"`
	Username          string     `json:"username,omitempty"`
	Status            string     `json:"status"`
	NextRetryAt       time.Time  `json:"next_retry_at,omitempty"`
}

type MobileAuthRepository interface {
	PendingMobileRegistration(context.Context, string, string, string) (MobileRegistration, error)
	MobileRegistrationRecoverable(context.Context, string, string) (bool, error)
	PrepareMobileRegistration(context.Context, string, string, string) (MobileRegistration, error)
	CompleteMobileRegistration(context.Context, string, MobileRegistration, string, bool) error
	MobileExists(context.Context, string) (bool, error)
	MobileIdentitySubject(context.Context, string, string) (string, error)
	UsernameExists(context.Context, string) (bool, error)
	CreateSliderCaptcha(context.Context, SliderCaptcha) error
	ConsumeSliderCaptcha(context.Context, uuid.UUID, string, SMSPurpose, int) error
	CreateSMSChallenge(context.Context, SMSChallenge) error
	CreateRegistrationOperation(context.Context, RegistrationOperation, SMSChallenge) error
	MarkSMSChallengeSent(context.Context, uuid.UUID, string, []byte) error
	MarkSMSChallengeFailed(context.Context, uuid.UUID) error
	SMSChallengeReady(context.Context, uuid.UUID, string, SMSPurpose) (SMSVerificationData, error)
	ConsumeSMSChallenge(context.Context, uuid.UUID, string, SMSPurpose, bool) error
	ConsumeRegistrationSMS(context.Context, uuid.UUID, uuid.UUID, string, bool) error
	RegistrationOperation(context.Context, uuid.UUID) (RegistrationOperation, error)
	MarkRegistrationIdentityCreating(context.Context, uuid.UUID) error
	MarkRegistrationCreateAttempted(context.Context, uuid.UUID) (bool, int64, error)
	MarkRegistrationIdentityCreated(context.Context, uuid.UUID, string, bool) error
	CommitRegistration(context.Context, uuid.UUID, string) (RegistrationOperation, error)
	ScheduleRegistrationRetry(context.Context, uuid.UUID, string) error
	CompensatePreIdentityRegistration(context.Context, uuid.UUID, string, int64) error
}

type SMSRateLimitError struct {
	RetryAfter time.Duration
}

func (e *SMSRateLimitError) Error() string { return ErrSMSRateLimited.Error() }
func (e *SMSRateLimitError) Unwrap() error { return ErrSMSRateLimited }

type MobileAuthService struct {
	repository         MobileAuthRepository
	sms                SMSProvider
	identities         IdentityProvisioner
	issuer             string
	now                func() time.Time
	loginResponseFloor time.Duration
	tokens             *RegistrationTokenSigner
	commerce           bool
}

func (s *MobileAuthService) WithCommerce(enabled bool) *MobileAuthService {
	s.commerce = enabled
	return s
}

func NewMobileAuthService(repository MobileAuthRepository, sms SMSProvider, identities IdentityProvisioner, issuer string, tokens *RegistrationTokenSigner) (*MobileAuthService, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if repository == nil || sms == nil || identities == nil || issuer == "" || tokens == nil {
		return nil, errors.New("mobile authentication is not fully configured")
	}
	return &MobileAuthService{repository: repository, sms: sms, identities: identities, issuer: issuer, tokens: tokens, now: func() time.Time { return time.Now().UTC() }, loginResponseFloor: 750 * time.Millisecond}, nil
}

func NormalizeMainlandMobile(value string) (string, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "+86")
	value = strings.TrimPrefix(value, "86")
	if !mainlandMobile.MatchString(value) {
		return "", ErrMobileInvalid
	}
	return "+86" + value, nil
}

func NormalizeRegistrationUsername(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if !registrationUsername.MatchString(value) {
		return "", ErrUsernameInvalid
	}
	return value, nil
}

func (s *MobileAuthService) PrecheckRegistration(ctx context.Context, rawUsername, rawMobile, passwordHandle, passwordPolicyVersion string) (string, error) {
	username, err := NormalizeRegistrationUsername(rawUsername)
	if err != nil {
		return "", err
	}
	mobile, err := NormalizeMainlandMobile(rawMobile)
	if err != nil {
		return "", err
	}
	if err := s.identities.ValidateMobileUser(ctx, uuid.Nil, username, mobile, passwordHandle, passwordPolicyVersion); err != nil {
		return "", err
	}
	if _, err := s.repository.PendingMobileRegistration(ctx, s.issuer, username, mobile); err == nil {
		return username, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	usernameExists, err := s.repository.UsernameExists(ctx, username)
	if err != nil {
		return "", err
	}
	if usernameExists {
		return "", ErrUsernameAlreadyRegistered
	}
	mobileExists, err := s.repository.MobileExists(ctx, mobile)
	if err != nil {
		return "", err
	}
	if mobileExists {
		return "", ErrMobileAlreadyRegistered
	}
	return username, nil
}

func (s *MobileAuthService) Send(ctx context.Context, rawUsername, rawMobile, passwordHandle, passwordPolicyVersion string, purpose SMSPurpose, captchaID uuid.UUID, captchaOffset int, source, device string) (SMSChallenge, error) {
	startedAt := time.Now()
	mobile, err := NormalizeMainlandMobile(rawMobile)
	if err != nil || purpose != SMSPurposeRegister && purpose != SMSPurposeLogin && purpose != SMSPurposeProfileMobile && purpose != SMSPurposePasswordReset {
		return SMSChallenge{}, ErrMobileInvalid
	}
	if captchaID == uuid.Nil || captchaOffset < 0 {
		return SMSChallenge{}, ErrCaptchaInvalid
	}
	if err := s.repository.ConsumeSliderCaptcha(ctx, captchaID, mobile, purpose, captchaOffset); err != nil {
		return SMSChallenge{}, err
	}
	username := ""
	if purpose == SMSPurposeRegister {
		if username, err = s.PrecheckRegistration(ctx, rawUsername, mobile, passwordHandle, passwordPolicyVersion); err != nil {
			return SMSChallenge{}, err
		}
	}
	exists, err := s.repository.MobileExists(ctx, mobile)
	if err != nil {
		return SMSChallenge{}, err
	}
	var legacyRegistration MobileRegistration
	if purpose == SMSPurposeRegister && exists {
		legacyRegistration, err = s.repository.PendingMobileRegistration(ctx, s.issuer, username, mobile)
		if err != nil {
			return SMSChallenge{}, ErrMobileAlreadyRegistered
		}
	}
	challenge := SMSChallenge{ID: uuid.Must(uuid.NewV7()), Mobile: mobile, Purpose: purpose, ExpiresAt: s.now().Add(5 * time.Minute), Source: strings.TrimSpace(source), Device: strings.TrimSpace(device), Provider: s.sms.Descriptor()}
	if purpose == SMSPurposeRegister {
		operationID := uuid.Must(uuid.NewV7())
		if legacyRegistration.UserID != uuid.Nil {
			operationID = legacyRegistration.UserID
		}
		if err := s.identities.ValidateMobileUser(ctx, operationID, username, mobile, passwordHandle, passwordPolicyVersion); err != nil {
			return SMSChallenge{}, err
		}
		token, tokenExpiresAt, tokenErr := s.tokens.Issue(operationID)
		if tokenErr != nil {
			return SMSChallenge{}, tokenErr
		}
		fingerprint := registrationFingerprint(username, mobile, challenge.ID, passwordPolicyVersion)
		tokenHash := registrationTokenHash(token)
		operation := RegistrationOperation{
			ID: operationID, CandidateUserID: uuid.Must(uuid.NewV7()), IssuerDigest: registrationIssuerDigest(s.issuer), LegacyIntent: legacyRegistration.UserID != uuid.Nil, QueryTokenHash: tokenHash[:], RequestFingerprint: fingerprint[:],
			Username: username, Mobile: mobile, ChallengeID: challenge.ID, PasswordPolicyVersion: passwordPolicyVersion,
			Status: "SMS_PENDING", NextRetryAt: challenge.ExpiresAt, CreatedAt: s.now(), UpdatedAt: s.now(),
		}
		if operation.LegacyIntent {
			operation.CandidateUserID = legacyRegistration.UserID
		}
		challenge.RegistrationOperationID = operationID
		challenge.RegistrationQueryToken = token
		challenge.RegistrationQueryTokenExpiresAt = tokenExpiresAt
		if err := s.repository.CreateRegistrationOperation(ctx, operation, challenge); err != nil {
			return SMSChallenge{}, err
		}
	} else if err := s.repository.CreateSMSChallenge(ctx, challenge); err != nil {
		return SMSChallenge{}, err
	}
	// Login delivery is deliberately detached from the external response. Known
	// and unknown mobiles therefore share the same status and minimum timing
	// bucket even when the provider is slow or unavailable.
	if purpose == SMSPurposeLogin || purpose == SMSPurposePasswordReset {
		if exists {
			go func() {
				deliveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
				defer cancel()
				_ = s.deliverSMS(deliveryCtx, challenge)
			}()
		}
		if err := waitForResponseFloor(ctx, startedAt, s.loginResponseFloor); err != nil {
			return SMSChallenge{}, err
		}
		return challenge, nil
	}
	if err := s.deliverSMS(ctx, challenge); err != nil {
		return SMSChallenge{}, err
	}
	return challenge, nil
}

func (s *MobileAuthService) VerifyPasswordReset(ctx context.Context, rawMobile string, challengeID uuid.UUID, code string) (string, error) {
	mobile, err := NormalizeMainlandMobile(rawMobile)
	if err != nil {
		return "", err
	}
	if err := s.verifyChallenge(ctx, challengeID, mobile, SMSPurposePasswordReset, code); err != nil {
		return "", err
	}
	return s.repository.MobileIdentitySubject(ctx, s.issuer, mobile)
}

func (s *MobileAuthService) VerifyProfileMobile(ctx context.Context, rawMobile string, challengeID uuid.UUID, code string) error {
	mobile, err := NormalizeMainlandMobile(rawMobile)
	if err != nil {
		return err
	}
	return s.verifyChallenge(ctx, challengeID, mobile, SMSPurposeProfileMobile, code)
}

func registrationFingerprint(username, mobile string, challengeID uuid.UUID, policyVersion string) [32]byte {
	return sha256.Sum256([]byte(strings.Join([]string{username, mobile, challengeID.String(), policyVersion, "registration-v1"}, "\x00")))
}

func (s *MobileAuthService) deliverSMS(ctx context.Context, challenge SMSChallenge) error {
	delivery, err := s.sms.SendCode(ctx, challenge.Mobile, challenge.Purpose, challenge.ID)
	if err != nil {
		if errors.Is(err, ErrSMSRejected) {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			_ = s.repository.MarkSMSChallengeFailed(cleanupCtx, challenge.ID)
		}
		return ErrSMSUnavailable
	}
	return s.repository.MarkSMSChallengeSent(ctx, challenge.ID, delivery.MessageID, delivery.VerificationData)
}

func waitForResponseFloor(ctx context.Context, startedAt time.Time, floor time.Duration) error {
	remaining := floor - time.Since(startedAt)
	if remaining <= 0 {
		return nil
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *MobileAuthService) Register(ctx context.Context, operationID uuid.UUID, queryToken, rawUsername, rawMobile string, challengeID uuid.UUID, code, passwordHandle, passwordPolicyVersion string) (RegistrationResult, error) {
	username, err := NormalizeRegistrationUsername(rawUsername)
	if err != nil {
		return RegistrationResult{}, err
	}
	mobile, err := NormalizeMainlandMobile(rawMobile)
	if err != nil {
		return RegistrationResult{}, err
	}
	operation, err := s.authorizeRegistrationQuery(ctx, operationID, queryToken)
	if err != nil {
		return RegistrationResult{}, err
	}
	fingerprint := registrationFingerprint(username, mobile, challengeID, passwordPolicyVersion)
	if operation.Username != username || operation.Mobile != mobile || operation.ChallengeID != challengeID || operation.PasswordPolicyVersion != passwordPolicyVersion || !hmac.Equal(operation.RequestFingerprint, fingerprint[:]) {
		return RegistrationResult{}, ErrRegistrationConflict
	}
	// A retry that already has an identity never applies its newly supplied
	// password. Persist that fact before returning a completed/recovered result.
	if operation.IdentitySubject != "" {
		if err := s.repository.MarkRegistrationIdentityCreated(ctx, operation.ID, operation.IdentitySubject, true); err != nil {
			return RegistrationResult{}, err
		}
		operation, err = s.repository.RegistrationOperation(ctx, operation.ID)
		if err != nil {
			return RegistrationResult{}, err
		}
	}
	if operation.Status == "COMPENSATED" || operation.Status == "EXPIRED" {
		return registrationResult(operation), ErrRegistrationRestart
	}
	if operation.Status == "PROVISIONING" || operation.Status == "ACCOUNT_COMMITTED" || operation.Status == "ACTIVE" {
		return registrationResult(operation), nil
	}
	if operation.Status == "REPAIR_REQUIRED" || operation.Status == "COMPENSATION_PENDING" {
		return registrationResult(operation), ErrRegistrationRecovering
	}
	if operation.Status == "SMS_PENDING" {
		if err := s.identities.ValidateMobileUser(ctx, operationID, username, mobile, passwordHandle, passwordPolicyVersion); err != nil {
			return RegistrationResult{}, err
		}
		verificationData, readyErr := s.repository.SMSChallengeReady(ctx, challengeID, mobile, SMSPurposeRegister)
		if readyErr != nil {
			return RegistrationResult{}, readyErr
		}
		verified, verifyErr := s.sms.VerifyCode(ctx, mobile, challengeID, verificationData, strings.TrimSpace(code))
		if verifyErr != nil {
			return RegistrationResult{}, ErrSMSUnavailable
		}
		if err := s.repository.ConsumeRegistrationSMS(ctx, operationID, challengeID, mobile, verified); err != nil {
			return RegistrationResult{}, err
		}
	}
	operation, err = s.repository.RegistrationOperation(ctx, operationID)
	if err != nil {
		return RegistrationResult{}, err
	}
	return s.resumeRegistration(ctx, operation)
}

func (s *MobileAuthService) RegistrationStatus(ctx context.Context, operationID uuid.UUID, queryToken string) (RegistrationResult, error) {
	operation, err := s.authorizeRegistrationQuery(ctx, operationID, queryToken)
	if err != nil {
		return RegistrationResult{}, err
	}
	result := registrationResult(operation)
	result.Username = ""
	return result, nil
}

func (s *MobileAuthService) authorizeRegistrationQuery(ctx context.Context, operationID uuid.UUID, token string) (RegistrationOperation, error) {
	if err := s.tokens.Validate(operationID, strings.TrimSpace(token)); err != nil {
		return RegistrationOperation{}, err
	}
	operation, err := s.repository.RegistrationOperation(ctx, operationID)
	if err != nil {
		return RegistrationOperation{}, ErrRegistrationQueryUnauthorized
	}
	hash := registrationTokenHash(strings.TrimSpace(token))
	if !hmac.Equal(operation.QueryTokenHash, hash[:]) || operation.IssuerDigest != registrationIssuerDigest(s.issuer) {
		return RegistrationOperation{}, ErrRegistrationQueryUnauthorized
	}
	return operation, nil
}

func (s *MobileAuthService) resumeRegistration(ctx context.Context, operation RegistrationOperation) (RegistrationResult, error) {
	if operation.IssuerDigest != registrationIssuerDigest(s.issuer) {
		return RegistrationResult{}, ErrRegistrationConflict
	}
	if operation.Status == "REGISTRATION_IN_PROGRESS" || operation.Status == "IDENTITY_CREATING" || (operation.Status == "REPAIR_REQUIRED" && operation.IdentitySubject == "") {
		if err := s.repository.MarkRegistrationIdentityCreating(ctx, operation.ID); err != nil {
			return RegistrationResult{}, err
		}
		var attemptVersion int64
		var firstAttempt bool
		remote, found, err := s.identities.FindMobileUserByRegistrationOperation(ctx, operation.ID, operation.Username, operation.Mobile, operation.LegacyIntent)
		if err == nil && !found {
			if firstAttempt, attemptVersion, err = s.repository.MarkRegistrationCreateAttempted(ctx, operation.ID); err != nil {
				return RegistrationResult{}, err
			}
			// From this point an unknown response may hide an accepted remote create.
			// Never compensate this operation merely because its password handle expires.
			attemptedAt := s.now()
			operation.IdentityCreateAttemptedAt = &attemptedAt
			remote, err = s.identities.CreateMobileUser(ctx, operation.ID, operation.Username, operation.Mobile, "", operation.PasswordPolicyVersion, operation.LegacyIntent, firstAttempt)
		}
		if err != nil {
			if errors.Is(err, ErrIdentityCreateNotDispatched) && firstAttempt && attemptVersion > 0 && !operation.LegacyIntent {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
				compensateErr := s.repository.CompensatePreIdentityRegistration(cleanupCtx, operation.ID, "IDENTITY_CREATE_NOT_DISPATCHED", attemptVersion)
				if compensateErr == nil {
					updated, readErr := s.repository.RegistrationOperation(cleanupCtx, operation.ID)
					cancel()
					if readErr != nil {
						return RegistrationResult{}, readErr
					}
					return registrationResult(updated), ErrRegistrationRestart
				}
				cancel()
			}
			operation = s.scheduleRegistrationRecovery(ctx, operation, "IDENTITY_UNAVAILABLE")
			return registrationResult(operation), ErrRegistrationRecovering
		}
		if err := s.repository.MarkRegistrationIdentityCreated(ctx, operation.ID, remote.Subject, remote.PasswordPreserved); err != nil {
			operation = s.scheduleRegistrationRecovery(ctx, operation, "IDENTITY_RESULT_PERSIST_FAILED")
			return registrationResult(operation), ErrRegistrationRecovering
		}
		operation, err = s.repository.RegistrationOperation(ctx, operation.ID)
		if err != nil {
			return RegistrationResult{}, err
		}
	}
	if operation.Status == "IDENTITY_CREATED" || operation.Status == "ACCOUNT_COMMITTING" || operation.Status == "REPAIR_REQUIRED" {
		committed, err := s.repository.CommitRegistration(ctx, operation.ID, s.issuer)
		if err != nil {
			operation = s.scheduleRegistrationRecovery(ctx, operation, "ACCOUNT_COMMIT_FAILED")
			return registrationResult(operation), ErrRegistrationRecovering
		}
		return registrationResult(committed), nil
	}
	return registrationResult(operation), nil
}

func (s *MobileAuthService) scheduleRegistrationRecovery(ctx context.Context, operation RegistrationOperation, code string) RegistrationOperation {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	_ = s.repository.ScheduleRegistrationRetry(cleanupCtx, operation.ID, code)
	if current, err := s.repository.RegistrationOperation(cleanupCtx, operation.ID); err == nil {
		return current
	}
	return operation
}

func registrationResult(operation RegistrationOperation) RegistrationResult {
	return RegistrationResult{PasswordPreserved: operation.PasswordPreserved, OperationID: operation.ID, UserID: operation.UserID, UserCode: operation.UserCode, Username: operation.Username, Status: operation.Status, NextRetryAt: operation.NextRetryAt}
}

func (s *MobileAuthService) VerifyLogin(ctx context.Context, rawMobile string, challengeID uuid.UUID, code string) (string, error) {
	mobile, err := NormalizeMainlandMobile(rawMobile)
	if err != nil {
		return "", ErrSMSInvalid
	}
	subject, err := s.repository.MobileIdentitySubject(ctx, s.issuer, mobile)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrSMSInvalid
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(subject) == "" {
		return "", ErrSMSInvalid
	}
	if err := s.verifyChallenge(ctx, challengeID, mobile, SMSPurposeLogin, code); err != nil {
		return "", err
	}
	return subject, nil
}

func (s *MobileAuthService) verifyChallenge(ctx context.Context, challengeID uuid.UUID, mobile string, purpose SMSPurpose, code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return ErrSMSInvalid
	}
	verificationData, err := s.repository.SMSChallengeReady(ctx, challengeID, mobile, purpose)
	if err != nil {
		return err
	}
	verified, err := s.sms.VerifyCode(ctx, mobile, challengeID, verificationData, code)
	if err != nil {
		return ErrSMSUnavailable
	}
	return s.repository.ConsumeSMSChallenge(ctx, challengeID, mobile, purpose, verified)
}

func (r *PostgresRepository) MobileExists(ctx context.Context, mobile string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE mobile_e164=$1 AND status<>'DELETED') OR EXISTS(SELECT 1 FROM profile_change_requests WHERE field='MOBILE' AND new_value=$1 AND status IN ('PENDING','VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY','REPAIR_REQUIRED'))`, mobile).Scan(&exists)
	return exists, err
}

func (r *PostgresRepository) MobileIdentitySubject(ctx context.Context, issuer, mobile string) (string, error) {
	var subject string
	err := r.pool.QueryRow(ctx, `SELECT i.subject FROM users u JOIN identities i ON i.user_id=u.id WHERE u.status<>'DELETED' AND i.issuer=$2 AND (u.mobile_e164=$1 OR EXISTS(SELECT 1 FROM profile_change_requests p WHERE p.user_id=u.id AND p.identity_issuer=i.issuer AND p.identity_subject=i.subject AND p.field='MOBILE' AND p.new_value=$1 AND p.identity_updated_at IS NOT NULL AND p.status IN ('IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY','REPAIR_REQUIRED'))) ORDER BY (u.mobile_e164=$1) DESC,i.last_seen_at DESC LIMIT 1`, mobile, issuer).Scan(&subject)
	return subject, err
}

func (r *PostgresRepository) UsernameExists(ctx context.Context, username string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE lower(username)=lower($1)) OR EXISTS(SELECT 1 FROM profile_change_requests WHERE field='USERNAME' AND new_value=lower($1) AND status IN ('PENDING','VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY','REPAIR_REQUIRED'))`, username).Scan(&exists)
	return exists, err
}

func (r *PostgresRepository) CreateSMSChallenge(ctx context.Context, challenge SMSChallenge) error {
	tx, err := r.pool.Begin(ctx)
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
	locks := []string{"mobile:" + challenge.Mobile, "source:" + challenge.Source, "device:" + challenge.Device}
	sort.Strings(locks)
	for _, lock := range locks {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lock); err != nil {
			return err
		}
	}
	now := r.now()
	mobileHash, sourceHash, deviceHash := sha256.Sum256([]byte(challenge.Mobile)), sha256.Sum256([]byte(challenge.Source)), sha256.Sum256([]byte(challenge.Device))
	var last *time.Time
	var oldest *time.Time
	var hourly int
	if err := tx.QueryRow(ctx, `SELECT max(created_at) FILTER (WHERE status<>'FAILED'),min(created_at) FILTER (WHERE created_at>$2 AND status<>'FAILED'),count(*) FILTER (WHERE created_at>$2 AND status<>'FAILED') FROM sms_challenges WHERE mobile_e164=$1`, challenge.Mobile, now.Add(-time.Hour)).Scan(&last, &oldest, &hourly); err != nil {
		return err
	}
	if err := smsRateLimit(now, last, oldest, hourly); err != nil {
		if recordErr := recordSMSAttempt(ctx, tx, challenge, mobileHash, sourceHash, deviceHash, "RATE_LIMITED", now); recordErr != nil {
			return recordErr
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return commitErr
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
		if err := tx.Commit(ctx); err != nil {
			return err
		}
		return &SMSRateLimitError{RetryAfter: time.Hour}
	}
	_, err = tx.Exec(ctx, `INSERT INTO sms_challenges(id,mobile_e164,purpose,code_hash,status,expires_at,created_at,updated_at,provider_id,adapter_version) VALUES($1,$2,$3,NULL,'CREATED',$4,$5,$5,$6,$7)`, challenge.ID, challenge.Mobile, challenge.Purpose, challenge.ExpiresAt, now, challenge.Provider.ID, challenge.Provider.AdapterVersion)
	if err != nil {
		return err
	}
	if err := recordSMSAttempt(ctx, tx, challenge, mobileHash, sourceHash, deviceHash, "ACCEPTED", now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func recordSMSAttempt(ctx context.Context, tx pgx.Tx, challenge SMSChallenge, mobileHash, sourceHash, deviceHash [32]byte, result string, now time.Time) error {
	var challengeID *uuid.UUID
	if result == "ACCEPTED" {
		challengeID = &challenge.ID
	}
	_, err := tx.Exec(ctx, `INSERT INTO sms_send_attempts(id,challenge_id,mobile_hash,source_hash,device_hash,purpose,result,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, uuid.Must(uuid.NewV7()), challengeID, mobileHash[:], sourceHash[:], deviceHash[:], challenge.Purpose, result, now)
	return err
}

func smsRateLimit(now time.Time, last, oldest *time.Time, hourly int) error {
	if last != nil && now.Sub(*last) < time.Minute {
		return &SMSRateLimitError{RetryAfter: time.Minute - now.Sub(*last)}
	}
	if hourly >= 5 {
		retryAfter := time.Hour
		if oldest != nil {
			retryAfter = max(time.Second, time.Hour-now.Sub(*oldest))
		}
		return &SMSRateLimitError{RetryAfter: retryAfter}
	}
	return nil
}

func (r *PostgresRepository) MarkSMSChallengeSent(ctx context.Context, id uuid.UUID, messageID string, verificationData []byte) error {
	_, err := r.pool.Exec(ctx, `UPDATE sms_challenges SET status='SENT',provider_message_id=NULLIF($2,''),code_hash=$3,sent_at=$4,updated_at=$4 WHERE id=$1 AND status='CREATED'`, id, messageID, verificationData, r.now())
	return err
}

func (r *PostgresRepository) MarkSMSChallengeFailed(ctx context.Context, id uuid.UUID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Only the provider's explicit non-acceptance reaches this method. Release
	// its unverified registration reservation in the same transaction.
	if _, err := tx.Exec(ctx, `UPDATE registration_operations SET status='EXPIRED',last_error_code='SMS_REJECTED',version=version+1,updated_at=$2 WHERE sms_challenge_id=$1 AND status='SMS_PENDING' AND identity_create_attempted_at IS NULL AND EXISTS(SELECT 1 FROM sms_challenges sc WHERE sc.id=$1 AND sc.status='CREATED')`, id, r.now()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE sms_challenges SET status='FAILED',updated_at=$2 WHERE id=$1 AND status='CREATED'`, id, r.now()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) SMSChallengeReady(ctx context.Context, id uuid.UUID, mobile string, purpose SMSPurpose) (SMSVerificationData, error) {
	var status string
	var attempts int
	var expires time.Time
	var verificationData []byte
	var providerID, adapterVersion string
	err := r.pool.QueryRow(ctx, `SELECT status,attempts,expires_at,code_hash,provider_id,adapter_version FROM sms_challenges WHERE id=$1 AND mobile_e164=$2 AND purpose=$3`, id, mobile, purpose).Scan(&status, &attempts, &expires, &verificationData, &providerID, &adapterVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return SMSVerificationData{}, ErrSMSInvalid
	}
	if err != nil {
		return SMSVerificationData{}, err
	}
	if status != "SENT" || attempts >= 5 || !expires.After(r.now()) {
		if status == "SENT" && !expires.After(r.now()) {
			_, _ = r.pool.Exec(ctx, `UPDATE sms_challenges SET status='EXPIRED',updated_at=$2 WHERE id=$1 AND status='SENT'`, id, r.now())
		}
		return SMSVerificationData{}, ErrSMSInvalid
	}
	return SMSVerificationData{Provider: SMSProviderDescriptor{ID: providerID, AdapterVersion: adapterVersion}, Opaque: verificationData}, nil
}

func (r *PostgresRepository) ConsumeSMSChallenge(ctx context.Context, id uuid.UUID, mobile string, purpose SMSPurpose, verified bool) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var status string
	var attempts int
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT status,attempts,expires_at FROM sms_challenges WHERE id=$1 AND mobile_e164=$2 AND purpose=$3 FOR UPDATE`, id, mobile, purpose).Scan(&status, &attempts, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSMSInvalid
	}
	if err != nil {
		return err
	}
	if status != "SENT" || attempts >= 5 || !expires.After(r.now()) {
		if status == "SENT" && !expires.After(r.now()) {
			_, err = tx.Exec(ctx, `UPDATE sms_challenges SET status='EXPIRED',updated_at=$2 WHERE id=$1`, id, r.now())
			if err != nil {
				return err
			}
			if err = tx.Commit(ctx); err != nil {
				return err
			}
		}
		return ErrSMSInvalid
	}
	if !verified {
		attempts++
		_, err = tx.Exec(ctx, `UPDATE sms_challenges SET attempts=$2,status=CASE WHEN $2>=5 THEN 'FAILED' ELSE status END,updated_at=$3 WHERE id=$1`, id, attempts, r.now())
		if err != nil {
			return err
		}
		if err = tx.Commit(ctx); err != nil {
			return err
		}
		return ErrSMSInvalid
	}
	_, err = tx.Exec(ctx, `UPDATE sms_challenges SET status='CONSUMED',consumed_at=$2,updated_at=$2 WHERE id=$1`, id, r.now())
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) RegisterMobileIdentity(ctx context.Context, issuer, subject, username, mobile string) (uuid.UUID, string, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return uuid.Nil, "", err
	}
	defer tx.Rollback(ctx)
	now, userID := r.now(), uuid.Must(uuid.NewV7())
	userCode, err := insertUserWithGeneratedCode(ctx, tx, func(database pgx.Tx, candidate string) error {
		_, insertErr := database.Exec(ctx, `INSERT INTO users(id,user_code,status,username,display_name,mobile_e164,mobile_verified_at,created_at,updated_at) VALUES($1,$2,'PROVISIONING',$3,$3,$4,$5,$5,$5)`, userID, candidate, username, mobile, now)
		return insertErr
	})
	if err != nil {
		if strings.Contains(err.Error(), "users_mobile_e164_unique_idx") {
			return uuid.Nil, "", ErrMobileAlreadyRegistered
		}
		if strings.Contains(err.Error(), "users_username_unique_idx") {
			return uuid.Nil, "", ErrUsernameAlreadyRegistered
		}
		return uuid.Nil, "", err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES($1,$2,$3,$4,$4)`, issuer, subject, userID, now); err != nil {
		return uuid.Nil, "", err
	}
	if err = enqueueCommerceCustomerSync(ctx, tx, "USER", userID, mobile, r.commerceProviderID, r.commerceAdapterVersion, "CUSTOMER_PROVISIONING", now); err != nil {
		return uuid.Nil, "", err
	}
	if err := appendAudit(ctx, tx, userID, "registration", nil, "account.mobile.register", "user", userID.String(), "SUCCEEDED", map[string]any{"mobile_verified": true}); err != nil {
		return uuid.Nil, "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, "", err
	}
	return userID, FormatUserCode(userCode), nil
}
