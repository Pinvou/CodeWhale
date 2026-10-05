package identityadapter

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type Service struct {
	vault        *PasswordVault
	revocations  *SessionRevocations
	serviceToken string
}

func NewService(vault *PasswordVault, serviceToken string) (*Service, error) {
	if vault == nil || strings.TrimSpace(serviceToken) == "" {
		return nil, errors.New("identity adapter is not fully configured")
	}
	return &Service{vault: vault, revocations: newSessionRevocations(vault.directory), serviceToken: strings.TrimSpace(serviceToken)}, nil
}

func (s *Service) Handler() http.Handler {
	router := httpserver.NewRouter()
	router.POST("/v1/registration/password-prechecks", httpserver.AdaptFunc(s.precheckPassword))
	internal := router.Group("/internal/v1")
	internal.Use(serviceauth.GinMiddleware(s.serviceToken))
	internal.POST("/registration/password-references/validate", httpserver.AdaptFunc(s.validateReference))
	internal.POST("/users/mobile", httpserver.AdaptFunc(s.createMobileUser))
	internal.GET("/registration-operations/:operation_id/user", httpserver.AdaptFunc(s.findRegistrationUser))
	internal.POST("/users/:subject/disable-and-confirm", httpserver.AdaptFunc(s.disableUserAndConfirm))
	internal.PUT("/users/:subject/username", httpserver.AdaptFunc(s.updateUsername))
	internal.PUT("/users/:subject/mobile", httpserver.AdaptFunc(s.updateMobile))
	internal.POST("/users/:subject/password-resets", httpserver.AdaptFunc(s.resetPassword))
	internal.POST("/users/:subject/revoke-sessions", httpserver.AdaptFunc(s.revokeSessions))
	internal.DELETE("/users/:subject", httpserver.AdaptFunc(s.deleteUser))
	return router
}

func (s *Service) updateUsername(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OperationID uuid.UUID `json:"operation_id"`
		Username    string    `json:"username"`
	}
	if !s.decodeRequest(w, r, &body) {
		return
	}
	if body.OperationID == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "operation_id is required", httpserver.RequestID(r.Context()))
		return
	}
	if err := s.vault.UpdateUsername(r.Context(), r.PathValue("subject"), body.OperationID, body.Username); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) updateMobile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OperationID uuid.UUID `json:"operation_id"`
		Mobile      string    `json:"mobile"`
	}
	if !s.decodeRequest(w, r, &body) {
		return
	}
	if body.OperationID == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "operation_id is required", httpserver.RequestID(r.Context()))
		return
	}
	if err := s.vault.UpdateMobile(r.Context(), r.PathValue("subject"), body.OperationID, body.Mobile); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) resetPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OperationID           uuid.UUID `json:"operation_id"`
		Mobile                string    `json:"mobile"`
		PasswordHandle        string    `json:"password_handle"`
		PasswordPolicyVersion string    `json:"password_policy_version"`
	}
	if !s.decodeRequest(w, r, &body) {
		return
	}
	if body.OperationID == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "operation_id is required", httpserver.RequestID(r.Context()))
		return
	}
	if err := s.vault.ResetPassword(r.Context(), body.OperationID, body.PasswordHandle, body.PasswordPolicyVersion, r.PathValue("subject"), body.Mobile); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) revokeSessions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OperationID  uuid.UUID `json:"operation_id"`
		FirstAttempt *bool     `json:"first_attempt"`
	}
	if !s.decodeRequest(w, r, &body) {
		return
	}
	if body.OperationID == uuid.Nil || body.FirstAttempt == nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "operation_id and first_attempt are required", httpserver.RequestID(r.Context()))
		return
	}
	if err := s.revocations.Revoke(r.Context(), r.PathValue("subject"), body.OperationID, *body.FirstAttempt); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) precheckPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if !s.decodeRequest(w, r, &body) {
		return
	}
	reference, err := s.vault.Precheck(body.Password)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusCreated, reference)
}

