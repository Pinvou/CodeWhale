package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/humandelegation"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/paymentcallback"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/resilience"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type Service struct {
	repository           Repository
	payments             *PaymentCoordinator
	authorizer           *authorization.Client
	approvalGate         ApprovalExecutionGate
	callbackAuth         *paymentcallback.Authenticator
	callbackProfiles     *paymentcallback.Profiles
	humanDelegations     *humandelegation.Verifier
	catalog              RechargeProductCatalogRepository
	catalogCreateOptions RechargeProductCreateOptionsRepository
	approvalEffects      ApprovalEffectReader
	now                  func() time.Time
}

func (s *Service) WithRechargeProductCatalog(catalog RechargeProductCatalogRepository) *Service {
	s.catalog = catalog
	s.catalogCreateOptions, _ = catalog.(RechargeProductCreateOptionsRepository)
	return s
}

func NewService(repository Repository) *Service {
	service := &Service{repository: repository, now: func() time.Time { return time.Now().UTC() }}
	service.approvalEffects, _ = repository.(ApprovalEffectReader)
	return service
}

func (s *Service) WithPayments(payments *PaymentCoordinator) *Service {
	s.payments = payments
	return s
}

func (s *Service) WithAuthorizer(authorizer *authorization.Client) *Service {
	s.authorizer = authorizer
	return s
}

func (s *Service) WithApprovalExecutionGate(gate ApprovalExecutionGate) *Service {
	s.approvalGate = gate
	return s
}

func (s *Service) WithPaymentCallbacks(authenticator *paymentcallback.Authenticator, profiles *paymentcallback.Profiles) *Service {
	s.callbackAuth = authenticator
	s.callbackProfiles = profiles
	return s
}

func (s *Service) WithHumanDelegations(verifier *humandelegation.Verifier) *Service {
	s.humanDelegations = verifier
	return s
}

