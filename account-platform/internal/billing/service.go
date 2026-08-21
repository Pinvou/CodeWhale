package billing

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type Service struct {
	repository Repository
	payments   *PaymentCoordinator
	now        func() time.Time
}

func NewService(repository Repository) *Service {
	return &Service{repository: repository, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) WithPayments(payments *PaymentCoordinator) *Service {
	s.payments = payments
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
	protected.POST("/internal/v1/holds/commit", httpserver.AdaptFunc(s.commit))
	protected.POST("/internal/v1/holds/release", httpserver.AdaptFunc(s.release))
	protected.GET("/internal/v1/pricing/current", httpserver.AdaptFunc(s.currentPrice))
	protected.GET("/internal/v1/pricing/:price_version_id", httpserver.AdaptFunc(s.priceByID))
	protected.POST("/internal/v1/adjustments", httpserver.AdaptFunc(s.requestAdjustment))
	protected.POST("/internal/v1/adjustments/:adjustment_id/approve", httpserver.AdaptFunc(s.approveAdjustment))
	protected.POST("/internal/v1/adjustments/:adjustment_id/reject", httpserver.AdaptFunc(s.rejectAdjustment))
	protected.GET("/v1/recharge/products", httpserver.AdaptFunc(s.listProducts))
	protected.GET("/v1/wallets/:wallet_id/balance", httpserver.AdaptFunc(s.getBalance))
	if s.payments != nil {
		protected.POST("/internal/v1/recharge/orders", httpserver.AdaptFunc(s.createRechargeOrder))
		protected.GET("/internal/v1/recharge/orders", httpserver.AdaptFunc(s.listRechargeOrders))
		protected.GET("/internal/v1/recharge/orders/recent", httpserver.AdaptFunc(s.getRecentRechargeOrder))
		protected.GET("/internal/v1/recharge/orders/:order_id", httpserver.AdaptFunc(s.getRechargeOrder))
		protected.POST("/internal/v1/recharge/orders/:order_id/close", httpserver.AdaptFunc(s.closeRechargeOrder))
		protected.POST("/internal/v1/recharge/orders/:order_id/refunds", httpserver.AdaptFunc(s.createRefund))
		protected.POST("/internal/v1/recharge/orders/:order_id/sync", httpserver.AdaptFunc(s.syncRechargeOrder))
		protected.POST("/internal/v1/reconciliation/runs", httpserver.AdaptFunc(s.runReconciliation))
		router.POST("/internal/payment/notifications", httpserver.AdaptFunc(s.paymentNotification))
	}
	if len(adminVerify) > 0 && adminVerify[0] != nil {
		admin := router.Group("/admin/v1")
		admin.Use(identity.GinMiddleware(adminVerify[0]), identity.RequireRealmRoleGin("platform-admin"))
		admin.GET("/overview", httpserver.AdaptFunc(s.adminOverview))
		admin.GET("/wallets", httpserver.AdaptFunc(s.adminWallets))
		admin.GET("/wallets/:wallet_id/ledger", httpserver.AdaptFunc(s.adminWalletLedger))
		admin.GET("/pricing-versions", httpserver.AdaptFunc(s.adminPriceVersions))
		admin.POST("/pricing-versions", httpserver.AdaptFunc(s.adminCreatePriceVersion))
		admin.GET("/payment-orders", httpserver.AdaptFunc(s.adminPaymentOrders))
		admin.POST("/payment-orders/:order_id/sync", httpserver.AdaptFunc(s.adminSyncPaymentOrder))
		admin.POST("/payment-orders/:order_id/repair", httpserver.AdaptFunc(s.adminRepairPaymentOrder))
		admin.POST("/payment-orders/:order_id/refunds", httpserver.AdaptFunc(s.adminRefundPaymentOrder))
		admin.GET("/adjustments", httpserver.AdaptFunc(s.adminAdjustments))
		admin.POST("/adjustments/:adjustment_id/approve", httpserver.AdaptFunc(s.adminApproveAdjustment))
		admin.POST("/adjustments/:adjustment_id/reject", httpserver.AdaptFunc(s.adminRejectAdjustment))
		admin.GET("/usage-holds", httpserver.AdaptFunc(s.adminUsageHolds))
		admin.POST("/usage-holds/:hold_id/release", httpserver.AdaptFunc(s.adminReleaseUsageHold))
		admin.GET("/reconciliation-runs", httpserver.AdaptFunc(s.adminReconciliationRuns))
		admin.POST("/reconciliation-runs", httpserver.AdaptFunc(s.adminRunReconciliation))
		admin.GET("/reconciliation-runs/:run_id/cases", httpserver.AdaptFunc(s.adminReconciliationCases))
	}
	return router
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

func (s *Service) approveAdjustment(w http.ResponseWriter, r *http.Request) {
	adjustmentID, err := uuid.Parse(r.PathValue("adjustment_id"))
	var body struct {
		ApprovedBy     uuid.UUID `json:"approved_by"`
		DecisionReason string    `json:"decision_reason"`
	}
	if decodeErr := api.DecodeJSON(w, r, &body); err != nil || decodeErr != nil || body.ApprovedBy == uuid.Nil || strings.TrimSpace(body.DecisionReason) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid adjustment approval", httpserver.RequestID(r.Context()))
		return
	}
	adjustment, err := s.repository.ApproveAdjustment(r.Context(), adjustmentID, body.ApprovedBy, body.DecisionReason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, adjustment)
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
		RequesterID uuid.UUID `json:"requester_id"`
		WalletID    uuid.UUID `json:"wallet_id"`
		ProductID   string    `json:"product_id"`
		Channel     string    `json:"channel"`
		Mobile      string    `json:"mobile"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.RequesterID == uuid.Nil || body.WalletID == uuid.Nil || body.ProductID == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid recharge order", httpserver.RequestID(r.Context()))
		return
	}
	order, created, err := s.payments.CreateMobileOrder(r.Context(), body.RequesterID, body.WalletID, body.ProductID, body.Channel, body.Mobile, r.Header.Get("Idempotency-Key"))
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
		RequesterID uuid.UUID `json:"requester_id"`
		AmountFen   int64     `json:"amount_fen"`
		Reason      string    `json:"reason"`
	}
	if decodeErr := api.DecodeJSON(w, r, &body); err != nil || decodeErr != nil || body.RequesterID == uuid.Nil || body.AmountFen <= 0 || strings.TrimSpace(body.Reason) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid refund request", httpserver.RequestID(r.Context()))
		return
	}
	refund, created, err := s.payments.CreateRefund(r.Context(), orderID, body.RequesterID, body.AmountFen, r.Header.Get("Idempotency-Key"), body.Reason)
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
		UsageRequestID uuid.UUID  `json:"usage_request_id"`
		WalletID       uuid.UUID  `json:"wallet_id"`
		Maximum        int64      `json:"maximum_microcredits"`
		PriceVersionID *uuid.UUID `json:"price_version_id"`
		LeaseSeconds   int        `json:"lease_seconds"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.UsageRequestID == uuid.Nil || body.WalletID == uuid.Nil || body.Maximum <= 0 {
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
	hold, err := s.repository.Reserve(r.Context(), body.UsageRequestID, body.WalletID, body.Maximum, body.PriceVersionID, s.now().Add(time.Duration(body.LeaseSeconds)*time.Second))
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
	case errors.Is(err, ErrInsufficientBalance):
		status, code, message = http.StatusPaymentRequired, "WALLET_INSUFFICIENT", "the wallet has insufficient available credits"
	case errors.Is(err, ErrWalletUnavailable):
		status, code, message = http.StatusLocked, "WALLET_UNAVAILABLE", "the wallet is not active"
	case errors.Is(err, ErrUsageAlreadyTerminal):
		status, code, message = http.StatusConflict, "USAGE_ALREADY_SETTLED", "the usage hold is already terminal"
	case errors.Is(err, ErrPaymentProvider):
		status, code, message = http.StatusBadGateway, "PAYMENT_PROVIDER_UNAVAILABLE", "the payment provider request could not be completed"
	}
	api.WriteError(w, status, code, message, httpserver.RequestID(r.Context()))
}
