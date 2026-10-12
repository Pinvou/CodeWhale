package main

import (
	"context"
	"log"
	"os"
	"strings"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
)

func main() {
	if err := httpserver.ConfigureProcessLogging("billing-service"); err != nil {
		log.Fatal("BILLING_LOG_CONFIGURATION_FAILED")
	}
	credential := serviceauth.CredentialFromEnvironment()
	if strings.TrimSpace(credential) == "" {
		log.Fatal("BILLING_SERVICE_IDENTITY_REQUIRED")
	}
	repository, err := billing.NewPostgresRepository(context.Background(), os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal("BILLING_DATABASE_INITIALIZATION_FAILED")
	}
	defer repository.Close()
	if err := httpserver.RunWithApplication("billing-service", billing.NewService(repository).Handler(credential), repository.Ping); err != nil {
		log.Fatal("BILLING_SERVER_FAILED")
	}
}
