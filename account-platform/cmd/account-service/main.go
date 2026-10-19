package main

import (
	"context"
	"encoding/base64"
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cmall"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
)

func main() {
	if err := httpserver.ConfigureProcessLogging("account-service"); err != nil {
		log.Fatal("ACCOUNT_LOGGING_CONFIGURATION_INVALID")
	}
	ctx := context.Background()
	cfg, err := config.Load("account-service")
	if err != nil {
		log.Fatal("ACCOUNT_CONFIGURATION_INVALID")
	}
	credential := serviceauth.CredentialFromEnvironment()
	if strings.TrimSpace(credential) == "" {
		log.Fatal("ACCOUNT_SERVICE_CREDENTIAL_REQUIRED")
	}
	repository, err := account.NewPostgresRepository(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal("ACCOUNT_DATABASE_UNAVAILABLE")
	}
	defer repository.Close()
	go observability.RunBackground(ctx, "mobile-retention", account.NewMobileRetentionWorker(repository, time.Hour).Run)
	walletPolicyVersion := strings.TrimSpace(os.Getenv("WALLET_PROVISIONING_POLICY_VERSION"))
	if walletPolicyVersion == "" {
		walletPolicyVersion = "wallet-provisioning-v1"
	}
	walletSLA, err := configuredWalletSLA(os.Getenv("WALLET_PROVISIONING_SLA_MINUTES"))
	if err != nil {
		log.Fatal("ACCOUNT_WALLET_SLA_CONFIGURATION_INVALID")
	}
	if err := repository.ConfigureWalletProvisioning(walletPolicyVersion, walletSLA); err != nil {
		log.Fatal(err)
	}

	commerceProviderID := valueOrDefault("COMMERCE_PROVIDER_ID", "h3c_mall")
	commerceAdapterVersion := valueOrDefault("COMMERCE_ADAPTER_VERSION", "h3c-mall-v1")
	if err := repository.ConfigureCommerceProvider(commerceProviderID, commerceAdapterVersion); err != nil {
		log.Fatal("ACCOUNT_COMMERCE_CONFIGURATION_INVALID")
	}
	if err := repository.EnsureCommerceProviderMappings(ctx); err != nil {
		log.Fatal("ACCOUNT_COMMERCE_MAPPINGS_UNAVAILABLE")
	}

	verify, err := identity.NewOIDCVerifyFuncWithJWKS(ctx, os.Getenv("OIDC_ISSUER"), os.Getenv("OIDC_JWKS_URL"), os.Getenv("OIDC_AUDIENCE"), cfg.Environment)
	if err != nil {
		log.Fatal("ACCOUNT_OIDC_CONFIGURATION_INVALID")
	}
	wallets, err := account.NewHTTPWalletProvisioner(os.Getenv("BILLING_HTTP_URL"), credential)
	if err != nil {
		log.Fatal("ACCOUNT_WALLET_AUTHORITY_INVALID")
	}
	identities, err := account.NewHTTPIdentityProvisioner(os.Getenv("IDENTITY_HTTP_URL"), credential)
	if err != nil {
		log.Fatal("ACCOUNT_IDENTITY_AUTHORITY_INVALID")
	}
	service := account.NewService(repository, wallets).WithServiceToken(credential).WithClientCompatibility(valueOrDefault("ACCOUNT_SERVICE_VERSION", "development"), valueOrDefault("ACCOUNT_CLIENT_CONTRACT_VERSION", "account-client-v1"))
	ownerCandidateCursorKey, decodeErr := base64.StdEncoding.Strict().DecodeString(strings.TrimSpace(os.Getenv("OWNER_CANDIDATE_CURSOR_HMAC_KEY_BASE64")))
	if decodeErr != nil || len(ownerCandidateCursorKey) != 32 {
		log.Fatal("ACCOUNT_OWNER_CURSOR_KEY_INVALID")
	}
	service.WithTokenOwnerCandidateCursorKey(ownerCandidateCursorKey)
	clear(ownerCandidateCursorKey)

	if err := configureOrganizationLifecycle(service, credential); err != nil {
		log.Fatal("ACCOUNT_ORGANIZATION_AUTHORITY_INVALID")
	}
	if err := configureApprovals(ctx, repository, service, credential, cfg.Environment); err != nil {
		log.Fatal(err)
	}
	if err := configureRegistrationBonus(ctx, repository, enabled("MOBILE_AUTH_ENABLED"), os.Getenv("REGISTRATION_BONUS_FINGERPRINT_KEY_ID"), os.Getenv("REGISTRATION_BONUS_FINGERPRINT_HMAC_KEY_BASE64")); err != nil {
		log.Fatal("REGISTRATION_BONUS_CONFIGURATION_INVALID")
	}
	go observability.RunBackground(ctx, "wallet-provisioning", account.NewWalletProvisioningWorker(repository, wallets, 5*time.Second).Run)
	go observability.RunBackground(ctx, "role-binding-expiry", account.NewRoleBindingExpiryWorker(repository, time.Minute).Run)
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
		mode := strings.ToLower(strings.TrimSpace(os.Getenv("SMS_PROVIDER")))
		aliyunConfig := account.AliyunSMSConfig{
			AccessKeyID: os.Getenv("ALIYUN_SMS_ACCESS_KEY_ID"), AccessKeySecret: os.Getenv("ALIYUN_SMS_ACCESS_KEY_SECRET"),
			SignName: os.Getenv("ALIYUN_SMS_SIGN_NAME"), RegisterTemplate: os.Getenv("ALIYUN_SMS_REGISTER_TEMPLATE"),
			LoginTemplate: os.Getenv("ALIYUN_SMS_LOGIN_TEMPLATE"), ProfileMobileTemplate: os.Getenv("ALIYUN_SMS_PROFILE_MOBILE_TEMPLATE"),
			PasswordResetTemplate: os.Getenv("ALIYUN_SMS_PASSWORD_RESET_TEMPLATE"), Endpoint: os.Getenv("ALIYUN_SMS_ENDPOINT"),
		}
		if strings.TrimSpace(aliyunConfig.ProfileMobileTemplate) == "" {
			aliyunConfig.ProfileMobileTemplate = aliyunConfig.LoginTemplate
		}
		if strings.TrimSpace(aliyunConfig.PasswordResetTemplate) == "" {
			aliyunConfig.PasswordResetTemplate = aliyunConfig.LoginTemplate
		}
		httpSMSConfig := account.HTTPGeneratedSMSConfig{Endpoint: os.Getenv("SMS_HTTP_ENDPOINT"), BearerToken: os.Getenv("SMS_HTTP_BEARER_TOKEN"), VerificationKey: verificationKey, AllowInsecure: cfg.Environment == "development" || cfg.Environment == "test"}
		sms, err := account.NewSMSProvider(account.SMSProviderConfig{Mode: mode, Aliyun: aliyunConfig, HTTPGenerated: httpSMSConfig})
		if err != nil {
			log.Fatal(err)
		}
		defaultSMSConfiguration := account.SMSConfiguration{Enabled: true, ProviderID: mode, AdapterVersion: "v1"}
		if mode == "aliyun-managed" {
			defaultSMSConfiguration.CredentialProfile = "aliyun-default"
			defaultSMSConfiguration.SignName = aliyunConfig.SignName
			defaultSMSConfiguration.Templates = account.SMSPurposeTemplates{Register: aliyunConfig.RegisterTemplate, Login: aliyunConfig.LoginTemplate, ProfileMobile: aliyunConfig.ProfileMobileTemplate, PasswordReset: aliyunConfig.PasswordResetTemplate}
		} else {
			defaultSMSConfiguration.CredentialProfile = "http-default"
		}
		aliyunProfileProbe := aliyunConfig
		if strings.TrimSpace(aliyunProfileProbe.SignName) == "" {
			aliyunProfileProbe.SignName = "readiness-probe"
		}
		if strings.TrimSpace(aliyunProfileProbe.RegisterTemplate) == "" {
			aliyunProfileProbe.RegisterTemplate = "readiness-probe"
		}
		if strings.TrimSpace(aliyunProfileProbe.LoginTemplate) == "" {
			aliyunProfileProbe.LoginTemplate = "readiness-probe"
		}
		if strings.TrimSpace(aliyunProfileProbe.ProfileMobileTemplate) == "" {
			aliyunProfileProbe.ProfileMobileTemplate = "readiness-probe"
		}
		if strings.TrimSpace(aliyunProfileProbe.PasswordResetTemplate) == "" {
			aliyunProfileProbe.PasswordResetTemplate = "readiness-probe"
		}
		_, aliyunProfileErr := account.NewAliyunSMSProvider(aliyunProfileProbe)
		_, httpProfileErr := account.NewHTTPGeneratedSMSProvider(httpSMSConfig)
		resolver, err := account.NewVersionedSMSProviderResolver(repository, sms, defaultSMSConfiguration, []account.SMSProviderProfile{
			{ID: "aliyun-default", ProviderID: "aliyun-managed", Configured: aliyunProfileErr == nil, Aliyun: aliyunConfig},
			{ID: "http-default", ProviderID: "http-generated", Configured: httpProfileErr == nil, HTTPGenerated: httpSMSConfig},
		})
		if err != nil {
			log.Fatal(err)
		}
		service.WithSMSConfiguration(resolver)
		auth, err := account.NewMobileAuthService(repository, resolver, identities, os.Getenv("OIDC_ISSUER"), registrationTokens)
		if err != nil {
			log.Fatal(err)
		}
		service.WithMobileAuth(auth, credential)
		profiles, err := account.NewProfileChangeService(repository, auth, identities)
		if err != nil {
			log.Fatal(err)
		}
		service.WithProfileChanges(profiles)
		go observability.RunBackground(ctx, "profile-change", account.NewProfileChangeWorker(profiles, 5*time.Second).Run)
		go observability.RunBackground(ctx, "registration-recovery", account.NewRegistrationRecoveryWorker(repository, auth, 5*time.Second).Run)

		// Commerce availability does not gate authentication; persisted jobs are
		// retained until the explicitly configured adapter becomes available.
		if strings.TrimSpace(os.Getenv("H3C_MALL_BASE_URL")) != "" {
			adapter, err := h3cmall.New(h3cmall.Config{BaseURL: os.Getenv("H3C_MALL_BASE_URL"), AppKey: os.Getenv("H3C_MALL_APP_KEY"), AppSecret: os.Getenv("H3C_MALL_APP_SECRET"), AllowInsecure: cfg.Environment == "development" || cfg.Environment == "test"})
			if err != nil {
				log.Fatal("ACCOUNT_COMMERCE_ADAPTER_INVALID")
			}
			go observability.RunBackground(ctx, "commerce-customer-sync", account.NewCommerceCustomerSyncWorker(repository, adapter, commerceProviderID, commerceAdapterVersion, 5*time.Second).Run)
		}
	}

	handler, err := account.NewTrustedSourceHandler(service.Handler(verify), os.Getenv("ACCOUNT_TRUSTED_SOURCE_PEER_IPS"))
	if err != nil {
		log.Fatal("ACCOUNT_TRUSTED_SOURCE_CONFIGURATION_INVALID")
	}
	if err := httpserver.RunWithApplicationAndCollectors("account-service", repository.WithApprovalWriterReadiness(handler), repository.Ping, repository.ApprovalPaginationCollectors()...); err != nil {
		log.Fatal("ACCOUNT_HTTP_SERVER_FAILED")
	}
}

func configuredWalletSLA(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 15 * time.Minute, nil
	}
	minutes, err := strconv.Atoi(raw)
	if err != nil || minutes < 1 || minutes > 24*60 {
		return 0, errors.New("wallet provisioning SLA must be 1 to 1440 minutes")
	}
	return time.Duration(minutes) * time.Minute, nil
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
