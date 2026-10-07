package account

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

var ErrWalletProvisioning = errors.New("personal wallet provisioning is incomplete")

type Service struct {
	repository       Repository
	wallets          WalletProvisioner
	mobileAuth       *MobileAuthService
	profiles         *ProfileChangeService
	serviceToken     string
	now              func() time.Time
	smsConfiguration *VersionedSMSProviderResolver
}

func (s *Service) WithSMSConfiguration(configuration *VersionedSMSProviderResolver) *Service {
	s.smsConfiguration = configuration
	return s
}

func (s *Service) WithProfileChanges(profiles *ProfileChangeService) *Service {
	s.profiles = profiles
	return s
}

func NewService(repository Repository, wallets WalletProvisioner) *Service {
	return &Service{repository: repository, wallets: wallets, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) WithMobileAuth(auth *MobileAuthService, serviceToken string) *Service {
	s.mobileAuth = auth
	return s.WithServiceToken(serviceToken)
}

func (s *Service) WithServiceToken(serviceToken string) *Service {
	s.serviceToken = strings.TrimSpace(serviceToken)
	return s
}

func (s *Service) Bootstrap(ctx context.Context, principal identity.Principal) (Me, error) {
	me, _, err := s.repository.Bootstrap(ctx, principal)
	return me, err
}

func (s *Service) Handler(verify identity.VerifyFunc) http.Handler {
	router := httpserver.NewRouter()
	internal := router.Group("/internal/v1")
	internal.Use(serviceauth.GinMiddleware(s.serviceToken))
	internal.POST("/authorization/check", serviceauth.Require("internal.authorization.check"), httpserver.AdaptFunc(s.checkPermission))
	if s.mobileAuth != nil {
		router.POST("/v1/auth/registration/precheck", httpserver.AdaptFunc(s.precheckMobileRegistration))
		router.POST("/v1/auth/captcha/challenges", httpserver.AdaptFunc(s.createSliderCaptcha))
		router.POST("/v1/auth/sms/challenges", httpserver.AdaptFunc(s.sendSMSChallenge))
		router.POST("/v1/auth/register", httpserver.AdaptFunc(s.registerMobile))
		router.GET("/v1/auth/registration-operations/:operation_id", httpserver.AdaptFunc(s.registrationOperationStatus))
		internal.POST("/auth/sms/verify", serviceauth.Require("internal.auth.sms.verify"), httpserver.AdaptFunc(s.verifyLoginSMS))
		if s.profiles != nil {
			router.POST("/v1/auth/password-resets", httpserver.AdaptFunc(s.resetForgottenPassword))
		}
	}
	protected := router.Group("")
	protected.Use(identity.GinMiddleware(verify), s.contextVersionHeaders())
	protected.POST("/v1/account/bootstrap", httpserver.AdaptFunc(s.bootstrap))
	protected.GET("/v1/me", s.requirePermission(PermissionPersonalProfileRead), httpserver.AdaptFunc(s.getMe))
	protected.GET("/v1/me/authorization-context", s.requirePermission(PermissionPersonalProfileRead), httpserver.AdaptFunc(s.getAuthorizationContext))
	if s.profiles != nil {
		protected.POST("/v1/me/password-changes", s.requirePermission(PermissionPersonalSecurityManage), httpserver.AdaptFunc(s.changePassword))
		protected.POST("/v1/me/profile-change-requests", s.requirePermission(PermissionPersonalProfileUpdate), httpserver.AdaptFunc(s.createProfileChange))
		protected.POST("/v1/me/profile-change-requests/:request_id/mobile-challenges", s.requirePermission(PermissionPersonalProfileUpdate), httpserver.AdaptFunc(s.sendProfileMobileChallenge))
		protected.POST("/v1/me/profile-change-requests/:request_id/actions/complete", s.requirePermission(PermissionPersonalProfileUpdate), httpserver.AdaptFunc(s.completeProfileChange))
		protected.GET("/v1/me/profile-change-requests/:request_id", s.requirePermission(PermissionPersonalProfileUpdate), httpserver.AdaptFunc(s.getProfileChange))
	}
	admin := protected.Group("/admin/v1")
	admin.GET("/users", s.requirePermission(PermissionPlatformUsersRead), httpserver.AdaptFunc(s.adminUsers))
	admin.PATCH("/users/:user_id/status", s.requirePermission(PermissionPlatformUsersStatusManage), approvalpolicy.Require("ACCOUNT_USER_STATUS_CHANGE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminSetUserStatus))
	admin.GET("/registration-operations", s.requirePermission(PermissionPlatformRegistrationsRepair), httpserver.AdaptFunc(s.adminRegistrationOperations))
	admin.POST("/registration-operations/:operation_id/actions/retry", s.requirePermission(PermissionPlatformRegistrationsRepair), approvalpolicy.Require("ACCOUNT_REGISTRATION_RETRY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRetryRegistration))
	admin.POST("/registration-operations/:operation_id/actions/compensate", s.requirePermission(PermissionPlatformRegistrationsRepair), approvalpolicy.Require("ACCOUNT_REGISTRATION_COMPENSATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminCompensateRegistration))
	admin.GET("/sms-configuration", s.requirePermission(PermissionPlatformSMSConfigRead), httpserver.AdaptFunc(s.adminSMSConfiguration))
	admin.PUT("/sms-configuration", s.requirePermission(PermissionPlatformSMSConfigManage), approvalpolicy.Require("ACCOUNT_SMS_CONFIGURATION_UPDATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminUpdateSMSConfiguration))
	if s.profiles != nil {
		admin.GET("/profile-change-requests", s.requirePermission(PermissionPlatformProfileChangesRepair), httpserver.AdaptFunc(s.adminProfileChanges))
		admin.POST("/profile-change-requests/:request_id/actions/retry", s.requirePermission(PermissionPlatformProfileChangesRepair), approvalpolicy.Require("ACCOUNT_PROFILE_CHANGE_RETRY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRetryProfileChange))
	}
	return router
}

func (s *Service) bootstrap(w http.ResponseWriter, r *http.Request) {
	me, err := s.Bootstrap(r.Context(), mustPrincipal(r.Context()))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("X-Context-Version", stringInt64(me.ActiveContext.ContextVersion))
	w.Header().Set("X-Authorization-Version", stringInt64(me.ActiveContext.AuthorizationVersion))
	w.Header().Set("X-Personal-Authorization-Version", stringInt64(me.ActiveContext.PersonalAuthorizationVersion))
	w.Header().Set("X-Platform-Authorization-Version", stringInt64(me.ActiveContext.PlatformAuthorizationVersion))
	w.Header().Set("X-Organization-Authorization-Version", stringInt64(me.ActiveContext.OrganizationAuthorizationVersion))
	api.WriteJSON(w, http.StatusOK, me)
}

func (s *Service) getMe(w http.ResponseWriter, r *http.Request) {
	me, err := s.repository.GetMe(r.Context(), mustPrincipal(r.Context()))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, me)
}

func (s *Service) getAuthorizationContext(w http.ResponseWriter, r *http.Request) {
	resolved, err := s.repository.ResolveAuthorizationContext(r.Context(), mustPrincipal(r.Context()))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Context-Version", stringInt64(resolved.ContextVersion))
	w.Header().Set("X-Authorization-Version", stringInt64(resolved.AuthorizationVersion))
	w.Header().Set("X-Personal-Authorization-Version", stringInt64(resolved.PersonalAuthorizationVersion))
	w.Header().Set("X-Platform-Authorization-Version", stringInt64(resolved.PlatformAuthorizationVersion))
	w.Header().Set("X-Organization-Authorization-Version", stringInt64(resolved.OrganizationAuthorizationVersion))
	api.WriteJSON(w, http.StatusOK, resolved)
}

func (s *Service) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := http.StatusInternalServerError, "INTERNAL_ERROR", "the request could not be completed"
	switch {
	case errors.Is(err, ErrNotFound):
		status, code, message = http.StatusNotFound, "NOT_FOUND", "the requested account resource was not found"
	case errors.Is(err, ErrForbidden):
		status, code, message = http.StatusForbidden, "ACCOUNT_FORBIDDEN", "the account action is not permitted"
	case errors.Is(err, ErrConflict):
		status, code, message = http.StatusConflict, "ACCOUNT_CONFLICT", "the account resource changed or conflicts with existing data"
	case errors.Is(err, ErrLastActiveSuperAdmin):
		status, code, message = http.StatusConflict, "LAST_ACTIVE_SUPER_ADMIN", "another active user must become super administrator first"
	case errors.Is(err, ErrAccountProvisioning):
		status, code, message = http.StatusConflict, "ACCOUNT_PROVISIONING", "the account is still provisioning"
	case errors.Is(err, ErrAccountRepairRequired):
		status, code, message = http.StatusConflict, "ACCOUNT_REPAIR_REQUIRED", "the account initialization requires repair"
	case errors.Is(err, ErrBootstrapPending):
		status, code, message = http.StatusForbidden, "ADMIN_BOOTSTRAP_PENDING", "the first administrator must complete secure activation"
	case errors.Is(err, ErrRecentAuthenticationRequired):
		status, code, message = http.StatusUnauthorized, "RECENT_AUTHENTICATION_REQUIRED", "recent authentication is required"
	}
	api.WriteError(w, status, code, message, httpserver.RequestID(r.Context()))
}

func mustPrincipal(ctx context.Context) identity.Principal {
	principal, ok := identity.FromContext(ctx)
	if !ok {
		panic("identity middleware did not provide a principal")
	}
	return principal
}

func pathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	value, err := uuid.Parse(r.PathValue(name))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid resource identifier", httpserver.RequestID(r.Context()))
		return uuid.Nil, false
	}
	return value, true
}

type HTTPWalletProvisioner struct {
	baseURL      *url.URL
	serviceToken string
	client       *http.Client
}

func NewHTTPWalletProvisioner(rawURL, serviceToken string) (*HTTPWalletProvisioner, error) {
	parsed, err := authorityEndpoint(rawURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(serviceToken) == "" {
		return nil, errors.New("INTERNAL_SERVICE_TOKEN is required")
	}
	return &HTTPWalletProvisioner{baseURL: parsed, serviceToken: serviceToken, client: observability.NewHTTPClient(5 * time.Second)}, nil
}

func (p *HTTPWalletProvisioner) EnsurePersonalWallet(ctx context.Context, userID uuid.UUID) error {
	if userID == uuid.Nil {
		return ErrWalletProvisioning
	}
	endpoint := *p.baseURL
	endpoint.Path = strings.TrimRight(endpoint.Path, "/") + "/internal/v1/wallets/ensure"
	body := strings.NewReader(fmt.Sprintf(`{"owner_type":"PERSONAL","owner_id":%q}`, userID.String()))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), body)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if err := serviceauth.SetAuthorization(request, p.serviceToken); err != nil {
		return err
	}
	request.Header.Set("Idempotency-Key", "personal-wallet:"+userID.String())
	response, err := p.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return fmt.Errorf("billing wallet provisioning returned HTTP %d", response.StatusCode)
	}
	var wallet struct {
		ID        uuid.UUID `json:"wallet_id"`
		OwnerType string    `json:"owner_type"`
		OwnerID   uuid.UUID `json:"owner_id"`
		Status    string    `json:"status"`
		Version   int64     `json:"version"`
	}
	if decodeAuthorityResponse(response.Body, &wallet) != nil || wallet.ID == uuid.Nil || wallet.OwnerType != "PERSONAL" || wallet.OwnerID != userID || wallet.Status != "ACTIVE" || wallet.Version < 1 {
		return ErrWalletProvisioning
	}
	return nil
}
