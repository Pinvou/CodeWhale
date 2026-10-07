package account

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrMobileInvalid                 = errors.New("mobile number is invalid")
	ErrMobileAlreadyRegistered       = errors.New("mobile number is already registered")
	ErrUsernameInvalid               = errors.New("username is invalid")
	ErrUsernameAlreadyRegistered     = errors.New("username is already registered")
	ErrPasswordReferenceInvalid      = errors.New("password reference is invalid or expired")
	ErrPasswordReferenceConflict     = errors.New("password reference conflicts with registration data")
	ErrPasswordInvalid               = errors.New("password does not satisfy the active policy")
	ErrPasswordEdgeSpace             = errors.New("password must not start or end with a space")
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
	Status                          string                `json:"delivery_status,omitempty"`
	Mobile                          string                `json:"-"`
	Purpose                         SMSPurpose            `json:"-"`
	ExpiresAt                       time.Time             `json:"expires_at"`
	Source                          string                `json:"-"`
	Device                          string                `json:"-"`
	Provider                        SMSProviderDescriptor `json:"-"`
	VerificationData                []byte                `json:"-"`
	DeliveryLeaseUntil              *time.Time            `json:"-"`
	DeliveryStartedAt               *time.Time            `json:"-"`
	DeliveryUncertainAt             *time.Time            `json:"-"`
	RegistrationOperationID         uuid.UUID             `json:"registration_operation_id,omitempty"`
	RegistrationQueryToken          string                `json:"registration_query_token,omitempty"`
	RegistrationQueryTokenExpiresAt time.Time             `json:"registration_query_token_expires_at,omitempty"`
}

type RegistrationOperation struct {
	ID                    uuid.UUID  `json:"registration_operation_id"`
	CandidateUserID       uuid.UUID  `json:"-"`
	QueryTokenHash        []byte     `json:"-"`
	RequestFingerprint    []byte     `json:"-"`
	Username              string     `json:"-"`
	Mobile                string     `json:"-"`
	ChallengeID           uuid.UUID  `json:"-"`
	PasswordPolicyVersion string     `json:"-"`
	IdentitySubject       string     `json:"-"`
	UserID                *uuid.UUID `json:"user_id,omitempty"`
	UserCode              string     `json:"user_code,omitempty"`
	Status                string     `json:"status"`
	Version               int64      `json:"-"`
	IdentityCreatedAt     *time.Time `json:"-"`
	NextRetryAt           time.Time  `json:"next_retry_at"`
	LastErrorCode         string     `json:"reason,omitempty"`
	CreatedAt             time.Time  `json:"-"`
	UpdatedAt             time.Time  `json:"updated_at"`
}

type RegistrationResult struct {
	OperationID uuid.UUID  `json:"registration_operation_id"`
	UserID      *uuid.UUID `json:"user_id,omitempty"`
	UserCode    string     `json:"user_code,omitempty"`
	Username    string     `json:"username,omitempty"`
	Status      string     `json:"status"`
	NextRetryAt time.Time  `json:"next_retry_at,omitempty"`
}

type MobileAuthRepository interface {
	MobileExists(context.Context, string) (bool, error)
	MobileIdentitySubject(context.Context, string, string) (string, error)
	AuthorizeMobilePersonalSecurity(context.Context, string, string) (bool, error)
	UsernameExists(context.Context, string) (bool, error)
	CreateSliderCaptcha(context.Context, SliderCaptcha) error
	ConsumeSliderCaptcha(context.Context, uuid.UUID, string, SMSPurpose, int) error
	CreateSMSChallenge(context.Context, SMSChallenge) error
	CreateRegistrationOperation(context.Context, RegistrationOperation, SMSChallenge) error
	MarkSMSChallengeSent(context.Context, uuid.UUID, string, []byte) error
	MarkSMSChallengeFailed(context.Context, uuid.UUID) error
	SMSChallengeReady(context.Context, uuid.UUID, string, SMSPurpose) (SMSVerificationData, error)
	ConsumeSMSChallenge(context.Context, uuid.UUID, string, SMSPurpose, bool) error
	AdmitSMSVerification(context.Context, uuid.UUID, string, SMSPurpose, string, string) error
	ConsumeRegistrationSMS(context.Context, uuid.UUID, uuid.UUID, string, bool) error
	RegistrationOperation(context.Context, uuid.UUID) (RegistrationOperation, error)
	MarkRegistrationIdentityCreating(context.Context, uuid.UUID) error
	MarkRegistrationIdentityCreated(context.Context, uuid.UUID, string) error
	CommitRegistration(context.Context, uuid.UUID, string) (RegistrationOperation, error)
	ScheduleRegistrationRetry(context.Context, uuid.UUID, string) error
	CompensatePreIdentityRegistration(context.Context, uuid.UUID, string) error
}