func (s *Service) Handler(serviceToken string, adminVerify ...identity.VerifyFunc) http.Handler {
	router := httpserver.NewRouter()
	protected := router.Group("")
	protected.Use(serviceauth.GinMiddleware(serviceToken))
	if s.payments != nil {
		protected.POST("/internal/v1/reconciliation/runs", serviceauth.Require("internal.billing.reconciliation.run"), httpserver.AdaptFunc(s.runReconciliation))
	}
	protected.GET("/internal/v1/approval-command-activations", serviceauth.Require("internal.billing.approval-activation.read"), httpserver.AdaptFunc(s.approvalCommandActivations))
	protected.POST("/internal/v1/approval-command-proofs", serviceauth.Require("internal.billing.approval-proof.read"), httpserver.AdaptFunc(s.internalBillingApprovalProof))
	protected.GET("/internal/v1/approval-effects/:approval_request_id", serviceauth.Require("internal.billing.approval-effect.read"), httpserver.AdaptFunc(s.approvalEffectObservation))
	protected.GET("/internal/v1/approval-recovery/results/:approval_request_id", serviceauth.Require("internal.billing.approval-result.read"), httpserver.AdaptFunc(s.billingApprovalRecoveryResult))
	protected.POST("/internal/v1/wallets/ensure", serviceauth.Require("internal.billing.wallet.ensure"), httpserver.AdaptFunc(s.ensureWallet))
	protected.GET("/internal/v1/wallets/:wallet_id/balance", serviceauth.Require("internal.billing.wallet.read"), httpserver.AdaptFunc(s.getBalance))
	protected.GET("/internal/v1/entitlement-packages", serviceauth.Require("internal.billing.entitlement.read"), httpserver.AdaptFunc(s.listEntitlementPackages))
	if s.catalog != nil {
		protected.POST("/internal/v1/product-admission-verifications/:verification_id/complete", serviceauth.Require("internal.billing.product-admission.verify"), httpserver.AdaptFunc(s.completeProductAdmissionVerification))
	}
	if s.catalog != nil {
		protected.POST("/internal/v1/product-admission-evidence/:evidence_id/revoke", serviceauth.Require("internal.billing.product-admission.revoke"), httpserver.AdaptFunc(s.revokeProductAdmissionInternal))
	}
	protected.POST("/internal/v1/adjustments", serviceauth.Require("internal.billing.adjustment.request"), httpserver.AdaptFunc(s.requestAdjustment))
	protected.POST("/internal/v1/adjustments/:adjustment_id/execute-approved", serviceauth.Require("internal.billing.adjustment.execute"), httpserver.AdaptFunc(s.executeApprovedAdjustment))
	protected.POST("/internal/v1/adjustments/:adjustment_id/reject", serviceauth.Require("internal.billing.adjustment.reject"), httpserver.AdaptFunc(s.rejectAdjustment))
	protected.POST("/internal/v1/chargebacks/confirmed", serviceauth.Require("internal.billing.chargeback.record"), approvalpolicy.Require("BILLING_CHARGEBACK_RECORD", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.recordConfirmedChargeback))
	protected.POST("/internal/v1/refunds/forced", serviceauth.Require("internal.billing.forced-refund.record"), approvalpolicy.Require("BILLING_FORCED_REFUND_RECORD", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.recordForcedRefund))
	protected.GET("/v1/recharge/products", serviceauth.Require("internal.billing.product.read"), httpserver.AdaptFunc(s.listProducts))
	if s.payments != nil {
		protected.GET("/v1/payment-capabilities", serviceauth.Require("internal.billing.payment-capability.read"), httpserver.AdaptFunc(s.paymentCapabilities))
	}
	protected.GET("/v1/wallets/:wallet_id/balance", serviceauth.Require("internal.billing.wallet.read"), httpserver.AdaptFunc(s.getBalance))
	if s.payments != nil {
		protected.POST("/internal/v1/recharge/orders", serviceauth.Require("internal.billing.recharge-order.create"), httpserver.AdaptFunc(s.createRechargeOrder))
	}
	if s.payments != nil {
		protected.GET("/internal/v1/recharge/orders", serviceauth.Require("internal.billing.recharge-order.read"), httpserver.AdaptFunc(s.listRechargeOrders))
	}
	if s.payments != nil {
		protected.GET("/internal/v1/recharge/orders/recent", serviceauth.Require("internal.billing.recharge-order.read"), httpserver.AdaptFunc(s.getRecentRechargeOrder))
	}
	if s.payments != nil {
		protected.GET("/internal/v1/recharge/orders/:order_id", serviceauth.Require("internal.billing.recharge-order.read"), httpserver.AdaptFunc(s.getRechargeOrder))
	}
	if s.payments != nil {
		protected.POST("/internal/v1/recharge/orders/:order_id/close", serviceauth.Require("internal.billing.recharge-order.close"), httpserver.AdaptFunc(s.closeRechargeOrder))
	}
	if s.payments != nil {
		protected.POST("/internal/v1/recharge/orders/:order_id/refunds", serviceauth.Require("internal.billing.refund.create"), httpserver.AdaptFunc(s.createRefund))
	}
	if s.payments != nil {
		protected.POST("/internal/v1/recharge/orders/:order_id/payment-claims", serviceauth.Require("internal.billing.payment-claim.create"), httpserver.AdaptFunc(s.addLatePaymentClaim))
	}
	if s.payments != nil {
		protected.POST("/internal/v1/recharge/orders/:order_id/sync", serviceauth.Require("internal.billing.recharge-order.sync"), httpserver.AdaptFunc(s.syncRechargeOrder))
	}
	if s.payments != nil {
		router.POST("/internal/payment/notifications", httpserver.AdaptFunc(s.paymentNotification))
		if s.callbackAuth != nil && s.callbackProfiles != nil {
			router.POST("/api/v1/payment-callback/token", httpserver.AdaptFunc(s.paymentCallbackToken))
		}
		if s.callbackAuth != nil && s.callbackProfiles != nil {
			router.POST("/api/v1/payment-callbacks", httpserver.AdaptFunc(s.providerPaymentCallback))
		}
	}
	if len(adminVerify) > 0 && adminVerify[0] != nil && s.authorizer != nil {
		admin := router.Group("/admin/v1")
		admin.Use(identity.GinMiddleware(adminVerify[0]))
		admin.GET("/reconciliation-runs", s.authorizer.Require("platform.usage.read"), httpserver.AdaptFunc(s.adminReconciliationRuns))
		admin.POST("/reconciliation-runs", s.authorizer.Require("platform.usage.reconcile.retry"), approvalpolicy.Require("BILLING_RECONCILIATION_RUN", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRunReconciliation))
		admin.POST("/reconciliation-imports", s.authorizer.Require("platform.usage.reconcile.retry"), approvalpolicy.Require("BILLING_PROVIDER_BILL_IMPORT", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminImportReconciliation))
		admin.GET("/reconciliation-cases", s.authorizer.Require("platform.usage.read"), httpserver.AdaptFunc(s.adminReconciliationCases))
		admin.GET("/reconciliation-runs/:run_id/cases", s.authorizer.Require("platform.usage.read"), httpserver.AdaptFunc(s.adminReconciliationCases))
		admin.POST("/reconciliation-cases/:case_id/commands", s.authorizer.Require("platform.usage.reconcile.retry"), approvalpolicy.Require("BILLING_RECONCILIATION_CASE_UPDATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminUpdateReconciliationCase))
		admin.GET("/overview", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminOverview))
		admin.GET("/finance-overviews/accounts", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminFinanceAccountsOverview))
		admin.GET("/finance-overviews/payments", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminFinancePaymentsOverview))
		admin.GET("/finance-overviews/products", s.authorizer.Require("platform.products.read"), httpserver.AdaptFunc(s.adminFinanceProductsOverview))
		admin.GET("/wallets", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminWallets))
		admin.GET("/wallets/:wallet_id/ledger", s.authorizer.Require("platform.billing.ledger.read"), httpserver.AdaptFunc(s.adminWalletLedger))
		admin.GET("/receivables", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminReceivables))
		admin.GET("/receivables/:receivable_id/allocations", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminReceivableAllocations))
		if s.catalog != nil {
			admin.GET("/recharge-products", s.authorizer.Require("platform.products.read"), httpserver.AdaptFunc(s.adminRechargeProducts))
		}
		if s.catalog != nil {
			admin.GET("/recharge-product-create-options", s.authorizer.Require("platform.products.create"), httpserver.AdaptFunc(s.adminRechargeProductCreateOptions))
		}
		if s.catalog != nil {
			admin.POST("/recharge-products", s.authorizer.Require("platform.products.create"), s.requireRecentProductAuthentication(), approvalpolicy.Require("BILLING_RECHARGE_PRODUCT_CREATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminCreateRechargeProduct))
		}
		if s.catalog != nil {
			admin.PUT("/recharge-products/:catalog_item_id/draft", s.authorizer.Require("platform.products.create"), s.requireRecentProductAuthentication(), approvalpolicy.Require("BILLING_RECHARGE_PRODUCT_DRAFT_UPDATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminUpdateRechargeProductDraft))
		}
		if s.catalog != nil {
			admin.GET("/recharge-products/:catalog_item_id/admission-verifications", s.authorizer.Require("platform.products.read"), httpserver.AdaptFunc(s.adminProductAdmissionVerifications))
		}
		if s.catalog != nil {
			admin.POST("/recharge-products/:catalog_item_id/admission-verifications", s.authorizer.Require("platform.products.publish"), s.requireRecentProductAuthentication(), approvalpolicy.Require("BILLING_RECHARGE_PRODUCT_ADMISSION_VERIFY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminStartProductAdmissionVerification))
		}
		if s.catalog != nil {
			admin.POST("/recharge-products/:catalog_item_id/admission-verifications/:verification_id/retry", s.authorizer.Require("platform.products.publish"), s.requireRecentProductAuthentication(), approvalpolicy.Require("BILLING_RECHARGE_PRODUCT_ADMISSION_VERIFY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRetryProductAdmissionVerification))
		}
		if s.catalog != nil {
			admin.POST("/recharge-products/:catalog_item_id/admission-verifications/:verification_id/revoke", s.authorizer.Require("platform.products.publish"), s.requireRecentProductAuthentication(), approvalpolicy.Require("BILLING_RECHARGE_PRODUCT_ADMISSION_REVOKE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRevokeProductAdmission))
		}
		if s.catalog != nil {
			admin.POST("/recharge-products/:catalog_item_id/publish", s.authorizer.Require("platform.products.publish"), s.requireRecentProductAuthentication(), approvalpolicy.Require("BILLING_RECHARGE_PRODUCT_PUBLISH", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminPublishRechargeProduct))
		}
		if s.catalog != nil {
			admin.POST("/recharge-products/:catalog_item_id/unpublish", s.authorizer.Require("platform.products.unpublish"), s.requireRecentProductAuthentication(), approvalpolicy.Require("BILLING_RECHARGE_PRODUCT_UNPUBLISH", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminUnpublishRechargeProduct))
		}
		if s.catalog != nil {
			admin.POST("/recharge-products/:catalog_item_id/delete", s.authorizer.Require("platform.products.delete"), s.requireRecentProductAuthentication(), approvalpolicy.Require("BILLING_RECHARGE_PRODUCT_DELETE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminDeleteRechargeProduct))
		}
		if s.catalog != nil {
			admin.POST("/recharge-products/:catalog_item_id/display-names", s.authorizer.Require("platform.products.publish"), s.requireRecentProductAuthentication(), approvalpolicy.Require("BILLING_RECHARGE_PRODUCT_DISPLAY_NAMES_UPDATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminUpdateRechargeProductDisplayNames))
		}
		admin.GET("/payment-orders", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminPaymentOrders))
		admin.GET("/refunds", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminRefunds))
		admin.GET("/payment-orders/:order_id", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminPaymentOrderDetail))
		admin.GET("/payment-provider-controls", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminPaymentProviderControls))
		admin.POST("/payment-orders/:order_id/sync", s.authorizer.Require("platform.billing.orders.sync"), approvalpolicy.Require("BILLING_PAYMENT_ORDER_SYNC", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminSyncPaymentOrder))
		admin.POST("/payment-orders/:order_id/repair", s.authorizer.Require("platform.billing.repairs.request"), approvalpolicy.Require("BILLING_PAYMENT_ORDER_EVIDENCE_REPAIR", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRepairPaymentOrder))
		admin.POST("/payment-orders/:order_id/refunds", s.authorizer.Require("platform.billing.repairs.request"), approvalpolicy.Require("BILLING_NORMAL_REFUND", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRefundPaymentOrder))
		admin.GET("/late-payment-reviews", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminLatePaymentReviews))
		admin.GET("/late-payment-reviews/:case_id", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminLatePaymentReviewDetail))
		admin.POST("/late-payment-reviews/:case_id/claim", s.authorizer.Require("platform.billing.orders.sync"), approvalpolicy.Require("BILLING_LATE_PAYMENT_CLAIM", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminClaimLatePaymentReview))
		admin.POST("/late-payment-reviews/:case_id/query", s.authorizer.Require("platform.billing.orders.sync"), approvalpolicy.Require("BILLING_LATE_PAYMENT_QUERY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminQueryLatePaymentReview))
		admin.POST("/late-payment-reviews/:case_id/schedule", s.authorizer.Require("platform.billing.orders.sync"), approvalpolicy.Require("BILLING_LATE_PAYMENT_SCHEDULE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminScheduleLatePaymentReview))
		admin.POST("/late-payment-reviews/:case_id/archive", s.authorizer.Require("platform.billing.orders.sync"), approvalpolicy.Require("BILLING_LATE_PAYMENT_ARCHIVE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminArchiveLatePaymentReview))
		admin.POST("/late-payment-reviews/:case_id/provider-blind", s.authorizer.Require("platform.billing.orders.sync"), approvalpolicy.Require("BILLING_PROVIDER_BLIND_ESCALATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminEscalateProviderBlind))
		admin.GET("/payment-anomalies", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminPaymentAnomalies))
		admin.GET("/payment-anomalies/:case_id", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminPaymentAnomalyDetail))
		admin.POST("/payment-anomalies/:case_id/claim", s.authorizer.Require("platform.billing.orders.sync"), approvalpolicy.Require("BILLING_PAYMENT_ANOMALY_CLAIM", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminClaimPaymentAnomaly))
		admin.POST("/payment-anomalies/:case_id/resolve", s.authorizer.Require("platform.billing.repairs.request"), approvalpolicy.Require("BILLING_PAYMENT_ANOMALY_RESOLVE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminResolvePaymentAnomaly))
		admin.GET("/adjustments", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminAdjustments))
		admin.POST("/adjustments", s.authorizer.Require("platform.billing.repairs.request"), approvalpolicy.Require("BILLING_ADJUSTMENT_REQUEST", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRequestAdjustment))
		admin.POST("/adjustments/:adjustment_id/approve", s.authorizer.Require("platform.billing.adjustments.approve"), approvalpolicy.Require("WALLET_ADJUSTMENT", approvalpolicy.ApprovalRequired), httpserver.AdaptFunc(s.adminApproveAdjustment))
		admin.POST("/adjustments/:adjustment_id/reject", s.authorizer.Require("platform.billing.adjustments.approve"), approvalpolicy.Require("BILLING_ADJUSTMENT_REJECT", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRejectAdjustment))
	}
	return router
}

func (s *Service) paymentCallbackToken(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	r.Body = http.MaxBytesReader(w, r.Body, 8<<10)
	clientID, clientSecret, ok := r.BasicAuth()
	if !ok || r.URL.RawQuery != "" || !hasMediaType(r, "application/x-www-form-urlencoded") || r.ParseForm() != nil || r.PostForm.Get("grant_type") != "client_credentials" || hasUnexpectedFormField(r.PostForm, "grant_type", "scope") {
		w.Header().Set("WWW-Authenticate", `Basic realm="payment-callback"`)
		api.WriteError(w, http.StatusUnauthorized, "INVALID_CLIENT", "callback client authentication failed", httpserver.RequestID(r.Context()))
		return
	}
	token, err := s.callbackAuth.Issue(clientID, clientSecret, r.PostForm.Get("scope"))
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Basic realm="payment-callback"`)
		api.WriteError(w, http.StatusUnauthorized, "INVALID_CLIENT", "callback client authentication failed", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, token)
}

func (s *Service) providerPaymentCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	principal, err := s.callbackAuth.VerifyBearer(r.Header.Get("Authorization"))
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="payment-callback"`)
		api.WriteError(w, http.StatusUnauthorized, "PAYMENT_CALLBACK_UNAUTHORIZED", "payment callback authentication failed", httpserver.RequestID(r.Context()))
		return
	}
	capabilities := s.payments.Capabilities()
	if principal.ProviderID != capabilities.ProviderID || principal.ProviderVersion != capabilities.Version {
		api.WriteError(w, http.StatusUnauthorized, "PAYMENT_CALLBACK_REJECTED", "payment callback rejected", httpserver.RequestID(r.Context()))
		return
	}
	if !hasMediaType(r, "application/json") {
		api.WriteError(w, http.StatusBadRequest, "INVALID_PAYMENT_CALLBACK", "invalid payment callback", httpserver.RequestID(r.Context()))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_PAYMENT_CALLBACK", "invalid payment callback", httpserver.RequestID(r.Context()))
		return
	}
	notification, err := s.callbackProfiles.Parse(principal, body, s.now())
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_PAYMENT_CALLBACK", "invalid payment callback", httpserver.RequestID(r.Context()))
		return
	}
	notificationID, _, err := s.payments.PersistProviderNotification(r.Context(), principal.ProviderID, principal.MerchantID, notification.ID, notification.SentAt, notification.Provider, body, paymentRequestSource(r))
	if err != nil {
		if errors.Is(err, ErrForbiddenPayment) {
			api.WriteError(w, http.StatusUnauthorized, "PAYMENT_CALLBACK_REJECTED", "payment callback rejected", httpserver.RequestID(r.Context()))
			return
		}
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "accepted", "notification_id": notificationID.String()})
}

func hasMediaType(r *http.Request, expected string) bool {
	mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(r.Header.Get("Content-Type")))
	return err == nil && strings.EqualFold(mediaType, expected)
}

func hasUnexpectedFormField(values map[string][]string, allowed ...string) bool {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	for key := range values {
		if _, ok := allowedSet[key]; !ok {
			return true
		}
	}
	return false
}

type chargebackRepository interface {
	RecordConfirmedChargebackForReview(context.Context, uuid.UUID, string, json.RawMessage) (ChargebackCase, error)
}

type forcedRefundRepository interface {
	RecordAndFreezeForcedRefund(context.Context, uuid.UUID, string, int64, json.RawMessage) (ForcedRefundCase, error)
}

type latePaymentClaimRepository interface {
	AddLatePaymentClaim(context.Context, uuid.UUID, uuid.UUID, string) (LatePaymentReviewCase, error)
}

func (s *Service) addLatePaymentClaim(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(latePaymentClaimRepository)
	orderID, parseErr := uuid.Parse(r.PathValue("order_id"))
	var body struct {
		RequesterID uuid.UUID `json:"requester_id"`
		Statement   string    `json:"statement"`
	}
	if decodeErr := api.DecodeJSON(w, r, &body); !ok || parseErr != nil || decodeErr != nil || body.RequesterID == uuid.Nil || len(strings.TrimSpace(body.Statement)) < 10 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "payment order, requester and statement are required", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.AddLatePaymentClaim(r.Context(), orderID, body.RequesterID, body.Statement)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, item)
}

func (s *Service) recordConfirmedChargeback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PaymentTransactionID uuid.UUID       `json:"payment_transaction_id"`
		ProviderChargebackID string          `json:"provider_chargeback_id"`
		PayloadSnapshot      json.RawMessage `json:"payload_snapshot"`
	}
	repository, ok := s.repository.(chargebackRepository)
	if err := api.DecodeJSON(w, r, &body); err != nil || !ok || body.PaymentTransactionID == uuid.Nil || strings.TrimSpace(body.ProviderChargebackID) == "" || len(body.PayloadSnapshot) == 0 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid confirmed chargeback", httpserver.RequestID(r.Context()))
		return
	}
	result, err := repository.RecordConfirmedChargebackForReview(r.Context(), body.PaymentTransactionID, body.ProviderChargebackID, body.PayloadSnapshot)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) recordForcedRefund(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PaymentTransactionID uuid.UUID       `json:"payment_transaction_id"`
		ProviderRefundID     string          `json:"provider_refund_id"`
		AmountFen            int64           `json:"amount_fen"`
		PayloadSnapshot      json.RawMessage `json:"payload_snapshot"`
	}
	repository, ok := s.repository.(forcedRefundRepository)
	if err := api.DecodeJSON(w, r, &body); err != nil || !ok || body.PaymentTransactionID == uuid.Nil || strings.TrimSpace(body.ProviderRefundID) == "" || body.AmountFen <= 0 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid forced refund evidence", httpserver.RequestID(r.Context()))
		return
	}
	result, err := repository.RecordAndFreezeForcedRefund(r.Context(), body.PaymentTransactionID, body.ProviderRefundID, body.AmountFen, body.PayloadSnapshot)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) paymentCapabilities(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, s.payments.ProductCapabilities())
}

func (s *Service) listRechargeOrders(w http.ResponseWriter, r *http.Request) {
	requesterID, requesterErr := uuid.Parse(r.URL.Query().Get("requester_id"))
	walletID, walletErr := uuid.Parse(r.URL.Query().Get("wallet_id"))
	if requesterErr != nil || walletErr != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid recharge order list", httpserver.RequestID(r.Context()))
		return
	}
	items, err := s.payments.ListOrders(r.Context(), requesterID, walletID, 50)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

type entitlementPackageRepository interface {
	ListPackages(context.Context, uuid.UUID, uuid.UUID, int) ([]EntitlementPackage, PackageSummary, error)
}

func (s *Service) listEntitlementPackages(w http.ResponseWriter, r *http.Request) {
	userID, userErr := uuid.Parse(r.URL.Query().Get("user_id"))
	walletID, walletErr := uuid.Parse(r.URL.Query().Get("wallet_id"))
	repository, ok := s.repository.(entitlementPackageRepository)
	if userErr != nil || walletErr != nil || !ok {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid entitlement package lookup", httpserver.RequestID(r.Context()))
		return
	}
	items, summary, err := repository.ListPackages(r.Context(), userID, walletID, 100)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"summary": summary, "items": items})
}

