package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/billing"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cmall"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cpayment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentsimulator"
)

func main() {
	if err := approvalpolicy.ValidateAll(map[string]approvalpolicy.Mode{"BILLING_PAYMENT_EXPIRY_RECONCILE": approvalpolicy.AuditOnly, "BILLING_ENTITLEMENT_PACKAGE_EXPIRE": approvalpolicy.AuditOnly, "BILLING_MANUAL_REVIEW_SLA_ESCALATE": approvalpolicy.AuditOnly}); err != nil {
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
	var workers sync.WaitGroup
	startWorker := func(run func()) {
		workers.Add(1)
		go func() { defer workers.Done(); run() }()
	}
	defer func() { cancel(); workers.Wait() }()
	if payments.Capabilities().RefundSupported && payments.Capabilities().RefundIdempotencySupported {
		stopRefundConsumer, consumeErr := broker.Consume(ctx, "billing-refund-executor-v1", "billing.refund.requested.v1", billing.NewRefundCommandConsumer(payments).Handle)
		if consumeErr != nil {
			log.Fatal(consumeErr)
		}
		defer stopRefundConsumer()
	}
	if payments.Capabilities().OrderQuerySupported {
		minimumQueryInterval := time.Duration(payments.Capabilities().MinimumQueryIntervalSeconds) * time.Second
		if reconciliationInterval < minimumQueryInterval {
			reconciliationInterval = minimumQueryInterval
		}
		startWorker(func() { billing.NewExpiredPaymentReconciler(repository, payments, reconciliationInterval).Run(ctx) })
	}
	startWorker(func() { billing.NewPackageExpiryWorker(repository, time.Minute).Run(ctx) })
	startWorker(func() { billing.NewManualReviewSLAWorker(repository, time.Minute).Run(ctx) })
	if payments.Capabilities().BillSupported {
		startWorker(func() { billing.NewDailyPaymentReconciliationWorker(payments).Run(ctx) })
	}
	if err := httpserver.RunWithApplication("billing-service", service.Handler(os.Getenv("INTERNAL_SERVICE_TOKEN"), verify), repository.Ping); err != nil {
		log.Fatal(err)
	}
}

func paymentCoordinator(ctx context.Context, repository *billing.PostgresRepository, environment string) (*billing.PaymentCoordinator, error) {
	providerID := strings.ToLower(strings.TrimSpace(os.Getenv("BILLING_PAYMENT_PROVIDER")))
	allowTestOnly := strings.EqualFold(strings.TrimSpace(os.Getenv("BILLING_ALLOW_TENTATIVE_PRICES")), "true")
	development := strings.EqualFold(environment, "development") || strings.EqualFold(environment, "test") || strings.TrimSpace(environment) == ""
	allowTestOnly = allowTestOnly && development
	var provider billing.PaymentProvider
	switch providerID {
	case "h3c_mall":
		client, err := h3cmall.New(h3cmall.Config{BaseURL: os.Getenv("H3C_MALL_BASE_URL"), AppKey: os.Getenv("H3C_MALL_APP_KEY"), AppSecret: os.Getenv("H3C_MALL_APP_SECRET"), AllowInsecure: environment == "development" || environment == "test"})
		if err != nil {
			return nil, err
		}
		products, err := repository.ActiveExternalProducts(ctx, "h3c_mall", "h3c-mall-v1", allowTestOnly)
		if err != nil {
			return nil, err
		}
		adapterProducts := make(map[string]h3cpayment.Product, len(products))
		for productID, product := range products {
			adapterProducts[productID] = h3cpayment.Product{SKU: product.SKU, AmountFen: product.AmountFen}
		}
		provider, err = h3cpayment.New(client, adapterProducts)
		if err != nil {
			return nil, err
		}
	case "", "simulator":
		var err error
		provider, err = paymentsimulator.New(os.Getenv("PAYMENT_SIMULATOR_URL"), os.Getenv("INTERNAL_SERVICE_TOKEN"))
		if err != nil {
			return nil, err
		}
		allowTestOnly = development
	default:
		return nil, errors.New("BILLING_PAYMENT_PROVIDER must be simulator or h3c_mall")
	}
	capabilities := provider.Capabilities()
	products, err := repository.ActiveExternalProducts(ctx, capabilities.ProviderID, capabilities.Version, allowTestOnly)
	if err != nil {
		return nil, err
	}
	coordinator, err := billing.NewPaymentCoordinatorWithProvider(repository, provider, products, environment)
	if err != nil {
		return nil, err
	}
	coordinator.AllowTentativePrices(allowTestOnly)
	if capabilities.NotificationSupported {
		err = coordinator.ConfigureNotifications("pinvou-development", os.Getenv("BILLING_PAYMENT_CALLBACK_URL"), map[string]string{
			"UNIONPAY": os.Getenv("PAYMENT_SIM_HMAC_UNIONPAY"), "ALIPAY": os.Getenv("PAYMENT_SIM_HMAC_ALIPAY"), "WECHAT": os.Getenv("PAYMENT_SIM_HMAC_WECHAT"),
		})
	}
	return coordinator, err
}
