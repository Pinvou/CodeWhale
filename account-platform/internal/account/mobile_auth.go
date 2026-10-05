package account

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrMobileInvalid             = errors.New("mobile number is invalid")
	ErrMobileAlreadyRegistered   = errors.New("mobile number is already registered")
	ErrUsernameInvalid           = errors.New("username is invalid")
	ErrUsernameAlreadyRegistered = errors.New("username is already registered")
	ErrPasswordInvalid           = errors.New("password is invalid")
	ErrCaptchaInvalid            = errors.New("slider captcha is invalid")
	ErrSMSRateLimited            = errors.New("SMS requests are rate limited")
	ErrSMSInvalid                = errors.New("SMS challenge is invalid")
	ErrSMSUnavailable            = errors.New("SMS service is unavailable")
	// Only an authoritative pre-delivery rejection may exempt a request from
	// the cooldown. Transport failures do not prove that no SMS was accepted.
	ErrSMSRejected = errors.New("SMS provider did not accept the request")
)

var mainlandMobile = regexp.MustCompile(`^1[3-9][0-9]{9}$`)
var registrationUsername = regexp.MustCompile(`^[a-z][a-z0-9._-]{2,31}$`)

type SMSChallenge struct {
	ID        uuid.UUID  `json:"challenge_id"`
	Mobile    string     `json:"-"`
	Purpose   SMSPurpose `json:"-"`
	ExpiresAt time.Time  `json:"expires_at"`
}

type MobileAuthRepository interface {
	MobileExists(context.Context, string) (bool, error)
	MobileIdentitySubject(context.Context, string, string) (string, error)
	UsernameExists(context.Context, string) (bool, error)
	CreateSliderCaptcha(context.Context, SliderCaptcha) error
	ConsumeSliderCaptcha(context.Context, uuid.UUID, string, SMSPurpose, int) error
	CreateSMSChallenge(context.Context, SMSChallenge) error
	MarkSMSChallengeSent(context.Context, uuid.UUID, string) error
	MarkSMSChallengeFailed(context.Context, uuid.UUID) error
	SMSChallengeReady(context.Context, uuid.UUID, string, SMSPurpose) error
	ConsumeSMSChallenge(context.Context, uuid.UUID, string, SMSPurpose, bool) error
	PendingMobileRegistration(context.Context, string, string, string) (MobileRegistration, error)
	MobileRegistrationRecoverable(context.Context, string, string) (bool, error)
	PrepareMobileRegistration(context.Context, string, string, string) (MobileRegistration, error)
	CompleteMobileRegistration(context.Context, string, MobileRegistration, string, bool) error
}

type MobileRegistration struct {
	UserID   uuid.UUID
	Username string
	Mobile   string
}

type MobileRegistrationResult struct {
	UserID            uuid.UUID `json:"user_id"`
	Username          string    `json:"username"`
	Status            string    `json:"status"`
	PasswordPreserved bool      `json:"password_preserved"`
}

type SMSRateLimitError struct {
	RetryAfter time.Duration
}

func (e *SMSRateLimitError) Error() string { return ErrSMSRateLimited.Error() }
func (e *SMSRateLimitError) Unwrap() error { return ErrSMSRateLimited }

type MobileAuthService struct {
	repository MobileAuthRepository
	sms        SMSProvider
	identities IdentityProvisioner
	issuer     string
	now        func() time.Time
	commerce   bool
}

// Commerce synchronization is optional and does not gate local registration
// or wallet activation. Enable it only when the external adapter is configured.
func (s *MobileAuthService) WithCommerce(enabled bool) *MobileAuthService {
	s.commerce = enabled
	return s
}