type SMSRateLimitError struct {
	RetryAfter time.Duration
}

type authRiskContextKey struct{}

type authRiskDimensions struct{ source, device string }

func withAuthRisk(ctx context.Context, source, device string) context.Context {
	source = strings.TrimSpace(source)
	device = strings.TrimSpace(device)
	if source == "" {
		source = "source:unknown"
	}
	if device == "" || len(device) > 128 {
		device = "device:absent:" + source
	}
	return context.WithValue(ctx, authRiskContextKey{}, authRiskDimensions{source: source, device: device})
}

func authRiskFromContext(ctx context.Context) authRiskDimensions {
	if dimensions, ok := ctx.Value(authRiskContextKey{}).(authRiskDimensions); ok {
		return dimensions
	}
	return authRiskDimensions{source: "source:unknown", device: "device:absent:source:unknown"}
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
	if purpose == SMSPurposeRegister && exists {
		return SMSChallenge{}, ErrMobileAlreadyRegistered
	}
	passwordResetAllowed := false
	if purpose == SMSPurposePasswordReset && exists {
		// Authorization failures deliberately collapse into the same accepted
		// response as an unknown mobile. No SMS is sent when Account cannot
		// strongly prove the personal security permission and lifecycle state.
		passwordResetAllowed, _ = s.repository.AuthorizeMobilePersonalSecurity(ctx, s.issuer, mobile)
	}
	provider, err := activeSMSProvider(ctx, s.sms)
	if err != nil {
		return SMSChallenge{}, ErrSMSUnavailable
	}
	challenge := SMSChallenge{ID: uuid.Must(uuid.NewV7()), Mobile: mobile, Purpose: purpose, ExpiresAt: s.now().Add(5 * time.Minute), Source: strings.TrimSpace(source), Device: strings.TrimSpace(device), Provider: provider.Descriptor()}
	if purpose == SMSPurposeRegister {
		operationID := uuid.Must(uuid.NewV7())
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
			ID: operationID, CandidateUserID: uuid.Must(uuid.NewV7()), QueryTokenHash: tokenHash[:], RequestFingerprint: fingerprint[:],
			Username: username, Mobile: mobile, ChallengeID: challenge.ID, PasswordPolicyVersion: passwordPolicyVersion,
			Status: "SMS_PENDING", NextRetryAt: challenge.ExpiresAt, CreatedAt: s.now(), UpdatedAt: s.now(),
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
		if exists && (purpose == SMSPurposeLogin || passwordResetAllowed) {
			go s.deliverSMS(context.WithoutCancel(ctx), challenge)
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

func (s *MobileAuthService) VerifyPasswordReset(ctx context.Context, rawMobile string, challengeID uuid.UUID, code string) (bool, error) {
	mobile, err := NormalizeMainlandMobile(rawMobile)
	if err != nil {
		return false, err
	}
	risk := authRiskFromContext(ctx)
	if err := s.repository.AdmitSMSVerification(ctx, challengeID, mobile, SMSPurposePasswordReset, risk.source, risk.device); err != nil {
		return false, err
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return false, ErrSMSInvalid
	}
	verificationData, err := s.repository.SMSChallengeReady(ctx, challengeID, mobile, SMSPurposePasswordReset)
	if err != nil {
		return false, err
	}
	provider, providerErr := smsProviderForVerification(ctx, s.sms, verificationData.Provider)
	if providerErr != nil {
		return false, ErrSMSUnavailable
	}
	verified, err := provider.VerifyCode(ctx, mobile, challengeID, verificationData, code)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrSMSUnavailable, err)
	}
	return verified, nil
}

func (s *MobileAuthService) VerifyProfileMobile(ctx context.Context, rawMobile string, challengeID uuid.UUID, code string) (bool, error) {
	mobile, err := NormalizeMainlandMobile(rawMobile)
	if err != nil {
		return false, err
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return false, ErrSMSInvalid
	}
	risk := authRiskFromContext(ctx)
	if err := s.repository.AdmitSMSVerification(ctx, challengeID, mobile, SMSPurposeProfileMobile, risk.source, risk.device); err != nil {
		return false, err
	}
	verificationData, err := s.repository.SMSChallengeReady(ctx, challengeID, mobile, SMSPurposeProfileMobile)
	if err != nil {
		return false, err
	}
	provider, providerErr := smsProviderForVerification(ctx, s.sms, verificationData.Provider)
	if providerErr != nil {
		return false, ErrSMSUnavailable
	}
	verified, err := provider.VerifyCode(ctx, mobile, challengeID, verificationData, code)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrSMSUnavailable, err)
	}
	return verified, nil
}

