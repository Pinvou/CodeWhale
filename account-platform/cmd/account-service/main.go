package main

import (
	"context"
	"encoding/base64"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cmall"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := approvalpolicy.ValidateAll(map[string]approvalpolicy.Mode{"ACCOUNT_WALLET_PROVISION": approvalpolicy.AuditOnly, "ACCOUNT_ROLE_BINDING_EXPIRE": approvalpolicy.AuditOnly, "ACCOUNT_APPROVAL_EXPIRE": approvalpolicy.AuditOnly, "ACCOUNT_PROFILE_CHANGE_RECOVERY": approvalpolicy.AuditOnly, "ACCOUNT_REGISTRATION_RECOVERY": approvalpolicy.AuditOnly, "ACCOUNT_COMMERCE_CUSTOMER_SYNC": approvalpolicy.AuditOnly}); err != nil {
		log.Fatal(err)
	}
	cfg, err := config.Load("account-service")
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
	commerceEnabled := strings.TrimSpace(os.Getenv("H3C_MALL_BASE_URL")) != ""
	if commerceEnabled {
		if err := repository.ConfigureCommerceProvider(commerceProviderID, commerceAdapterVersion); err != nil {
			log.Fatal(err)
		}
		if err := repository.EnsureCommerceProviderMappings(ctx); err != nil {
			log.Fatal(err)
		}
	}
	verify, err := identity.NewOIDCVerifyFunc(ctx, os.Getenv("OIDC_ISSUER"), os.Getenv("OIDC_AUDIENCE"), cfg.Environment, os.Getenv("OIDC_JWKS_URL"))
	if err != nil {
		log.Fatal(err)
	}
	wallets, err := account.NewHTTPWalletProvisioner(os.Getenv("BILLING_HTTP_URL"), os.Getenv("INTERNAL_SERVICE_TOKEN"))
	if err != nil {
		log.Fatal(err)
	}
	tokenOrganizationLifecycle, err := account.NewHTTPOrganizationLifecycleClient(os.Getenv("TOKEN_GATEWAY_HTTP_URL"), os.Getenv("INTERNAL_SERVICE_TOKEN"), "TOKEN")
	if err != nil {
		log.Fatal(err)
	}
	billingOrganizationLifecycle, err := account.NewHTTPOrganizationLifecycleClient(os.Getenv("BILLING_HTTP_URL"), os.Getenv("INTERNAL_SERVICE_TOKEN"), "BILLING")
	if err != nil {
		log.Fatal(err)
	}
	service := account.NewService(repository, wallets).
		WithServiceToken(os.Getenv("INTERNAL_SERVICE_TOKEN")).
		WithOrganizationLifecycle(billingOrganizationLifecycle, tokenOrganizationLifecycle).
		WithOrganizationUnitLifecycle(billingOrganizationLifecycle, tokenOrganizationLifecycle).
		WithOrganizationMemberLifecycle(billingOrganizationLifecycle)
	go account.NewWalletProvisioningWorker(repository, wallets, 5*time.Second).Run(ctx)
	go account.NewRoleBindingExpiryWorker(repository, time.Minute).Run(ctx)
	go account.NewApprovalExpiryWorker(repository, time.Minute).Run(ctx)
	if enabled("MOBILE_AUTH_ENABLED") {
		registrationQueryKey, decodeErr := base64.RawStdEncoding.DecodeString(strings.TrimSpace(os.Getenv("REGISTRATION_QUERY_HMAC_KEY_BASE64")))
		if decodeErr != nil {
			log.Fatal("REGISTRATION_QUERY_HMAC_KEY_BASE64 must be unpadded standard Base64")
		}
		registrationTokens, err := account.NewRegistrationTokenSigner(registrationQueryKey)
		if err != nil {
			log.Fatal(err)
		}
		var verificationKey []byte
		if strings.EqualFold(strings.TrimSpace(os.Getenv("SMS_PROVIDER")), "http-generated") {
			verificationKey, decodeErr = base64.RawStdEncoding.DecodeString(strings.TrimSpace(os.Getenv("SMS_VERIFICATION_HMAC_KEY_BASE64")))
			if decodeErr != nil || len(verificationKey) < 32 {
				log.Fatal("SMS_VERIFICATION_HMAC_KEY_BASE64 must be unpadded standard Base64 containing at least 32 bytes")
			}
		}
		sms, err := account.NewSMSProvider(account.SMSProviderConfig{
			Mode: os.Getenv("SMS_PROVIDER"), Endpoint: os.Getenv("SMS_HTTP_ENDPOINT"), ServiceToken: os.Getenv("SMS_HTTP_SERVICE_TOKEN"),
			AllowInsecure: cfg.Environment == "development" || cfg.Environment == "test",
			Aliyun: account.AliyunSMSConfig{
				AccessKeyID: os.Getenv("ALIYUN_SMS_ACCESS_KEY_ID"), AccessKeySecret: os.Getenv("ALIYUN_SMS_ACCESS_KEY_SECRET"),
				SignName: os.Getenv("ALIYUN_SMS_SIGN_NAME"), RegisterTemplate: os.Getenv("ALIYUN_SMS_REGISTER_TEMPLATE"),
				LoginTemplate: os.Getenv("ALIYUN_SMS_LOGIN_TEMPLATE"), Endpoint: os.Getenv("ALIYUN_SMS_ENDPOINT"),
				ProfileMobileTemplate: os.Getenv("ALIYUN_SMS_PROFILE_MOBILE_TEMPLATE"), PasswordResetTemplate: os.Getenv("ALIYUN_SMS_PASSWORD_RESET_TEMPLATE"),
			},
			HTTPGenerated: account.HTTPGeneratedSMSConfig{Endpoint: os.Getenv("SMS_HTTP_ENDPOINT"), BearerToken: os.Getenv("SMS_HTTP_BEARER_TOKEN"), VerificationKey: verificationKey, AllowInsecure: cfg.Environment == "development" || cfg.Environment == "test"},
		})
		if err != nil {
			log.Fatal(err)
		}
		identities, err := account.NewHTTPIdentityProvisioner(os.Getenv("IDENTITY_HTTP_URL"), os.Getenv("INTERNAL_SERVICE_TOKEN"))
		if err != nil {
			log.Fatal(err)
		}
		auth, err := account.NewMobileAuthService(repository, sms, identities, os.Getenv("OIDC_ISSUER"), registrationTokens)
		if err != nil {
			log.Fatal(err)
		}
		auth.WithCommerce(commerceEnabled)
		service.WithMobileAuth(auth, os.Getenv("INTERNAL_SERVICE_TOKEN"))
		profiles, err := account.NewProfileChangeService(repository, auth, identities)
		if err != nil {
			log.Fatal(err)
		}
		service.WithProfileChanges(profiles)
		go account.NewProfileChangeWorker(profiles, 5*time.Second).Run(ctx)
		go account.NewRegistrationRecoveryWorker(repository, auth, 5*time.Second).Run(ctx)

		// Authentication and Commerce Customer provisioning are independent.
		// Only an explicitly configured adapter creates new synchronization jobs.
		if commerceEnabled {
			mall, err := h3cmall.New(h3cmall.Config{BaseURL: os.Getenv("H3C_MALL_BASE_URL"), AppKey: os.Getenv("H3C_MALL_APP_KEY"), AppSecret: os.Getenv("H3C_MALL_APP_SECRET"), AllowInsecure: cfg.Environment == "development" || cfg.Environment == "test"})
			if err != nil {
				log.Fatal(err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				account.NewCommerceCustomerSyncWorker(repository, mall, commerceProviderID, commerceAdapterVersion, 5*time.Second).Run(ctx)
			}()
			defer func() { cancel(); <-done }()
		}
	}
	if err := httpserver.RunWithApplication("account-service", service.Handler(verify), repository.Ping); err != nil {
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