func (s *Service) requestAdjustment(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WalletID           uuid.UUID `json:"wallet_id"`
		AmountMicrocredits int64     `json:"amount_microcredits"`
		Reason             string    `json:"reason"`
		RequestedBy        uuid.UUID `json:"requested_by"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.WalletID == uuid.Nil || body.RequestedBy == uuid.Nil || body.AmountMicrocredits == 0 || strings.TrimSpace(body.Reason) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid manual adjustment request", httpserver.RequestID(r.Context()))
		return
	}
	adjustment, err := s.repository.RequestAdjustment(r.Context(), body.WalletID, body.AmountMicrocredits, body.Reason, body.RequestedBy)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, adjustment)
}

func (s *Service) rejectAdjustment(w http.ResponseWriter, r *http.Request) {
	adjustmentID, err := uuid.Parse(r.PathValue("adjustment_id"))
	var body struct {
		RejectedBy     uuid.UUID `json:"rejected_by"`
		DecisionReason string    `json:"decision_reason"`
	}
	if decodeErr := api.DecodeJSON(w, r, &body); err != nil || decodeErr != nil || body.RejectedBy == uuid.Nil || strings.TrimSpace(body.DecisionReason) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid adjustment rejection", httpserver.RequestID(r.Context()))
		return
	}
	adjustment, err := s.repository.RejectAdjustment(r.Context(), adjustmentID, body.RejectedBy, body.DecisionReason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, adjustment)
}

func (s *Service) createRechargeOrder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequesterID            uuid.UUID `json:"requester_id"`
		WalletID               uuid.UUID `json:"wallet_id"`
		ProductID              string    `json:"product_id"`
		Channel                string    `json:"channel"`
		Mobile                 string    `json:"mobile"`
		CommerceCustomerStatus string    `json:"commerce_customer_status"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.RequesterID == uuid.Nil || body.WalletID == uuid.Nil || body.ProductID == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid recharge order", httpserver.RequestID(r.Context()))
		return
	}
	if s.payments.Capabilities().CommerceCustomerRequired && body.CommerceCustomerStatus != "READY" {
		s.writeError(w, r, ErrCommerceCustomerNotReady)
		return
	}
	if err := s.payments.AdmitCreate(r.Context(), body.RequesterID, nil, paymentRequestSource(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	var order PaymentOrder
	var created bool
	var err error
	order, created, err = s.payments.CreateMobileOrder(r.Context(), body.RequesterID, body.WalletID, body.ProductID, body.Channel, body.Mobile, r.Header.Get("Idempotency-Key"))

	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, status, order)
}