func registrationFingerprint(username, mobile string, challengeID uuid.UUID, policyVersion string) [32]byte {
	return sha256.Sum256([]byte(strings.Join([]string{username, mobile, challengeID.String(), policyVersion, "registration-v1"}, "\x00")))
}

func (s *MobileAuthService) deliverSMS(ctx context.Context, challenge SMSChallenge) error {
	provider, err := smsProviderForSend(ctx, s.sms, challenge.Provider)
	if err != nil {
		return ErrSMSUnavailable
	}
	delivery, err := provider.SendCode(ctx, challenge.Mobile, challenge.Purpose, challenge.ID)
	if err != nil {
		_ = s.repository.MarkSMSChallengeFailed(context.WithoutCancel(ctx), challenge.ID)
		return fmt.Errorf("%w: %v", ErrSMSUnavailable, err)
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

func (s *MobileAuthService) Register(ctx context.Context, operationID uuid.UUID, queryToken, _rawUsername, _rawMobile string, _ uuid.UUID, code, passwordHandle, _passwordPolicyVersion string) (RegistrationResult, error) {
	operation, err := s.authorizeRegistrationQuery(ctx, operationID, queryToken)
	if err != nil {
		return RegistrationResult{}, err
	}
	// The query token selects the immutable registration operation. All identity
	// and SMS bindings are read back from that server-side record; duplicate
	// browser fields are accepted only for wire compatibility and never become
	// an independent source of truth.
	if err := s.identities.ValidateMobileUser(ctx, operationID, operation.Username, operation.Mobile, passwordHandle, operation.PasswordPolicyVersion); err != nil {
		if operation.UserID != nil && errors.Is(err, ErrPasswordReferenceInvalid) {
			return registrationResult(operation), nil
		}
		return RegistrationResult{}, err
	}
	fingerprint := registrationFingerprint(operation.Username, operation.Mobile, operation.ChallengeID, operation.PasswordPolicyVersion)
	if !hmac.Equal(operation.RequestFingerprint, fingerprint[:]) {
		slog.WarnContext(ctx, "registration operation conflict",
			"stage", "fingerprint_check",
			"operation_id", operation.ID.String(),
			"operation_status", operation.Status,
			"operation_version", operation.Version,
			"stored_fingerprint", hex.EncodeToString(operation.RequestFingerprint),
			"calculated_fingerprint", hex.EncodeToString(fingerprint[:]),
			"stored_fingerprint_length", len(operation.RequestFingerprint),
			"username_length", len(operation.Username),
			"mobile_length", len(operation.Mobile),
			"policy_version", operation.PasswordPolicyVersion,
			"challenge_id", operation.ChallengeID.String(),
		)
		return RegistrationResult{}, ErrRegistrationConflict
	}
	if operation.Status == "COMPENSATED" || operation.Status == "EXPIRED" {
		return registrationResult(operation), ErrRegistrationRestart
	}
	if operation.Status == "PROVISIONING" || operation.Status == "ACCOUNT_COMMITTED" {
		return registrationResult(operation), nil
	}
	if operation.Status == "SMS_PENDING" {
		risk := authRiskFromContext(ctx)
		if err := s.repository.AdmitSMSVerification(ctx, operation.ChallengeID, operation.Mobile, SMSPurposeRegister, risk.source, risk.device); err != nil {
			return RegistrationResult{}, err
		}
		verificationData, readyErr := s.repository.SMSChallengeReady(ctx, operation.ChallengeID, operation.Mobile, SMSPurposeRegister)
		if readyErr != nil {
			return RegistrationResult{}, readyErr
		}
		provider, providerErr := smsProviderForVerification(ctx, s.sms, verificationData.Provider)
		if providerErr != nil {
			return RegistrationResult{}, ErrSMSUnavailable
		}
		verified, verifyErr := provider.VerifyCode(ctx, operation.Mobile, operation.ChallengeID, verificationData, strings.TrimSpace(code))
		if verifyErr != nil {
			return RegistrationResult{}, fmt.Errorf("%w: %v", ErrSMSUnavailable, verifyErr)
		}
		if err := s.repository.ConsumeRegistrationSMS(ctx, operationID, operation.ChallengeID, operation.Mobile, verified); err != nil {
			if errors.Is(err, ErrRegistrationConflict) {
				slog.WarnContext(ctx, "registration operation conflict", "stage", "consume_sms", "operation_id", operation.ID.String(), "operation_status", operation.Status, "operation_version", operation.Version)
			}
			return RegistrationResult{}, err
		}
	}
	operation, err = s.repository.RegistrationOperation(ctx, operationID)
	if err != nil {
		return RegistrationResult{}, err
	}
	result, err := s.resumeRegistration(ctx, operation)
	if errors.Is(err, ErrRegistrationConflict) {
		slog.WarnContext(ctx, "registration operation conflict", "stage", "resume_registration", "operation_id", operation.ID.String(), "operation_status", operation.Status, "operation_version", operation.Version)
	}
	return result, err
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
	if !hmac.Equal(operation.QueryTokenHash, hash[:]) {
		return RegistrationOperation{}, ErrRegistrationQueryUnauthorized
	}
	return operation, nil
}

func (s *MobileAuthService) resumeRegistration(ctx context.Context, operation RegistrationOperation) (RegistrationResult, error) {
	if operation.Status == "REGISTRATION_IN_PROGRESS" || operation.Status == "IDENTITY_CREATING" {
		if err := s.repository.MarkRegistrationIdentityCreating(ctx, operation.ID); err != nil {
			return RegistrationResult{}, err
		}
		subject, found, err := s.identities.FindMobileUserByRegistrationOperation(ctx, operation.ID)
		if err == nil && !found {
			subject, err = s.identities.CreateMobileUser(ctx, operation.ID, operation.Username, operation.Mobile, "", operation.PasswordPolicyVersion)
		}
		if err != nil {
			if errors.Is(err, ErrPasswordReferenceInvalid) {
				_ = s.repository.CompensatePreIdentityRegistration(context.WithoutCancel(ctx), operation.ID, "PASSWORD_REFERENCE_EXPIRED")
				operation, _ = s.repository.RegistrationOperation(context.WithoutCancel(ctx), operation.ID)
				return registrationResult(operation), ErrRegistrationRestart
			}
			_ = s.repository.ScheduleRegistrationRetry(context.WithoutCancel(ctx), operation.ID, "IDENTITY_UNAVAILABLE")
			operation, _ = s.repository.RegistrationOperation(context.WithoutCancel(ctx), operation.ID)
			return registrationResult(operation), ErrRegistrationRecovering
		}
		if err := s.repository.MarkRegistrationIdentityCreated(ctx, operation.ID, subject); err != nil {
			_ = s.repository.ScheduleRegistrationRetry(context.WithoutCancel(ctx), operation.ID, "IDENTITY_RESULT_PERSIST_FAILED")
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
			_ = s.repository.ScheduleRegistrationRetry(context.WithoutCancel(ctx), operation.ID, "ACCOUNT_COMMIT_FAILED")
			operation, _ = s.repository.RegistrationOperation(context.WithoutCancel(ctx), operation.ID)
			return registrationResult(operation), ErrRegistrationRecovering
		}
		return registrationResult(committed), nil
	}
	return registrationResult(operation), nil
}

func registrationResult(operation RegistrationOperation) RegistrationResult {
	return RegistrationResult{OperationID: operation.ID, UserID: operation.UserID, UserCode: operation.UserCode, Username: operation.Username, Status: operation.Status, NextRetryAt: operation.NextRetryAt}
}

func (s *MobileAuthService) VerifyLogin(ctx context.Context, rawMobile string, challengeID uuid.UUID, code string) (subject string, resultErr error) {
	startedAt := s.now()
	defer func() {
		if err := waitForResponseFloor(ctx, startedAt, s.loginResponseFloor); err != nil && resultErr == nil {
			subject, resultErr = "", err
		}
	}()
	mobile, err := NormalizeMainlandMobile(rawMobile)
	if err != nil {
		return "", ErrSMSInvalid
	}
	risk := authRiskFromContext(ctx)
	if err := s.repository.AdmitSMSVerification(ctx, challengeID, mobile, SMSPurposeLogin, risk.source, risk.device); err != nil {
		return "", err
	}
	subject, err = s.repository.MobileIdentitySubject(ctx, s.issuer, mobile)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrSMSInvalid
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(subject) == "" {
		return "", ErrSMSInvalid
	}
	if err := s.verifyChallengeAccepted(ctx, challengeID, mobile, SMSPurposeLogin, code); err != nil {
		return "", err
	}
	return subject, nil
}

func (s *MobileAuthService) verifyChallenge(ctx context.Context, challengeID uuid.UUID, mobile string, purpose SMSPurpose, code string) error {
	risk := authRiskFromContext(ctx)
	if err := s.repository.AdmitSMSVerification(ctx, challengeID, mobile, purpose, risk.source, risk.device); err != nil {
		return err
	}
	return s.verifyChallengeAccepted(ctx, challengeID, mobile, purpose, code)
}

func (s *MobileAuthService) verifyChallengeAccepted(ctx context.Context, challengeID uuid.UUID, mobile string, purpose SMSPurpose, code string) error {
	code = strings.TrimSpace(code)
	if code == "" {
		return ErrSMSInvalid
	}
	verificationData, err := s.repository.SMSChallengeReady(ctx, challengeID, mobile, purpose)
	if err != nil {
		return err
	}
	provider, providerErr := smsProviderForVerification(ctx, s.sms, verificationData.Provider)
	if providerErr != nil {
		return ErrSMSUnavailable
	}
	verified, err := provider.VerifyCode(ctx, mobile, challengeID, verificationData, code)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSMSUnavailable, err)
	}
	return s.repository.ConsumeSMSChallenge(ctx, challengeID, mobile, purpose, verified)
}

