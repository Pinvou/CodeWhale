package account

import (
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

func (s *Service) precheckMobileRegistration(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username              string `json:"username"`
		Mobile                string `json:"mobile"`
		PasswordHandle        string `json:"password_handle"`
		PasswordPolicyVersion string `json:"password_policy_version"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	username, err := s.mobileAuth.PrecheckRegistration(r.Context(), body.Username, body.Mobile, body.PasswordHandle, body.PasswordPolicyVersion)
	if err != nil {
		s.writeMobileAuthError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"username": username, "available": true})
}

func (s *Service) createSliderCaptcha(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mobile  string     `json:"mobile"`
		Purpose SMSPurpose `json:"purpose"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	challenge, err := s.mobileAuth.CreateSliderCaptcha(r.Context(), body.Mobile, SMSPurpose(strings.ToUpper(string(body.Purpose))))
	if err != nil {
		s.writeMobileAuthError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, challenge)
}

func (s *Service) sendSMSChallenge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username              string     `json:"username"`
		Mobile                string     `json:"mobile"`
		PasswordHandle        string     `json:"password_handle"`
		PasswordPolicyVersion string     `json:"password_policy_version"`
		Purpose               SMSPurpose `json:"purpose"`
		CaptchaID             uuid.UUID  `json:"captcha_id"`
		CaptchaOffset         int        `json:"captcha_offset"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	challenge, err := s.mobileAuth.Send(r.Context(), body.Username, body.Mobile, body.PasswordHandle, body.PasswordPolicyVersion, SMSPurpose(strings.ToUpper(string(body.Purpose))), body.CaptchaID, body.CaptchaOffset, requestSource(r), r.Header.Get("X-Device-ID"))
	if err != nil {
		s.writeMobileAuthError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusAccepted, map[string]any{
		"challenge_id":        challenge.ID,
		"expires_at":          challenge.ExpiresAt,
		"retry_after_seconds": 60,
	})
}

func (s *Service) registerMobile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username              string    `json:"username"`
		Mobile                string    `json:"mobile"`
		ChallengeID           uuid.UUID `json:"challenge_id"`
		Code                  string    `json:"code"`
		PasswordHandle        string    `json:"password_handle"`
		PasswordPolicyVersion string    `json:"password_policy_version"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.ChallengeID == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid mobile registration request", httpserver.RequestID(r.Context()))
		return
	}
	userID, userCode, err := s.mobileAuth.Register(r.Context(), body.Username, body.Mobile, body.ChallengeID, body.Code, body.PasswordHandle, body.PasswordPolicyVersion)
	if err != nil {
		s.writeMobileAuthError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	username, _ := NormalizeRegistrationUsername(body.Username)
	api.WriteJSON(w, http.StatusCreated, map[string]any{"user_id": userID, "user_code": userCode, "username": username, "status": "PROVISIONING"})
}

func (s *Service) verifyLoginSMS(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mobile      string    `json:"mobile"`
		ChallengeID uuid.UUID `json:"challenge_id"`
		Code        string    `json:"code"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.ChallengeID == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid SMS login verification request", httpserver.RequestID(r.Context()))
		return
	}
	subject, err := s.mobileAuth.VerifyLogin(r.Context(), body.Mobile, body.ChallengeID, body.Code)
	if err != nil {
		api.WriteError(w, http.StatusUnauthorized, "SMS_CODE_INVALID", "the verification code is invalid or expired", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("X-Pinvou-Identity-Subject", subject)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) writeMobileAuthError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := http.StatusInternalServerError, "AUTH_INTERNAL_ERROR", "the authentication request could not be completed"
	switch {
	case errors.Is(err, ErrMobileInvalid):
		status, code, message = http.StatusBadRequest, "MOBILE_INVALID", "a valid mainland China mobile number is required"
	case errors.Is(err, ErrMobileAlreadyRegistered):
		status, code, message = http.StatusConflict, "MOBILE_ALREADY_REGISTERED", "the mobile number is already registered"
	case errors.Is(err, ErrUsernameInvalid):
		status, code, message = http.StatusBadRequest, "USERNAME_INVALID", "username must start with a letter and contain 3 to 32 letters, digits, dots, underscores, or hyphens"
	case errors.Is(err, ErrUsernameAlreadyRegistered):
		status, code, message = http.StatusConflict, "USERNAME_ALREADY_REGISTERED", "the username is already registered"
	case errors.Is(err, ErrPasswordReferenceInvalid):
		status, code, message = http.StatusGone, "PASSWORD_REFERENCE_INVALID", "the password precheck has expired; restart registration"
	case errors.Is(err, ErrPasswordReferenceConflict):
		status, code, message = http.StatusConflict, "PASSWORD_REFERENCE_CONFLICT", "the password precheck belongs to different registration data"
	case errors.Is(err, ErrCaptchaInvalid):
		status, code, message = http.StatusBadRequest, "CAPTCHA_INVALID", "the slider verification is invalid or expired"
	case errors.Is(err, ErrSMSRateLimited):
		status, code, message = http.StatusTooManyRequests, "SMS_RATE_LIMITED", "too many verification code requests"
		retryAfter := 60
		var limited *SMSRateLimitError
		if errors.As(err, &limited) {
			retryAfter = max(1, int(math.Ceil(limited.RetryAfter.Seconds())))
		}
		w.Header().Set("Retry-After", fmt.Sprint(retryAfter))
		api.WriteJSON(w, status, api.Error{Code: code, Message: message, RequestID: httpserver.RequestID(r.Context()), Details: map[string]any{"retry_after_seconds": retryAfter}})
		return
	case errors.Is(err, ErrSMSInvalid):
		status, code, message = http.StatusUnauthorized, "SMS_CODE_INVALID", "the verification code is invalid or expired"
	case errors.Is(err, ErrSMSUnavailable):
		status, code, message = http.StatusServiceUnavailable, "SMS_UNAVAILABLE", "the SMS service is temporarily unavailable"
	}
	if status >= http.StatusInternalServerError {
		slog.ErrorContext(r.Context(), "mobile authentication request failed", "error", err, "request_id", httpserver.RequestID(r.Context()))
	}
	api.WriteError(w, status, code, message, httpserver.RequestID(r.Context()))
}
