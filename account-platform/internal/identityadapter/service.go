package identityadapter

import (
	"errors"
	"net/http"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
)

type Service struct {
	vault        *PasswordVault
	serviceToken string
}

func NewService(vault *PasswordVault, serviceToken string) (*Service, error) {
	if vault == nil || strings.TrimSpace(serviceToken) == "" {
		return nil, errors.New("identity adapter is not fully configured")
	}
	return &Service{vault: vault, serviceToken: strings.TrimSpace(serviceToken)}, nil
}

func (s *Service) Handler() http.Handler {
	router := httpserver.NewRouter()
	router.POST("/v1/registration/password-prechecks", httpserver.AdaptFunc(s.precheckPassword))
	internal := router.Group("/internal/v1")
	internal.Use(serviceauth.GinMiddleware(s.serviceToken))
	internal.POST("/registration/password-references/validate", httpserver.AdaptFunc(s.validateReference))
	internal.POST("/users/mobile", httpserver.AdaptFunc(s.createMobileUser))
	internal.DELETE("/users/:subject", httpserver.AdaptFunc(s.deleteUser))
	return router
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
	PasswordHandle        string `json:"password_handle"`
	PasswordPolicyVersion string `json:"password_policy_version"`
	Username              string `json:"username"`
	Mobile                string `json:"mobile"`
}

func (s *Service) validateReference(w http.ResponseWriter, r *http.Request) {
	var body registrationReferenceRequest
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	if err := s.vault.ValidateReference(body.PasswordHandle, body.PasswordPolicyVersion, body.Username, body.Mobile); err != nil {
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
	subject, err := s.vault.CreateMobileUser(r.Context(), body.PasswordHandle, body.PasswordPolicyVersion, body.Username, body.Mobile)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, map[string]string{"subject": subject})
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
