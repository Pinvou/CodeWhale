package main

import (
	"context"
	"log"
	"os"
	"strconv"
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
	verify, err := identity.NewOIDCVerifyFuncWithJWKS(ctx, os.Getenv("OIDC_ISSUER"), os.Getenv("OIDC_JWKS_URL"), os.Getenv("OIDC_AUDIENCE"), cfg.Environment)
	if err != nil {
		log.Fatal(err)
	}
	wallets, err := account.NewHTTPWalletProvisioner(os.Getenv("BILLING_HTTP_URL"), os.Getenv("INTERNAL_SERVICE_TOKEN"))
	if err != nil {
		log.Fatal(err)
	}
	service := account.NewService(repository, wallets)
	if enabled("MOBILE_AUTH_ENABLED") {
		sms, err := account.NewAliyunSMSProvider(account.AliyunSMSConfig{
			AccessKeyID: os.Getenv("ALIYUN_SMS_ACCESS_KEY_ID"), AccessKeySecret: os.Getenv("ALIYUN_SMS_ACCESS_KEY_SECRET"),
			SignName: os.Getenv("ALIYUN_SMS_SIGN_NAME"), RegisterTemplate: os.Getenv("ALIYUN_SMS_REGISTER_TEMPLATE"),
			LoginTemplate: os.Getenv("ALIYUN_SMS_LOGIN_TEMPLATE"), Endpoint: os.Getenv("ALIYUN_SMS_ENDPOINT"),
		})
		if err != nil {
			log.Fatal(err)
		}
		identities, err := account.NewKeycloakProvisioner(
			os.Getenv("KEYCLOAK_ADMIN_API_URL"), os.Getenv("KEYCLOAK_PROVISIONER_TOKEN_URL"), os.Getenv("KEYCLOAK_REALM"),
			os.Getenv("KEYCLOAK_PROVISIONER_CLIENT_ID"), os.Getenv("KEYCLOAK_PROVISIONER_CLIENT_SECRET"),
		)
		if err != nil {
			log.Fatal(err)
		}
		auth, err := account.NewMobileAuthService(repository, sms, identities, os.Getenv("OIDC_ISSUER"), os.Getenv("MOBILE_AUTH_HASH_SECRET"))
		if err != nil {
			log.Fatal(err)
		}
		service.WithMobileAuth(auth, os.Getenv("INTERNAL_SERVICE_TOKEN"))

		mall, err := h3cmall.New(h3cmall.Config{BaseURL: os.Getenv("H3C_MALL_BASE_URL"), AppKey: os.Getenv("H3C_MALL_APP_KEY"), AppSecret: os.Getenv("H3C_MALL_APP_SECRET"), AllowInsecure: cfg.Environment == "development" || cfg.Environment == "test"})
		if err != nil {
			log.Fatal(err)
		}
		go account.NewMallSyncWorker(repository, mall, 5*time.Second).Run(ctx)
	}
	if err := httpserver.RunWithApplication("account-service", service.Handler(verify), repository.Ping); err != nil {
		log.Fatal(err)
	}
}

func enabled(name string) bool {
	value, err := strconv.ParseBool(os.Getenv(name))
	return err == nil && value
}
