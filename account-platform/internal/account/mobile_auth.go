package account

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"sort"
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
	ErrPasswordReferenceInvalid  = errors.New("password reference is invalid or expired")
	ErrPasswordReferenceConflict = errors.New("password reference conflicts with registration data")
	ErrCaptchaInvalid            = errors.New("slider captcha is invalid")
	ErrSMSRateLimited            = errors.New("SMS requests are rate limited")
	ErrSMSInvalid                = errors.New("SMS challenge is invalid")
	ErrSMSUnavailable            = errors.New("SMS service is unavailable")
)

var mainlandMobile = regexp.MustCompile(`^1[3-9][0-9]{9}$`)
var registrationUsername = regexp.MustCompile(`^[a-z][a-z0-9._-]{2,31}$`)

type SMSChallenge struct {
	ID        uuid.UUID             `json:"challenge_id"`
	Mobile    string                `json:"-"`
	Purpose   SMSPurpose            `json:"-"`
	ExpiresAt time.Time             `json:"expires_at"`
	Source    string                `json:"-"`
	Device    string                `json:"-"`
	Provider  SMSProviderDescriptor `json:"-"`
}

type MobileAuthRepository interface {
	MobileExists(context.Context, string) (bool, error)
	MobileIdentitySubject(context.Context, string, string) (string, error)
	UsernameExists(context.Context, string) (bool, error)
	CreateSliderCaptcha(context.Context, SliderCaptcha) error
	ConsumeSliderCaptcha(context.Context, uuid.UUID, string, SMSPurpose, int) error
	CreateSMSChallenge(context.Context, SMSChallenge) error
	MarkSMSChallengeSent(context.Context, uuid.UUID, string, []byte) error
	MarkSMSChallengeFailed(context.Context, uuid.UUID) error
	SMSChallengeReady(context.Context, uuid.UUID, string, SMSPurpose) (SMSVerificationData, error)
	ConsumeSMSChallenge(context.Context, uuid.UUID, string, SMSPurpose, bool) error
	RegisterMobileIdentity(context.Context, string, string, string, string) (uuid.UUID, string, error)
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
}

func NewMobileAuthService(repository MobileAuthRepository, sms SMSProvider, identities IdentityProvisioner, issuer string) (*MobileAuthService, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if repository == nil || sms == nil || identities == nil || issuer == "" {
		return nil, errors.New("mobile authentication is not fully configured")
	}
	return &MobileAuthService{repository: repository, sms: sms, identities: identities, issuer: issuer, now: func() time.Time { return time.Now().UTC() }, loginResponseFloor: 750 * time.Millisecond}, nil
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
	if err := s.identities.ValidateMobileUser(ctx, username, mobile, passwordHandle, passwordPolicyVersion); err != nil {
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
	if err != nil || purpose != SMSPurposeRegister && purpose != SMSPurposeLogin {
		return SMSChallenge{}, ErrMobileInvalid
	}
	if captchaID == uuid.Nil || captchaOffset < 0 {
		return SMSChallenge{}, ErrCaptchaInvalid
	}
	if err := s.repository.ConsumeSliderCaptcha(ctx, captchaID, mobile, purpose, captchaOffset); err != nil {
		return SMSChallenge{}, err
	}
	if purpose == SMSPurposeRegister {
		if _, err := s.PrecheckRegistration(ctx, rawUsername, mobile, passwordHandle, passwordPolicyVersion); err != nil {
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
	challenge := SMSChallenge{ID: uuid.Must(uuid.NewV7()), Mobile: mobile, Purpose: purpose, ExpiresAt: s.now().Add(5 * time.Minute), Source: strings.TrimSpace(source), Device: strings.TrimSpace(device), Provider: s.sms.Descriptor()}
	if err := s.repository.CreateSMSChallenge(ctx, challenge); err != nil {
		return SMSChallenge{}, err
	}
	// Login delivery is deliberately detached from the external response. Known
	// and unknown mobiles therefore share the same status and minimum timing
	// bucket even when the provider is slow or unavailable.
	if purpose == SMSPurposeLogin {
		if exists {
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

func (s *MobileAuthService) deliverSMS(ctx context.Context, challenge SMSChallenge) error {
	delivery, err := s.sms.SendCode(ctx, challenge.Mobile, challenge.Purpose, challenge.ID)
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

func (s *MobileAuthService) Register(ctx context.Context, rawUsername, rawMobile string, challengeID uuid.UUID, code, passwordHandle, passwordPolicyVersion string) (uuid.UUID, string, error) {
	username, err := NormalizeRegistrationUsername(rawUsername)
	if err != nil {
		return uuid.Nil, "", err
	}
	mobile, err := NormalizeMainlandMobile(rawMobile)
	if err != nil {
		return uuid.Nil, "", err
	}
	exists, err := s.repository.UsernameExists(ctx, username)
	if err != nil {
		return uuid.Nil, "", err
	}
	if exists {
		return uuid.Nil, "", ErrUsernameAlreadyRegistered
	}
	if err := s.verifyChallenge(ctx, challengeID, mobile, SMSPurposeRegister, code); err != nil {
		return uuid.Nil, "", err
	}
	subject, err := s.identities.CreateMobileUser(ctx, username, mobile, passwordHandle, passwordPolicyVersion)
	if err != nil {
		return uuid.Nil, "", err
	}
	userID, userCode, err := s.repository.RegisterMobileIdentity(ctx, s.issuer, subject, username, mobile)
	if err != nil {
		_ = s.identities.DeleteUser(context.WithoutCancel(ctx), subject)
		return uuid.Nil, "", err
	}
	return userID, userCode, nil
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
		return fmt.Errorf("%w: %v", ErrSMSUnavailable, err)
	}
	return s.repository.ConsumeSMSChallenge(ctx, challengeID, mobile, purpose, verified)
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
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE lower(username)=lower($1) AND status<>'DELETED')`, username).Scan(&exists)
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
count(*) FILTER (WHERE source_hash=$1 AND occurred_at>$3-interval '1 hour'),
count(*) FILTER (WHERE source_hash=$1 AND occurred_at>$3-interval '1 day'),
count(*) FILTER (WHERE device_hash=$2 AND occurred_at>$3-interval '1 hour')
FROM sms_send_attempts WHERE occurred_at>$3-interval '1 day'`, sourceHash[:], deviceHash[:], now).Scan(&sourceHour, &sourceDay, &deviceHour); err != nil {
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
	_, err := r.pool.Exec(ctx, `UPDATE sms_challenges SET status='FAILED',updated_at=$2 WHERE id=$1 AND status='CREATED'`, id, r.now())
	return err
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
			_, _ = tx.Exec(ctx, `UPDATE sms_challenges SET status='EXPIRED',updated_at=$2 WHERE id=$1`, id, r.now())
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
	if _, err = tx.Exec(ctx, `INSERT INTO mall_user_sync_jobs(user_id,mobile_e164,status,next_attempt_at,created_at,updated_at) VALUES($1,$2,'PENDING',$3,$3,$3)`, userID, mobile, now); err != nil {
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