func (s *Service) getRechargeOrder(w http.ResponseWriter, r *http.Request) {
	orderID, err := uuid.Parse(r.PathValue("order_id"))
	requesterID, requesterErr := uuid.Parse(r.URL.Query().Get("requester_id"))
	if err != nil || requesterErr != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid recharge order lookup", httpserver.RequestID(r.Context()))
		return
	}
	// Authorize against the local order before making a provider request. This
	// keeps an authenticated caller from using guessed order IDs to trigger
	// Mall lookups for another account.
	order, err := s.payments.GetOrder(r.Context(), orderID, requesterID)
	if err == nil {
		var dispatch bool
		dispatch, err = s.payments.AdmitOrderQuery(r.Context(), order, paymentRequestSource(r))
		if err == nil && dispatch {
			order, err = s.payments.SyncOrder(r.Context(), order.ID)
		}
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	maskClosedPaymentInstructions(&order)
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, order)
}

func (s *Service) getRecentRechargeOrder(w http.ResponseWriter, r *http.Request) {
	requesterID, requesterErr := uuid.Parse(r.URL.Query().Get("requester_id"))
	walletID, walletErr := uuid.Parse(r.URL.Query().Get("wallet_id"))
	if requesterErr != nil || walletErr != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid recent recharge order lookup", httpserver.RequestID(r.Context()))
		return
	}
	order, err := s.payments.RecentOrder(r.Context(), requesterID, walletID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"order": order})
}

