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
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/humandelegation"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

var ErrWalletProvisioning = errors.New("personal wallet provisioning is incomplete")

type Service struct {
	approvalPrechecks           ApprovalPrecheckSource
	organizationLifecycle       []OrganizationLifecycleClient
	organizationUnitLifecycle   []OrganizationUnitLifecycleClient
	organizationMemberLifecycle []OrganizationMemberLifecycleClient
	serviceVersion              string
	clientContractVersion       string
	ownerCandidateCursorKey     []byte
	repository                  Repository
	humanDelegations            *humandelegation.Signer
	recoveryResults             ApprovalRecoveryResultReader
	recoveryEffects             approvaleffect.Reader
	wallets                     WalletProvisioner
	mobileAuth                  *MobileAuthService
	profiles                    *ProfileChangeService
	serviceToken                string
	now                         func() time.Time
	smsConfiguration            *VersionedSMSProviderResolver
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
	internal.GET("/users/:user_id/account-closure-state", serviceauth.Require("internal.account.closure.read"), httpserver.AdaptFunc(s.internalAccountClosureState))
	internal.GET("/client-compatibility", serviceauth.Require("internal.client-compatibility.read"), httpserver.AdaptFunc(s.internalClientCompatibility))
	internal.GET("/registration-bonus-eligibility/readiness", serviceauth.Require("internal.registration-bonus-eligibility.read"), httpserver.AdaptFunc(s.internalRegistrationBonusEligibilityReadiness))
	internal.POST("/token-owner-authority", serviceauth.Require("internal.token-owner-authority.read"), httpserver.AdaptFunc(s.internalTokenOwnerAuthority))
	internal.GET("/token-owner-candidates", serviceauth.Require("internal.token-owner-candidates.read"), httpserver.AdaptFunc(s.internalTokenOwnerCandidates))
	internal.POST("/authorization/check", serviceauth.Require("internal.authorization.check"), httpserver.AdaptFunc(s.checkPermission))
	internal.POST("/authorization/human-delegations", serviceauth.Require("internal.authorization.delegate"), httpserver.AdaptFunc(s.createHumanDelegation))
	internal.GET("/approval-recovery/plan", serviceauth.Require("internal.approval-recovery.plan"), httpserver.AdaptFunc(s.internalApprovalRecoveryPlan))
	internal.POST("/approval-recovery/confirm", serviceauth.Require("internal.approval-recovery.confirm"), httpserver.AdaptFunc(s.internalConfirmApprovalRecovery))
	internal.POST("/approval-recovery/zero-effect", serviceauth.Require("internal.approval-recovery.zero-effect"), httpserver.AdaptFunc(s.internalInvalidateApprovalRecoveryZeroEffect))
	internal.POST("/approval-requests/:approval_request_id/executions", serviceauth.Require("internal.approval-execution.begin"), approvalpolicy.Require("APPROVAL_EXECUTION_BEGIN", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.beginApprovalExecution))
	internal.POST("/approval-requests/:approval_request_id/executions/complete", serviceauth.Require("internal.approval-execution.complete"), approvalpolicy.Require("APPROVAL_EXECUTION_COMPLETE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.completeApprovalExecution))
	internal.GET("/approval-authorization/events/:event_id/route", serviceauth.Require("internal.approval-authorization.consume"), httpserver.AdaptFunc(s.resolveApprovalAuthorizationRoute))
	internal.GET("/approval-authorization/events/:event_id/binding", serviceauth.Require("internal.approval-authorization.consume"), httpserver.AdaptFunc(s.resolveApprovalAuthorizationBinding))
	internal.GET("/approval-authorization/events/:event_id/deliveries/:durable", serviceauth.Require("internal.approval-authorization.consume"), httpserver.AdaptFunc(s.approvalAuthorizationDeliveryState))
	internal.POST("/approval-authorization/events/:event_id/deliveries/:durable/observe", serviceauth.Require("internal.approval-authorization.consume"), httpserver.AdaptFunc(s.observeApprovalAuthorizationDelivery))
	internal.POST("/approval-authorization/events/:event_id/deliveries/:durable/attempts/grant", serviceauth.Require("internal.approval-authorization.consume"), httpserver.AdaptFunc(s.grantApprovalAuthorizationDomainAttempt))
	internal.POST("/approval-authorization/events/:event_id/deliveries/:durable/attempts/failure", serviceauth.Require("internal.approval-authorization.consume"), httpserver.AdaptFunc(s.recordApprovalAuthorizationDomainFailure))
	internal.POST("/approval-authorization/events/:event_id/deliveries/:durable/result", serviceauth.Require("internal.approval-authorization.consume"), httpserver.AdaptFunc(s.recordApprovalAuthorizationResult))
	internal.POST("/approval-authorization/events/:event_id/deliveries/:durable/dlq-puback", serviceauth.Require("internal.approval-authorization.consume"), httpserver.AdaptFunc(s.recordApprovalAuthorizationDLQPubAck))
	internal.POST("/approval-authorization/events/:event_id/deliveries/:durable/terminal", serviceauth.Require("internal.approval-authorization.consume"), httpserver.AdaptFunc(s.recordApprovalAuthorizationTerminal))
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
	protected.GET("/v1/me/account-closure-request", s.requirePermission("personal.account.close.read"), httpserver.AdaptFunc(s.getAccountClosure))
	protected.POST("/v1/me/account-closure-requests", s.requirePermission("personal.account.close.request"), httpserver.AdaptFunc(s.createAccountClosure))
	protected.POST("/v1/account/bootstrap", httpserver.AdaptFunc(s.bootstrap))
	protected.GET("/v1/me/commerce-customer-mapping", s.requirePermission(PermissionPersonalProfileRead), httpserver.AdaptFunc(s.getCommerceCustomerMapping))
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
	admin.GET("/account-closure-requests", s.requirePermission("platform.account-closures.read"), httpserver.AdaptFunc(s.adminAccountClosures))
	admin.POST("/account-closure-requests/:closure_request_id/actions/retry", s.requirePermission("platform.account-closures.repair"), approvalpolicy.Require("ACCOUNT_CLOSURE_RETRY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRetryAccountClosure))
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
	admin.GET("/approval-policies", httpserver.AdaptFunc(s.adminApprovalPolicies))
	admin.GET("/approval-requests", httpserver.AdaptFunc(s.adminApprovalRequests))
	admin.POST("/approval-requests", approvalpolicy.Require("APPROVAL_DRAFT_CREATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminCreateApprovalDraft))
	admin.GET("/approval-requests/:approval_request_id", httpserver.AdaptFunc(s.adminApprovalRequest))
	admin.PUT("/approval-requests/:approval_request_id", approvalpolicy.Require("APPROVAL_DRAFT_UPDATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminUpdateApprovalDraft))
	admin.POST("/approval-requests/:approval_request_id/actions/submit", approvalpolicy.Require("APPROVAL_REQUEST_SUBMIT", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminSubmitApprovalRequest))
	admin.POST("/approval-requests/:approval_request_id/actions/withdraw", approvalpolicy.Require("APPROVAL_REQUEST_WITHDRAW", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminWithdrawApprovalRequest))
	admin.POST("/approval-requests/:approval_request_id/actions/decide", approvalpolicy.Require("APPROVAL_REQUEST_DECIDE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminDecideApprovalRequest))
	adminV2 := protected.Group("/admin/v2")
	adminV2.POST("/approval-command-prechecks", s.requirePermission("platform.billing.repairs.request"), httpserver.AdaptFunc(s.adminApprovalCommandPrecheck))
	adminV2.GET("/approval-capability", httpserver.AdaptFunc(s.adminApprovalCapability))
	adminV2.GET("/approval-directory", httpserver.AdaptFunc(s.adminApprovalDirectory))
	adminV2.GET("/approval-requests", httpserver.AdaptFunc(s.adminApprovalRequestsV2))
	adminV2.POST("/approval-requests", approvalpolicy.Require("APPROVAL_DRAFT_CREATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminCreateApprovalDraftV2))
	adminV2.GET("/approval-requests/:approval_request_id", httpserver.AdaptFunc(s.adminApprovalRequestV2))
	adminV2.PUT("/approval-requests/:approval_request_id", approvalpolicy.Require("APPROVAL_DRAFT_UPDATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminUpdateApprovalDraftV2))
	adminV2.POST("/approval-requests/:approval_request_id/actions/submit", approvalpolicy.Require("APPROVAL_REQUEST_SUBMIT", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminSubmitApprovalRequestV2))
	adminV2.POST("/approval-requests/:approval_request_id/actions/withdraw", approvalpolicy.Require("APPROVAL_REQUEST_WITHDRAW", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminWithdrawApprovalRequestV2))
	adminV2.POST("/approval-requests/:approval_request_id/actions/decide", approvalpolicy.Require("APPROVAL_REQUEST_DECIDE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminDecideApprovalRequestV2))
	s.registerOrganizationRoutes(protected, admin)
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
	case errors.Is(err, ErrInvalidAccountClosureRequest):
		status, code, message = http.StatusBadRequest, "INVALID_REQUEST", "invalid account closure command"
	case errors.Is(err, ErrNotFound):
		status, code, message = http.StatusNotFound, "NOT_FOUND", "the requested account resource was not found"
	case errors.Is(err, ErrForbidden):
		status, code, message = http.StatusForbidden, "ACCOUNT_FORBIDDEN", "the account action is not permitted"
	case errors.Is(err, ErrAccountClosing):
		status, code, message = http.StatusForbidden, "ACCOUNT_CLOSING", "account closure is in progress"
	case errors.Is(err, ErrConflict):
		status, code, message = http.StatusConflict, "ACCOUNT_CONFLICT", "the account resource changed or conflicts with existing data"
	case errors.Is(err, ErrLastOwner):
		status, code, message = http.StatusConflict, "LAST_OWNER_REQUIRED", "the organization must retain at least one owner"
	case errors.Is(err, ErrInvalidHierarchy):
		status, code, message = http.StatusUnprocessableEntity, "ORG_UNIT_HIERARCHY_INVALID", "the requested organization hierarchy is invalid"
	case errors.Is(err, ErrInvitationExpired):
		status, code, message = http.StatusGone, "INVITATION_EXPIRED", "the invitation has expired"
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
	case errors.Is(err, ErrInvalidApprovalInput):
		status, code, message = http.StatusBadRequest, "INVALID_APPROVAL_REQUEST", "the approval request is invalid"
	case errors.Is(err, ErrApprovalSelfDecision):
		status, code, message = http.StatusConflict, "APPROVAL_SELF_DECISION_FORBIDDEN", "the requester cannot decide their own approval request"
	case errors.Is(err, ErrApprovalExpired):
		status, code, message = http.StatusGone, "APPROVAL_REQUEST_EXPIRED", "the approval request has expired"
	case errors.Is(err, ErrApprovalInvalidated):
		status, code, message = http.StatusConflict, "APPROVAL_REQUEST_INVALIDATED", "the approval request no longer matches current policy, authorization, or resource facts"
	case errors.Is(err, ErrApprovalRequestNotFound):
		status, code, message = http.StatusNotFound, "APPROVAL_REQUEST_NOT_FOUND", "the approval request was not found"
	case errors.Is(err, ErrApprovalAuthorizationChanged):
		status, code, message = http.StatusConflict, "APPROVAL_AUTHORIZATION_CHANGED", "approval authorization changed while the response was being prepared"
	case errors.Is(err, ErrApprovalSnapshotTimeout):
		status, code, message = http.StatusGatewayTimeout, "APPROVAL_SNAPSHOT_TIMEOUT", "the authoritative approval snapshot timed out"
	case errors.Is(err, ErrApprovalSnapshotUnavailable):
		status, code, message = http.StatusServiceUnavailable, "APPROVAL_SNAPSHOT_UNAVAILABLE", "the authoritative approval snapshot is temporarily unavailable"
	case errors.Is(err, ErrApprovalWriterUnavailable):
		w.Header().Set("Retry-After", "5")
		w.Header().Set("Cache-Control", "no-store")
		status, code, message = http.StatusServiceUnavailable, "APPROVAL_WRITER_UNAVAILABLE", "the approval authorization writer is temporarily unavailable"
	case errors.Is(err, ErrApprovalProofRejected):
		status, code, message = http.StatusConflict, "APPROVAL_PROOF_REJECTED", "the authoritative domain proof rejected the approval"
	case errors.Is(err, ErrApprovalProofInvalid):
		status, code, message = http.StatusBadGateway, "APPROVAL_PROOF_INVALID", "the authoritative domain proof was invalid"
	case errors.Is(err, ErrApprovalProofTooLarge):
		status, code, message = http.StatusBadGateway, "APPROVAL_PROOF_TOO_LARGE", "the authoritative domain proof exceeded the size limit"
	case errors.Is(err, ErrApprovalProofUnavailable):
		status, code, message = http.StatusServiceUnavailable, "APPROVAL_PROOF_UNAVAILABLE", "the authoritative domain proof is temporarily unavailable"
	case errors.Is(err, ErrApprovalProofTimeout):
		status, code, message = http.StatusGatewayTimeout, "APPROVAL_PROOF_TIMEOUT", "the authoritative domain proof timed out"
	case errors.Is(err, ErrApprovalRequestChanged):
		status, code, message = http.StatusConflict, "APPROVAL_REQUEST_CHANGED", "the approval request changed after the proof checkpoint"
	case errors.Is(err, ErrApprovalActionControlChanged):
		status, code, message = http.StatusConflict, "APPROVAL_ACTION_CONTROL_CHANGED", "approval actions changed; reload the approval detail"
	case errors.Is(err, ErrApprovalRelayFenced):
		w.Header().Set("Retry-After", "5")
		status, code, message = http.StatusServiceUnavailable, "APPROVAL_RELAY_FENCED", "approval authorization relay is fenced"
	case errors.Is(err, ErrApprovalQuiescing):
		w.Header().Set("Retry-After", "5")
		status, code, message = http.StatusServiceUnavailable, "APPROVAL_QUIESCING", "approval authorization is quiescing"
	case errors.Is(err, ErrApprovalSafeRollback):
		w.Header().Set("Retry-After", "5")
		status, code, message = http.StatusServiceUnavailable, "APPROVAL_SAFE_ROLLBACK", "approval authorization is in safe rollback"
	case errors.Is(err, ErrApprovalSnapshotRejected):
		status, code, message = http.StatusConflict, "APPROVAL_SNAPSHOT_REJECTED", "the authoritative approval snapshot request was rejected"
	case errors.Is(err, ErrApprovalCommandNotExecutable):
		status, code, message = http.StatusConflict, "APPROVAL_COMMAND_NOT_EXECUTABLE", "approval command is not executable"
	case errors.Is(err, ErrTokenGovernanceInactive):
		w.Header().Set("Cache-Control", "no-store")
		status, code, message = http.StatusServiceUnavailable, "TOKEN_COMMAND_GOVERNANCE_INACTIVE", "Token command governance is not active"
	case errors.Is(err, ErrBillingGovernanceInactive):
		status, code, message = http.StatusServiceUnavailable, "BILLING_COMMAND_GOVERNANCE_INACTIVE", "Billing command governance is not active"
	case errors.Is(err, ErrApprovalConsumerBindingInvalid):
		status, code, message = http.StatusConflict, "APPROVAL_AUTHORIZATION_BINDING_INVALID", "the approval authorization binding or delivery state is invalid"
	case errors.Is(err, ErrApprovalConsumerBindingUnavailable):
		w.Header().Set("Retry-After", "5")
		w.Header().Set("Cache-Control", "no-store")
		status, code, message = http.StatusServiceUnavailable, "APPROVAL_AUTHORIZATION_UNAVAILABLE", "approval authorization state is temporarily unavailable"
	case errors.Is(err, ErrBillingGovernanceInactive), errors.Is(err, ErrBillingGovernanceUnavailable):
		w.Header().Set("Cache-Control", "no-store")
		status, code, message = http.StatusServiceUnavailable, "BILLING_COMMAND_GOVERNANCE_UNAVAILABLE", "Billing command governance is unavailable"
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

func (s *Service) WithHumanDelegations(signer *humandelegation.Signer) *Service {
	s.humanDelegations = signer
	return s
}

func (s *Service) WithApprovalRecoveryResults(reader ApprovalRecoveryResultReader) *Service {
	s.recoveryResults = reader
	return s
}

func (s *Service) WithApprovalRecoveryEffects(reader approvaleffect.Reader) *Service {
	s.recoveryEffects = reader
	return s
}

func (s *Service) getCommerceCustomerMapping(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(interface {
		GetCommerceCustomerMapping(context.Context, identity.Principal) (CommerceCustomerMapping, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "COMMERCE_MAPPING_UNAVAILABLE", "commerce mapping is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	mapping, err := repository.GetCommerceCustomerMapping(r.Context(), mustPrincipal(r.Context()))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, mapping)
}

func (s *Service) WithOrganizationLifecycle(clients ...OrganizationLifecycleClient) *Service {
	s.organizationLifecycle = append([]OrganizationLifecycleClient(nil), clients...)
	return s
}

func (s *Service) WithOrganizationUnitLifecycle(clients ...OrganizationUnitLifecycleClient) *Service {
	s.organizationUnitLifecycle = append([]OrganizationUnitLifecycleClient(nil), clients...)
	return s
}

func (s *Service) WithOrganizationMemberLifecycle(clients ...OrganizationMemberLifecycleClient) *Service {
	s.organizationMemberLifecycle = append([]OrganizationMemberLifecycleClient(nil), clients...)
	return s
}

func (s *Service) WithTokenOwnerCandidateCursorKey(key []byte) *Service {
	s.ownerCandidateCursorKey = append([]byte{}, key...)
	return s
}
