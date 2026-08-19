package account

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrMobileInvalid           = errors.New("mobile number is invalid")
	ErrMobileAlreadyRegistered = errors.New("mobile number is already registered")
	ErrSMSRateLimited          = errors.New("SMS requests are rate limited")
	ErrSMSInvalid              = errors.New("SMS challenge is invalid")
	ErrSMSUnavailable          = errors.New("SMS service is unavailable")
)

var mainlandMobile = regexp.MustCompile(`^1[3-9][0-9]{9}$`)

type SMSChallenge struct {
	ID        uuid.UUID  `json:"challenge_id"`
	Mobile    string     `json:"-"`
	Purpose   SMSPurpose `json:"-"`
	ExpiresAt time.Time  `json:"expires_at"`
}

type MobileAuthRepository interface {
	MobileExists(context.Context, string) (bool, error)
	CreateSMSChallenge(context.Context, SMSChallenge, []byte) error
	MarkSMSChallengeSent(context.Context, uuid.UUID, string) error
	MarkSMSChallengeFailed(context.Context, uuid.UUID) error
	ConsumeSMSChallenge(context.Context, uuid.UUID, string, SMSPurpose, []byte) error
	RegisterMobileIdentity(context.Context, string, string, string) (uuid.UUID, error)
}

type MobileAuthService struct {
	repository MobileAuthRepository
	sms        SMSProvider
	identities IdentityProvisioner
	issuer     string
	hashSecret []byte
	now        func() time.Time
	code       func() (string, error)
}

func NewMobileAuthService(repository MobileAuthRepository, sms SMSProvider, identities IdentityProvisioner, issuer, hashSecret string) (*MobileAuthService, error) {
	issuer = strings.TrimRight(strings.TrimSpace(issuer), "/")
	if repository == nil || sms == nil || identities == nil || issuer == "" || len(hashSecret) < 32 {
		return nil, errors.New("mobile authentication is not fully configured")
	}
	return &MobileAuthService{repository: repository, sms: sms, identities: identities, issuer: issuer, hashSecret: []byte(hashSecret), now: func() time.Time { return time.Now().UTC() }, code: generateSMSCode}, nil
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

func (s *MobileAuthService) Send(ctx context.Context, rawMobile string, purpose SMSPurpose) (SMSChallenge, error) {
	mobile, err := NormalizeMainlandMobile(rawMobile)
	if err != nil || purpose != SMSPurposeRegister && purpose != SMSPurposeLogin {
		return SMSChallenge{}, ErrMobileInvalid
	}
	exists, err := s.repository.MobileExists(ctx, mobile)
	if err != nil {
		return SMSChallenge{}, err
	}
	if purpose == SMSPurposeRegister && exists {
		return SMSChallenge{}, ErrMobileAlreadyRegistered
	}
	challenge := SMSChallenge{ID: uuid.Must(uuid.NewV7()), Mobile: mobile, Purpose: purpose, ExpiresAt: s.now().Add(5 * time.Minute)}
	code, err := s.code()
	if err != nil {
		return SMSChallenge{}, ErrSMSUnavailable
	}
	hash := s.codeHash(challenge, code)
	if err := s.repository.CreateSMSChallenge(ctx, challenge, hash); err != nil {
		return SMSChallenge{}, err
	}
	// Unknown login accounts receive the same API response without sending an
	// SMS, preventing the endpoint from becoming an account-enumeration oracle.
	if purpose == SMSPurposeLogin && !exists {
		return challenge, nil
	}
	messageID, err := s.sms.SendCode(ctx, mobile, purpose, code)
	if err != nil {
		_ = s.repository.MarkSMSChallengeFailed(context.WithoutCancel(ctx), challenge.ID)
		return SMSChallenge{}, ErrSMSUnavailable
	}
	if err := s.repository.MarkSMSChallengeSent(ctx, challenge.ID, messageID); err != nil {
		return SMSChallenge{}, err
	}
	return challenge, nil
}

func (s *MobileAuthService) Register(ctx context.Context, rawMobile string, challengeID uuid.UUID, code, password string) (uuid.UUID, error) {
	mobile, err := NormalizeMainlandMobile(rawMobile)
	if err != nil || !validPassword(password) {
		return uuid.Nil, ErrSMSInvalid
	}
	challenge := SMSChallenge{ID: challengeID, Mobile: mobile, Purpose: SMSPurposeRegister}
	if err := s.repository.ConsumeSMSChallenge(ctx, challengeID, mobile, SMSPurposeRegister, s.codeHash(challenge, strings.TrimSpace(code))); err != nil {
		return uuid.Nil, err
	}
	subject, err := s.identities.CreateMobileUser(ctx, mobile, password)
	if err != nil {
		return uuid.Nil, err
	}
	userID, err := s.repository.RegisterMobileIdentity(ctx, s.issuer, subject, mobile)
	if err != nil {
		_ = s.identities.DeleteUser(context.WithoutCancel(ctx), subject)
		return uuid.Nil, err
	}
	return userID, nil
}

func (s *MobileAuthService) VerifyLogin(ctx context.Context, rawMobile string, challengeID uuid.UUID, code string) error {
	mobile, err := NormalizeMainlandMobile(rawMobile)
	if err != nil {
		return ErrSMSInvalid
	}
	challenge := SMSChallenge{ID: challengeID, Mobile: mobile, Purpose: SMSPurposeLogin}
	return s.repository.ConsumeSMSChallenge(ctx, challengeID, mobile, SMSPurposeLogin, s.codeHash(challenge, strings.TrimSpace(code)))
}

func (s *MobileAuthService) codeHash(challenge SMSChallenge, code string) []byte {
	mac := hmac.New(sha256.New, s.hashSecret)
	_, _ = mac.Write([]byte(challenge.ID.String()))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(challenge.Mobile))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(challenge.Purpose))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(code))
	return mac.Sum(nil)
}