type registrationReferenceRequest struct {
	RegistrationOperationID uuid.UUID `json:"registration_operation_id"`
	PasswordHandle          string    `json:"password_handle"`
	PasswordPolicyVersion   string    `json:"password_policy_version"`
	Username                string    `json:"username"`
	Mobile                  string    `json:"mobile"`
	LegacyIntent            bool      `json:"legacy_intent"`
	FirstCreateAttempt      bool      `json:"first_create_attempt"`
}

func (s *Service) validateReference(w http.ResponseWriter, r *http.Request) {
	var body registrationReferenceRequest
	if !s.decodeRequest(w, r, &body) {
		return
	}
	if err := s.vault.ValidateReference(body.RegistrationOperationID, body.PasswordHandle, body.PasswordPolicyVersion, body.Username, body.Mobile); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) createMobileUser(w http.ResponseWriter, r *http.Request) {
	var body registrationReferenceRequest
	if !s.decodeRequest(w, r, &body) {
		return
	}
	identity, err := s.vault.CreateMobileUser(r.Context(), body.RegistrationOperationID, body.PasswordHandle, body.PasswordPolicyVersion, body.Username, body.Mobile, body.LegacyIntent, body.FirstCreateAttempt)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, identity)
}

func (s *Service) findRegistrationUser(w http.ResponseWriter, r *http.Request) {
	operationID, err := uuid.Parse(r.PathValue("operation_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid registration operation", httpserver.RequestID(r.Context()))
		return
	}
	legacyIntent := false
	if raw, present := r.URL.Query()["legacy_intent"]; present {
		if len(raw) != 1 || (raw[0] != "true" && raw[0] != "false") {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid registration intent kind", httpserver.RequestID(r.Context()))
			return
		}
		legacyIntent, _ = strconv.ParseBool(raw[0])
	}
	identity, found, err := s.vault.FindMobileUserByRegistrationOperation(r.Context(), operationID, legacyIntent)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	identity.PasswordPreserved = true
	api.WriteJSON(w, http.StatusOK, identity)
}

func (s *Service) disableUserAndConfirm(w http.ResponseWriter, r *http.Request) {
	if err := s.vault.DisableUserAndConfirm(r.Context(), r.PathValue("subject")); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) deleteUser(w http.ResponseWriter, r *http.Request) {
	if err := s.vault.DeleteUser(r.Context(), r.PathValue("subject")); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := http.StatusInternalServerError, "IDENTITY_INTERNAL_ERROR", "the identity request could not be completed"
	switch {
	case errors.Is(err, ErrSessionRevocationUnconfirmed):
		status, code, message = http.StatusServiceUnavailable, "IDENTITY_INTERNAL_ERROR", "the session revocation could not be confirmed"
	case errors.Is(err, ErrSessionRevocationConflict):
		status, code, message = http.StatusConflict, "CONFLICT", "the session revocation belongs to different operation facts"
	case errors.Is(err, ErrPasswordEdgeSpace):
		status, code, message = http.StatusBadRequest, "PASSWORD_EDGE_SPACE_NOT_ALLOWED", "password must not start or end with a space"
	case errors.Is(err, ErrPasswordInvalid):
		status, code, message = http.StatusBadRequest, "PASSWORD_INVALID", "password must be 8 to 128 printable ASCII characters and contain a letter and a digit"
	case errors.Is(err, ErrReferenceInvalid):
		status, code, message = http.StatusGone, "PASSWORD_REFERENCE_INVALID", "the password precheck has expired; restart registration"
	case errors.Is(err, ErrCreateNotDispatched):
		status, code, message = http.StatusGone, "IDENTITY_CREATE_NOT_DISPATCHED", "registration expired before identity creation; restart registration"
	case errors.Is(err, ErrReferenceConflict):
		status, code, message = http.StatusConflict, "PASSWORD_REFERENCE_CONFLICT", "the password precheck belongs to different registration data"
	case errors.Is(err, ErrUsernameAlreadyRegistered):
		status, code, message = http.StatusConflict, "USERNAME_ALREADY_REGISTERED", "the username is already registered"
	}
	api.WriteError(w, status, code, message, httpserver.RequestID(r.Context()))
}

func (s *Service) decodeRequest(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := api.DecodeJSON(w, r, target); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "request body is invalid", httpserver.RequestID(r.Context()))
		return false
	}
	return true
}
