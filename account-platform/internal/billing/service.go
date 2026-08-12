package billing

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
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

func (s *Service) Handler(serviceToken string) http.Handler {
	root := http.NewServeMux()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/v1/wallets/ensure", s.ensureWallet)
	mux.HandleFunc("GET /internal/v1/wallets/{wallet_id}/balance", s.getBalance)
	mux.HandleFunc("POST /internal/v1/holds/reserve", s.reserve)
	mux.HandleFunc("POST /internal/v1/holds/commit", s.commit)
	mux.HandleFunc("POST /internal/v1/holds/release", s.release)
	mux.HandleFunc("GET /v1/recharge/products", s.listProducts)
	mux.HandleFunc("GET /v1/wallets/{wallet_id}/balance", s.getBalance)
	if s.payments != nil {
		mux.HandleFunc("POST /internal/v1/recharge/orders", s.createRechargeOrder)
		root.HandleFunc("POST /internal/payment/notifications", s.paymentNotification)
	}
	root.Handle("/", serviceauth.Middleware(serviceToken, mux))
	return root
}

func (s *Service) createRechargeOrder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RequesterID uuid.UUID `json:"requester_id"`
		WalletID    uuid.UUID `json:"wallet_id"`
		ProductID   string    `json:"product_id"`
		Channel     string    `json:"channel"`
		CallbackURL string    `json:"callback_url"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.RequesterID == uuid.Nil || body.WalletID == uuid.Nil || body.ProductID == "" || body.CallbackURL == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid recharge order", httpserver.RequestID(r.Context()))
		return
	}
	order, created, err := s.payments.CreateOrder(r.Context(), body.RequesterID, body.WalletID, body.ProductID, body.Channel, r.Header.Get("Idempotency-Key"), body.CallbackURL)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	api.WriteJSON(w, status, order)
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
	var version int64
	for _, product := range products {
		if product.CatalogVersion > version {
			version = product.CatalogVersion
		}
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"catalog_version": version, "items": products})
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
	}
	api.WriteError(w, status, code, message, httpserver.RequestID(r.Context()))
}
