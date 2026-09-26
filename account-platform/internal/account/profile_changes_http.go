package account

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

func (s *Service) createProfileChange(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Field string `json:"field"`
		Value string `json:"value"`
	}
	if api.DecodeJSON(w, r, &body) != nil {
		return
	}
	change, err := s.profiles.Create(r.Context(), mustPrincipal(r.Context()), body.Field, body.Value)
	if err != nil {
		s.writeProfileError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusCreated, change)
}

func (s *Service) sendProfileMobileChallenge(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("request_id"))
	if err != nil {
		s.writeProfileError(w, r, ErrNotFound)
		return
	}
	var body struct {
		CaptchaID     uuid.UUID `json:"captcha_id"`
		CaptchaOffset int       `json:"captcha_offset"`
	}
	if api.DecodeJSON(w, r, &body) != nil {
		return
	}
	challenge, err := s.profiles.SendMobileCode(r.Context(), mustPrincipal(r.Context()), id, body.CaptchaID, body.CaptchaOffset, requestSource(r), r.Header.Get("X-Device-ID"))
	if err != nil {
		s.writeProfileError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusAccepted, map[string]any{"challenge_id": challenge.ID, "delivery_status": challenge.Status, "expires_at": challenge.ExpiresAt, "retry_after_seconds": 60})
}

func (s *Service) completeProfileChange(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("request_id"))
	if err != nil {
		s.writeProfileError(w, r, ErrNotFound)
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if api.DecodeJSON(w, r, &body) != nil {
		return
	}
	ctx := withAuthRisk(r.Context(), requestSource(r), r.Header.Get("X-Device-ID"))
	principal := mustPrincipal(r.Context())
	change, err := s.profiles.Complete(ctx, principal, id, body.Code)
	if err != nil {
		if recovering, loadErr := s.profiles.store.ProfileChange(r.Context(), id, nil); loadErr == nil &&
			recovering.IdentityIssuer == principal.Issuer && recovering.IdentitySubject == principal.Subject &&
			(recovering.Status == "RETRY" || recovering.Status == "REPAIR_REQUIRED") {
			w.Header().Set("Cache-Control", "no-store")
			api.WriteJSON(w, http.StatusAccepted, recovering)
			return
		}
		s.writeProfileError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, change)
}

func (s *Service) getProfileChange(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("request_id"))
	if err != nil {
		s.writeProfileError(w, r, ErrNotFound)
		return
	}
	change, err := s.profiles.store.ProfileChange(r.Context(), id, nil)
	principal := mustPrincipal(r.Context())
	if err != nil || change.IdentityIssuer != principal.Issuer || change.IdentitySubject != principal.Subject {
		s.writeProfileError(w, r, ErrNotFound)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, change)
}

func (s *Service) resetForgottenPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mobile                string    `json:"mobile"`
		ChallengeID           uuid.UUID `json:"challenge_id"`
		Code                  string    `json:"code"`
		PasswordHandle        string    `json:"password_handle"`
		PasswordPolicyVersion string    `json:"password_policy_version"`
	}
	if api.DecodeJSON(w, r, &body) != nil || body.ChallengeID == uuid.Nil {
		return
	}
	ctx := withAuthRisk(r.Context(), requestSource(r), r.Header.Get("X-Device-ID"))
	if err := s.profiles.ResetPassword(ctx, body.Mobile, body.ChallengeID, body.Code, body.PasswordHandle, body.PasswordPolicyVersion); err != nil {
		s.writeProfileError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) changePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PasswordHandle        string `json:"password_handle"`
		PasswordPolicyVersion string `json:"password_policy_version"`
	}
	if api.DecodeJSON(w, r, &body) != nil {
		return
	}
	if err := s.profiles.ChangePassword(r.Context(), mustPrincipal(r.Context()), body.PasswordHandle, body.PasswordPolicyVersion); err != nil {
		s.writeProfileError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) writeProfileError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := http.StatusInternalServerError, "PROFILE_CHANGE_FAILED", "the profile change could not be completed"
	switch {
	case errors.Is(err, ErrRecentAuthenticationRequired):
		status, code, message = http.StatusUnauthorized, "RECENT_AUTHENTICATION_REQUIRED", "sign in again before changing profile details"
	case errors.Is(err, ErrUsernameInvalid):
		status, code, message = http.StatusBadRequest, "USERNAME_INVALID", "the username is invalid"
	case errors.Is(err, ErrUsernameAlreadyRegistered):
		status, code, message = http.StatusConflict, "USERNAME_ALREADY_REGISTERED", "the username is already registered"
	case errors.Is(err, ErrMobileInvalid):
		status, code, message = http.StatusBadRequest, "MOBILE_INVALID", "the mobile number is invalid"
	case errors.Is(err, ErrMobileAlreadyRegistered):
		status, code, message = http.StatusConflict, "MOBILE_ALREADY_REGISTERED", "the mobile number is already registered"
	case errors.Is(err, ErrCaptchaInvalid):
		status, code, message = http.StatusBadRequest, "CAPTCHA_INVALID", "the slider verification is invalid or expired"
	case errors.Is(err, ErrProfileChangeConflict), errors.Is(err, ErrConflict):
		status, code, message = http.StatusConflict, "PROFILE_CHANGE_CONFLICT", "another profile change or account update conflicts with this request"
	case errors.Is(err, ErrProfileChangeVerificationRequired):
		status, code, message = http.StatusBadRequest, "MOBILE_VERIFICATION_REQUIRED", "verify the new mobile number first"
	case errors.Is(err, ErrSMSInvalid):
		status, code, message = http.StatusUnauthorized, "SMS_CODE_INVALID", "the verification code is invalid or expired"
	case errors.Is(err, ErrSMSRateLimited):
		status, code, message = http.StatusTooManyRequests, "SMS_RATE_LIMITED", "too many verification code attempts"
		retryAfter := 60
		var limited *SMSRateLimitError
		if errors.As(err, &limited) {
			retryAfter = max(1, int(math.Ceil(limited.RetryAfter.Seconds())))
		}
		w.Header().Set("Retry-After", fmt.Sprint(retryAfter))
	case errors.Is(err, ErrSMSUnavailable):
		status, code, message = http.StatusServiceUnavailable, "SMS_UNAVAILABLE", "the SMS service is temporarily unavailable"
	case errors.Is(err, ErrPasswordReferenceInvalid):
		status, code, message = http.StatusGone, "PASSWORD_REFERENCE_INVALID", "the password precheck has expired"
	case errors.Is(err, ErrForbidden):
		status, code, message = http.StatusForbidden, "PERMISSION_DENIED", "the requested permission is not granted"
	case errors.Is(err, ErrNotFound):
		status, code, message = http.StatusNotFound, "PROFILE_CHANGE_NOT_FOUND", "the profile change was not found"
	}
	api.WriteError(w, status, code, strings.TrimSpace(message), httpserver.RequestID(r.Context()))
}
