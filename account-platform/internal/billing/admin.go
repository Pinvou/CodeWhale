package billing

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

type AdminOverview struct {
	Wallets                  int64 `json:"wallets"`
	PostedMicrocredits       int64 `json:"posted_microcredits"`
	ReservedMicrocredits     int64 `json:"reserved_microcredits"`
	PendingPaymentOrders     int64 `json:"pending_payment_orders"`
	ReviewPaymentOrders      int64 `json:"review_payment_orders"`
	PendingAdjustments       int64 `json:"pending_adjustments"`
	ActiveOrReviewUsageHolds int64 `json:"active_or_review_usage_holds"`
}

type AdminPaymentOrder struct {
	ID                 uuid.UUID  `json:"order_id"`
	MerchantOrderNo    string     `json:"merchant_order_no"`
	RequesterID        uuid.UUID  `json:"requester_id"`
	WalletID           uuid.UUID  `json:"wallet_id"`
	Channel            string     `json:"channel"`
	AmountFen          int64      `json:"amount_fen"`
	CreditMicrocredits int64      `json:"credit_microcredits"`
	Status             string     `json:"status"`
	FulfillmentStatus  string     `json:"fulfillment_status"`
	PaidAt             *time.Time `json:"paid_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

type AdminHold struct {
	ID                 uuid.UUID `json:"hold_id"`
	UsageRequestID     uuid.UUID `json:"usage_request_id"`
	WalletID           uuid.UUID `json:"wallet_id"`
	Status             string    `json:"status"`
	MaxMicrocredits    int64     `json:"max_microcredits"`
	ActualMicrocredits *int64    `json:"actual_microcredits,omitempty"`
	ExpiresAt          time.Time `json:"expires_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type adminRepository interface {
	AdminOverview(context.Context) (AdminOverview, error)
	AdminWallets(context.Context, int) ([]Wallet, error)
	AdminPaymentOrders(context.Context, int) ([]AdminPaymentOrder, error)
	AdminAdjustments(context.Context, int) ([]ManualAdjustment, error)
	AdminUsageHolds(context.Context, int) ([]AdminHold, error)
}

func billingAdminLimit(r *http.Request) int {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		return 50
	}
	if limit > 200 {
		return 200
	}
	return limit
}

func (s *Service) requireAdminRepository(w http.ResponseWriter, r *http.Request) (adminRepository, bool) {
	repository, ok := s.repository.(adminRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "billing administration is unavailable", httpserver.RequestID(r.Context()))
	}
	return repository, ok
}

func (s *Service) writeAdminCollection(w http.ResponseWriter, r *http.Request, load func(adminRepository, int) (any, error)) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	items, err := load(repository, billingAdminLimit(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminOverview(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	result, err := repository.AdminOverview(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminWallets(w http.ResponseWriter, r *http.Request) {
	s.writeAdminCollection(w, r, func(repository adminRepository, limit int) (any, error) {
		return repository.AdminWallets(r.Context(), limit)
	})
}

func (s *Service) adminPaymentOrders(w http.ResponseWriter, r *http.Request) {
	s.writeAdminCollection(w, r, func(repository adminRepository, limit int) (any, error) {
		return repository.AdminPaymentOrders(r.Context(), limit)
	})
}

func (s *Service) adminAdjustments(w http.ResponseWriter, r *http.Request) {
	s.writeAdminCollection(w, r, func(repository adminRepository, limit int) (any, error) {
		return repository.AdminAdjustments(r.Context(), limit)
	})
}

func (s *Service) adminUsageHolds(w http.ResponseWriter, r *http.Request) {
	s.writeAdminCollection(w, r, func(repository adminRepository, limit int) (any, error) {
		return repository.AdminUsageHolds(r.Context(), limit)
	})
}

func (r *PostgresRepository) AdminOverview(ctx context.Context) (AdminOverview, error) {
	var result AdminOverview
	err := r.pool.QueryRow(ctx, `
SELECT
  (SELECT count(*) FROM wallets),
  (SELECT COALESCE(sum(posted_microcredits),0) FROM wallets),
  (SELECT COALESCE(sum(reserved_microcredits),0) FROM wallets),
  (SELECT count(*) FROM payment_orders WHERE status IN ('CREATED','PENDING')),
  (SELECT count(*) FROM payment_orders WHERE status='MANUAL_REVIEW' OR fulfillment_status='MANUAL_REVIEW'),
  (SELECT count(*) FROM manual_adjustments WHERE status='PENDING_APPROVAL'),
  (SELECT count(*) FROM usage_holds WHERE status IN ('ACTIVE','REVIEW'))`).Scan(
		&result.Wallets, &result.PostedMicrocredits, &result.ReservedMicrocredits,
		&result.PendingPaymentOrders, &result.ReviewPaymentOrders, &result.PendingAdjustments,
		&result.ActiveOrReviewUsageHolds,
	)
	return result, err
}

func (r *PostgresRepository) AdminWallets(ctx context.Context, limit int) ([]Wallet, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,owner_type,owner_id,status,posted_microcredits,reserved_microcredits,posted_microcredits-reserved_microcredits,version FROM wallets ORDER BY updated_at DESC,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Wallet, 0)
	for rows.Next() {
		var item Wallet
		if err := rows.Scan(&item.ID, &item.OwnerType, &item.OwnerID, &item.Status, &item.PostedMicrocredits, &item.ReservedMicrocredits, &item.AvailableMicrocredits, &item.Version); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminPaymentOrders(ctx context.Context, limit int) ([]AdminPaymentOrder, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,merchant_order_no,requester_id,wallet_id,channel,amount_fen,credit_microcredits,status,fulfillment_status,paid_at,created_at,updated_at FROM payment_orders ORDER BY updated_at DESC,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminPaymentOrder, 0)
	for rows.Next() {
		var item AdminPaymentOrder
		if err := rows.Scan(&item.ID, &item.MerchantOrderNo, &item.RequesterID, &item.WalletID, &item.Channel, &item.AmountFen, &item.CreditMicrocredits, &item.Status, &item.FulfillmentStatus, &item.PaidAt, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminAdjustments(ctx context.Context, limit int) ([]ManualAdjustment, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,wallet_id,amount_microcredits,reason,requested_by,approved_by,rejected_by,status,journal_entry_id,decision_reason,created_at,updated_at,decided_at FROM manual_adjustments ORDER BY updated_at DESC,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ManualAdjustment, 0)
	for rows.Next() {
		var item ManualAdjustment
		if err := rows.Scan(&item.ID, &item.WalletID, &item.AmountMicrocredits, &item.Reason, &item.RequestedBy, &item.ApprovedBy, &item.RejectedBy, &item.Status, &item.JournalEntryID, &item.DecisionReason, &item.CreatedAt, &item.UpdatedAt, &item.DecidedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminUsageHolds(ctx context.Context, limit int) ([]AdminHold, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,usage_request_id,wallet_id,status,max_microcredits,actual_microcredits,expires_at,updated_at FROM usage_holds ORDER BY updated_at DESC,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminHold, 0)
	for rows.Next() {
		var item AdminHold
		if err := rows.Scan(&item.ID, &item.UsageRequestID, &item.WalletID, &item.Status, &item.MaxMicrocredits, &item.ActualMicrocredits, &item.ExpiresAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
