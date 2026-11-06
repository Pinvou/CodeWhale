package account

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

type updateSMSConfigurationRequest struct {
	ExpectedRevision  int64               `json:"expected_revision"`
	Enabled           bool                `json:"enabled"`
	ProviderID        string              `json:"provider_id"`
	CredentialProfile string              `json:"credential_profile"`
	SignName          string              `json:"sign_name"`
	Templates         SMSPurposeTemplates `json:"templates"`
	Reason            string              `json:"reason"`
}

func (s *Service) adminSMSConfiguration(w http.ResponseWriter, r *http.Request) {
	if s.smsConfiguration == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "SMS_CONFIGURATION_UNAVAILABLE", "SMS configuration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	projection, err := s.smsConfiguration.Projection(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, projection)
}

func (s *Service) adminUpdateSMSConfiguration(w http.ResponseWriter, r *http.Request) {
	if s.smsConfiguration == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "SMS_CONFIGURATION_UNAVAILABLE", "SMS configuration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var body updateSMSConfigurationRequest
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid SMS configuration request", httpserver.RequestID(r.Context()))
		return
	}
	candidate, err := s.smsConfiguration.ValidateCandidate(SMSConfiguration{
		Enabled:           body.Enabled,
		ProviderID:        body.ProviderID,
		AdapterVersion:    "v1",
		CredentialProfile: body.CredentialProfile,
		SignName:          body.SignName,
		Templates:         body.Templates,
	})
	if err != nil {
		s.writeSMSConfigurationError(w, r, err)
		return
	}
	updated, replayed, err := s.smsConfiguration.store.UpdateSMSConfiguration(
		r.Context(), mustPrincipal(r.Context()), candidate, body.ExpectedRevision, body.Reason, strings.TrimSpace(r.Header.Get("Idempotency-Key")),
	)
	if err != nil {
		s.writeSMSConfigurationError(w, r, err)
		return
	}
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
	projection := s.smsConfiguration.projectionFor(updated)
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, projection)
}

func (s *Service) writeSMSConfigurationError(w http.ResponseWriter, r *http.Request, err error) {
	requestID := httpserver.RequestID(r.Context())
	switch {
	case errors.Is(err, ErrSMSConfigurationInvalid):
		api.WriteError(w, http.StatusBadRequest, "SMS_CONFIGURATION_INVALID", "SMS configuration is invalid", requestID)
	case errors.Is(err, ErrSMSConfigurationConflict):
		api.WriteError(w, http.StatusConflict, "SMS_CONFIGURATION_CONFLICT", "SMS configuration changed; reload and retry", requestID)
	case errors.Is(err, ErrSMSConfigurationKeyReused):
		api.WriteError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED", "idempotency key was already used for another request", requestID)
	case errors.Is(err, ErrSMSProviderProfileUnavailable):
		api.WriteError(w, http.StatusUnprocessableEntity, "SMS_PROVIDER_PROFILE_UNAVAILABLE", "SMS provider profile is not configured by deployment", requestID)
	default:
		s.writeError(w, r, err)
	}
}
