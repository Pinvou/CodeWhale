package main

import (
	"context"
	"encoding/base64"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/identityadapter"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

func main() {
	if err := httpserver.ConfigureProcessLogging("identity-service"); err != nil {
		log.Fatal(err)
	}
	if !enabled("MOBILE_AUTH_ENABLED") {
		if err := httpserver.Run("identity-service"); err != nil {
			log.Fatal(err)
		}
		return
	}

	production := strings.HasPrefix(strings.ToLower(strings.TrimSpace(os.Getenv("APP_ENV"))), "production")
	directory, err := identityadapter.NewKeycloakDirectoryWithConfig(identityadapter.KeycloakConnectionConfig{
		AdminURL: os.Getenv("KEYCLOAK_ADMIN_API_URL"), TokenURL: os.Getenv("KEYCLOAK_PROVISIONER_TOKEN_URL"),
		Realm: os.Getenv("KEYCLOAK_REALM"), ClientID: os.Getenv("KEYCLOAK_PROVISIONER_CLIENT_ID"),
		ClientSecret: os.Getenv("KEYCLOAK_PROVISIONER_CLIENT_SECRET"), SecretFile: os.Getenv("KEYCLOAK_PROVISIONER_CLIENT_SECRET_FILE"),
		CAFile: os.Getenv("KEYCLOAK_CA_FILE"), Production: production,
	})
	if err != nil {
		log.Fatal(err)
	}
	key, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(os.Getenv("IDENTITY_PASSWORD_HMAC_KEY_BASE64")))
	if err != nil {
		log.Fatal("IDENTITY_PASSWORD_HMAC_KEY_BASE64 must be unpadded standard Base64")
	}
	vault, err := identityadapter.NewPasswordVault(key, directory)
	if err != nil {
		log.Fatal(err)
	}
	var tokenHandlers []http.Handler
	ready := directory.Ping
	if strings.TrimSpace(os.Getenv("AUTH_RISK_REDIS_URL")) != "" {
		admission, admissionErr := identityadapter.NewRedisTokenAdmissionWithCA(os.Getenv("AUTH_RISK_REDIS_URL"), os.Getenv("AUTH_RISK_HMAC_KEY_BASE64"), os.Getenv("AUTH_RISK_REDIS_CA_FILE"))
		if admissionErr != nil {
			log.Fatal(admissionErr)
		}
		proxy, proxyErr := identityadapter.NewTokenProxyWithTrust(os.Getenv("KEYCLOAK_PROVISIONER_TOKEN_URL"), admission, os.Getenv("KEYCLOAK_CA_FILE"), production)
		if proxyErr != nil {
			log.Fatal(proxyErr)
		}
		tokenHandlers = append(tokenHandlers, proxy)
		ready = func(ctx context.Context) error {
			if err := directory.Ping(ctx); err != nil {
				return err
			}
			return proxy.Ping(ctx)
		}
	}
	service, err := identityadapter.NewService(vault, os.Getenv("INTERNAL_SERVICE_TOKEN"), tokenHandlers...)
	if err != nil {
		log.Fatal(err)
	}
	if err := httpserver.RunWithApplication("identity-service", service.Handler(), ready); err != nil {
		log.Fatal(err)
	}
}

func enabled(name string) bool {
	value, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(name)))
	return err == nil && value
}
