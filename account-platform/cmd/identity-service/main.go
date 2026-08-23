package main

import (
	"encoding/base64"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/identityadapter"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

func main() {
	if !enabled("MOBILE_AUTH_ENABLED") {
		if err := httpserver.Run("identity-service"); err != nil {
			log.Fatal(err)
		}
		return
	}

	directory, err := identityadapter.NewKeycloakDirectory(
		os.Getenv("KEYCLOAK_ADMIN_API_URL"), os.Getenv("KEYCLOAK_PROVISIONER_TOKEN_URL"), os.Getenv("KEYCLOAK_REALM"),
		os.Getenv("KEYCLOAK_PROVISIONER_CLIENT_ID"), os.Getenv("KEYCLOAK_PROVISIONER_CLIENT_SECRET"),
	)
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
	service, err := identityadapter.NewService(vault, os.Getenv("INTERNAL_SERVICE_TOKEN"))
	if err != nil {
		log.Fatal(err)
	}
	if err := httpserver.RunWithApplication("identity-service", service.Handler(), nil); err != nil {
		log.Fatal(err)
	}
}

func enabled(name string) bool {
	value, err := strconv.ParseBool(strings.TrimSpace(os.Getenv(name)))
	return err == nil && value
}
