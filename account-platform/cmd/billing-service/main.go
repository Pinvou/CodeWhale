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
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/clientcompat"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/config"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/events"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cmall"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cpayment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentsimulator"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
)

func main() {
	if err := httpserver.ConfigureProcessLogging("billing-service"); err != nil {
		log.Fatal(err)
	}
	if err := approvalpolicy.ValidateAll(map[string]approvalpolicy.Mode{"BILLING_PAYMENT_EXPIRY_RECONCILE": approvalpolicy.AuditOnly, "BILLING_ENTITLEMENT_PACKAGE_EXPIRE": approvalpolicy.AuditOnly, "BILLING_MANUAL_REVIEW_SLA_ESCALATE": approvalpolicy.AuditOnly}); err != nil {
		log.Fatal(err)
	}
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
	payments, err := paymentCoordinator(ctx, repository, cfg.Environment)
	if err != nil {
		log.Fatal(err)
	}
	serviceCredential := serviceauth.CredentialFromEnvironment()
	authorizer, err := authorization.NewClient(os.Getenv("ACCOUNT_HTTP_URL"), serviceCredential)
	if err != nil {
		log.Fatal(err)
	}
	approvalGate, err := billing.NewHTTPApprovalExecutionGate(os.Getenv("ACCOUNT_HTTP_URL"), serviceCredential)
	if err != nil {
		log.Fatal(err)
	}
	closureStates, err := billing.NewHTTPAccountClosureStateVerifier(os.Getenv("ACCOUNT_HTTP_URL"), serviceCredential)
	if err != nil {
		log.Fatal(err)
	}
	service := billing.NewService(repository).WithPayments(payments).WithAuthorizer(authorizer).WithApprovalExecutionGate(approvalGate).WithAccountClosureStateVerifier(closureStates)
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
	broker, err := events.NewNATSBroker(workerContext, os.Getenv("NATS_URL"), "billing-command-executor")
	if err != nil {
		log.Fatal(err)
	}
	defer broker.Close()
	stopApprovalConsumer, err := broker.Consume(workerContext, "billing-command-executor-v1", "account.approval-command.authorized.v1", billing.NewApprovalCommandConsumer(service).Handle)
	if err != nil {
		log.Fatal(err)
	}
	defer stopApprovalConsumer()
	if payments.Capabilities().RefundSupported && payments.Capabilities().RefundIdempotencySupported {
		stopRefundConsumer, consumeErr := broker.Consume(workerContext, "billing-refund-executor-v1", "billing.refund.requested.v1", billing.NewRefundCommandConsumer(payments).Handle)
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
		go observability.RunBackground(workerContext, "expired-payment-reconciliation", billing.NewExpiredPaymentReconciler(repository, payments, reconciliationInterval).Run)
	}
	if payments.Capabilities().NotificationSupported {
		go observability.RunBackground(workerContext, "payment-notification-inbox", billing.NewPaymentNotificationWorker(payments, 50*time.Millisecond).Run)
	}
	go observability.RunBackground(workerContext, "package-expiry", billing.NewPackageExpiryWorker(repository, time.Minute).Run)
	go observability.RunBackground(workerContext, "manual-review-sla", billing.NewManualReviewSLAWorker(repository, time.Minute).Run)
	if payments.Capabilities().BillSupported {
		go observability.RunBackground(workerContext, "daily-payment-reconciliation", billing.NewDailyPaymentReconciliationWorker(payments).Run)
	}
	compatibility, err := clientcompat.FromEnv()
	if err != nil {
		log.Fatal(err)
	}
	if err := httpserver.RunWithApplication("billing-service", compatibility.Wrap(service.Handler(os.Getenv("INTERNAL_SERVICE_TOKEN"), verify)), repository.Ping); err != nil {
		log.Fatal(err)
	}
}

func paymentCoordinator(ctx context.Context, repository *billing.PostgresRepository, environment string) (*billing.PaymentCoordinator, error) {
	providerID := strings.ToLower(strings.TrimSpace(os.Getenv("BILLING_PAYMENT_PROVIDER")))
	allowTestOnly := strings.EqualFold(strings.TrimSpace(os.Getenv("BILLING_ALLOW_TENTATIVE_PRICES")), "true")
	var provider billing.PaymentProvider
	switch providerID {
	case "h3c_mall":
		client, err := h3cmall.New(h3cmall.Config{BaseURL: os.Getenv("H3C_MALL_BASE_URL"), AppKey: os.Getenv("H3C_MALL_APP_KEY"), AppSecret: os.Getenv("H3C_MALL_APP_SECRET")})
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
	case "development_simulator":
		if !strings.EqualFold(environment, "development") && !strings.EqualFold(environment, "test") {
			return nil, errors.New("development payment provider is forbidden outside development or test")
		}
		var err error
		provider, err = paymentsimulator.New(os.Getenv("PAYMENT_PROVIDER_BASE_URL"), os.Getenv("INTERNAL_SERVICE_TOKEN"))
		if err != nil {
			return nil, err
		}
		allowTestOnly = true
	default:
		return nil, errors.New("BILLING_PAYMENT_PROVIDER must select an explicit installed payment adapter")
	}
	capabilities := provider.Capabilities()
	products, err := repository.ActiveExternalProducts(ctx, capabilities.ProviderID, capabilities.Version, allowTestOnly)
	if err != nil {
		return nil, err
	}
	coordinator, err := billing.NewPaymentCoordinatorWithProvider(repository, provider, products)
	if err != nil {
		return nil, err
	}
	coordinator.AllowTentativePrices(allowTestOnly)
	if capabilities.NotificationSupported {
		err = coordinator.ConfigureNotifications(os.Getenv("PAYMENT_MERCHANT_ID"), os.Getenv("BILLING_PAYMENT_CALLBACK_URL"), map[string]string{
			"UNIONPAY": os.Getenv("PAYMENT_NOTIFICATION_HMAC_UNIONPAY"), "ALIPAY": os.Getenv("PAYMENT_NOTIFICATION_HMAC_ALIPAY"), "WECHAT": os.Getenv("PAYMENT_NOTIFICATION_HMAC_WECHAT"),
		})
	}
	return coordinator, err
}
