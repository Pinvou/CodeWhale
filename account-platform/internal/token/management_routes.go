package token

import (
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
)

func (s *Service) registerManagementRoutes(router *httpserver.Router, personal *httpserver.RouteGroup) {
	if !s.managementEnabled {
		return
	}
	protected := router.Group("")
	protected.Use(identity.GinMiddleware(s.managementVerify))
	internal := router.Group("/internal/v1")
	internal.Use(serviceauth.GinMiddleware(s.managementServiceToken))
	internal.GET("/users/:user_id/account-closure-assessment", serviceauth.Require("internal.token.account-closure.assess"), httpserver.AdaptFunc(s.accountClosureAssessment))
	internal.DELETE("/users/:user_id/replay-payloads", serviceauth.Require("internal.token.account-closure.replay-clear"), httpserver.AdaptFunc(s.anonymizeUserReplayPayloads))
	internal.PUT("/organizations/:organization_id/lifecycle", serviceauth.Require("internal.token.organization.lifecycle"), approvalpolicy.Require("TOKEN_ORGANIZATION_LIFECYCLE_APPLY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.applyOrganizationLifecycle))
	internal.PUT("/organizations/:organization_id/units/:unit_id/lifecycle", serviceauth.Require("internal.token.organization-unit.lifecycle"), approvalpolicy.Require("TOKEN_ORGANIZATION_UNIT_LIFECYCLE_APPLY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.organizationUnitDeletionBlockers))
	internal.POST("/holds/expiry-proof", serviceauth.Require("internal.token.hold-expiry.prove"), httpserver.AdaptFunc(s.expiredHoldProof))
	internal.GET("/token-governance/activation", serviceauth.Require("internal.authorization.check"), httpserver.AdaptFunc(s.internalTokenGovernanceActivation))
	internal.GET("/approval-effects/:approval_request_id", serviceauth.Require("internal.token.approval-effect.read"), httpserver.AdaptFunc(s.approvalEffectObservation))
	internal.POST("/commands/model-lifecycle/execute", serviceauth.Require("internal.token.model-lifecycle.execute"), s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.executeApprovedModelLifecycle))
	internal.POST("/commands/catalog-rollback/execute", serviceauth.Require("internal.token.catalog-rollback.execute"), s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.executeApprovedCatalogRollback))
	internal.POST("/commands/credential-revoke/execute", serviceauth.Require("internal.token.credential-revoke.execute"), s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.executeApprovedCredentialRevoke))
	internal.POST("/commands/model-lifecycle/precheck", serviceauth.Require("internal.token.model-lifecycle.execute"), s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.approvedTokenCommandPrecheck("TOKEN_MODEL_LIFECYCLE_CHANGE")))
	internal.POST("/commands/catalog-rollback/precheck", serviceauth.Require("internal.token.catalog-rollback.execute"), s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.approvedTokenCommandPrecheck("TOKEN_MODEL_CATALOG_ROLLBACK")))
	internal.POST("/commands/credential-revoke/precheck", serviceauth.Require("internal.token.credential-revoke.execute"), s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.approvedTokenCommandPrecheck("TOKEN_CREDENTIAL_REVOKE")))
	internal.GET("/commands/model-lifecycle/result", serviceauth.Require("internal.token.model-lifecycle.execute"), s.requireKnownTokenCommandGovernance(), httpserver.AdaptFunc(s.approvedTokenCommandResult("TOKEN_MODEL_LIFECYCLE_CHANGE")))
	internal.GET("/commands/catalog-rollback/result", serviceauth.Require("internal.token.catalog-rollback.execute"), s.requireKnownTokenCommandGovernance(), httpserver.AdaptFunc(s.approvedTokenCommandResult("TOKEN_MODEL_CATALOG_ROLLBACK")))
	internal.GET("/commands/credential-revoke/result", serviceauth.Require("internal.token.credential-revoke.execute"), s.requireKnownTokenCommandGovernance(), httpserver.AdaptFunc(s.approvedTokenCommandResult("TOKEN_CREDENTIAL_REVOKE")))
	internal.GET("/approval-recovery/results/:approval_request_id", serviceauth.Require("internal.token.approval-result.read"), httpserver.AdaptFunc(s.tokenApprovalRecoveryResult))
	internal.POST("/approval-command-prechecks", serviceauth.Require("internal.token.approval-snapshot.read"), httpserver.AdaptFunc(s.internalApprovalCommandPrecheck))
	internal.POST("/approval-command-proofs", serviceauth.Require("internal.token.approval-proof.read"), httpserver.AdaptFunc(s.internalTokenApprovalProof))
	// Cancellation is actor-bound in the repository and deliberately remains
	// reachable after a management permission or feature-gate revocation so an
	// already issued one-time launch can always be terminalized.
	protected.POST("/admin/v1/credential-results/:result_ref/revoke-window", httpserver.AdaptFunc(s.adminRevokeCredentialLaunch))
	admin := protected.Group("/admin/v1")
	if s.authorizer != nil {
		admin.GET("/overview", s.authorizer.RequireAny("platform.tokens.read", "platform.usage.read", "platform.system.status.read"), httpserver.AdaptFunc(s.adminOverview))
		admin.GET("/operations-overview", s.authorizer.RequireAnyAnd([]string{"platform.models.read"}, []string{"platform.tokens.read", "platform.usage.read"}), httpserver.AdaptFunc(s.adminTokenOperationsOverview))
		admin.GET("/overviews/model-catalog", s.authorizer.RequireAny("platform.models.read"), httpserver.AdaptFunc(s.adminModelCatalogOverview))
		admin.GET("/overviews/token-resources", s.authorizer.RequireAny("platform.tokens.read"), httpserver.AdaptFunc(s.adminTokenResourcesOverview))
		admin.GET("/token-resources/readiness", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminTokenResourceReadiness))
		admin.GET("/overviews/model-operations", s.authorizer.RequireAny("platform.tokens.read", "platform.usage.read"), httpserver.AdaptFunc(s.adminModelOperationsPageOverview))
		admin.GET("/usage-requests", s.authorizer.Require("platform.usage.read"), httpserver.AdaptFunc(s.adminUsageRequests))
		admin.GET("/usage-requests/:usage_request_id/evidence", s.authorizer.Require("platform.usage.read"), httpserver.AdaptFunc(s.adminUsageEvidence))
		admin.GET("/reconciliation-cases", s.authorizer.Require("platform.usage.read"), httpserver.AdaptFunc(s.adminReconciliationCases))
		admin.GET("/usage-requests/:usage_request_id", s.authorizer.Require("platform.usage.read"), httpserver.AdaptFunc(s.adminUsageRequest))
		admin.GET("/commands/TOKEN_USAGE_RECONCILIATION_RETRY/readiness", s.authorizer.Require("platform.usage.reconcile.retry"), httpserver.AdaptFunc(s.adminUsageReconciliationCommandReadiness))
		admin.GET("/token-accounts", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminTokenAccounts))
		admin.GET("/token-accounts/:token_account_id", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminTokenAccount))
		admin.GET("/token-accounts/:token_account_id/credentials", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminTokenCredentials))
		admin.GET("/token-accounts/:token_account_id/routes", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminTokenRoutes))
		admin.POST("/token-accounts/:token_account_id/credential-launches/enroll", s.authorizer.Require("platform.tokens.manage"), approvalpolicy.Require("TOKEN_CREDENTIAL_ENROLL", approvalpolicy.AuditOnly), s.requireCredentialIsolationEnabled(), s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.registeredTokenHandler("TOKEN_CREDENTIAL_ENROLL", "ISOLATED")))
		admin.POST("/token-accounts/:token_account_id/credential-launches/rotate", s.authorizer.Require("platform.tokens.manage"), approvalpolicy.Require("TOKEN_CREDENTIAL_ROTATE", approvalpolicy.AuditOnly), s.requireCredentialIsolationEnabled(), s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.registeredTokenHandler("TOKEN_CREDENTIAL_ROTATE", "ISOLATED")))
		admin.GET("/credential-results/:result_ref", s.authorizer.Require("platform.tokens.read"), s.requireCredentialIsolationEnabled(), httpserver.AdaptFunc(s.adminCredentialIsolationResult))
		admin.GET("/protocols", s.authorizer.Require("platform.pricing.read"), httpserver.AdaptFunc(s.adminProtocols))
		admin.GET("/formulas", s.authorizer.Require("platform.pricing.read"), httpserver.AdaptFunc(s.adminFormulas))
		admin.GET("/formulas/:formula_version_id/preview", s.authorizer.Require("platform.pricing.read"), httpserver.AdaptFunc(s.adminPreviewFormula))
		admin.GET("/models", s.authorizer.Require("platform.models.read"), httpserver.AdaptFunc(s.adminCatalogModels))
		admin.GET("/models/:model_id", s.authorizer.Require("platform.models.read"), httpserver.AdaptFunc(s.adminModelDetail))
		admin.GET("/models/:model_id/versions", s.authorizer.Require("platform.models.read"), httpserver.AdaptFunc(s.adminModelVersions))
		admin.GET("/models/:model_id/lifecycle-plans", s.authorizer.Require("platform.models.read"), httpserver.AdaptFunc(s.adminLifecyclePlans))
		admin.GET("/models/:model_id/lifecycle-plans/:lifecycle_plan_id", s.authorizer.Require("platform.models.read"), httpserver.AdaptFunc(s.adminLifecyclePlan))
		admin.POST("/models/:model_id/lifecycle-plans", s.authorizer.Require("platform.models.lifecycle.request"), s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.adminCreateLifecyclePlan))
		admin.POST("/models/:model_id/lifecycle-plans/:lifecycle_plan_id/cancel", s.authorizer.RequireAny("platform.models.manage", "platform.models.lifecycle.request"), s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.adminCancelLifecyclePlan))
		admin.GET("/models/:model_id/impact/lifecycle", s.authorizer.Require("platform.models.read"), httpserver.AdaptFunc(s.adminModelLifecycleImpact))
		admin.GET("/model-catalog/impact/rollback", s.authorizer.Require("platform.models.read"), httpserver.AdaptFunc(s.adminCatalogRollbackImpact))
		admin.GET("/provisionings", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminProvisionings))
		admin.GET("/provisionings/:provisioning_id", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminProvisioning))
		admin.GET("/provisionings/:provisioning_id/attempts", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminProvisioningAttempts))
		admin.GET("/owner-candidates", s.authorizer.Require("platform.tokens.owner-candidates.read"), httpserver.AdaptFunc(s.adminOwnerCandidates))
		admin.GET("/resource-audit", s.authorizer.RequireAnyAnd([]string{"platform.models.read", "platform.tokens.read"}, []string{"platform.audit.read"}), httpserver.AdaptFunc(s.adminResourceAudit))
		admin.POST("/commands:precheck", s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.adminTokenCommandPrecheck))
		admin.POST("/commands", s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.adminCreateTokenCommand))
		admin.GET("/commands/by-idempotency-key", s.requireKnownTokenCommandGovernance(), httpserver.AdaptFunc(s.adminTokenCommandByIdempotencyKey))
		admin.GET("/commands/recovery-queue", s.requireKnownTokenCommandGovernance(), httpserver.AdaptFunc(s.adminTokenAccountRecoveryQueue))
		admin.GET("/commands/:command_reference", s.requireKnownTokenCommandGovernance(), httpserver.AdaptFunc(s.adminTokenCommandResult))
		admin.POST("/commands/:command_reference/recovery-actions", s.requireNewTokenCommandGovernanceActive(), httpserver.AdaptFunc(s.adminTokenAccountRecoveryAction))
		admin.GET("/commands/:command_reference/recovery-actions/by-idempotency-key", s.requireKnownTokenCommandGovernance(), httpserver.AdaptFunc(s.adminTokenAccountRecoveryActionByKey))
		admin.GET("/commands/:command_reference/recovery-actions/:action_id", s.requireKnownTokenCommandGovernance(), httpserver.AdaptFunc(s.adminTokenAccountRecoveryActionResult))
		admin.GET("/token-accounts/:token_account_id/impact/status", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminTokenAccountStatusImpact))
		admin.GET("/token-accounts/:token_account_id/impact/route", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminTokenRouteImpact))
		admin.GET("/token-accounts/:token_account_id/impact/credential", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminTokenCredentialImpact))
	}
	adminV2 := protected.Group("/admin/v2")
	if s.authorizer != nil {
		adminV2.GET("/token-accounts/:token_account_id", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminTokenAccountV2))
		adminV2.GET("/provisionings/:provisioning_id", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminProvisioningV2))
		adminV2.GET("/token-accounts/:token_account_id/impact/route", s.authorizer.Require("platform.tokens.read"), httpserver.AdaptFunc(s.adminStableTokenRouteImpact))
	}
}
