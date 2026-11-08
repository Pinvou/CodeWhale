package account

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/passwordpreparation"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrPasswordPreparation = errors.New("password preparation authorization is invalid or unavailable")

type passwordPreparationRepository interface {
	IssuePasswordPreparation(context.Context, uuid.UUID, uuid.UUID, string, SMSPurpose, int, string, string) (time.Time, error)
	ClaimPasswordPreparation(context.Context, uuid.UUID, uuid.UUID, string, string) (passwordpreparation.Facts, error)
	ConsumePasswordPreparationSMS(context.Context, uuid.UUID, string, SMSPurpose, string, string) error
}

func (s *RegistrationTokenSigner) preparationToken(id uuid.UUID) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("pinvou:password-preparation:v1:" + id.String()))
	return id.String() + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (s *RegistrationTokenSigner) preparationID(token string) (uuid.UUID, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return uuid.Nil, ErrPasswordPreparation
	}
	id, err := uuid.Parse(parts[0])
	if err != nil || id == uuid.Nil || !hmac.Equal([]byte(token), []byte(s.preparationToken(id))) {
		return uuid.Nil, ErrPasswordPreparation
	}
	return id, nil
}

func preparationDigest(value string) []byte { digest := sha256.Sum256([]byte(value)); return digest[:] }

func (r *PostgresRepository) IssuePasswordPreparation(ctx context.Context, id, captcha uuid.UUID, mobile string, purpose SMSPurpose, offset int, source, device string) (time.Time, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return time.Time{}, err
	}
	defer tx.Rollback(ctx)
	// Serialize identical operations before the captcha lock, including an ACK-lost retry.
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, "password-preparation:"+id.String()); err != nil {
		return time.Time{}, err
	}
	raw, _ := json.Marshal([]any{captcha, mobile, purpose, offset, source, device})
	digest := preparationDigest(string(raw))
	var previous []byte
	var expires time.Time
	err = tx.QueryRow(ctx, `SELECT request_digest,expires_at FROM password_preparation_authorizations WHERE id=$1 FOR UPDATE`, id).Scan(&previous, &expires)
	if err == nil {
		if !hmac.Equal(previous, digest) {
			return time.Time{}, ErrRegistrationConflict
		}
		return expires, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, err
	}
	if err = r.consumeSliderCaptchaTx(ctx, tx, captcha, mobile, purpose, offset); err != nil {
		if errors.Is(err, ErrCaptchaInvalid) {
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return time.Time{}, commitErr
			}
		}
		return time.Time{}, err
	}
	if err = tx.QueryRow(ctx, `SELECT expires_at FROM slider_captcha_challenges WHERE id=$1`, captcha).Scan(&expires); err != nil {
		return time.Time{}, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO password_preparation_authorizations(id,captcha_id,mobile_e164,purpose,source_digest,device_digest,request_digest,expires_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, id, captcha, mobile, purpose, preparationDigest(source), preparationDigest(device), digest, expires)
	if err != nil {
		return time.Time{}, err
	}
	return expires, tx.Commit(ctx)
}

func (r *PostgresRepository) ClaimPasswordPreparation(ctx context.Context, id, owner uuid.UUID, source, device string) (passwordpreparation.Facts, error) {
	var result passwordpreparation.Facts
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	var savedOwner *uuid.UUID
	var sourceDigest, deviceDigest []byte
	var now time.Time
	err = tx.QueryRow(ctx, `SELECT id,mobile_e164,purpose,expires_at,identity_incarnation,source_digest,device_digest FROM password_preparation_authorizations WHERE id=$1 FOR UPDATE`, id).Scan(&result.ID, &result.Mobile, &result.Purpose, &result.ExpiresAt, &savedOwner, &sourceDigest, &deviceDigest)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrPasswordPreparation
	}
	if err != nil {
		return result, err
	}
	if owner == uuid.Nil || !hmac.Equal(sourceDigest, preparationDigest(source)) || !hmac.Equal(deviceDigest, preparationDigest(device)) || savedOwner != nil && *savedOwner != owner {
		return result, ErrPasswordPreparation
	}
	if savedOwner == nil {
		if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
			return result, err
		}
		if !result.ExpiresAt.After(now) {
			return result, ErrPasswordPreparation
		}
		if _, err = tx.Exec(ctx, `UPDATE password_preparation_authorizations SET identity_incarnation=$2 WHERE id=$1`, id, owner); err != nil {
			return result, err
		}
	}
	// A claimed grant is never transferred or renewed. Identity may only replay its existing record after expiry.
	return result, tx.Commit(ctx)
}