func generateSMSCode() (string, error) {
	var value [4]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", binary.BigEndian.Uint32(value[:])%1_000_000), nil
}

func validPassword(password string) bool {
	if len(password) < 8 || len(password) > 128 {
		return false
	}
	var letter, digit bool
	for _, r := range password {
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

func (r *PostgresRepository) CreateSMSChallenge(ctx context.Context, challenge SMSChallenge, hash []byte) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	lock := challenge.Mobile + ":" + string(challenge.Purpose)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lock); err != nil {
		return err
	}
	var last *time.Time
	var hourly int
	if err := tx.QueryRow(ctx, `SELECT max(created_at),count(*) FILTER (WHERE created_at>$3-interval '1 hour') FROM sms_challenges WHERE mobile_e164=$1 AND purpose=$2`, challenge.Mobile, challenge.Purpose, r.now()).Scan(&last, &hourly); err != nil {
		return err
	}
	if last != nil && r.now().Sub(*last) < time.Minute || hourly >= 5 {
		return ErrSMSRateLimited
	}
	now := r.now()
	_, err = tx.Exec(ctx, `INSERT INTO sms_challenges(id,mobile_e164,purpose,code_hash,status,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,'CREATED',$5,$6,$6)`, challenge.ID, challenge.Mobile, challenge.Purpose, hash, challenge.ExpiresAt, now)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) MarkSMSChallengeSent(ctx context.Context, id uuid.UUID, messageID string) error {
	_, err := r.pool.Exec(ctx, `UPDATE sms_challenges SET status='SENT',provider_message_id=NULLIF($2,''),sent_at=$3,updated_at=$3 WHERE id=$1 AND status='CREATED'`, id, messageID, r.now())
	return err
}

func (r *PostgresRepository) MarkSMSChallengeFailed(ctx context.Context, id uuid.UUID) error {
	_, err := r.pool.Exec(ctx, `UPDATE sms_challenges SET status='FAILED',updated_at=$2 WHERE id=$1 AND status='CREATED'`, id, r.now())
	return err
}

func (r *PostgresRepository) ConsumeSMSChallenge(ctx context.Context, id uuid.UUID, mobile string, purpose SMSPurpose, expected []byte) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var actual []byte
	var status string
	var attempts int
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT code_hash,status,attempts,expires_at FROM sms_challenges WHERE id=$1 AND mobile_e164=$2 AND purpose=$3 FOR UPDATE`, id, mobile, purpose).Scan(&actual, &status, &attempts, &expires)
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
	if len(actual) != len(expected) || subtle.ConstantTimeCompare(actual, expected) != 1 {
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

func (r *PostgresRepository) RegisterMobileIdentity(ctx context.Context, issuer, subject, mobile string) (uuid.UUID, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)
	now, userID := r.now(), uuid.Must(uuid.NewV7())
	masked := strings.TrimPrefix(mobile, "+86")
	display := masked[:3] + "****" + masked[7:]
	_, err = tx.Exec(ctx, `INSERT INTO users(id,status,display_name,mobile_e164,mobile_verified_at,created_at,updated_at) VALUES($1,'PROVISIONING',$2,$3,$4,$4,$4)`, userID, display, mobile, now)
	if err != nil {
		if strings.Contains(err.Error(), "users_mobile_e164_unique_idx") {
			return uuid.Nil, ErrMobileAlreadyRegistered
		}
		return uuid.Nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO identities(issuer,subject,user_id,created_at,last_seen_at) VALUES($1,$2,$3,$4,$4)`, issuer, subject, userID, now); err != nil {
		return uuid.Nil, err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO mall_user_sync_jobs(user_id,mobile_e164,status,next_attempt_at,created_at,updated_at) VALUES($1,$2,'PENDING',$3,$3,$3)`, userID, mobile, now); err != nil {
		return uuid.Nil, err
	}
	if err := appendAudit(ctx, tx, userID, "registration", nil, "account.mobile.register", "user", userID.String(), "SUCCEEDED", map[string]any{"mobile_verified": true}); err != nil {
		return uuid.Nil, err
	}
	return userID, tx.Commit(ctx)
}