func activeSMSProvider(ctx context.Context, provider SMSProvider) (SMSProvider, error) {
	if routed, ok := provider.(routedSMSProvider); ok {
		return routed.ActiveProvider(ctx)
	}
	return provider, nil
}

func smsProviderForSend(ctx context.Context, provider SMSProvider, descriptor SMSProviderDescriptor) (SMSProvider, error) {
	if routed, ok := provider.(routedSMSProvider); ok {
		return routed.ProviderForSend(ctx, descriptor)
	}
	if provider.Descriptor() != descriptor {
		return nil, ErrSMSUnavailable
	}
	return provider, nil
}

func smsProviderForVerification(ctx context.Context, provider SMSProvider, descriptor SMSProviderDescriptor) (SMSProvider, error) {
	if routed, ok := provider.(routedSMSProvider); ok {
		return routed.ProviderForVerification(ctx, descriptor)
	}
	if provider.Descriptor() != descriptor {
		return nil, ErrSMSUnavailable
	}
	return provider, nil
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

func (r *PostgresRepository) AuthorizeMobilePersonalSecurity(ctx context.Context, issuer, mobile string) (bool, error) {
	subject, err := r.MobileIdentitySubject(ctx, issuer, mobile)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	decision, err := r.Authorize(ctx, identity.Principal{Issuer: issuer, Subject: subject}, PermissionPersonalSecurityManage, nil, nil)
	if errors.Is(err, ErrForbidden) {
		return false, nil
	}
	return err == nil && decision.Allowed, err
}

func (r *PostgresRepository) UsernameExists(ctx context.Context, username string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE lower(username)=lower($1) AND status<>'DELETED') OR EXISTS(SELECT 1 FROM profile_change_requests WHERE field='USERNAME' AND new_value=lower($1) AND status IN ('PENDING','VERIFIED','IDENTITY_UPDATED','ACCOUNT_COMMITTED','RETRY','REPAIR_REQUIRED'))`, username).Scan(&exists)
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
	if err := enforceSMSRateLimits(ctx, tx, challenge, now); err != nil {
		if !errors.Is(err, ErrSMSRateLimited) {
			return err
		}
		if commitErr := tx.Commit(ctx); commitErr != nil {
			return commitErr
		}
		return err
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
	_, err := tx.Exec(ctx, `INSERT INTO sms_send_attempts(id,challenge_id,mobile_hash,source_hash,device_hash,purpose,result,occurred_at,provider_id,adapter_version) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, uuid.Must(uuid.NewV7()), challengeID, mobileHash[:], sourceHash[:], deviceHash[:], challenge.Purpose, result, now, challenge.Provider.ID, challenge.Provider.AdapterVersion)
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
	_, err := r.pool.Exec(ctx, `UPDATE sms_challenges SET status='FAILED',updated_at=$2 WHERE id=$1 AND status='CREATED'`, id, r.now())
	return err
}