func (s *Service) closeRechargeOrder(w http.ResponseWriter, r *http.Request) {
	orderID, err := uuid.Parse(r.PathValue("order_id"))
	var body struct {
		RequesterID uuid.UUID `json:"requester_id"`
	}
	if decodeErr := api.DecodeJSON(w, r, &body); err != nil || decodeErr != nil || body.RequesterID == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid recharge order close request", httpserver.RequestID(r.Context()))
		return
	}
	order, err := s.payments.CloseOrder(r.Context(), orderID, body.RequesterID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, order)
}

func (s *Service) createRefund(w http.ResponseWriter, r *http.Request) {
	if !s.payments.NormalRefundCreationEnabled() {
		s.writeError(w, r, ErrNormalRefundCreationDisabled)
		return
	}
	orderID, err := uuid.Parse(r.PathValue("order_id"))
	var body struct {
		RequesterID          uuid.UUID `json:"requester_id"`
		PaymentTransactionID uuid.UUID `json:"payment_transaction_id"`
		EntitlementPackageID uuid.UUID `json:"entitlement_package_id"`
		Reason               string    `json:"reason"`
	}
	if decodeErr := api.DecodeJSON(w, r, &body); err != nil || decodeErr != nil || body.RequesterID == uuid.Nil || body.PaymentTransactionID == uuid.Nil || body.EntitlementPackageID == uuid.Nil || strings.TrimSpace(body.Reason) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid refund request", httpserver.RequestID(r.Context()))
		return
	}
	if err := s.payments.AdmitRefund(r.Context(), body.RequesterID, body.PaymentTransactionID, body.EntitlementPackageID, paymentRequestSource(r)); err != nil {
		s.writeError(w, r, err)
		return
	}
	refund, created, err := s.payments.CreateRefund(r.Context(), orderID, body.RequesterID, body.PaymentTransactionID, body.EntitlementPackageID, r.Header.Get("Idempotency-Key"), body.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	api.WriteJSON(w, status, refund)
}