func (r *PostgresRepository) ConsumePasswordPreparationSMS(ctx context.Context, id uuid.UUID, mobile string, purpose SMSPurpose, source, device string) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var expires, now time.Time
	var consumed bool
	err = tx.QueryRow(ctx, `SELECT expires_at,sms_consumed_at IS NOT NULL FROM password_preparation_authorizations WHERE id=$1 AND mobile_e164=$2 AND purpose=$3 AND source_digest=$4 AND device_digest=$5 FOR UPDATE`, id, mobile, purpose, preparationDigest(source), preparationDigest(device)).Scan(&expires, &consumed)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPasswordPreparation
	}
	if err != nil {
		return err
	}
	// A lock wait never extends the capability. Read wall time only after acquiring it.
	if err = tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	if consumed || !expires.After(now) {
		return ErrPasswordPreparation
	}
	if _, err = tx.Exec(ctx, `UPDATE password_preparation_authorizations SET sms_consumed_at=$2 WHERE id=$1`, id, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) createPasswordPreparation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OperationID uuid.UUID  `json:"operation_id"`
		CaptchaID   uuid.UUID  `json:"captcha_id"`
		Offset      int        `json:"captcha_offset"`
		Mobile      string     `json:"mobile"`
		Purpose     SMSPurpose `json:"purpose"`
	}
	if api.DecodeJSON(w, r, &body) != nil || body.OperationID == uuid.Nil || body.CaptchaID == uuid.Nil || body.Offset < 0 || body.Purpose != SMSPurposeRegister && body.Purpose != SMSPurposePasswordReset {
		s.writeMobileAuthError(w, r, ErrCaptchaInvalid)
		return
	}
	mobile, err := NormalizeMainlandMobile(body.Mobile)
	if err != nil {
		s.writeMobileAuthError(w, r, err)
		return
	}
	repository, ok := s.mobileAuth.repository.(passwordPreparationRepository)
	if !ok {
		s.writeMobileAuthError(w, r, ErrSMSUnavailable)
		return
	}
	risk := authRiskFromContext(withAuthRisk(r.Context(), requestSource(r), r.Header.Get("X-Device-ID")))
	expires, err := repository.IssuePasswordPreparation(r.Context(), body.OperationID, body.CaptchaID, mobile, body.Purpose, body.Offset, risk.source, risk.device)
	if err != nil {
		s.writeMobileAuthError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusCreated, passwordpreparation.Authorization{ID: body.OperationID, Token: s.mobileAuth.tokens.preparationToken(body.OperationID), ExpiresAt: expires})
}

func (s *Service) claimPasswordPreparation(w http.ResponseWriter, r *http.Request) {
	var body passwordpreparation.Claim
	if api.DecodeJSON(w, r, &body) != nil {
		s.writeMobileAuthError(w, r, ErrPasswordPreparation)
		return
	}
	id, err := s.mobileAuth.tokens.preparationID(body.Token)
	if err != nil {
		s.writeMobileAuthError(w, r, err)
		return
	}
	repository, ok := s.mobileAuth.repository.(passwordPreparationRepository)
	if !ok {
		s.writeMobileAuthError(w, r, ErrSMSUnavailable)
		return
	}
	risk := authRiskFromContext(withAuthRisk(r.Context(), body.Source, body.Device))
	facts, err := repository.ClaimPasswordPreparation(r.Context(), id, body.Owner, risk.source, risk.device)
	if err != nil {
		s.writeMobileAuthError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, facts)
}

type passwordPreparationContextKey struct{}

func withPasswordPreparation(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, passwordPreparationContextKey{}, token)
}

func (s *MobileAuthService) consumePasswordPreparation(ctx context.Context, captcha uuid.UUID, mobile string, purpose SMSPurpose, offset int, source, device string) error {
	token, _ := ctx.Value(passwordPreparationContextKey{}).(string)
	if token == "" {
		return s.repository.ConsumeSliderCaptcha(ctx, captcha, mobile, purpose, offset)
	}
	if purpose != SMSPurposeRegister && purpose != SMSPurposePasswordReset {
		return ErrPasswordPreparation
	}
	id, err := s.tokens.preparationID(token)
	if err != nil {
		return err
	}
	repository, ok := s.repository.(passwordPreparationRepository)
	if !ok {
		return ErrSMSUnavailable
	}
	risk := authRiskFromContext(withAuthRisk(ctx, source, device))
	return repository.ConsumePasswordPreparationSMS(ctx, id, mobile, purpose, risk.source, risk.device)
}

type passwordReferencePreparationReader interface {
	PasswordReferencePreparation(context.Context, string, string) (passwordpreparation.Reference, error)
}

func (p *HTTPIdentityProvisioner) PasswordReferencePreparation(ctx context.Context, handle, policy string) (passwordpreparation.Reference, error) {
	var facts passwordpreparation.Reference
	err := p.request(ctx, http.MethodPost, "/internal/v1/registration/password-references/preparation", map[string]string{"password_handle": handle, "password_policy_version": policy}, &facts)
	return facts, err
}

func (s *MobileAuthService) validatePreparationReference(ctx context.Context, handle, policy, mobile string, purpose SMSPurpose) error {
	token, _ := ctx.Value(passwordPreparationContextKey{}).(string)
	if token == "" {
		return nil
	} // Original accepted references still use a newly consumed captcha.
	reader, ok := s.identities.(passwordReferencePreparationReader)
	if !ok {
		return ErrSMSUnavailable
	}
	facts, err := reader.PasswordReferencePreparation(ctx, handle, policy)
	if err != nil {
		return err
	}
	if facts.Legacy {
		if facts.ID != uuid.Nil || facts.Mobile != "" || facts.Purpose != "" {
			return ErrPasswordReferenceInvalid
		}
	} else if facts.ID == uuid.Nil || facts.Mobile != mobile || facts.Purpose != string(purpose) {
		return ErrPasswordReferenceConflict
	}
	return nil
}
