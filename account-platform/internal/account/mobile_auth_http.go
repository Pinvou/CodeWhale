package account

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

func (s *Service) sendSMSChallenge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mobile  string     `json:"mobile"`
		Purpose SMSPurpose `json:"purpose"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	challenge, err := s.mobileAuth.Send(r.Context(), body.Mobile, SMSPurpose(strings.ToUpper(string(body.Purpose))))
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
		Mobile      string    `json:"mobile"`
		ChallengeID uuid.UUID `json:"challenge_id"`
		Code        string    `json:"code"`
		Password    string    `json:"password"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.ChallengeID == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid mobile registration request", httpserver.RequestID(r.Context()))
		return
	}
	userID, err := s.mobileAuth.Register(r.Context(), body.Mobile, body.ChallengeID, body.Code, body.Password)
	if err != nil {
		s.writeMobileAuthError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusCreated, map[string]any{"user_id": userID, "status": "PROVISIONING"})
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
	if err := s.mobileAuth.VerifyLogin(r.Context(), body.Mobile, body.ChallengeID, body.Code); err != nil {
		api.WriteError(w, http.StatusUnauthorized, "SMS_CODE_INVALID", "the verification code is invalid or expired", httpserver.RequestID(r.Context()))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) writeMobileAuthError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := http.StatusInternalServerError, "AUTH_INTERNAL_ERROR", "the authentication request could not be completed"
	switch {
	case errors.Is(err, ErrMobileInvalid):
		status, code, message = http.StatusBadRequest, "MOBILE_INVALID", "a valid mainland China mobile number is required"
	case errors.Is(err, ErrMobileAlreadyRegistered):
		status, code, message = http.StatusConflict, "MOBILE_ALREADY_REGISTERED", "the mobile number is already registered"
	case errors.Is(err, ErrSMSRateLimited):
		status, code, message = http.StatusTooManyRequests, "SMS_RATE_LIMITED", "too many verification code requests"
		w.Header().Set("Retry-After", "60")
	case errors.Is(err, ErrSMSInvalid):
		status, code, message = http.StatusUnauthorized, "SMS_CODE_INVALID", "the verification code is invalid or expired"
	case errors.Is(err, ErrSMSUnavailable):
		status, code, message = http.StatusServiceUnavailable, "SMS_UNAVAILABLE", "the SMS service is temporarily unavailable"
	}
	api.WriteError(w, status, code, message, httpserver.RequestID(r.Context()))
}
