package main

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
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
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3ccallback"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cmall"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/h3cpayment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/humandelegation"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/manualreview"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/observability"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentcallback"
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
	onCallSchedule, err := manualreview.ParseOnCallSchedule(os.Getenv("MANUAL_REVIEW_ON_CALL_USER_IDS"), strings.HasPrefix(strings.ToLower(cfg.Environment), "production"))
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
	modelPriceProjectionEnabled, err := optionalBooleanEnvironment("BILLING_ADMIN_MODEL_PRICE_PROJECTION_ENABLED", false)
	if err != nil {
		log.Fatal(err)
	}
	service := billing.NewService(repository).WithPayments(payments).WithRechargeProductCatalog(repository).WithAuthorizer(authorizer).WithApprovalExecutionGate(approvalGate).WithAccountClosureStateVerifier(closureStates).WithAdminModelPriceProjection(modelPriceProjectionEnabled)
	humanDelegationKeys, err := humandelegation.DecodeVerificationKeysJSON(os.Getenv("HUMAN_DELEGATION_VERIFICATION_KEYS_JSON"))
	if err != nil {
		log.Fatal(err)
	}
	humanDelegations, err := humandelegation.NewVerifier(humanDelegationKeys)
	if err != nil {
		log.Fatal(err)
	}
	service.WithHumanDelegations(humanDelegations)
	if strings.EqualFold(strings.TrimSpace(os.Getenv("BILLING_PAYMENT_PROVIDER")), "h3c_mall") {
		authenticator, profiles, callbackErr := paymentCallbackConfiguration(payments.Capabilities())
		if callbackErr != nil {
			log.Fatal(callbackErr)
		}
		service.WithPaymentCallbacks(authenticator, profiles)
	}
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
	broker, err := events.NewNATSBroker(workerContext, os.Getenv("NATS_URL"), "billing-command-executor", events.NATSConfig{Environment: cfg.Environment, CAFile: os.Getenv("NATS_CA_FILE")})
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
	testAdmissionEvidenceTTL, err := testAdmissionEvidenceTTLFromEnvironment()
	if err != nil {
		log.Fatal(err)
	}
	go observability.RunBackground(workerContext, "package-expiry", billing.NewPackageExpiryWorker(repository, time.Minute).Run)
	go observability.RunBackground(workerContext, "product-admission-expiry", billing.NewProductAdmissionExpiryWorker(repository, time.Minute).Run)
	go observability.RunBackground(workerContext, "product-admission-verification", billing.NewProductAdmissionVerificationWorker(repository, payments.Capabilities(), 5*time.Second, testAdmissionEvidenceTTL).Run)
	go observability.RunBackground(workerContext, "manual-review-sla", billing.NewManualReviewSLAWorker(repository, time.Minute, onCallSchedule).Run)
	go observability.RunBackground(workerContext, "admin-pagination-cleanup", billing.NewAdminPaginationCleanupWorker(repository, time.Minute).Run)
	if payments.Capabilities().BillSupported {
		go observability.RunBackground(workerContext, "daily-payment-reconciliation", billing.NewDailyPaymentReconciliationWorker(payments).Run)
	}
	compatibility, err := clientcompat.FromEnv()
	if err != nil {
		log.Fatal(err)
	}
	handler := service.Handler(os.Getenv("INTERNAL_SERVICE_TOKEN"), verify)
	handler = compatibility.WrapExcept(handler, "/api/v1/payment-callback/token", "/api/v1/payment-callbacks")
	collectors := append(repository.AdminPaginationCollectors(), service.AdminModelPriceProjectionCollectors()...)
	if err := httpserver.RunWithApplicationAndCollectors("billing-service", handler, repository.Ping, collectors...); err != nil {
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
		provider, err = h3cpayment.New(client, func(resolveContext context.Context, sku string) (h3cpayment.Product, error) {
			product, resolveErr := repository.ExternalProductBySKU(resolveContext, "h3c_mall", "h3c-mall-v1", sku)
			return h3cpayment.Product{SKU: product.SKU, AmountFen: product.AmountFen}, resolveErr
		})
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
	coordinator, err := billing.NewPaymentCoordinatorWithProvider(repository, provider)
	if err != nil {
		return nil, err
	}
	coordinator.AllowTentativePrices(allowTestOnly)
	normalRefundCreationEnabled, err := optionalBooleanEnvironment("BILLING_NORMAL_REFUND_CREATION_ENABLED", false)
	if err != nil {
		return nil, err
	}
	coordinator.AllowNormalRefundCreation(normalRefundCreationEnabled)
	if capabilities.NotificationSupported && capabilities.ProviderID == "h3c_mall" {
		err = coordinator.ConfigureTrustedNotifications(os.Getenv("PAYMENT_MERCHANT_ID"))
	} else if capabilities.NotificationSupported {
		err = coordinator.ConfigureNotifications(os.Getenv("PAYMENT_MERCHANT_ID"), os.Getenv("BILLING_PAYMENT_CALLBACK_URL"), map[string]string{
			"UNIONPAY": os.Getenv("PAYMENT_NOTIFICATION_HMAC_UNIONPAY"), "ALIPAY": os.Getenv("PAYMENT_NOTIFICATION_HMAC_ALIPAY"), "WECHAT": os.Getenv("PAYMENT_NOTIFICATION_HMAC_WECHAT"),
		})
	}
	return coordinator, err
}