func (r *PostgresRepository) SMSChallengeReady(ctx context.Context, id uuid.UUID, mobile string, purpose SMSPurpose) (SMSVerificationData, error) {
	var status string
	var attempts int
	var expires time.Time
	var deliveryStartedAt *time.Time
	var verificationData []byte
	var providerID, adapterVersion string
	err := r.pool.QueryRow(ctx, `SELECT status,attempts,expires_at,code_hash,provider_id,adapter_version,delivery_started_at FROM sms_challenges WHERE id=$1 AND mobile_e164=$2 AND purpose=$3`, id, mobile, purpose).Scan(&status, &attempts, &expires, &verificationData, &providerID, &adapterVersion, &deliveryStartedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return SMSVerificationData{}, ErrSMSInvalid
	}
	if err != nil {
		return SMSVerificationData{}, err
	}
	if status != "SENT" && !(status == "CREATED" && deliveryStartedAt != nil) || attempts >= 5 || !expires.After(r.now()) {
		if (status == "SENT" || status == "CREATED") && !expires.After(r.now()) {
			_, _ = r.pool.Exec(ctx, `UPDATE sms_challenges SET status='EXPIRED',delivery_lease_id=NULL,delivery_lease_until=NULL,updated_at=$2 WHERE id=$1 AND status IN ('CREATED','SENT')`, id, r.now())
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
	var deliveryStartedAt *time.Time
	err = tx.QueryRow(ctx, `SELECT status,attempts,expires_at,delivery_started_at FROM sms_challenges WHERE id=$1 AND mobile_e164=$2 AND purpose=$3 FOR UPDATE`, id, mobile, purpose).Scan(&status, &attempts, &expires, &deliveryStartedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSMSInvalid
	}
	if err != nil {
		return err
	}
	if status != "SENT" && !(status == "CREATED" && deliveryStartedAt != nil) || attempts >= 5 || !expires.After(r.now()) {
		if (status == "SENT" || status == "CREATED") && !expires.After(r.now()) {
			_, _ = tx.Exec(ctx, `UPDATE sms_challenges SET status='EXPIRED',delivery_lease_id=NULL,delivery_lease_until=NULL,updated_at=$2 WHERE id=$1`, id, r.now())
			_ = tx.Commit(ctx)
		}
		return ErrSMSInvalid
	}
	if !verified {
		attempts++
		_, _ = tx.Exec(ctx, `UPDATE sms_challenges SET attempts=$2,status=CASE WHEN $2>=5 THEN 'FAILED' ELSE status END,updated_at=$3 WHERE id=$1`, id, attempts, r.now())
		_ = tx.Commit(ctx)
		return ErrSMSInvalid
	}
	_, err = tx.Exec(ctx, `UPDATE sms_challenges SET status='CONSUMED',consumed_at=$2,updated_at=$2 WHERE id=$1`, id, r.now())
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) AdmitSMSVerification(ctx context.Context, challengeID uuid.UUID, mobile string, purpose SMSPurpose, source, device string) error {
	if source == "" {
		source = "source:unknown"
	}
	if device == "" {
		device = "device:absent:" + source
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	mobileHash, sourceHash, deviceHash := sha256.Sum256([]byte(mobile)), sha256.Sum256([]byte(source)), sha256.Sum256([]byte(device))
	locks := []string{"sms-verify-device:" + device, "sms-verify-mobile:" + mobile, "sms-verify-source:" + source}
	sort.Strings(locks)
	for _, lock := range locks {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lock); err != nil {
			return err
		}
	}
	providerID := "unknown"
	_ = tx.QueryRow(ctx, `SELECT provider_id FROM sms_challenges WHERE id=$1 AND mobile_e164=$2 AND purpose=$3`, challengeID, mobile, purpose).Scan(&providerID)
	// Verification has its own provider envelope, separate from send admission.
	// Serialize the count and accepted-attempt write across all risk dimensions.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "sms-provider-verify:"+providerID); err != nil {
		return err
	}
	now := r.now()
	// Verification has a separate provider safety envelope of 375/minute and is
	// admitted at 80% (300/minute), independent from send quotas.
	const providerVerificationLimit = 375 * 80 / 100
	var sourceWindow, deviceWindow, mobileWindow, purposeSourceWindow, providerMinute int
	var sourceOldest, deviceOldest, mobileOldest, purposeOldest, providerOldest *time.Time
	if err := tx.QueryRow(ctx, `SELECT
count(*) FILTER (WHERE result='ACCEPTED' AND source_hash=$1 AND occurred_at>$4::timestamptz-interval '10 minutes'), min(occurred_at) FILTER (WHERE result='ACCEPTED' AND source_hash=$1 AND occurred_at>$4::timestamptz-interval '10 minutes'),
count(*) FILTER (WHERE result='ACCEPTED' AND device_hash=$2 AND occurred_at>$4::timestamptz-interval '10 minutes'), min(occurred_at) FILTER (WHERE result='ACCEPTED' AND device_hash=$2 AND occurred_at>$4::timestamptz-interval '10 minutes'),
count(*) FILTER (WHERE result='ACCEPTED' AND mobile_hash=$3 AND occurred_at>$4::timestamptz-interval '10 minutes'), min(occurred_at) FILTER (WHERE result='ACCEPTED' AND mobile_hash=$3 AND occurred_at>$4::timestamptz-interval '10 minutes'),
count(*) FILTER (WHERE result='ACCEPTED' AND source_hash=$1 AND purpose=$5 AND occurred_at>$4::timestamptz-interval '10 minutes'), min(occurred_at) FILTER (WHERE result='ACCEPTED' AND source_hash=$1 AND purpose=$5 AND occurred_at>$4::timestamptz-interval '10 minutes'),
count(*) FILTER (WHERE result='ACCEPTED' AND provider_id=$6 AND occurred_at>$4::timestamptz-interval '1 minute'), min(occurred_at) FILTER (WHERE result='ACCEPTED' AND provider_id=$6 AND occurred_at>$4::timestamptz-interval '1 minute')
FROM sms_verification_attempts WHERE occurred_at>$4::timestamptz-interval '10 minutes'`, sourceHash[:], deviceHash[:], mobileHash[:], now, purpose, providerID).Scan(
		&sourceWindow, &sourceOldest, &deviceWindow, &deviceOldest, &mobileWindow, &mobileOldest, &purposeSourceWindow, &purposeOldest, &providerMinute, &providerOldest,
	); err != nil {
		return err
	}
	result := "ACCEPTED"
	retryAfter := time.Duration(0)
	limits := []struct {
		reached bool
		oldest  *time.Time
		window  time.Duration
	}{
		{sourceWindow >= 30, sourceOldest, 10 * time.Minute}, {deviceWindow >= 20, deviceOldest, 10 * time.Minute},
		{mobileWindow >= 10, mobileOldest, 10 * time.Minute}, {purposeSourceWindow >= 20, purposeOldest, 10 * time.Minute},
		{providerMinute >= providerVerificationLimit, providerOldest, time.Minute},
	}
	for _, limit := range limits {
		if limit.reached && limit.oldest != nil {
			retryAfter = max(retryAfter, limit.oldest.Add(limit.window).Sub(now))
		}
	}
	if retryAfter > 0 {
		result = "RATE_LIMITED"
	}
	var storedChallengeID *uuid.UUID
	if providerID != "unknown" {
		storedChallengeID = &challengeID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO sms_verification_attempts(id,challenge_id,mobile_hash,source_hash,device_hash,purpose,provider_id,result,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`, uuid.Must(uuid.NewV7()), storedChallengeID, mobileHash[:], sourceHash[:], deviceHash[:], purpose, providerID, result, now); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	if result == "RATE_LIMITED" {
		return &SMSRateLimitError{RetryAfter: max(retryAfter, time.Second)}
	}
	return nil
}
