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
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cmall"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

func main() {
	ctx := context.Background()
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
	wallets, err := account.NewHTTPWalletProvisioner(os.Getenv("BILLING_HTTP_URL"), os.Getenv("INTERNAL_SERVICE_TOKEN"))
	if err != nil {
		log.Fatal(err)
	}
	service := account.NewService(repository, wallets).WithServiceToken(os.Getenv("INTERNAL_SERVICE_TOKEN"))
	go account.NewWalletProvisioningWorker(repository, wallets, 5*time.Second).Run(ctx)
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
		identities, err := account.NewHTTPIdentityProvisioner(os.Getenv("IDENTITY_HTTP_URL"), os.Getenv("INTERNAL_SERVICE_TOKEN"))
		if err != nil {
			log.Fatal(err)
		}
		auth, err := account.NewMobileAuthService(repository, sms, identities, os.Getenv("OIDC_ISSUER"), registrationTokens)
		if err != nil {
			log.Fatal(err)
		}
		service.WithMobileAuth(auth, os.Getenv("INTERNAL_SERVICE_TOKEN"))
		profiles, err := account.NewProfileChangeService(repository, auth, identities)
		if err != nil {
			log.Fatal(err)
		}
		service.WithProfileChanges(profiles)
		go account.NewProfileChangeWorker(profiles, 5*time.Second).Run(ctx)
		go account.NewRegistrationRecoveryWorker(repository, auth, 5*time.Second).Run(ctx)

		// Authentication and Commerce Customer provisioning are independent.
		// Keep authentication available while the configured Commerce Adapter is
		// unavailable; supplier-neutral jobs remain queued until it is enabled.
		if strings.TrimSpace(os.Getenv("H3C_MALL_BASE_URL")) != "" {
			mall, err := h3cmall.New(h3cmall.Config{BaseURL: os.Getenv("H3C_MALL_BASE_URL"), AppKey: os.Getenv("H3C_MALL_APP_KEY"), AppSecret: os.Getenv("H3C_MALL_APP_SECRET"), AllowInsecure: cfg.Environment == "development" || cfg.Environment == "test"})
			if err != nil {
				log.Fatal(err)
			}
			go account.NewCommerceCustomerSyncWorker(repository, mall, commerceProviderID, commerceAdapterVersion, 5*time.Second).Run(ctx)
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
