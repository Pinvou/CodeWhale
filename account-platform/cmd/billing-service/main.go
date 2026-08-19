package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

func main() {
	ctx := context.Background()
	cfg, err := config.Load("billing-service")
	if err != nil {
		log.Fatal(err)
	}
	repository, err := billing.NewPostgresRepository(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer repository.Close()
	payments, err := billing.NewPaymentCoordinator(repository, os.Getenv("PAYMENT_SIMULATOR_URL"), os.Getenv("INTERNAL_SERVICE_TOKEN"), "pinvou-development", os.Getenv("BILLING_PAYMENT_CALLBACK_URL"), map[string]string{
		"UNIONPAY": os.Getenv("PAYMENT_SIM_HMAC_UNIONPAY"), "ALIPAY": os.Getenv("PAYMENT_SIM_HMAC_ALIPAY"), "WECHAT": os.Getenv("PAYMENT_SIM_HMAC_WECHAT"),
	})
	if err != nil {
		log.Fatal(err)
	}
	service := billing.NewService(repository).WithPayments(payments)
	verify, err := identity.NewOIDCVerifyFuncWithJWKS(ctx, os.Getenv("OIDC_ISSUER"), os.Getenv("OIDC_JWKS_URL"), os.Getenv("OIDC_AUDIENCE"), cfg.Environment)
	if err != nil {
		log.Fatal(err)
	}
	reconciliationInterval := 15 * time.Second
	if raw := strings.TrimSpace(os.Getenv("BILLING_PAYMENT_EXPIRY_INTERVAL_SECONDS")); raw != "" {
		seconds, parseErr := strconv.Atoi(raw)
		if parseErr != nil || seconds < 1 || seconds > 300 {
			log.Fatal("BILLING_PAYMENT_EXPIRY_INTERVAL_SECONDS must be between 1 and 300")
		}
		reconciliationInterval = time.Duration(seconds) * time.Second
	}
	workerContext, stopWorker := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopWorker()
	go billing.NewExpiredPaymentReconciler(repository, payments, reconciliationInterval).Run(workerContext)
	if err := httpserver.RunWithApplication("billing-service", service.Handler(os.Getenv("INTERNAL_SERVICE_TOKEN"), verify), repository.Ping); err != nil {
		log.Fatal(err)
	}
}
