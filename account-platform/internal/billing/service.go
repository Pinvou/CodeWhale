package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/payment"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type Service struct {
	repository   Repository
	payments     *PaymentCoordinator
	authorizer   *authorization.Client
	approvalGate ApprovalExecutionGate
	now          func() time.Time
}

func NewService(repository Repository) *Service {
	return &Service{repository: repository, now: func() time.Time { return time.Now().UTC() }}
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

func (s *Service) Handler(serviceToken string, adminVerify ...identity.VerifyFunc) http.Handler {
	router := httpserver.NewRouter()
	protected := router.Group("")
	protected.Use(serviceauth.GinMiddleware(serviceToken))
	protected.POST("/internal/v1/wallets/ensure", httpserver.AdaptFunc(s.ensureWallet))
	protected.GET("/internal/v1/wallets/:wallet_id/balance", httpserver.AdaptFunc(s.getBalance))
	protected.GET("/internal/v1/entitlement-packages", httpserver.AdaptFunc(s.listEntitlementPackages))
	protected.POST("/internal/v1/holds/reserve", httpserver.AdaptFunc(s.reserve))
	protected.POST("/internal/v1/holds/extend", httpserver.AdaptFunc(s.extendHold))
	protected.POST("/internal/v1/holds/commit", httpserver.AdaptFunc(s.commit))
	protected.POST("/internal/v1/holds/release", httpserver.AdaptFunc(s.release))
	protected.GET("/internal/v1/users/:user_id/account-closure-assessment", httpserver.AdaptFunc(s.accountClosureAssessment))
	protected.GET("/internal/v1/pricing/current", httpserver.AdaptFunc(s.currentPrice))
	protected.GET("/internal/v1/pricing/:price_version_id", httpserver.AdaptFunc(s.priceByID))
	protected.PUT("/internal/v1/pricing/formula-bindings/:formula_version_id", httpserver.AdaptFunc(s.putFormulaBinding))
	protected.POST("/internal/v1/adjustments", httpserver.AdaptFunc(s.requestAdjustment))
	protected.POST("/internal/v1/adjustments/:adjustment_id/execute-approved", httpserver.AdaptFunc(s.executeApprovedAdjustment))
	protected.POST("/internal/v1/adjustments/:adjustment_id/reject", httpserver.AdaptFunc(s.rejectAdjustment))
	protected.POST("/internal/v1/chargebacks/confirmed", approvalpolicy.Require("BILLING_CHARGEBACK_RECORD", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.recordConfirmedChargeback))
	protected.POST("/internal/v1/refunds/forced", approvalpolicy.Require("BILLING_FORCED_REFUND_RECORD", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.recordForcedRefund))
	protected.PUT("/internal/v1/organizations/:organization_id/lifecycle", approvalpolicy.Require("BILLING_ORGANIZATION_LIFECYCLE_APPLY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.applyOrganizationLifecycle))
	protected.PUT("/internal/v1/organizations/:organization_id/units/:unit_id/lifecycle", approvalpolicy.Require("BILLING_ORGANIZATION_UNIT_LIFECYCLE_APPLY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.organizationUnitDeletionBlockers))
	protected.PUT("/internal/v1/organizations/:organization_id/members/:user_id/lifecycle", approvalpolicy.Require("BILLING_ORGANIZATION_MEMBER_LIFECYCLE_APPLY", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.applyOrganizationMemberLifecycle))
	protected.GET("/internal/v1/organizations/:organization_id/quota-grants", httpserver.AdaptFunc(s.listMemberQuotaGrants))
	protected.POST("/internal/v1/organizations/:organization_id/quota-grants:batch", httpserver.AdaptFunc(s.allocateMemberQuotaBatch))
	protected.POST("/internal/v1/organizations/:organization_id/quota-grants/:grant_id/revoke", httpserver.AdaptFunc(s.revokeMemberQuotaGrant))
	protected.GET("/v1/recharge/products", httpserver.AdaptFunc(s.listProducts))
	if s.payments != nil {
		protected.GET("/v1/payment-capabilities", httpserver.AdaptFunc(s.paymentCapabilities))
	}
	protected.GET("/v1/wallets/:wallet_id/balance", httpserver.AdaptFunc(s.getBalance))
	if s.payments != nil {
		protected.POST("/internal/v1/recharge/orders", httpserver.AdaptFunc(s.createRechargeOrder))
		protected.GET("/internal/v1/recharge/orders", httpserver.AdaptFunc(s.listRechargeOrders))
		protected.GET("/internal/v1/recharge/orders/recent", httpserver.AdaptFunc(s.getRecentRechargeOrder))
		protected.GET("/internal/v1/recharge/orders/:order_id", httpserver.AdaptFunc(s.getRechargeOrder))
		protected.POST("/internal/v1/recharge/orders/:order_id/close", httpserver.AdaptFunc(s.closeRechargeOrder))
		protected.POST("/internal/v1/recharge/orders/:order_id/refunds", httpserver.AdaptFunc(s.createRefund))
		protected.POST("/internal/v1/recharge/orders/:order_id/payment-claims", httpserver.AdaptFunc(s.addLatePaymentClaim))
		protected.POST("/internal/v1/recharge/orders/:order_id/sync", httpserver.AdaptFunc(s.syncRechargeOrder))
		protected.POST("/internal/v1/reconciliation/runs", httpserver.AdaptFunc(s.runReconciliation))
		router.POST("/internal/payment/notifications", httpserver.AdaptFunc(s.paymentNotification))
	}
	if len(adminVerify) > 0 && adminVerify[0] != nil && s.authorizer != nil {
		admin := router.Group("/admin/v1")
		admin.Use(identity.GinMiddleware(adminVerify[0]))
		admin.GET("/overview", s.authorizer.Require("platform.system.status.read"), httpserver.AdaptFunc(s.adminOverview))
		admin.GET("/wallets", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminWallets))
		admin.GET("/wallets/:wallet_id/ledger", s.authorizer.Require("platform.billing.ledger.read"), httpserver.AdaptFunc(s.adminWalletLedger))
		admin.GET("/receivables", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminReceivables))
		admin.GET("/receivables/:receivable_id/allocations", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminReceivableAllocations))
		admin.GET("/pricing-versions", s.authorizer.Require("platform.pricing.read"), httpserver.AdaptFunc(s.adminPriceVersions))
		admin.POST("/pricing-versions", s.authorizer.Require("platform.pricing.manage"), approvalpolicy.Require("BILLING_PRICE_VERSION_CREATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminCreatePriceVersion))
		admin.POST("/pricing/drafts", s.authorizer.Require("platform.pricing.manage"), approvalpolicy.Require("BILLING_PRICE_DRAFT_CREATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminCreatePriceDraftV2))
		admin.GET("/pricing", s.authorizer.Require("platform.pricing.read"), httpserver.AdaptFunc(s.adminManagedPrices))
		admin.PUT("/pricing/:price_version_id/draft", s.authorizer.Require("platform.pricing.manage"), approvalpolicy.Require("BILLING_PRICE_DRAFT_UPDATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminUpdatePriceDraft))
		admin.GET("/pricing/:price_version_id/preview", s.authorizer.Require("platform.pricing.read"), httpserver.AdaptFunc(s.adminPreviewPrice))
		admin.POST("/pricing/:price_version_id/publish", s.authorizer.Require("platform.pricing.manage"), approvalpolicy.Require("BILLING_PRICE_PUBLISH", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminPublishPrice))
		admin.POST("/pricing/:price_version_id/transition", s.authorizer.Require("platform.pricing.manage"), approvalpolicy.Require("BILLING_PRICE_TRANSITION", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminTransitionPrice))
		admin.GET("/metering-units", s.authorizer.Require("platform.pricing.read"), httpserver.AdaptFunc(s.adminMeteringUnits))
		admin.POST("/metering-units", s.authorizer.Require("platform.pricing.manage"), approvalpolicy.Require("BILLING_METERING_UNIT_CREATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminCreateMeteringUnit))
		admin.POST("/metering-units/:unit_definition_id/deactivate", s.authorizer.Require("platform.pricing.manage"), approvalpolicy.Require("BILLING_METERING_UNIT_DEACTIVATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminDeactivateMeteringUnit))
		admin.GET("/payment-orders", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminPaymentOrders))
		admin.GET("/refunds", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminRefunds))
		admin.GET("/payment-orders/:order_id", s.authorizer.Require("platform.billing.wallets.read"), httpserver.AdaptFunc(s.adminPaymentOrderDetail))
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
		admin.GET("/usage-holds", s.authorizer.Require("platform.usage.read"), httpserver.AdaptFunc(s.adminUsageHolds))
		admin.GET("/usage-holds/:hold_id", s.authorizer.Require("platform.usage.read"), httpserver.AdaptFunc(s.adminUsageHold))
		admin.POST("/usage-holds/:hold_id/release", s.authorizer.Require("platform.billing.holds.release"), approvalpolicy.Require("BILLING_USAGE_HOLD_SAFE_RELEASE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminReleaseUsageHold))
		admin.GET("/reconciliation-runs", s.authorizer.Require("platform.usage.read"), httpserver.AdaptFunc(s.adminReconciliationRuns))
		admin.POST("/reconciliation-runs", s.authorizer.Require("platform.usage.reconcile.retry"), approvalpolicy.Require("BILLING_RECONCILIATION_RUN", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminRunReconciliation))
		admin.POST("/reconciliation-imports", s.authorizer.Require("platform.usage.reconcile.retry"), approvalpolicy.Require("BILLING_PROVIDER_BILL_IMPORT", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminImportReconciliation))
		admin.GET("/reconciliation-runs/:run_id/cases", s.authorizer.Require("platform.usage.read"), httpserver.AdaptFunc(s.adminReconciliationCases))
		admin.POST("/reconciliation-cases/:case_id/commands", s.authorizer.Require("platform.usage.reconcile.retry"), approvalpolicy.Require("BILLING_RECONCILIATION_CASE_UPDATE", approvalpolicy.AuditOnly), httpserver.AdaptFunc(s.adminUpdateReconciliationCase))
	}
	return router
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
	api.WriteJSON(w, http.StatusOK, s.payments.Capabilities())
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
		RequesterID               uuid.UUID  `json:"requester_id"`
		WalletID                  uuid.UUID  `json:"wallet_id"`
		ProductID                 string     `json:"product_id"`
		Channel                   string     `json:"channel"`
		Mobile                    string     `json:"mobile"`
		CommerceCustomerStatus    string     `json:"commerce_customer_status"`
		BeneficiaryOrganizationID *uuid.UUID `json:"beneficiary_organization_id"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.RequesterID == uuid.Nil || body.WalletID == uuid.Nil || body.ProductID == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid recharge order", httpserver.RequestID(r.Context()))
		return
	}
	if s.payments.Capabilities().CommerceCustomerRequired && body.CommerceCustomerStatus != "READY" {
		s.writeError(w, r, ErrCommerceCustomerNotReady)
		return
	}
	var order PaymentOrder
	var created bool
	var err error
	if body.BeneficiaryOrganizationID != nil {
		order, created, err = s.payments.CreateOrganizationMobileOrder(r.Context(), body.RequesterID, body.WalletID, *body.BeneficiaryOrganizationID, body.ProductID, body.Channel, body.Mobile, r.Header.Get("Idempotency-Key"))
	} else {
		order, created, err = s.payments.CreateMobileOrder(r.Context(), body.RequesterID, body.WalletID, body.ProductID, body.Channel, body.Mobile, r.Header.Get("Idempotency-Key"))
	}
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
		order, err = s.payments.SyncOrder(r.Context(), order.ID)
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
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

func (s *Service) paymentNotification(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid notification", httpserver.RequestID(r.Context()))
		return
	}
	err = s.payments.ProcessNotification(r.Context(), r.Header.Get("X-Pinvou-Payment-Channel"), r.Header.Get("X-Pinvou-Payment-Signature"), body)
	if errors.Is(err, ErrForbiddenPayment) {
		api.WriteError(w, http.StatusUnauthorized, "PAYMENT_NOTIFICATION_REJECTED", "payment notification rejected", httpserver.RequestID(r.Context()))
		return
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]string{"status": "accepted"})
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
		products = s.payments.FilterProducts(products)
	}
	var version int64
	for _, product := range products {
		if product.CatalogVersion > version {
			version = product.CatalogVersion
		}
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"catalog_version": version, "items": products})
}

func (s *Service) currentPrice(w http.ResponseWriter, r *http.Request) {
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	if model == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "model is required", httpserver.RequestID(r.Context()))
		return
	}
	price, err := s.repository.CurrentPrice(r.Context(), model)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, price)
}

func (s *Service) priceByID(w http.ResponseWriter, r *http.Request) {
	priceID, err := uuid.Parse(r.PathValue("price_version_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid price version identifier", httpserver.RequestID(r.Context()))
		return
	}
	price, err := s.repository.PriceByID(r.Context(), priceID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, price)
}

func (s *Service) reserve(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UsageRequestID    uuid.UUID       `json:"usage_request_id"`
		WalletID          uuid.UUID       `json:"wallet_id"`
		Maximum           int64           `json:"maximum_microcredits"`
		PriceVersionID    *uuid.UUID      `json:"price_version_id"`
		LeaseSeconds      int             `json:"lease_seconds"`
		OrganizationID    *uuid.UUID      `json:"organization_id"`
		MemberUserID      *uuid.UUID      `json:"member_user_id"`
		MembershipVersion int64           `json:"membership_version"`
		ModelID           string          `json:"model_id"`
		ProtocolCode      string          `json:"protocol_code"`
		FormulaVersionID  uuid.UUID       `json:"formula_version_id"`
		PricingSnapshot   json.RawMessage `json:"pricing_snapshot"`
		RequestSnapshot   json.RawMessage `json:"request_snapshot"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.UsageRequestID == uuid.Nil || body.WalletID == uuid.Nil || body.Maximum < 0 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid credit reservation", httpserver.RequestID(r.Context()))
		return
	}
	if body.LeaseSeconds == 0 {
		body.LeaseSeconds = 300
	}
	if body.LeaseSeconds < 30 || body.LeaseSeconds > 1800 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "hold lease must be between 30 and 1800 seconds", httpserver.RequestID(r.Context()))
		return
	}
	var hold Hold
	var err error
	var pricing *HoldPricingContext
	if strings.TrimSpace(body.ModelID) != "" || strings.TrimSpace(body.ProtocolCode) != "" || body.FormulaVersionID != uuid.Nil || len(body.PricingSnapshot) > 0 || len(body.RequestSnapshot) > 0 {
		pricing = &HoldPricingContext{ModelID: strings.TrimSpace(body.ModelID), ProtocolCode: strings.TrimSpace(body.ProtocolCode), FormulaVersionID: body.FormulaVersionID, PricingSnapshot: body.PricingSnapshot, RequestSnapshot: body.RequestSnapshot}
		if pricing.ModelID == "" || pricing.ProtocolCode == "" || pricing.FormulaVersionID == uuid.Nil || !json.Valid(pricing.PricingSnapshot) || !json.Valid(pricing.RequestSnapshot) {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "incomplete hold pricing context", httpserver.RequestID(r.Context()))
			return
		}
	}
	if body.OrganizationID != nil || body.MemberUserID != nil {
		repository, ok := s.repository.(interface {
			ReserveMemberQuota(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, int64, int64, *uuid.UUID, time.Time) (Hold, error)
		})
		if !ok || body.OrganizationID == nil || body.MemberUserID == nil || body.MembershipVersion < 1 {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid member quota reservation", httpserver.RequestID(r.Context()))
			return
		}
		if priced, supported := s.repository.(interface {
			ReserveMemberQuotaPriced(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, int64, int64, *uuid.UUID, time.Time, *HoldPricingContext) (Hold, error)
		}); supported && pricing != nil {
			hold, err = priced.ReserveMemberQuotaPriced(r.Context(), body.UsageRequestID, body.WalletID, *body.OrganizationID, *body.MemberUserID, body.MembershipVersion, body.Maximum, body.PriceVersionID, s.now().Add(time.Duration(body.LeaseSeconds)*time.Second), pricing)
		} else {
			hold, err = repository.ReserveMemberQuota(r.Context(), body.UsageRequestID, body.WalletID, *body.OrganizationID, *body.MemberUserID, body.MembershipVersion, body.Maximum, body.PriceVersionID, s.now().Add(time.Duration(body.LeaseSeconds)*time.Second))
		}
	} else {
		if priced, supported := s.repository.(interface {
			ReservePriced(context.Context, uuid.UUID, uuid.UUID, int64, *uuid.UUID, time.Time, *HoldPricingContext) (Hold, error)
		}); supported && pricing != nil {
			hold, err = priced.ReservePriced(r.Context(), body.UsageRequestID, body.WalletID, body.Maximum, body.PriceVersionID, s.now().Add(time.Duration(body.LeaseSeconds)*time.Second), pricing)
		} else {
			hold, err = s.repository.Reserve(r.Context(), body.UsageRequestID, body.WalletID, body.Maximum, body.PriceVersionID, s.now().Add(time.Duration(body.LeaseSeconds)*time.Second))
		}
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, hold)
}

func (s *Service) extendHold(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UsageRequestID         uuid.UUID `json:"usage_request_id"`
		HoldID                 uuid.UUID `json:"hold_id"`
		LeaseSeconds           int       `json:"lease_seconds"`
		AdditionalMicrocredits int64     `json:"additional_microcredits"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.UsageRequestID == uuid.Nil || body.HoldID == uuid.Nil || body.AdditionalMicrocredits < 0 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid hold extension", httpserver.RequestID(r.Context()))
		return
	}
	if body.LeaseSeconds == 0 {
		body.LeaseSeconds = 900
	}
	if body.LeaseSeconds < 30 || body.LeaseSeconds > 1800 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "hold lease must be between 30 and 1800 seconds", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(interface {
		ExtendHold(context.Context, uuid.UUID, uuid.UUID, time.Time, int64) (Hold, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "HOLD_EXTENSION_UNAVAILABLE", "hold extension is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	hold, err := repository.ExtendHold(r.Context(), body.UsageRequestID, body.HoldID, s.now().Add(time.Duration(body.LeaseSeconds)*time.Second), body.AdditionalMicrocredits)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, hold)
}

func (s *Service) commit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UsageRequestID uuid.UUID      `json:"usage_request_id"`
		HoldID         uuid.UUID      `json:"hold_id"`
		Actual         int64          `json:"actual_microcredits"`
		UsageSnapshot  map[string]any `json:"usage_snapshot"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.UsageRequestID == uuid.Nil || body.HoldID == uuid.Nil || body.Actual < 0 || body.UsageSnapshot == nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid usage settlement", httpserver.RequestID(r.Context()))
		return
	}
	hold, err := s.repository.Commit(r.Context(), body.UsageRequestID, body.HoldID, body.Actual, body.UsageSnapshot)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, hold)
}

func (s *Service) release(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UsageRequestID uuid.UUID `json:"usage_request_id"`
		HoldID         uuid.UUID `json:"hold_id"`
		Reason         string    `json:"reason"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.UsageRequestID == uuid.Nil || body.HoldID == uuid.Nil || strings.TrimSpace(body.Reason) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid hold release", httpserver.RequestID(r.Context()))
		return
	}
	hold, err := s.repository.Release(r.Context(), body.UsageRequestID, body.HoldID, body.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, hold)
}

func (s *Service) writeError(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := http.StatusInternalServerError, "INTERNAL_ERROR", "the billing request could not be completed"
	switch {
	case errors.Is(err, ErrNotFound):
		status, code, message = http.StatusNotFound, "NOT_FOUND", "the billing resource was not found"
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
