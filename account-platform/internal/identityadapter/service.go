package identityadapter

import (
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type Service struct {
	vault        *PasswordVault
	serviceToken string
	tokenProxy   http.Handler
}

func NewService(vault *PasswordVault, serviceToken string, tokenProxy ...http.Handler) (*Service, error) {
	workloadIdentityConfigured := strings.TrimSpace(os.Getenv("WORKLOAD_IDENTITY_ISSUER")) != "" &&
		strings.TrimSpace(os.Getenv("WORKLOAD_IDENTITY_JWKS_URL")) != "" &&
		strings.TrimSpace(os.Getenv("WORKLOAD_IDENTITY_AUDIENCE")) != "" &&
		strings.TrimSpace(os.Getenv("WORKLOAD_IDENTITY_RULES_JSON")) != ""
	if vault == nil || (strings.TrimSpace(serviceToken) == "" && !workloadIdentityConfigured) {
		return nil, errors.New("identity adapter is not fully configured")
	}
	service := &Service{vault: vault, serviceToken: strings.TrimSpace(serviceToken)}
	if len(tokenProxy) > 0 {
		service.tokenProxy = tokenProxy[0]
	}
	return service, nil
}

func (s *Service) Handler() http.Handler {
	router := httpserver.NewRouter()
	router.POST("/v1/registration/password-prechecks", httpserver.AdaptFunc(s.precheckPassword))
	if s.tokenProxy != nil {
		router.POST("/realms/pinvou/protocol/openid-connect/token", httpserver.AdaptFunc(s.tokenProxy.ServeHTTP))
	}
	internal := router.Group("/internal/v1")
	internal.Use(serviceauth.GinMiddleware(s.serviceToken))
	internal.POST("/registration/password-references/validate", serviceauth.Require("internal.identity.password-reference.validate"), httpserver.AdaptFunc(s.validateReference))
	internal.POST("/users/mobile", serviceauth.Require("internal.identity.user.create"), httpserver.AdaptFunc(s.createMobileUser))
	internal.GET("/registration-operations/:operation_id/user", serviceauth.Require("internal.identity.registration.read"), httpserver.AdaptFunc(s.findRegistrationUser))
	internal.POST("/users/:subject/disable-and-confirm", serviceauth.Require("internal.identity.user.disable"), httpserver.AdaptFunc(s.disableUserAndConfirm))
	internal.PUT("/users/:subject/username", serviceauth.Require("internal.identity.username.update"), httpserver.AdaptFunc(s.updateUsername))
	internal.PUT("/users/:subject/mobile", serviceauth.Require("internal.identity.mobile.update"), httpserver.AdaptFunc(s.updateMobile))
	internal.POST("/users/:subject/password-resets", serviceauth.Require("internal.identity.password.reset"), httpserver.AdaptFunc(s.resetPassword))
	internal.POST("/users/:subject/revoke-sessions", serviceauth.Require("internal.identity.sessions.revoke"), httpserver.AdaptFunc(s.revokeSessions))
	internal.DELETE("/users/:subject", serviceauth.Require("internal.identity.user.delete"), httpserver.AdaptFunc(s.deleteUser))
	return router
}

func (s *Service) updateUsername(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OperationID  uuid.UUID `json:"operation_id"`
		FenceVersion int64     `json:"fence_version"`
		Username     string    `json:"username"`
	}
	if api.DecodeJSON(w, r, &body) != nil || body.OperationID == uuid.Nil || body.FenceVersion <= 0 {
		return
	}
	if err := s.vault.UpdateUsername(r.Context(), r.PathValue("subject"), body.OperationID, body.FenceVersion, body.Username); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) updateMobile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OperationID  uuid.UUID `json:"operation_id"`
		FenceVersion int64     `json:"fence_version"`
		Mobile       string    `json:"mobile"`
	}
	if api.DecodeJSON(w, r, &body) != nil || body.OperationID == uuid.Nil || body.FenceVersion <= 0 {
		return
	}
	if err := s.vault.UpdateMobile(r.Context(), r.PathValue("subject"), body.OperationID, body.FenceVersion, body.Mobile); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) resetPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OperationID           uuid.UUID `json:"operation_id"`
		FenceVersion          int64     `json:"fence_version"`
		Mobile                string    `json:"mobile"`
		PasswordHandle        string    `json:"password_handle"`
		PasswordPolicyVersion string    `json:"password_policy_version"`
	}
	if api.DecodeJSON(w, r, &body) != nil || body.OperationID == uuid.Nil || body.FenceVersion <= 0 {
		return
	}
	if err := s.vault.ResetPassword(r.Context(), body.OperationID, body.FenceVersion, body.PasswordHandle, body.PasswordPolicyVersion, r.PathValue("subject"), body.Mobile); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) revokeSessions(w http.ResponseWriter, r *http.Request) {
	if err := s.vault.RevokeSessions(r.Context(), r.PathValue("subject")); err != nil {
		s.writeError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Service) precheckPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
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
}

func (s *Service) validateReference(w http.ResponseWriter, r *http.Request) {
	var body registrationReferenceRequest
	if err := api.DecodeJSON(w, r, &body); err != nil {
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
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	subject, err := s.vault.CreateMobileUser(r.Context(), body.RegistrationOperationID, body.PasswordHandle, body.PasswordPolicyVersion, body.Username, body.Mobile)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, map[string]string{"subject": subject})
}

func (s *Service) findRegistrationUser(w http.ResponseWriter, r *http.Request) {
	operationID, err := uuid.Parse(r.PathValue("operation_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid registration operation", httpserver.RequestID(r.Context()))
		return
	}
	subject, found, err := s.vault.FindMobileUserByRegistrationOperation(r.Context(), operationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]string{"subject": subject})
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
	case errors.Is(err, ErrPasswordEdgeSpace):
		status, code, message = http.StatusBadRequest, "PASSWORD_EDGE_SPACE_NOT_ALLOWED", "password must not start or end with a space"
	case errors.Is(err, ErrPasswordInvalid):
		status, code, message = http.StatusBadRequest, "PASSWORD_INVALID", "password must be 8 to 128 printable ASCII characters and contain a letter and a digit"
	case errors.Is(err, ErrReferenceInvalid):
		status, code, message = http.StatusGone, "PASSWORD_REFERENCE_INVALID", "the password precheck has expired; restart registration"
	case errors.Is(err, ErrReferenceConflict):
		status, code, message = http.StatusConflict, "PASSWORD_REFERENCE_CONFLICT", "the password precheck belongs to different registration data"
	case errors.Is(err, ErrUsernameAlreadyRegistered):
		status, code, message = http.StatusConflict, "USERNAME_ALREADY_REGISTERED", "the username is already registered"
	}
	api.WriteError(w, status, code, message, httpserver.RequestID(r.Context()))
}
