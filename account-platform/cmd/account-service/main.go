package main

import (
	"context"
	"encoding/base64"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/clientcompat"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cmall"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/humandelegation"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/manualreview"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/tokengovernance"
	"github.com/Pinvou/pinvou3/account-platform/internal/token"
)

func main() {
	if err := httpserver.ConfigureProcessLogging("account-service"); err != nil {
		log.Fatal(err)
	}
	if err := approvalpolicy.ValidateAll(map[string]approvalpolicy.Mode{"ACCOUNT_WALLET_PROVISION": approvalpolicy.AuditOnly, "ACCOUNT_ROLE_BINDING_EXPIRE": approvalpolicy.AuditOnly, "ACCOUNT_APPROVAL_EXPIRE": approvalpolicy.AuditOnly, "ACCOUNT_PROFILE_CHANGE_RECOVERY": approvalpolicy.AuditOnly, "ACCOUNT_REGISTRATION_RECOVERY": approvalpolicy.AuditOnly, "ACCOUNT_COMMERCE_CUSTOMER_SYNC": approvalpolicy.AuditOnly, "ACCOUNT_CLOSURE_RETRY": approvalpolicy.AuditOnly}); err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	cfg, err := config.Load("account-service")
	if err != nil {
		log.Fatal(err)
	}
	onCallSchedule, err := manualreview.ParseOnCallSchedule(os.Getenv("MANUAL_REVIEW_ON_CALL_USER_IDS"), strings.HasPrefix(strings.ToLower(cfg.Environment), "production"))
	if err != nil {
		log.Fatal(err)
	}
	repository, err := account.NewPostgresRepository(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer repository.Close()
	walletPolicyVersion := strings.TrimSpace(os.Getenv("WALLET_PROVISIONING_POLICY_VERSION"))
	if walletPolicyVersion == "" {
		walletPolicyVersion = "wallet-provisioning-v1"
	}
	walletSLA := 15 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("WALLET_PROVISIONING_SLA_MINUTES")); raw != "" {
		minutes, parseErr := strconv.Atoi(raw)
		if parseErr != nil {
			log.Fatal("WALLET_PROVISIONING_SLA_MINUTES must be an integer")
		}
		walletSLA = time.Duration(minutes) * time.Minute
	}
	if err := repository.ConfigureWalletProvisioning(walletPolicyVersion, walletSLA); err != nil {
		log.Fatal(err)
	}
	commerceProviderID := valueOrDefault("COMMERCE_PROVIDER_ID", "h3c_mall")
	commerceAdapterVersion := valueOrDefault("COMMERCE_ADAPTER_VERSION", "h3c-mall-v1")
	if err := repository.ConfigureCommerceProvider(commerceProviderID, commerceAdapterVersion); err != nil {
		log.Fatal(err)
	}
	if err := repository.EnsureCommerceProviderMappings(ctx); err != nil {
		log.Fatal(err)
	}
	verify, err := identity.NewOIDCVerifyFuncWithJWKS(ctx, os.Getenv("OIDC_ISSUER"), os.Getenv("OIDC_JWKS_URL"), os.Getenv("OIDC_AUDIENCE"), cfg.Environment)
	if err != nil {
		log.Fatal(err)
	}
	serviceCredential := serviceauth.CredentialFromEnvironment()
	policyDigest, err := approvalpolicy.PolicySetDigest()
	if err != nil {
		log.Fatal(err)
	}
	handlerDigest, err := token.TokenCommandHandlerDigest()
	if err != nil {
		log.Fatal(err)
	}
	governance, err := tokengovernance.NewClient(os.Getenv("TOKEN_GATEWAY_HTTP_URL"), serviceCredential, policyDigest, handlerDigest)
	if err != nil {
		log.Fatal("Token governance reader configuration is invalid: ", err)
	}
	repository.WithTokenGovernanceActivation(governance)
	approvalSnapshots, err := account.NewHTTPApprovalSnapshotSource(os.Getenv("TOKEN_GATEWAY_HTTP_URL"), serviceCredential)
	if err != nil {
		log.Fatal(err)
	}
	repository.WithApprovalSnapshotSource(approvalSnapshots)
	wallets, err := account.NewHTTPWalletProvisioner(os.Getenv("BILLING_HTTP_URL"), serviceCredential)
	if err != nil {
		log.Fatal(err)
	}
	tokenOrganizationLifecycle, err := account.NewHTTPOrganizationLifecycleClient(os.Getenv("TOKEN_GATEWAY_HTTP_URL"), serviceCredential, "TOKEN")
	if err != nil {
		log.Fatal(err)
	}
	billingOrganizationLifecycle, err := account.NewHTTPOrganizationLifecycleClient(os.Getenv("BILLING_HTTP_URL"), serviceCredential, "BILLING")
	if err != nil {
		log.Fatal(err)
	}
	billingAccountClosure, err := account.NewHTTPAccountClosureClient(os.Getenv("BILLING_HTTP_URL"), serviceCredential, "BILLING")
	if err != nil {
		log.Fatal(err)
	}
	tokenAccountClosure, err := account.NewHTTPAccountClosureClient(os.Getenv("TOKEN_GATEWAY_HTTP_URL"), serviceCredential, "TOKEN")
	if err != nil {
		log.Fatal(err)
	}
	identities, err := account.NewHTTPIdentityProvisioner(os.Getenv("IDENTITY_HTTP_URL"), serviceCredential)
	if err != nil {
		log.Fatal(err)
	}
	service := account.NewService(repository, wallets).
		WithServiceToken(serviceCredential).
		WithClientCompatibility(valueOrDefault("ACCOUNT_SERVICE_VERSION", "development"), valueOrDefault("ACCOUNT_CLIENT_CONTRACT_VERSION", "account-client-v1")).
		WithOrganizationLifecycle(billingOrganizationLifecycle, tokenOrganizationLifecycle).
		WithOrganizationUnitLifecycle(billingOrganizationLifecycle, tokenOrganizationLifecycle).
		WithOrganizationMemberLifecycle(billingOrganizationLifecycle)
	ownerCandidateCursorKey, decodeErr := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(os.Getenv("OWNER_CANDIDATE_CURSOR_HMAC_KEY_BASE64")))
	if decodeErr != nil || len(ownerCandidateCursorKey) != 32 {
		log.Fatal("OWNER_CANDIDATE_CURSOR_HMAC_KEY_BASE64 must be standard Base64 encoding exactly 32 random bytes")
	}
	service.WithTokenOwnerCandidateCursorKey(ownerCandidateCursorKey)
	clear(ownerCandidateCursorKey)
	humanDelegationKey, err := humandelegation.DecodeSigningKey(os.Getenv("HUMAN_DELEGATION_SIGNING_KEY_BASE64URL"))
	if err != nil {
		log.Fatal(err)
	}
	humanDelegationSigner, err := humandelegation.NewSigner(os.Getenv("HUMAN_DELEGATION_SIGNING_KEY_ID"), humanDelegationKey)
	clear(humanDelegationKey)
	if err != nil {
		log.Fatal(err)
	}
	service.WithHumanDelegations(humanDelegationSigner)
	auditExportKey, decodeErr := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(os.Getenv("AUDIT_EXPORT_MASTER_KEY_BASE64")))
	if decodeErr != nil {
		log.Fatal("AUDIT_EXPORT_MASTER_KEY_BASE64 must be standard Base64")
	}
	auditExports, err := account.NewAuditExportService(repository, auditExportKey, cfg.Environment)
	clear(auditExportKey)
	if err != nil {
		log.Fatal(err)
	}
	service.WithAuditExports(auditExports)
	go observability.RunBackground(ctx, "audit-export", auditExports.Run)
	go observability.RunBackground(ctx, "wallet-provisioning", account.NewWalletProvisioningWorker(repository, wallets, 5*time.Second).Run)
	go observability.RunBackground(ctx, "role-binding-expiry", account.NewRoleBindingExpiryWorker(repository, time.Minute).Run)
	go observability.RunBackground(ctx, "approval-expiry", account.NewApprovalExpiryWorker(repository, time.Minute).Run)
	go observability.RunBackground(ctx, "manual-review-sla", account.NewManualReviewSLAWorker(repository, onCallSchedule, time.Minute).Run)
	go observability.RunBackground(ctx, "account-closure", account.NewAccountClosureWorker(repository, 5*time.Second, billingAccountClosure, tokenAccountClosure).WithCleanup(identities, billingAccountClosure).Run)
	if enabled("MOBILE_AUTH_ENABLED") {
		registrationQueryKey, decodeErr := base64.RawStdEncoding.DecodeString(strings.TrimSpace(os.Getenv("REGISTRATION_QUERY_HMAC_KEY_BASE64")))
		if decodeErr != nil {
			log.Fatal("REGISTRATION_QUERY_HMAC_KEY_BASE64 must be unpadded standard Base64")
		}
		registrationTokens, err := account.NewRegistrationTokenSigner(registrationQueryKey)
		if err != nil {
			log.Fatal(err)
		}
		verificationKey, decodeErr := base64.RawStdEncoding.DecodeString(strings.TrimSpace(os.Getenv("SMS_VERIFICATION_HMAC_KEY_BASE64")))
		if decodeErr != nil {
			log.Fatal("SMS_VERIFICATION_HMAC_KEY_BASE64 must be unpadded standard Base64")
		}
		sms, err := account.NewSMSProvider(account.SMSProviderConfig{
			Mode: os.Getenv("SMS_PROVIDER"),
			Aliyun: account.AliyunSMSConfig{
				AccessKeyID: os.Getenv("ALIYUN_SMS_ACCESS_KEY_ID"), AccessKeySecret: os.Getenv("ALIYUN_SMS_ACCESS_KEY_SECRET"),
				SignName: os.Getenv("ALIYUN_SMS_SIGN_NAME"), RegisterTemplate: os.Getenv("ALIYUN_SMS_REGISTER_TEMPLATE"),
				LoginTemplate: os.Getenv("ALIYUN_SMS_LOGIN_TEMPLATE"), Endpoint: os.Getenv("ALIYUN_SMS_ENDPOINT"),
			},
			HTTPGenerated: account.HTTPGeneratedSMSConfig{Endpoint: os.Getenv("SMS_HTTP_ENDPOINT"), BearerToken: os.Getenv("SMS_HTTP_BEARER_TOKEN"), VerificationKey: verificationKey, AllowInsecure: cfg.Environment == "development" || cfg.Environment == "test"},
		})
		if err != nil {
			log.Fatal(err)
		}
		auth, err := account.NewMobileAuthService(repository, sms, identities, os.Getenv("OIDC_ISSUER"), registrationTokens)
		if err != nil {
			log.Fatal(err)
		}
		service.WithMobileAuth(auth, serviceCredential)
		profiles, err := account.NewProfileChangeService(repository, auth, identities)
		if err != nil {
			log.Fatal(err)
		}
		service.WithProfileChanges(profiles)
		go observability.RunBackground(ctx, "profile-change", account.NewProfileChangeWorker(profiles, 5*time.Second).Run)
		go observability.RunBackground(ctx, "registration-recovery", account.NewRegistrationRecoveryWorker(repository, auth, 5*time.Second).Run)

		// Authentication and Commerce Customer provisioning are independent.
		// Keep authentication available while the configured Commerce Adapter is
		// unavailable; supplier-neutral jobs remain queued until it is enabled.
		if strings.TrimSpace(os.Getenv("H3C_MALL_BASE_URL")) != "" {
			mall, err := h3cmall.New(h3cmall.Config{BaseURL: os.Getenv("H3C_MALL_BASE_URL"), AppKey: os.Getenv("H3C_MALL_APP_KEY"), AppSecret: os.Getenv("H3C_MALL_APP_SECRET"), AllowInsecure: cfg.Environment == "development" || cfg.Environment == "test"})
			if err != nil {
				log.Fatal(err)
			}
			go observability.RunBackground(ctx, "commerce-customer-sync", account.NewCommerceCustomerSyncWorker(repository, mall, commerceProviderID, commerceAdapterVersion, 5*time.Second).Run)
		}
	}
	compatibility, err := clientcompat.FromEnv()
	if err != nil {
		log.Fatal(err)
	}
	if err := httpserver.RunWithApplication("account-service", compatibility.Wrap(service.Handler(verify)), repository.Ping); err != nil {
		log.Fatal(err)
	}
}

func enabled(name string) bool {
	value, err := strconv.ParseBool(os.Getenv(name))
	return err == nil && value
}

func valueOrDefault(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}
