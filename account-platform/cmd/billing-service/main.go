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
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
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
		log.Fatal("BILLING_LOG_CONFIGURATION_FAILED")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cfg, err := config.Load("billing-service")
	if err != nil {
		log.Fatal("BILLING_CONFIGURATION_INVALID")
	}
	credential := serviceauth.CredentialFromEnvironment()
	if strings.TrimSpace(credential) == "" {
		log.Fatal("BILLING_SERVICE_IDENTITY_REQUIRED")
	}
	if err := approvalpolicy.ValidateAll(map[string]approvalpolicy.Mode{"BILLING_PAYMENT_EXPIRY_RECONCILE": approvalpolicy.AuditOnly, "BILLING_ENTITLEMENT_PACKAGE_EXPIRE": approvalpolicy.AuditOnly, "BILLING_MANUAL_REVIEW_SLA_ESCALATE": approvalpolicy.AuditOnly, "BILLING_DAILY_RECONCILIATION": approvalpolicy.AuditOnly}); err != nil {
		log.Fatal("BILLING_COMMAND_POLICY_INVALID")
	}
	onCall, err := manualreview.ParseOnCallSchedule(os.Getenv("MANUAL_REVIEW_ON_CALL_USER_IDS"), strings.HasPrefix(strings.ToLower(cfg.Environment), "production"))
	if err != nil {
		log.Fatal("BILLING_ON_CALL_SCHEDULE_INVALID")
	}
	repository, err := billing.NewPostgresRepository(ctx, os.Getenv("DATABASE_URL"))
	if err != nil {
		log.Fatal("BILLING_DATABASE_INITIALIZATION_FAILED")
	}
	defer repository.Close()
	expiredHoldClient, err := expiredHoldClientFromEnvironment(credential)
	if err != nil {
		log.Fatal("BILLING_HOLD_EXPIRY_CONFIGURATION_INVALID")
	}
	payments, err := paymentCoordinator(ctx, repository, cfg.Environment)
	if err != nil {
		log.Fatal("BILLING_PAYMENT_CONFIGURATION_INVALID")
	}
	authorizer, err := authorization.NewClient(os.Getenv("ACCOUNT_HTTP_URL"), credential)
	if err != nil {
		log.Fatal("BILLING_AUTHORIZATION_CONFIGURATION_INVALID")
	}
	gate, err := billing.NewHTTPApprovalExecutionGate(os.Getenv("ACCOUNT_HTTP_URL"), credential)
	if err != nil {
		log.Fatal("BILLING_APPROVAL_GATE_CONFIGURATION_INVALID")
	}
	modelPriceProjectionEnabled, err := optionalBooleanEnvironment("BILLING_ADMIN_MODEL_PRICE_PROJECTION_ENABLED", false)
	if err != nil {
		log.Fatal("BILLING_MODEL_PRICE_PROJECTION_CONFIGURATION_INVALID")
	}
	bonusEligibility, err := billing.NewHTTPRegistrationBonusEligibilityVerifier(os.Getenv("ACCOUNT_HTTP_URL"), credential)
	if err != nil {
		log.Fatal("BILLING_REGISTRATION_BONUS_CONFIGURATION_INVALID")
	}
	service := billing.NewService(repository).WithPayments(payments).WithRechargeProductCatalog(repository).WithAuthorizer(authorizer).WithApprovalExecutionGate(gate).WithAdminModelPriceProjection(modelPriceProjectionEnabled).WithRegistrationBonusEligibilityVerifier(bonusEligibility)
	closureStates, err := billing.NewHTTPAccountClosureStateVerifier(os.Getenv("ACCOUNT_HTTP_URL"), credential)
	if err != nil {
		log.Fatal("BILLING_ACCOUNT_CLOSURE_CONFIGURATION_INVALID")
	}
	service.WithAccountClosureStateVerifier(closureStates)
	keys, err := humandelegation.DecodeVerificationKeysJSON(os.Getenv("HUMAN_DELEGATION_VERIFICATION_KEYS_JSON"))
	if err != nil {
		log.Fatal("BILLING_HUMAN_DELEGATION_KEYS_INVALID")
	}
	delegation, err := humandelegation.NewVerifier(keys)
	if err != nil {
		log.Fatal("BILLING_HUMAN_DELEGATION_CONFIGURATION_INVALID")
	}
	service.WithHumanDelegations(delegation)
	if strings.EqualFold(strings.TrimSpace(os.Getenv("BILLING_PAYMENT_PROVIDER")), "h3c_mall") {
		authenticator, profiles, e := paymentCallbackConfiguration(payments.Capabilities())
		if e != nil {
			log.Fatal("BILLING_CALLBACK_CONFIGURATION_INVALID")
		}
		service.WithPaymentCallbacks(authenticator, profiles)
	}
	verify, err := identity.NewOIDCVerifyFuncWithJWKS(ctx, os.Getenv("OIDC_ISSUER"), os.Getenv("OIDC_JWKS_URL"), os.Getenv("OIDC_AUDIENCE"), cfg.Environment)
	if err != nil {
		log.Fatal("BILLING_OIDC_CONFIGURATION_INVALID")
	}
	resources, err := billingApprovalConsumerResources(time.Now().UTC())
	if err != nil {
		log.Fatal("BILLING_APPROVAL_CONSUMER_RESOURCES_UNAVAILABLE")
	}
	broker, err := events.NewNATSBroker(ctx, os.Getenv("NATS_URL"), "billing-command-executor", events.NATSConfig{Environment: cfg.Environment, ResourceMode: "provisioned", CAFile: os.Getenv("NATS_CA_FILE"), SeedFile: os.Getenv("NATS_NKEY_SEED_FILE"), CredentialsFile: os.Getenv("NATS_CREDENTIALS_FILE"), RequireExplicitAuthentication: true, ApprovalConsumerResources: resources})
	if err != nil {
		log.Fatal("BILLING_EVENT_BROKER_UNAVAILABLE")
	}
	defer broker.Close()
	for _, descriptor := range approvalconsumer.BillingCommands() {
		consumer, e := billing.NewAuthorizationCommandConsumer(service, gate, descriptor.Durable)
		if e != nil {
			log.Fatal("BILLING_APPROVAL_CONSUMER_INVALID")
		}
		stopConsumer, e := broker.ConsumeAuthorization(ctx, descriptor.Durable, consumer.Handle)
		if e != nil {
			log.Fatal("BILLING_APPROVAL_CONSUMER_UNAVAILABLE")
		}
		defer stopConsumer()
	}
	stopRefund, err := broker.ConsumeUntilResolved(ctx, "billing-refund-executor-v1", "billing.refund.requested.v1", billing.NewRefundCommandConsumer(payments).Handle)
	if err != nil {
		log.Fatal("BILLING_REFUND_CONSUMER_UNAVAILABLE")
	}
	defer stopRefund()
	stopMembership, err := broker.ConsumeUntilResolved(ctx, "billing-member-lifecycle-v1", "account.membership.changed.v1", billing.NewMemberLifecycleConsumer(repository).Handle)
	if err != nil {
		log.Fatal("BILLING_MEMBERSHIP_CONSUMER_UNAVAILABLE")
	}
	defer stopMembership()
	interval := 15 * time.Second
	if raw := strings.TrimSpace(os.Getenv("BILLING_PAYMENT_EXPIRY_INTERVAL_SECONDS")); raw != "" {
		seconds, e := strconv.Atoi(raw)
		if e != nil || seconds < 1 || seconds > 300 {
			log.Fatal("BILLING_PAYMENT_EXPIRY_INTERVAL_INVALID")
		}
		interval = time.Duration(seconds) * time.Second
	}
	if payments.Capabilities().OrderQuerySupported {
		interval = max(interval, time.Duration(payments.Capabilities().MinimumQueryIntervalSeconds)*time.Second)
		go observability.RunBackground(ctx, "expired-payment-reconciliation", billing.NewExpiredPaymentReconciler(repository, payments, interval).Run)
		go observability.RunBackground(ctx, "archive-blind-recovery", billing.NewArchiveBlindRecoveryWorker(repository, payments, time.Minute).Run)
	}
	if payments.Capabilities().NotificationSupported {
		go observability.RunBackground(ctx, "payment-notification-inbox", billing.NewPaymentNotificationWorker(payments, 50*time.Millisecond).Run)
	}
	admissionTTL, err := testAdmissionEvidenceTTLFromEnvironment()
	if err != nil {
		log.Fatal("BILLING_TEST_ADMISSION_TTL_INVALID")
	}
	go observability.RunBackground(ctx, "package-expiry", billing.NewPackageExpiryWorker(repository, time.Minute).Run)
	go observability.RunBackground(ctx, "expired-hold-recovery", billing.NewExpiredHoldRecoveryWorker(repository, expiredHoldClient, time.Minute).Run)
	go observability.RunBackground(ctx, "product-admission-expiry", billing.NewProductAdmissionExpiryWorker(repository, time.Minute).Run)
	go observability.RunBackground(ctx, "product-admission-verification", billing.NewProductAdmissionVerificationWorker(repository, payments.Capabilities(), 5*time.Second, admissionTTL).Run)
	go observability.RunBackground(ctx, "manual-review-sla", billing.NewManualReviewSLAWorker(repository, time.Minute, onCall).Run)
	go observability.RunBackground(ctx, "admin-pagination-cleanup", billing.NewAdminPaginationCleanupWorker(repository, time.Minute).Run)
	if payments.Capabilities().BillSupported || payments.Capabilities().BillFileImportSupported {
		go observability.RunBackground(ctx, "daily-payment-reconciliation", billing.NewDailyPaymentReconciliationWorker(payments).Run)
	}
	handler := service.Handler(credential, verify)
	ready := func(c context.Context) error {
		if err := repository.Ping(c); err != nil {
			return err
		}
		if err := broker.Ping(c); err != nil {
			return err
		}
		_, err := billingApprovalConsumerResources(time.Now().UTC())
		return err
	}
	collectors := append(repository.AdminPaginationCollectors(), service.AdminModelPriceProjectionCollectors()...)
	if err := httpserver.RunWithApplicationAndCollectors("billing-service", handler, ready, collectors...); err != nil {
		log.Fatal("BILLING_SERVER_FAILED")
	}
}

func expiredHoldClientFromEnvironment(credential string) (billing.ExpiredHoldProofClient, error) {
	endpoint := strings.TrimSpace(os.Getenv("TOKEN_HOLD_EXPIRY_URL"))
	if endpoint == "" {
		// Earlier waves have no Token management endpoint. The worker still
		// records expired holds for manual review; missing trust is never proof.
		return nil, nil
	}
	client, err := serviceauth.NewWorkloadHTTPClient(os.Getenv("WORKLOAD_IDENTITY_CA_FILE"))
	if err != nil {
		return nil, err
	}
	return billing.NewHTTPExpiredHoldProofClient(endpoint, credential, client)
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
		var err error
		provider, err = paymentsimulator.New(os.Getenv("PAYMENT_PROVIDER_BASE_URL"), serviceauth.CredentialFromEnvironment())
		if err != nil {
			return nil, err
		}
		allowTestOnly = strings.EqualFold(environment, "development") || strings.EqualFold(environment, "test")
	default:
		return nil, errors.New("BILLING_PAYMENT_PROVIDER must select an explicit installed payment adapter")
	}
	capabilities := provider.Capabilities()
	coordinator, err := billing.NewPaymentCoordinatorWithProvider(repository, provider, environment)
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