func NewMobileAuthService(repository MobileAuthRepository, sms SMSProvider, identities IdentityProvisioner, issuer string) (*MobileAuthService, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if repository == nil || sms == nil || identities == nil || issuer == "" {
		return nil, errors.New("mobile authentication is not fully configured")
	}
	return &MobileAuthService{repository: repository, sms: sms, identities: identities, issuer: issuer, now: func() time.Time { return time.Now().UTC() }}, nil
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

func (s *MobileAuthService) PrecheckRegistration(ctx context.Context, rawUsername, rawMobile, password string) (string, error) {
	username, err := NormalizeRegistrationUsername(rawUsername)
	if err != nil {
		return "", err
	}
	mobile, err := NormalizeMainlandMobile(rawMobile)
	if err != nil {
		return "", err
	}
	if !validPassword(password) {
		return "", ErrPasswordInvalid
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

func (s *MobileAuthService) Send(ctx context.Context, rawUsername, rawMobile, password string, purpose SMSPurpose, captchaID uuid.UUID, captchaOffset int) (SMSChallenge, error) {
	mobile, err := NormalizeMainlandMobile(rawMobile)
	if err != nil || purpose != SMSPurposeRegister && purpose != SMSPurposeLogin {
		return SMSChallenge{}, ErrMobileInvalid
	}
	if purpose == SMSPurposeRegister {
		if _, err := s.PrecheckRegistration(ctx, rawUsername, mobile, password); err != nil {
			return SMSChallenge{}, err
		}
	}
	if captchaID == uuid.Nil || captchaOffset < 0 {
		return SMSChallenge{}, ErrCaptchaInvalid
	}
	if err := s.repository.ConsumeSliderCaptcha(ctx, captchaID, mobile, purpose, captchaOffset); err != nil {
		return SMSChallenge{}, err
	}
	exists, err := s.repository.MobileExists(ctx, mobile)
	if err != nil {
		return SMSChallenge{}, err
	}
	challenge := SMSChallenge{ID: uuid.Must(uuid.NewV7()), Mobile: mobile, Purpose: purpose, ExpiresAt: s.now().Add(5 * time.Minute)}
	if err := s.repository.CreateSMSChallenge(ctx, challenge); err != nil {
		return SMSChallenge{}, err
	}
	// Unknown login accounts receive the same API response without sending an
	// SMS, preventing the endpoint from becoming an account-enumeration oracle.
	if purpose == SMSPurposeLogin && !exists {
		return challenge, nil
	}
	messageID, err := s.sms.SendCode(ctx, mobile, purpose, challenge.ID)
	if err != nil {
		if errors.Is(err, ErrSMSRejected) {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			defer cancel()
			_ = s.repository.MarkSMSChallengeFailed(cleanupCtx, challenge.ID)
		}
		return SMSChallenge{}, fmt.Errorf("%w: %v", ErrSMSUnavailable, err)
	}
	if err := s.repository.MarkSMSChallengeSent(ctx, challenge.ID, messageID); err != nil {
		return SMSChallenge{}, err
	}
	return challenge, nil
}

func (s *MobileAuthService) Register(ctx context.Context, rawUsername, rawMobile string, challengeID uuid.UUID, code, password string) (MobileRegistrationResult, error) {
	username, err := NormalizeRegistrationUsername(rawUsername)
	if err != nil {
		return MobileRegistrationResult{}, err
	}
	mobile, err := NormalizeMainlandMobile(rawMobile)
	if err != nil {
		return MobileRegistrationResult{}, err
	}
	if !validPassword(password) {
		return MobileRegistrationResult{}, ErrPasswordInvalid
	}
	if _, err := s.PrecheckRegistration(ctx, username, mobile, password); err != nil {
		return MobileRegistrationResult{}, err
	}
	if err := s.verifyChallenge(ctx, challengeID, mobile, SMSPurposeRegister, code); err != nil {
		return MobileRegistrationResult{}, err
	}
	registration, err := s.repository.PrepareMobileRegistration(ctx, s.issuer, username, mobile)
	if err != nil {
		return MobileRegistrationResult{}, err
	}
	subject, preserved, err := s.identities.CreateMobileUser(ctx, registration, password)
	if err != nil {
		return MobileRegistrationResult{}, err
	}
	if err := s.repository.CompleteMobileRegistration(ctx, s.issuer, registration, subject, s.commerce); err != nil {
		return MobileRegistrationResult{}, err
	}
	return MobileRegistrationResult{UserID: registration.UserID, Username: registration.Username, Status: "PROVISIONING", PasswordPreserved: preserved}, nil
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
	if err := s.repository.SMSChallengeReady(ctx, challengeID, mobile, purpose); err != nil {
		return err
	}
	verified, err := s.sms.VerifyCode(ctx, mobile, challengeID, code)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSMSUnavailable, err)
	}
	return s.repository.ConsumeSMSChallenge(ctx, challengeID, mobile, purpose, verified)
}

func validPassword(password string) bool {
	if len(password) < 8 || len(password) > 128 {
		return false
	}
	var letter, digit bool
	for _, r := range password {
		if r < '!' || r > '~' {
			return false
		}
		letter = letter || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z'
		digit = digit || r >= '0' && r <= '9'
	}
	return letter && digit
}

func (r *PostgresRepository) MobileExists(ctx context.Context, mobile string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE mobile_e164=$1 AND status<>'DELETED')`, mobile).Scan(&exists)
	return exists, err
}

func (r *PostgresRepository) MobileIdentitySubject(ctx context.Context, issuer, mobile string) (string, error) {
	var subject string
	err := r.pool.QueryRow(ctx, `SELECT i.subject FROM users u JOIN identities i ON i.user_id=u.id WHERE u.mobile_e164=$1 AND u.status<>'DELETED' AND i.issuer=$2 ORDER BY i.last_seen_at DESC LIMIT 1`, mobile, issuer).Scan(&subject)
	return subject, err
}

func (r *PostgresRepository) UsernameExists(ctx context.Context, username string) (bool, error) {
	var exists bool
	// Account deletion retains the realm identity and its unique username.
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE lower(username)=lower($1))`, username).Scan(&exists)
	return exists, err
}

