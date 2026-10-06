package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cmall"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
)

func main() {
	if err := approvalpolicy.ValidateAll(map[string]approvalpolicy.Mode{"BILLING_PAYMENT_EXPIRY_RECONCILE": approvalpolicy.AuditOnly, "BILLING_ENTITLEMENT_PACKAGE_EXPIRE": approvalpolicy.AuditOnly}); err != nil {
		log.Fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	cfg, err := config.Load("billing-service")
	if err != nil {
		log.Fatal(err)
	}
	repository, err := billing.NewPostgresRepository(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	defer repository.Close()
	payments, err := paymentCoordinator(ctx, repository, cfg.Environment)
	if err != nil {
		log.Fatal(err)
	}
	authorizer, err := authorization.NewClient(os.Getenv("ACCOUNT_HTTP_URL"), os.Getenv("INTERNAL_SERVICE_TOKEN"))
	if err != nil {
		log.Fatal(err)
	}
	approvalGate, err := billing.NewHTTPApprovalExecutionGate(os.Getenv("ACCOUNT_HTTP_URL"), os.Getenv("INTERNAL_SERVICE_TOKEN"))
	if err != nil {
		log.Fatal(err)
	}
	service := billing.NewService(repository).WithPayments(payments).WithAuthorizer(authorizer).WithApprovalExecutionGate(approvalGate)
	waivers, err := billing.NewHTTPWaiverCoordinator(os.Getenv("TOKEN_GATEWAY_HTTP_URL"), os.Getenv("INTERNAL_SERVICE_TOKEN"))
	if err != nil {
		log.Fatal("Token waiver coordinator is not configured")
	}
	service.WithWaiverCoordinator(waivers)
	verify, err := identity.NewOIDCVerifyFunc(ctx, os.Getenv("OIDC_ISSUER"), os.Getenv("OIDC_AUDIENCE"), cfg.Environment, os.Getenv("OIDC_JWKS_URL"))
	if err != nil {
		log.Fatal(err)
	}
	expiryDone := make(chan struct{})
	go func() {
		defer close(expiryDone)
		service.RunHoldExpiry(ctx, time.Minute, func(error) { log.Print("hold expiry review failed; reservations retained") })
	}()
	defer func() { cancel(); <-expiryDone }()
	reconciliationInterval := 15 * time.Second
	if raw := strings.TrimSpace(os.Getenv("BILLING_PAYMENT_EXPIRY_INTERVAL_SECONDS")); raw != "" {
		seconds, parseErr := strconv.Atoi(raw)
		if parseErr != nil || seconds < 1 || seconds > 300 {
			log.Fatal("BILLING_PAYMENT_EXPIRY_INTERVAL_SECONDS must be between 1 and 300")
		}
		reconciliationInterval = time.Duration(seconds) * time.Second
	}
	broker, err := events.NewNATSBroker(ctx, os.Getenv("NATS_URL"), "billing-command-executor")
	if err != nil {
		log.Fatal(err)
	}
	defer broker.Close()
	stopApprovalConsumer, err := broker.Consume(ctx, "billing-command-executor-v1", "account.approval-command.authorized.v1", billing.NewApprovalCommandConsumer(service).Handle)
	if err != nil {
		log.Fatal(err)
	}
	defer stopApprovalConsumer()
	stopMembershipConsumer, err := broker.Consume(ctx, "billing-member-lifecycle-v1", "account.membership.changed.v1", billing.NewMemberLifecycleConsumer(repository).Handle)
	if err != nil {
		log.Fatal(err)
	}
	defer stopMembershipConsumer()
	paymentExpiryDone := make(chan struct{})
	go func() {
		defer close(paymentExpiryDone)
		billing.NewExpiredPaymentReconciler(repository, payments, reconciliationInterval).Run(ctx)
	}()
	defer func() { cancel(); <-paymentExpiryDone }()
	packageExpiryDone := make(chan struct{})
	go func() {
		defer close(packageExpiryDone)
		billing.NewPackageExpiryWorker(repository, time.Minute).Run(ctx)
	}()
	defer func() { cancel(); <-packageExpiryDone }()
	if err := httpserver.RunWithApplication("billing-service", service.Handler(os.Getenv("INTERNAL_SERVICE_TOKEN"), verify), repository.Ping); err != nil {
		log.Fatal(err)
	}
}

func paymentCoordinator(ctx context.Context, repository *billing.PostgresRepository, environment string) (*billing.PaymentCoordinator, error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("BILLING_PAYMENT_PROVIDER"))) {
	case "h3c_mall":
		products, err := repository.ActiveExternalProducts(ctx)
		if err != nil {
			return nil, err
		}
		client, err := h3cmall.New(h3cmall.Config{BaseURL: os.Getenv("H3C_MALL_BASE_URL"), AppKey: os.Getenv("H3C_MALL_APP_KEY"), AppSecret: os.Getenv("H3C_MALL_APP_SECRET"), AllowInsecure: environment == "development" || environment == "test"})
		if err != nil {
			return nil, err
		}
		provider, err := billing.NewH3CMallProvider(client, products)
		if err != nil {
			return nil, err
		}
		coordinator, err := billing.NewPaymentCoordinatorWithProvider(repository, provider, products)
		if err != nil {
			return nil, err
		}
		return coordinator.AllowTentativePrices(strings.EqualFold(strings.TrimSpace(os.Getenv("BILLING_ALLOW_TENTATIVE_PRICES")), "true")), nil
	case "", "simulator":
		coordinator, err := billing.NewPaymentCoordinator(repository, os.Getenv("PAYMENT_SIMULATOR_URL"), os.Getenv("INTERNAL_SERVICE_TOKEN"), "pinvou-development", os.Getenv("BILLING_PAYMENT_CALLBACK_URL"), map[string]string{
			"UNIONPAY": os.Getenv("PAYMENT_SIM_HMAC_UNIONPAY"), "ALIPAY": os.Getenv("PAYMENT_SIM_HMAC_ALIPAY"), "WECHAT": os.Getenv("PAYMENT_SIM_HMAC_WECHAT"),
		})
		if err != nil {
			return nil, err
		}
		// Development simulator prices are test fixtures, not production quotes.
		// An external production provider retains the explicit opt-in above.
		development := environment == "development" || environment == "test"
		return coordinator.AllowTentativePrices(development || strings.EqualFold(strings.TrimSpace(os.Getenv("BILLING_ALLOW_TENTATIVE_PRICES")), "true")), nil
	default:
		return nil, errors.New("BILLING_PAYMENT_PROVIDER must be simulator or h3c_mall")
	}
}