func (s *Service) syncRechargeOrder(w http.ResponseWriter, r *http.Request) {
	orderID, err := uuid.Parse(r.PathValue("order_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid recharge order", httpserver.RequestID(r.Context()))
		return
	}
	order, err := s.payments.SyncOrder(r.Context(), orderID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, order)
}

func (s *Service) paymentNotification(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid notification", httpserver.RequestID(r.Context()))
		return
	}
	var notificationID uuid.UUID
	notificationID, _, err = s.payments.PersistNotification(r.Context(), r.Header.Get("X-Pinvou-Payment-Channel"), r.Header.Get("X-Pinvou-Payment-Signature"), body, paymentRequestSource(r))
	if errors.Is(err, ErrForbiddenPayment) {
		api.WriteError(w, http.StatusUnauthorized, "PAYMENT_NOTIFICATION_REJECTED", "payment notification rejected", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusAccepted, map[string]string{"status": "accepted", "notification_id": notificationID.String()})
}

func (s *Service) ensureWallet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		OwnerType string    `json:"owner_type"`
		OwnerID   uuid.UUID `json:"owner_id"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.OwnerID == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid wallet owner", httpserver.RequestID(r.Context()))
		return
	}
	wallet, created, err := s.repository.EnsureWallet(r.Context(), strings.ToUpper(body.OwnerType), body.OwnerID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	api.WriteJSON(w, status, wallet)
}

func (s *Service) getBalance(w http.ResponseWriter, r *http.Request) {
	walletID, err := uuid.Parse(r.PathValue("wallet_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid wallet identifier", httpserver.RequestID(r.Context()))
		return
	}
	wallet, err := s.repository.GetWallet(r.Context(), walletID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, wallet)
}

func (s *Service) listProducts(w http.ResponseWriter, r *http.Request) {
	currency := r.URL.Query().Get("currency")
	if currency == "" {
		currency = "CNY"
	}
	products, err := s.repository.ListProducts(r.Context(), currency, r.URL.Query().Get("channel"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if s.payments != nil {
		products, err = s.payments.FilterProducts(r.Context(), products)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		products, err = s.payments.ApplyProductAvailability(r.Context(), products)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	var version int64
	for _, product := range products {
		if product.CatalogVersion > version {
			version = product.CatalogVersion
		}
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"catalog_version": version, "items": products})
}

func (s *Service) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := http.StatusInternalServerError, "INTERNAL_ERROR", "the billing request could not be completed"
	switch {
	case errors.Is(err, ErrApprovalActivationUnavailable):
		status, code, message = http.StatusServiceUnavailable, "BILLING_APPROVAL_ACTIVATION_UNAVAILABLE", "billing approval command activation is temporarily unavailable"
	case errors.Is(err, ErrNormalRefundCreationDisabled):
		status, code, message = http.StatusConflict, "NORMAL_REFUND_CREATION_DISABLED", "normal refund creation is currently unavailable"
	case errors.Is(err, ErrPaymentRateLimited):
		retryAfter := 1
		var limited *PaymentRateLimitError
		if errors.As(err, &limited) {
			retryAfter = max(1, int(limited.RetryAfter.Seconds()+0.999))
		}
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		api.WriteJSON(w, http.StatusTooManyRequests, api.Error{Code: "PAYMENT_RATE_LIMITED", Message: "the payment action exceeds a rate limit", RequestID: httpserver.RequestID(r.Context()), Details: map[string]any{"retry_after_seconds": retryAfter}})
		return
	case errors.Is(err, resilience.ErrCircuitOpen):
		status, code, message = http.StatusServiceUnavailable, "PAYMENT_PROVIDER_CIRCUIT_OPEN", "the payment provider capability is temporarily unavailable"
		var open *resilience.CircuitOpenError
		if errors.As(err, &open) {
			w.Header().Set("Retry-After", strconv.Itoa(max(1, int(open.RetryAfter.Seconds()+0.999))))
		}
	case errors.Is(err, ErrNotFound):
		status, code, message = http.StatusNotFound, "NOT_FOUND", "the billing resource was not found"
	case errors.Is(err, ErrMemberLifecycleConflict):
		status, code, message = http.StatusConflict, "MEMBER_LIFECYCLE_CONFLICT", "the organization membership changed or is no longer active"
	case errors.Is(err, ErrMemberQuotaInsufficient):
		status, code, message = http.StatusPaymentRequired, "MEMBER_QUOTA_INSUFFICIENT", "the organization member has insufficient granted quota"
	case errors.Is(err, ErrConflict):
		status, code, message = http.StatusConflict, "BILLING_CONFLICT", "the billing request conflicts with existing state"
	case errors.Is(err, payment.ErrUnsupported):
		status, code, message = http.StatusUnprocessableEntity, "PAYMENT_CAPABILITY_UNSUPPORTED", "the selected payment provider does not support this operation"
	case errors.Is(err, ErrInsufficientBalance):
		status, code, message = http.StatusPaymentRequired, "WALLET_INSUFFICIENT", "the wallet has insufficient available credits"
	case errors.Is(err, ErrWalletUnavailable):
		status, code, message = http.StatusLocked, "WALLET_UNAVAILABLE", "the wallet is not active"
	case errors.Is(err, ErrUsageAlreadyTerminal):
		status, code, message = http.StatusConflict, "USAGE_ALREADY_SETTLED", "the usage hold is already terminal"
	case errors.Is(err, ErrUsagePlatformLossReview):
		status, code, message = http.StatusConflict, "USAGE_PLATFORM_LOSS_REVIEW", "the usage was reported after its entitlement expired and requires platform-loss review"
	case errors.Is(err, ErrPaymentProvider):
		status, code, message = http.StatusBadGateway, "PAYMENT_PROVIDER_UNAVAILABLE", "the payment provider request could not be completed"
	case errors.Is(err, ErrCommerceCustomerNotReady):
		status, code, message = http.StatusConflict, "COMMERCE_CUSTOMER_NOT_READY", "the payment provider requires a ready Commerce Customer mapping"
	case errors.Is(err, ErrInsufficientComponentCapacity):
		status, code, message = http.StatusConflict, "INSUFFICIENT_COMPONENT_CAPACITY", "the selected entitlement component has insufficient allocatable capacity"
	case errors.Is(err, ErrIdempotencyConflict):
		status, code, message = http.StatusConflict, "IDEMPOTENCY_CONFLICT", "the idempotency key was used with a different request"
	case errors.Is(err, ErrPriceMissing):
		status, code, message = http.StatusUnprocessableEntity, "PRICE_MISSING", "an exact price or required unit configuration is missing"
	case errors.Is(err, ErrPriceAmbiguous):
		status, code, message = http.StatusConflict, "PRICE_AMBIGUOUS", "more than one price matches the request"
	case errors.Is(err, ErrPriceUnavailable):
		status, code, message = http.StatusServiceUnavailable, "PRICE_UNAVAILABLE", "pricing is explicitly unavailable for this model"
	case errors.Is(err, ErrPriceWindowConflict):
		status, code, message = http.StatusConflict, "PRICE_WINDOW_CONFLICT", "the price effective window overlaps a published version"
	case errors.Is(err, ErrPriceETagConflict):
		status, code, message = http.StatusPreconditionFailed, "PRICE_ETAG_CONFLICT", "the pricing space changed; reload it before retrying"
	}
	api.WriteError(w, status, code, message, httpserver.RequestID(r.Context()))
}

func (s *Service) runReconciliation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Channel      string `json:"channel"`
		BusinessDate string `json:"business_date"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	businessDate, err := time.Parse("2006-01-02", body.BusinessDate)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid reconciliation date", httpserver.RequestID(r.Context()))
		return
	}
	run, err := s.payments.RunReconciliation(r.Context(), body.Channel, businessDate)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, run)
}