func (r *PostgresRepository) CreateSMSChallenge(ctx context.Context, challenge SMSChallenge) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, challenge.Mobile); err != nil {
		return err
	}
	now := r.now()
	var last *time.Time
	var oldest *time.Time
	var hourly int
	if err := tx.QueryRow(ctx, `SELECT max(created_at) FILTER (WHERE status<>'FAILED'),min(created_at) FILTER (WHERE created_at>$2 AND status<>'FAILED'),count(*) FILTER (WHERE created_at>$2 AND status<>'FAILED') FROM sms_challenges WHERE mobile_e164=$1`, challenge.Mobile, now.Add(-time.Hour)).Scan(&last, &oldest, &hourly); err != nil {
		return err
	}
	if err := smsRateLimit(now, last, oldest, hourly); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO sms_challenges(id,mobile_e164,purpose,code_hash,status,expires_at,created_at,updated_at) VALUES($1,$2,$3,NULL,'CREATED',$4,$5,$5)`, challenge.ID, challenge.Mobile, challenge.Purpose, challenge.ExpiresAt, now)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
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

func (r *PostgresRepository) MarkSMSChallengeSent(ctx context.Context, id uuid.UUID, messageID string) error {
	_, err := r.pool.Exec(ctx, `UPDATE sms_challenges SET status='SENT',provider_message_id=NULLIF($2,''),sent_at=$3,updated_at=$3 WHERE id=$1 AND status='CREATED'`, id, messageID, r.now())
	return err
}

func (r *PostgresRepository) MarkSMSChallengeFailed(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE sms_challenges SET status='FAILED',updated_at=$2 WHERE id=$1 AND status='CREATED'`, id, r.now())
	return err
}

func (r *PostgresRepository) SMSChallengeReady(ctx context.Context, id uuid.UUID, mobile string, purpose SMSPurpose) error {
	var status string
	var attempts int
	var expires time.Time
	err := r.pool.QueryRow(ctx, `SELECT status,attempts,expires_at FROM sms_challenges WHERE id=$1 AND mobile_e164=$2 AND purpose=$3`, id, mobile, purpose).Scan(&status, &attempts, &expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrSMSInvalid
	}
	if err != nil {
		return err
	}
	if status != "SENT" || attempts >= 5 || !expires.After(r.now()) {
		if status == "SENT" && !expires.After(r.now()) {
			_, _ = r.pool.Exec(ctx, `UPDATE sms_challenges SET status='EXPIRED',updated_at=$2 WHERE id=$1 AND status='SENT'`, id, r.now())
		}
		return ErrSMSInvalid
	}
	return nil
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
			if _, err := tx.Exec(ctx, `UPDATE sms_challenges SET status='EXPIRED',updated_at=$2 WHERE id=$1`, id, r.now()); err != nil {
				return err
			}
			if err := tx.Commit(ctx); err != nil {
				return err
			}
		}
		return ErrSMSInvalid
	}
	if !verified {
		attempts++
		if _, err := tx.Exec(ctx, `UPDATE sms_challenges SET attempts=$2,status=CASE WHEN $2>=5 THEN 'FAILED' ELSE status END,updated_at=$3 WHERE id=$1`, id, attempts, r.now()); err != nil {
			return err
		}
		if err := tx.Commit(ctx); err != nil {
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