func optionalBooleanEnvironment(name string, fallback bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("%s must be a boolean: %w", name, err)
	}
	return value, nil
}

func testAdmissionEvidenceTTLFromEnvironment() (time.Duration, error) {
	const (
		fallback = 30 * 24 * time.Hour
		minimum  = time.Hour
		maximum  = 90 * 24 * time.Hour
	)
	raw := strings.TrimSpace(os.Getenv("BILLING_TEST_ADMISSION_EVIDENCE_TTL"))
	if raw == "" {
		return fallback, nil
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("BILLING_TEST_ADMISSION_EVIDENCE_TTL must be a Go duration: %w", err)
	}
	if ttl < minimum || ttl > maximum {
		return 0, errors.New("BILLING_TEST_ADMISSION_EVIDENCE_TTL must be between 1h and 2160h")
	}
	return ttl, nil
}

func paymentCallbackConfiguration(capabilities payment.Capabilities) (*paymentcallback.Authenticator, *paymentcallback.Profiles, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(os.Getenv("PAYMENT_CALLBACK_TOKEN_SIGNING_KEY_BASE64")))
	if err != nil {
		return nil, nil, errors.New("PAYMENT_CALLBACK_TOKEN_SIGNING_KEY_BASE64 must be valid standard Base64")
	}
	ttl := 5 * time.Minute
	if raw := strings.TrimSpace(os.Getenv("PAYMENT_CALLBACK_TOKEN_TTL_SECONDS")); raw != "" {
		seconds, parseErr := strconv.Atoi(raw)
		if parseErr != nil || seconds < 60 || seconds > 900 {
			return nil, nil, errors.New("PAYMENT_CALLBACK_TOKEN_TTL_SECONDS must be between 60 and 900")
		}
		ttl = time.Duration(seconds) * time.Second
	}
	authenticator, err := paymentcallback.NewAuthenticator(paymentcallback.AuthConfig{
		Clients: []paymentcallback.ClientConfig{{
			ClientID: os.Getenv("PAYMENT_CALLBACK_CLIENT_ID"), ClientSecret: os.Getenv("PAYMENT_CALLBACK_CLIENT_SECRET"),
			ProviderID: capabilities.ProviderID, ProviderVersion: capabilities.Version, MerchantID: os.Getenv("PAYMENT_MERCHANT_ID"),
		}},
		SigningKey: key, KeyID: os.Getenv("PAYMENT_CALLBACK_TOKEN_KEY_ID"), TokenTTL: ttl,
	})
	if err != nil {
		return nil, nil, err
	}
	profiles, err := paymentcallback.NewProfiles(h3ccallback.NewProfile())
	if err != nil {
		return nil, nil, err
	}
	return authenticator, profiles, nil
}
