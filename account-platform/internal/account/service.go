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
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/humandelegation"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

var ErrWalletProvisioning = errors.New("personal wallet provisioning is incomplete")

type Service struct {
	repository                  Repository
	wallets                     WalletProvisioner
	mobileAuth                  *MobileAuthService
	profiles                    *ProfileChangeService
	organizationLifecycle       []OrganizationLifecycleClient
	organizationUnitLifecycle   []OrganizationUnitLifecycleClient
	organizationMemberLifecycle []OrganizationMemberLifecycleClient
	serviceToken                string
	serviceVersion              string
	clientContractVersion       string
	now                         func() time.Time
	auditExports                *AuditExportService
	humanDelegations            *humandelegation.Signer
	ownerCandidateCursorKey     []byte
}

func (s *Service) WithAuditExports(exports *AuditExportService) *Service {
	s.auditExports = exports
	return s
}

func (s *Service) WithHumanDelegations(signer *humandelegation.Signer) *Service {
	s.humanDelegations = signer
	return s
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

func (s *Service) WithTokenOwnerCandidateCursorKey(key []byte) *Service {
	s.ownerCandidateCursorKey = append([]byte(nil), key...)
	return s
}

func (s *Service) WithClientCompatibility(serviceVersion, contractVersion string) *Service {
	s.serviceVersion = strings.TrimSpace(serviceVersion)
	s.clientContractVersion = strings.TrimSpace(contractVersion)
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
	internal.POST("/authorization/human-delegations", serviceauth.Require("internal.authorization.delegate"), httpserver.AdaptFunc(s.createHumanDelegation))
	internal.GET("/token-owner-candidates", serviceauth.Require("internal.token-owner-candidates.read"), httpserver.AdaptFunc(s.internalTokenOwnerCandidates))
	internal.POST("/token-account-authority/snapshot", serviceauth.Require("internal.account.token-owner-authority.check"), httpserver.AdaptFunc(s.internalTokenOwnerAuthority))
	internal.GET("/client-compatibility", serviceauth.Require("internal.client-compatibility.read"), httpserver.AdaptFunc(s.internalClientCompatibility))
	internal.GET("/users/:user_id/account-closure-state", serviceauth.Require("internal.account-closure-state.read"), httpserver.AdaptFunc(s.internalAccountClosureState))
	internal.POST("/approval-requests/:approval_request_id/executions", serviceauth.Require("internal.approval-execution.begin"), approvalpolicy.Require("APPROVAL_EXECUTION_BEGIN", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.beginApprovalExecution))
	internal.POST("/approval-requests/:approval_request_id/executions/complete", serviceauth.Require("internal.approval-execution.complete"), approvalpolicy.Require("APPROVAL_EXECUTION_COMPLETE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.completeApprovalExecution))
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
	protected.Use(identity.GinMiddleware(verify))
	protected.Use(s.contextVersionHeaders())
	protected.POST("/v1/account/bootstrap", httpserver.AdaptFunc(s.bootstrap))
	protected.GET("/v1/me", s.requirePermission(PermissionPersonalProfileRead), httpserver.AdaptFunc(s.getMe))
	protected.POST("/v1/me/account-closure-requests", s.requirePermission(PermissionPersonalAccountClosureRequest), httpserver.AdaptFunc(s.createAccountClosure))
	protected.GET("/v1/me/account-closure-request", s.requirePermission(PermissionPersonalAccountClosureRead), httpserver.AdaptFunc(s.getAccountClosure))
	protected.GET("/v1/me/commerce-customer-mapping", s.requirePermission(PermissionPersonalProfileRead), httpserver.AdaptFunc(s.getCommerceCustomerMapping))
	if s.profiles != nil {
		protected.POST("/v1/me/password-changes", s.requirePermission(PermissionPersonalSecurityManage), httpserver.AdaptFunc(s.changePassword))
		protected.POST("/v1/me/profile-change-requests", s.requirePermission(PermissionPersonalProfileUpdate), httpserver.AdaptFunc(s.createProfileChange))
		protected.POST("/v1/me/profile-change-requests/:request_id/mobile-challenges", s.requirePermission(PermissionPersonalProfileUpdate), httpserver.AdaptFunc(s.sendProfileMobileChallenge))
		protected.POST("/v1/me/profile-change-requests/:request_id/actions/complete", s.requirePermission(PermissionPersonalProfileUpdate), httpserver.AdaptFunc(s.completeProfileChange))
		protected.GET("/v1/me/profile-change-requests/:request_id", s.requirePermission(PermissionPersonalProfileUpdate), httpserver.AdaptFunc(s.getProfileChange))
	}
	protected.GET("/v1/me/authorization-context", s.requirePermission(PermissionPersonalProfileRead), httpserver.AdaptFunc(s.getAuthorizationContext))
	protected.GET("/v1/me/role-bindings", s.requirePermission(PermissionPersonalProfileRead), httpserver.AdaptFunc(s.listMyRoleBindings))
	protected.GET("/v1/me/organizations", s.requirePermission(PermissionPersonalProfileRead), httpserver.AdaptFunc(s.listOrganizations))
	protected.POST("/v1/sessions/current/active-context", s.requirePermission(PermissionPersonalContextSwitch), httpserver.AdaptFunc(s.setActiveContext))
	protected.GET("/v1/organizations/:organization_id/units", httpserver.AdaptFunc(s.listUnits))
	protected.POST("/v1/organizations/:organization_id/units", httpserver.AdaptFunc(s.createUnit))
	protected.GET("/v1/organizations/:organization_id/members", httpserver.AdaptFunc(s.listMembers))
	protected.GET("/v1/organizations/:organization_id/quota-members", httpserver.AdaptFunc(s.listQuotaMembers))
	protected.GET("/v1/organizations/:organization_id/role-bindings", httpserver.AdaptFunc(s.listOrganizationRoleBindings))
	protected.POST("/v1/organizations/:organization_id/role-bindings", httpserver.AdaptFunc(s.grantOrganizationRoleBinding))
	protected.DELETE("/v1/organizations/:organization_id/role-bindings/:binding_id", httpserver.AdaptFunc(s.revokeOrganizationRoleBinding))
	protected.PUT("/v1/organizations/:organization_id/members/:user_id/roles", httpserver.AdaptFunc(s.replaceMemberRoles))
	protected.PATCH("/v1/organizations/:organization_id/units/:unit_id/parent", httpserver.AdaptFunc(s.moveUnit))
	protected.POST("/v1/organizations/:organization_id/invitations", httpserver.AdaptFunc(s.createInvitation))
	protected.GET("/v1/me/invitations", s.requirePermission(PermissionPersonalInvitationsRead), httpserver.AdaptFunc(s.listMyInvitations))
	protected.DELETE("/v1/organizations/:organization_id/invitations/:invitation_id", httpserver.AdaptFunc(s.revokeInvitation))
	protected.POST("/v1/invitations/accept", s.requirePermission(PermissionPersonalInvitationsAccept), httpserver.AdaptFunc(s.acceptInvitation))
	protected.DELETE("/v1/organizations/:organization_id/members/:user_id", httpserver.AdaptFunc(s.revokeMembership))
	protected.POST("/v1/organizations/:organization_id/ownership", httpserver.AdaptFunc(s.transferOwnership))
	admin := protected.Group("/admin/v1")
	admin.GET("/overview", s.requireAnyPermission(PermissionPlatformUsersRead, PermissionPlatformOrganizationsRead), httpserver.AdaptFunc(s.adminOverview))
	admin.GET("/roles", s.requirePermission(PermissionPlatformRolesRead), httpserver.AdaptFunc(s.adminRoles))
	admin.GET("/role-bindings", s.requirePermission(PermissionPlatformRolesRead), httpserver.AdaptFunc(s.adminRoleBindings))
	admin.POST("/role-bindings", s.requirePermission(PermissionPlatformRolesAssign), approvalpolicy.Require("ACCOUNT_ROLE_BINDING_GRANT", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminGrantRoleBinding))
	admin.DELETE("/role-bindings/:binding_id", s.requirePermission(PermissionPlatformRolesAssign), approvalpolicy.Require("ACCOUNT_ROLE_BINDING_REVOKE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRevokeRoleBinding))
	admin.GET("/users", s.requirePermission(PermissionPlatformUsersRead), httpserver.AdaptFunc(s.adminUsers))
	admin.GET("/user-code-lookups/:user_code", httpserver.AdaptFunc(s.adminUserByCode))
	admin.GET("/users/:user_id/memberships", s.requirePermission(PermissionPlatformUsersRead), httpserver.AdaptFunc(s.adminUserMemberships))
	admin.PATCH("/users/:user_id/status", s.requirePermission(PermissionPlatformUsersStatusManage), approvalpolicy.Require("ACCOUNT_USER_STATUS_CHANGE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminSetUserStatus))
	admin.GET("/organizations", s.requirePermission(PermissionPlatformOrganizationsRead), httpserver.AdaptFunc(s.adminOrganizations))
	admin.POST("/organizations", s.requirePermission(PermissionPlatformOrganizationsCreate), approvalpolicy.Require("ACCOUNT_ORGANIZATION_CREATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminCreateOrganization))
	admin.PATCH("/organizations/:organization_id", s.requirePermission(PermissionPlatformOrganizationsProfileManage), approvalpolicy.Require("ACCOUNT_ORGANIZATION_RENAME", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRenameOrganization))
	admin.PATCH("/organizations/:organization_id/status", s.requirePermission(PermissionPlatformOrganizationsStatusManage), approvalpolicy.Require("ACCOUNT_ORGANIZATION_STATUS_CHANGE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminSetOrganizationStatus))
	admin.GET("/organizations/:organization_id/deletion-readiness", s.requirePermission(PermissionPlatformOrganizationsDelete), httpserver.AdaptFunc(s.adminOrganizationDeletionReadiness))
	admin.DELETE("/organizations/:organization_id", s.requirePermission(PermissionPlatformOrganizationsDelete), approvalpolicy.Require("ACCOUNT_ORGANIZATION_DELETE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminDeleteOrganization))
	admin.GET("/organizations/:organization_id/units", s.requirePermission(PermissionPlatformOrganizationsRead), httpserver.AdaptFunc(s.adminUnits))
	admin.GET("/organizations/:organization_id/members", s.requirePermission(PermissionPlatformOrganizationsRead), httpserver.AdaptFunc(s.adminMembers))
	admin.POST("/organizations/:organization_id/members", s.requirePermission(PermissionPlatformOrganizationsMembersManage), approvalpolicy.Require("ACCOUNT_MEMBER_ADD", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminAddMember))
	admin.PUT("/organizations/:organization_id/members/:user_id/roles", s.requirePermission(PermissionPlatformOrganizationsMembersManage), approvalpolicy.Require("ACCOUNT_MEMBER_ROLES_REPLACE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminReplaceMemberRoles))
	admin.DELETE("/organizations/:organization_id/members/:user_id", s.requirePermission(PermissionPlatformOrganizationsMembersManage), approvalpolicy.Require("ACCOUNT_MEMBER_REMOVE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRemoveMember))
	admin.POST("/organizations/:organization_id/ownership", s.requirePermission(PermissionPlatformOrganizationsOwnerTransfer), approvalpolicy.Require("ACCOUNT_OWNERSHIP_TRANSFER", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminTransferOwnership))
	admin.POST("/organizations/:organization_id/units", s.requirePermission(PermissionPlatformOrganizationsUnitsManage), approvalpolicy.Require("ACCOUNT_UNIT_CREATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminCreateUnit))
	admin.PATCH("/organizations/:organization_id/units/:unit_id", s.requirePermission(PermissionPlatformOrganizationsUnitsManage), approvalpolicy.Require("ACCOUNT_UNIT_RENAME", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRenameUnit))
	admin.PATCH("/organizations/:organization_id/units/:unit_id/parent", s.requirePermission(PermissionPlatformOrganizationsUnitsManage), approvalpolicy.Require("ACCOUNT_UNIT_MOVE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminMoveUnit))
	admin.DELETE("/organizations/:organization_id/units/:unit_id", s.requirePermission(PermissionPlatformOrganizationsUnitsManage), approvalpolicy.Require("ACCOUNT_UNIT_DELETE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminDeleteUnit))
	admin.GET("/audit-logs", s.requirePermission(PermissionPlatformAuditRead), httpserver.AdaptFunc(s.adminAuditLogs))
	admin.GET("/audit-logs/:audit_id", s.requirePermission(PermissionPlatformAuditRead), httpserver.AdaptFunc(s.adminAuditLog))
	if s.auditExports != nil {
		admin.POST("/audit-exports", s.requirePermission(PermissionPlatformAuditExport), approvalpolicy.Require("ACCOUNT_AUDIT_EXPORT_CREATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.createAuditExport))
		admin.GET("/audit-exports/:export_id", s.requirePermission(PermissionPlatformAuditExport), httpserver.AdaptFunc(s.getAuditExport))
		admin.POST("/audit-exports/:export_id/download-tickets", s.requirePermission(PermissionPlatformAuditExport), approvalpolicy.Require("ACCOUNT_AUDIT_EXPORT_DOWNLOAD_TICKET", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.issueAuditExportDownloadTicket))
		admin.GET("/audit-exports/:export_id/download", s.requirePermission(PermissionPlatformAuditExport), httpserver.AdaptFunc(s.downloadAuditExport))
	}
	admin.GET("/approval-policies", s.requirePermission(PermissionPlatformRolesRead), httpserver.AdaptFunc(s.adminApprovalPolicies))
	admin.GET("/approval-requests", httpserver.AdaptFunc(s.adminApprovalRequests))
	admin.POST("/approval-requests", approvalpolicy.Require("APPROVAL_DRAFT_CREATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminCreateApprovalDraft))
	admin.GET("/approval-requests/:approval_request_id", httpserver.AdaptFunc(s.adminApprovalRequest))
	admin.PUT("/approval-requests/:approval_request_id", approvalpolicy.Require("APPROVAL_DRAFT_UPDATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminUpdateApprovalDraft))
	admin.POST("/approval-requests/:approval_request_id/actions/submit", approvalpolicy.Require("APPROVAL_REQUEST_SUBMIT", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminSubmitApprovalRequest))
	admin.POST("/approval-requests/:approval_request_id/actions/withdraw", approvalpolicy.Require("APPROVAL_REQUEST_WITHDRAW", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminWithdrawApprovalRequest))
	admin.POST("/approval-requests/:approval_request_id/actions/decide", approvalpolicy.Require("APPROVAL_REQUEST_DECIDE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminDecideApprovalRequest))
	admin.GET("/registration-operations", s.requirePermission(PermissionPlatformRegistrationsRepair), httpserver.AdaptFunc(s.adminRegistrationOperations))
	admin.POST("/registration-operations/:operation_id/actions/retry", s.requirePermission(PermissionPlatformRegistrationsRepair), approvalpolicy.Require("ACCOUNT_REGISTRATION_RETRY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRetryRegistration))
	admin.POST("/registration-operations/:operation_id/actions/compensate", s.requirePermission(PermissionPlatformRegistrationsRepair), approvalpolicy.Require("ACCOUNT_REGISTRATION_COMPENSATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminCompensateRegistration))
	admin.GET("/account-closure-requests", s.requirePermission(PermissionPlatformAccountClosuresRead), httpserver.AdaptFunc(s.adminAccountClosures))
	admin.POST("/account-closure-requests/:closure_request_id/actions/retry", s.requirePermission(PermissionPlatformAccountClosuresRepair), approvalpolicy.Require("ACCOUNT_CLOSURE_RETRY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRetryAccountClosure))
	if s.profiles != nil {
		admin.GET("/profile-change-requests", s.requirePermission(PermissionPlatformProfileChangesRepair), httpserver.AdaptFunc(s.adminProfileChanges))
		admin.POST("/profile-change-requests/:request_id/actions/retry", s.requirePermission(PermissionPlatformProfileChangesRepair), approvalpolicy.Require("ACCOUNT_PROFILE_CHANGE_RETRY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRetryProfileChange))
	}
	return router
}

func (s *Service) listUnits(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	rootUnitID, err := optionalQueryUUID(r, "root_unit_id")
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid resource identifier", httpserver.RequestID(r.Context()))
		return
	}
	tree, err := s.repository.ListUnits(r.Context(), mustPrincipal(r.Context()), organizationID, rootUnitID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, tree)
}

func (s *Service) listMembers(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	unitID, err := optionalQueryUUID(r, "unit_id")
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid resource identifier", httpserver.RequestID(r.Context()))
		return
	}
	members, err := s.repository.ListMembers(r.Context(), mustPrincipal(r.Context()), organizationID, unitID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": members})
}

func (s *Service) listQuotaMembers(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	members, err := s.repository.ListQuotaMembers(r.Context(), mustPrincipal(r.Context()), organizationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": members})
}

func (s *Service) listMyRoleBindings(w http.ResponseWriter, r *http.Request) {
	items, err := s.repository.ListMyRoleBindings(r.Context(), mustPrincipal(r.Context()))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) listOrganizationRoleBindings(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	items, err := s.repository.ListOrganizationRoleBindings(r.Context(), mustPrincipal(r.Context()), organizationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

type roleBindingGrantBody struct {
	UserID         uuid.UUID  `json:"user_id"`
	RoleCode       string     `json:"role_code"`
	OrganizationID *uuid.UUID `json:"organization_id"`
	UnitID         *uuid.UUID `json:"unit_id"`
	StartsAt       *time.Time `json:"starts_at"`
	ExpiresAt      *time.Time `json:"expires_at"`
	IdempotencyKey string     `json:"idempotency_key"`
	Reason         string     `json:"reason"`
}

func (s *Service) grantOrganizationRoleBinding(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body roleBindingGrantBody
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	item, err := s.repository.GrantOrganizationRoleBinding(r.Context(), mustPrincipal(r.Context()), RoleBindingGrant{UserID: body.UserID, RoleCode: body.RoleCode, OrganizationID: &organizationID, UnitID: body.UnitID, StartsAt: body.StartsAt, ExpiresAt: body.ExpiresAt, IdempotencyKey: body.IdempotencyKey, Reason: body.Reason})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusCreated, item)
}

func (s *Service) revokeOrganizationRoleBinding(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	bindingID, ok := pathUUID(w, r, "binding_id")
	if !ok {
		return
	}
	item, err := s.repository.RevokeOrganizationRoleBinding(r.Context(), mustPrincipal(r.Context()), organizationID, bindingID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, item)
}

func (s *Service) replaceMemberRoles(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	userID, ok := pathUUID(w, r, "user_id")
	if !ok {
		return
	}
	var body struct {
		Roles []string `json:"roles"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	member, err := s.repository.ReplaceMemberRoles(r.Context(), mustPrincipal(r.Context()), organizationID, userID, body.Roles)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, member)
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

func (s *Service) getCommerceCustomerMapping(w http.ResponseWriter, r *http.Request) {
	mapping, err := s.repository.GetCommerceCustomerMapping(r.Context(), mustPrincipal(r.Context()))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, mapping)
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

func (s *Service) listOrganizations(w http.ResponseWriter, r *http.Request) {
	organizations, err := s.repository.ListOrganizations(r.Context(), mustPrincipal(r.Context()))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": organizations})
}

func (s *Service) setActiveContext(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ContextType            string     `json:"context_type"`
		OrganizationID         *uuid.UUID `json:"organization_id"`
		ExpectedContextVersion int64      `json:"expected_context_version"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.ExpectedContextVersion < 1 || (body.ContextType != "PERSONAL" && body.ContextType != "ORGANIZATION") || (body.ContextType == "PERSONAL" && body.OrganizationID != nil) || (body.ContextType == "ORGANIZATION" && body.OrganizationID == nil) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid active context", httpserver.RequestID(r.Context()))
		return
	}
	context, err := s.repository.SetActiveContext(r.Context(), mustPrincipal(r.Context()), body.OrganizationID, body.ExpectedContextVersion)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("X-Context-Version", stringInt64(context.ContextVersion))
	w.Header().Set("X-Authorization-Version", stringInt64(context.AuthorizationVersion))
	w.Header().Set("X-Personal-Authorization-Version", stringInt64(context.PersonalAuthorizationVersion))
	w.Header().Set("X-Platform-Authorization-Version", stringInt64(context.PlatformAuthorizationVersion))
	w.Header().Set("X-Organization-Authorization-Version", stringInt64(context.OrganizationAuthorizationVersion))
	api.WriteJSON(w, http.StatusOK, context)
}

func (s *Service) createUnit(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body struct {
		ParentID       *uuid.UUID `json:"parent_id"`
		Name           string     `json:"name"`
		IdempotencyKey string     `json:"idempotency_key"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid organization unit request", httpserver.RequestID(r.Context()))
		return
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, body.IdempotencyKey, "unit-create:"+organizationID.String()+":"+strings.TrimSpace(body.Name)+":"+fmt.Sprint(body.ParentID))
	if !validOrganizationUnitIdempotencyKey(idempotencyKey) {
		api.WriteError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "an idempotency key between 8 and 120 characters is required", httpserver.RequestID(r.Context()))
		return
	}
	result, err := s.repository.CreateUnit(r.Context(), mustPrincipal(r.Context()), organizationID, body.ParentID, body.Name, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if _, err := s.applyOrganizationUnitLifecycle(r.Context(), organizationID, result.Unit.ID, "ACTIVE", result.StructureVersion); err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_UNIT_LIFECYCLE_UNAVAILABLE", "the unit was created but dependent lifecycle activation must be retried with the same idempotency key", httpserver.RequestID(r.Context()))
		return
	}
	status := http.StatusCreated
	if result.Replayed {
		status = http.StatusOK
	}
	api.WriteJSON(w, status, result)
}

func (s *Service) moveUnit(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	unitID, ok := pathUUID(w, r, "unit_id")
	if !ok {
		return
	}
	var body struct {
		ParentID                 *uuid.UUID `json:"parent_id"`
		ExpectedStructureVersion int64      `json:"expected_structure_version"`
		ExpectedVersion          int64      `json:"expected_version"`
		IdempotencyKey           string     `json:"idempotency_key"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid unit move request", httpserver.RequestID(r.Context()))
		return
	}
	if body.ExpectedStructureVersion == 0 {
		body.ExpectedStructureVersion = body.ExpectedVersion
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, body.IdempotencyKey, "unit-move:"+organizationID.String()+":"+unitID.String()+":"+fmt.Sprint(body.ParentID)+":"+fmt.Sprint(body.ExpectedStructureVersion))
	if body.ExpectedStructureVersion < 1 || !validOrganizationUnitIdempotencyKey(idempotencyKey) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "expected_structure_version and a valid idempotency key are required", httpserver.RequestID(r.Context()))
		return
	}
	result, err := s.repository.MoveUnit(r.Context(), mustPrincipal(r.Context()), organizationID, unitID, body.ParentID, body.ExpectedStructureVersion, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) createInvitation(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body struct {
		TargetType     string   `json:"target_type"`
		Target         string   `json:"target"`
		Email          string   `json:"email"`
		Roles          []string `json:"roles"`
		ExpiresInHours int      `json:"expires_in_hours"`
		IdempotencyKey string   `json:"idempotency_key"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.ExpiresInHours < 1 || body.ExpiresInHours > 24*30 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid invitation request", httpserver.RequestID(r.Context()))
		return
	}
	if strings.TrimSpace(body.TargetType) == "" && strings.TrimSpace(body.Email) != "" {
		body.TargetType, body.Target = "EMAIL", body.Email
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, body.IdempotencyKey, "invitation-create:"+organizationID.String()+":"+strings.ToUpper(strings.TrimSpace(body.TargetType))+":"+strings.TrimSpace(body.Target))
	invitation, err := s.repository.CreateInvitation(r.Context(), mustPrincipal(r.Context()), organizationID, InvitationCreateRequest{TargetType: body.TargetType, Target: body.Target, Roles: body.Roles, ExpiresInHours: body.ExpiresInHours, IdempotencyKey: idempotencyKey})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, invitation)
}

func (s *Service) listMyInvitations(w http.ResponseWriter, r *http.Request) {
	items, err := s.repository.ListMyInvitations(r.Context(), mustPrincipal(r.Context()))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) revokeInvitation(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	invitationID, ok := pathUUID(w, r, "invitation_id")
	if !ok {
		return
	}
	invitation, err := s.repository.RevokeInvitation(r.Context(), mustPrincipal(r.Context()), organizationID, invitationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, invitation)
}

func (s *Service) acceptInvitation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token        string     `json:"token"`
		InvitationID *uuid.UUID `json:"invitation_id"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || (strings.TrimSpace(body.Token) == "") == (body.InvitationID == nil) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid invitation token", httpserver.RequestID(r.Context()))
		return
	}
	result, err := s.repository.AcceptInvitation(r.Context(), mustPrincipal(r.Context()), body.Token, body.InvitationID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.applyOrganizationMemberLifecycle(r.Context(), result, "ACTIVE"); err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_MEMBER_LIFECYCLE_UNAVAILABLE", "the invitation was accepted locally but grant activation must be retried", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) revokeMembership(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	userID, ok := pathUUID(w, r, "user_id")
	if !ok {
		return
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, r.URL.Query().Get("idempotency_key"), "membership-remove:"+organizationID.String()+":"+userID.String())
	result, err := s.repository.RevokeMembership(r.Context(), mustPrincipal(r.Context()), organizationID, userID, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := s.applyOrganizationMemberLifecycle(r.Context(), result, "REVOKED"); err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_MEMBER_LIFECYCLE_UNAVAILABLE", "the membership was revoked locally but grant suspension must be retried with the same idempotency key", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) transferOwnership(w http.ResponseWriter, r *http.Request) {
	organizationID, ok := pathUUID(w, r, "organization_id")
	if !ok {
		return
	}
	var body struct {
		NewOwnerUserID uuid.UUID `json:"new_owner_user_id"`
		IdempotencyKey string    `json:"idempotency_key"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.NewOwnerUserID == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid ownership transfer request", httpserver.RequestID(r.Context()))
		return
	}
	idempotencyKey := organizationUnitIdempotencyKey(r, body.IdempotencyKey, "ownership-transfer:"+organizationID.String()+":"+body.NewOwnerUserID.String())
	result, err := s.repository.TransferOwnership(r.Context(), mustPrincipal(r.Context()), organizationID, body.NewOwnerUserID, idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
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
	case errors.Is(err, ErrInvalidApprovalInput):
		status, code, message = http.StatusBadRequest, "INVALID_APPROVAL_REQUEST", "the approval request is invalid"
	case errors.Is(err, ErrRecentAuthenticationRequired):
		status, code, message = http.StatusUnauthorized, "RECENT_AUTHENTICATION_REQUIRED", "authentication within the last 15 minutes is required"
	case errors.Is(err, ErrAuditExportQuota):
		status, code, message = http.StatusTooManyRequests, "AUDIT_EXPORT_QUOTA_EXCEEDED", "the audit export limit was exceeded"
	case errors.Is(err, ErrLastOwner):
		status, code, message = http.StatusConflict, "LAST_OWNER_REQUIRED", "the organization must retain at least one owner"
	case errors.Is(err, ErrLastActiveSuperAdmin):
		status, code, message = http.StatusConflict, "LAST_ACTIVE_SUPER_ADMIN", "another active user must become super administrator first"
	case errors.Is(err, ErrInvalidHierarchy):
		status, code, message = http.StatusUnprocessableEntity, "ORG_UNIT_HIERARCHY_INVALID", "the requested organization hierarchy is invalid"
	case errors.Is(err, ErrInvitationExpired):
		status, code, message = http.StatusGone, "INVITATION_EXPIRED", "the invitation has expired"
	case errors.Is(err, ErrAccountProvisioning):
		status, code, message = http.StatusConflict, "ACCOUNT_PROVISIONING", "the account is still provisioning"
	case errors.Is(err, ErrAccountRepairRequired):
		status, code, message = http.StatusConflict, "ACCOUNT_REPAIR_REQUIRED", "the account initialization requires repair"
	case errors.Is(err, ErrAccountClosing):
		status, code, message = http.StatusConflict, "ACCOUNT_CLOSING", "the account is in its closure quiet period"
	case errors.Is(err, ErrBootstrapPending):
		status, code, message = http.StatusForbidden, "ADMIN_BOOTSTRAP_PENDING", "the first administrator must complete secure activation"
	case errors.Is(err, ErrApprovalSelfDecision):
		status, code, message = http.StatusConflict, "APPROVAL_SELF_DECISION_FORBIDDEN", "the requester cannot decide their own approval request"
	case errors.Is(err, ErrApprovalExpired):
		status, code, message = http.StatusGone, "APPROVAL_REQUEST_EXPIRED", "the approval request has expired"
	case errors.Is(err, ErrApprovalInvalidated):
		status, code, message = http.StatusConflict, "APPROVAL_REQUEST_INVALIDATED", "the approval request no longer matches current policy, authorization, or resource facts"
	case errors.Is(err, ErrApprovalSnapshotTimeout):
		status, code, message = http.StatusGatewayTimeout, "APPROVAL_SNAPSHOT_TIMEOUT", "the authoritative approval snapshot timed out"
	case errors.Is(err, ErrApprovalSnapshotUnavailable):
		status, code, message = http.StatusServiceUnavailable, "APPROVAL_SNAPSHOT_UNAVAILABLE", "the authoritative approval snapshot is temporarily unavailable"
	case errors.Is(err, ErrApprovalWriterUnavailable):
		w.Header().Set("Retry-After", "5")
		w.Header().Set("Cache-Control", "no-store")
		status, code, message = http.StatusServiceUnavailable, "APPROVAL_WRITER_UNAVAILABLE", "the approval authorization writer is temporarily unavailable"
	case errors.Is(err, ErrApprovalSnapshotRejected):
		status, code, message = http.StatusConflict, "APPROVAL_SNAPSHOT_REJECTED", "the authoritative approval snapshot request was rejected"
	case errors.Is(err, ErrApprovalCommandNotExecutable):
		status, code, message = http.StatusConflict, "APPROVAL_COMMAND_NOT_EXECUTABLE", "approval command is not executable"
	case errors.Is(err, ErrTokenGovernanceInactive):
		status, code, message = http.StatusServiceUnavailable, "TOKEN_COMMAND_GOVERNANCE_INACTIVE", "Token command governance is not active"
	case errors.Is(err, ErrBillingGovernanceInactive):
		status, code, message = http.StatusServiceUnavailable, "BILLING_COMMAND_GOVERNANCE_INACTIVE", "Billing command governance is not active"
	case errors.Is(err, ErrBillingGovernanceUnavailable):
		status, code, message = http.StatusServiceUnavailable, "BILLING_COMMAND_GOVERNANCE_UNAVAILABLE", "Billing command governance is temporarily unavailable"
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
	parsed, err := url.Parse(strings.TrimRight(strings.TrimSpace(rawURL), "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, errors.New("BILLING_HTTP_URL must be an absolute URL")
	}
	if strings.TrimSpace(serviceToken) == "" {
		return nil, errors.New("INTERNAL_SERVICE_TOKEN is required")
	}
	return &HTTPWalletProvisioner{baseURL: parsed, serviceToken: serviceToken, client: observability.NewHTTPClient(5 * time.Second)}, nil
}

func (p *HTTPWalletProvisioner) EnsurePersonalWallet(ctx context.Context, userID uuid.UUID) error {
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
	return nil
}
