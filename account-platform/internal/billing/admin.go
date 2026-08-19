package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type AdminOverview struct {
	Wallets                  int64 `json:"wallets"`
	PostedMicrocredits       int64 `json:"posted_microcredits"`
	ReservedMicrocredits     int64 `json:"reserved_microcredits"`
	PendingPaymentOrders     int64 `json:"pending_payment_orders"`
	ExpiredPaymentOrders     int64 `json:"expired_payment_orders"`
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

type AdminPriceVersion struct {
	ID                    uuid.UUID  `json:"price_version_id"`
	Model                 string     `json:"model"`
	InputPerMillion       int64      `json:"input_per_million"`
	OutputPerMillion      int64      `json:"output_per_million"`
	CachedInputPerMillion int64      `json:"cached_input_per_million"`
	EffectiveFrom         time.Time  `json:"effective_from"`
	EffectiveTo           *time.Time `json:"effective_to,omitempty"`
	CreatedAt             time.Time  `json:"created_at"`
}

type AdminLedgerEntry struct {
	JournalEntryID   uuid.UUID `json:"journal_entry_id"`
	EntryType        string    `json:"entry_type"`
	BusinessRef      string    `json:"business_reference"`
	Description      string    `json:"description"`
	DeltaMicrocredit int64     `json:"delta_microcredits"`
	CreatedAt        time.Time `json:"created_at"`
}

type AdminReconciliationCase struct {
	ID               uuid.UUID       `json:"case_id"`
	RunID            uuid.UUID       `json:"reconciliation_run_id"`
	PaymentOrderID   *uuid.UUID      `json:"payment_order_id,omitempty"`
	MerchantOrderNo  string          `json:"merchant_order_no"`
	CaseType         string          `json:"case_type"`
	Status           string          `json:"status"`
	ProviderSnapshot json.RawMessage `json:"provider_snapshot,omitempty"`
	InternalSnapshot json.RawMessage `json:"internal_snapshot,omitempty"`
	Resolution       *string         `json:"resolution,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	ResolvedAt       *time.Time      `json:"resolved_at,omitempty"`
}

type adminRepository interface {
	AdminOverview(context.Context) (AdminOverview, error)
	AdminWallets(context.Context, int) ([]Wallet, error)
	AdminPaymentOrders(context.Context, int) ([]AdminPaymentOrder, error)
	AdminAdjustments(context.Context, int) ([]ManualAdjustment, error)
	AdminUsageHolds(context.Context, int) ([]AdminHold, error)
	AdminPriceVersions(context.Context, int) ([]AdminPriceVersion, error)
	AdminCreatePriceVersion(context.Context, string, int64, int64, int64) (AdminPriceVersion, error)
	AdminWalletLedger(context.Context, uuid.UUID, int) ([]AdminLedgerEntry, error)
	AdminPaymentOrder(context.Context, uuid.UUID) (AdminPaymentOrder, error)
	AdminReleaseUsageHold(context.Context, uuid.UUID, string, string) (AdminHold, error)
	AdminReconciliationRuns(context.Context, int) ([]ReconciliationRun, error)
	AdminReconciliationCases(context.Context, uuid.UUID, int) ([]AdminReconciliationCase, error)
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

func adminActor(principal identity.Principal) uuid.UUID {
	if actor, err := uuid.Parse(principal.Subject); err == nil {
		return actor
	}
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(principal.Issuer+"|"+principal.Subject))
}

func adminPrincipal(r *http.Request) identity.Principal {
	principal, _ := identity.FromContext(r.Context())
	return principal
}

func (s *Service) adminPriceVersions(w http.ResponseWriter, r *http.Request) {
	s.writeAdminCollection(w, r, func(repository adminRepository, limit int) (any, error) {
		return repository.AdminPriceVersions(r.Context(), limit)
	})
}

func (s *Service) adminCreatePriceVersion(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	var body struct {
		Model                 string `json:"model"`
		InputPerMillion       int64  `json:"input_per_million"`
		OutputPerMillion      int64  `json:"output_per_million"`
		CachedInputPerMillion int64  `json:"cached_input_per_million"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	price, err := repository.AdminCreatePriceVersion(r.Context(), body.Model, body.InputPerMillion, body.OutputPerMillion, body.CachedInputPerMillion)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, price)
}

func (s *Service) adminWalletLedger(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	walletID, err := uuid.Parse(r.PathValue("wallet_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid wallet", httpserver.RequestID(r.Context()))
		return
	}
	items, err := repository.AdminWalletLedger(r.Context(), walletID, billingAdminLimit(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminDecideAdjustment(w http.ResponseWriter, r *http.Request, approve bool) {
	adjustmentID, err := uuid.Parse(r.PathValue("adjustment_id"))
	var body struct {
		Reason string `json:"reason"`
	}
	if decodeErr := api.DecodeJSON(w, r, &body); err != nil || decodeErr != nil || strings.TrimSpace(body.Reason) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "decision reason is required", httpserver.RequestID(r.Context()))
		return
	}
	actor := adminActor(adminPrincipal(r))
	var adjustment ManualAdjustment
	if approve {
		adjustment, err = s.repository.ApproveAdjustment(r.Context(), adjustmentID, actor, body.Reason)
	} else {
		adjustment, err = s.repository.RejectAdjustment(r.Context(), adjustmentID, actor, body.Reason)
	}
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, adjustment)
}

func (s *Service) adminApproveAdjustment(w http.ResponseWriter, r *http.Request) {
	s.adminDecideAdjustment(w, r, true)
}
func (s *Service) adminRejectAdjustment(w http.ResponseWriter, r *http.Request) {
	s.adminDecideAdjustment(w, r, false)
}

func (s *Service) adminSyncPaymentOrder(w http.ResponseWriter, r *http.Request) {
	orderID, err := uuid.Parse(r.PathValue("order_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid payment order", httpserver.RequestID(r.Context()))
		return
	}
	if s.payments == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "payment operations are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	order, err := s.payments.SyncOrder(r.Context(), orderID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, order)
}

func (s *Service) adminRefundPaymentOrder(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	orderID, err := uuid.Parse(r.PathValue("order_id"))
	var body struct {
		AmountFen int64  `json:"amount_fen"`
		Reason    string `json:"reason"`
	}
	if decodeErr := api.DecodeJSON(w, r, &body); err != nil || decodeErr != nil || body.AmountFen <= 0 || strings.TrimSpace(body.Reason) == "" || s.payments == nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "refund amount and reason are required", httpserver.RequestID(r.Context()))
		return
	}
	order, err := repository.AdminPaymentOrder(r.Context(), orderID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	idempotencyKey := "admin-refund-" + uuid.Must(uuid.NewV7()).String()
	refund, _, err := s.payments.CreateRefund(r.Context(), orderID, order.RequesterID, body.AmountFen, idempotencyKey, body.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, refund)
}

func (s *Service) adminReleaseUsageHold(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	holdID, err := uuid.Parse(r.PathValue("hold_id"))
	var body struct {
		Reason string `json:"reason"`
	}
	if decodeErr := api.DecodeJSON(w, r, &body); err != nil || decodeErr != nil || strings.TrimSpace(body.Reason) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "release reason is required", httpserver.RequestID(r.Context()))
		return
	}
	principal := adminPrincipal(r)
	hold, err := repository.AdminReleaseUsageHold(r.Context(), holdID, body.Reason, principal.Issuer+"|"+principal.Subject)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, hold)
}

func (s *Service) adminReconciliationRuns(w http.ResponseWriter, r *http.Request) {
	s.writeAdminCollection(w, r, func(repository adminRepository, limit int) (any, error) {
		return repository.AdminReconciliationRuns(r.Context(), limit)
	})
}

func (s *Service) adminRunReconciliation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Channel      string `json:"channel"`
		BusinessDate string `json:"business_date"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	date, err := time.Parse("2006-01-02", body.BusinessDate)
	if err != nil || s.payments == nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "valid channel and business date are required", httpserver.RequestID(r.Context()))
		return
	}
	run, err := s.payments.RunReconciliation(r.Context(), body.Channel, date)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, run)
}

func (s *Service) adminReconciliationCases(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid reconciliation run", httpserver.RequestID(r.Context()))
		return
	}
	items, err := repository.AdminReconciliationCases(r.Context(), runID, billingAdminLimit(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (r *PostgresRepository) AdminOverview(ctx context.Context) (AdminOverview, error) {
	var result AdminOverview
	err := r.pool.QueryRow(ctx, `
SELECT
  (SELECT count(*) FROM wallets),
  (SELECT COALESCE(sum(posted_microcredits),0) FROM wallets),
  (SELECT COALESCE(sum(reserved_microcredits),0) FROM wallets),
  (SELECT count(*) FROM payment_orders WHERE status IN ('CREATED','PENDING') AND expires_at > now()),
  (SELECT count(*) FROM payment_orders WHERE status IN ('CREATED','PENDING') AND expires_at <= now()),
  (SELECT count(*) FROM payment_orders WHERE status='MANUAL_REVIEW' OR fulfillment_status='MANUAL_REVIEW'),
  (SELECT count(*) FROM manual_adjustments WHERE status='PENDING_APPROVAL'),
  (SELECT count(*) FROM usage_holds WHERE status IN ('ACTIVE','REVIEW'))`).Scan(
		&result.Wallets, &result.PostedMicrocredits, &result.ReservedMicrocredits,
		&result.PendingPaymentOrders, &result.ExpiredPaymentOrders, &result.ReviewPaymentOrders, &result.PendingAdjustments,
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

func (r *PostgresRepository) AdminPriceVersions(ctx context.Context, limit int) ([]AdminPriceVersion, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,effective_to,created_at FROM pricing_versions ORDER BY effective_from DESC,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminPriceVersion, 0)
	for rows.Next() {
		var item AdminPriceVersion
		if err := rows.Scan(&item.ID, &item.Model, &item.InputPerMillion, &item.OutputPerMillion, &item.CachedInputPerMillion, &item.EffectiveFrom, &item.EffectiveTo, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminCreatePriceVersion(ctx context.Context, model string, input, output, cached int64) (AdminPriceVersion, error) {
	model = strings.TrimSpace(model)
	if model == "" || len(model) > 160 || input < 0 || output < 0 || cached < 0 {
		return AdminPriceVersion{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return AdminPriceVersion{}, err
	}
	defer tx.Rollback(ctx)
	now := r.now()
	var currentFrom time.Time
	err = tx.QueryRow(ctx, `SELECT effective_from FROM pricing_versions WHERE model=$1 AND effective_to IS NULL FOR UPDATE`, model).Scan(&currentFrom)
	if err == nil {
		if !now.After(currentFrom) {
			now = currentFrom.Add(time.Microsecond)
		}
		if _, err := tx.Exec(ctx, `UPDATE pricing_versions SET effective_to=$2 WHERE model=$1 AND effective_to IS NULL`, model, now); err != nil {
			return AdminPriceVersion{}, err
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return AdminPriceVersion{}, err
	}
	item := AdminPriceVersion{ID: uuid.Must(uuid.NewV7()), Model: model, InputPerMillion: input, OutputPerMillion: output, CachedInputPerMillion: cached, EffectiveFrom: now, CreatedAt: now}
	if _, err := tx.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at) VALUES($1,$2,$3,$4,$5,$6,$6)`, item.ID, model, input, output, cached, now); err != nil {
		return AdminPriceVersion{}, err
	}
	return item, tx.Commit(ctx)
}

func (r *PostgresRepository) AdminWalletLedger(ctx context.Context, walletID uuid.UUID, limit int) ([]AdminLedgerEntry, error) {
	var exists bool
	if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wallets WHERE id=$1)`, walletID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := r.pool.Query(ctx, `
SELECT je.id,je.entry_type,je.business_reference,je.description,lp.delta_microcredits,lp.created_at
FROM ledger_postings lp JOIN journal_entries je ON je.id=lp.journal_entry_id JOIN ledger_accounts la ON la.id=lp.ledger_account_id
WHERE la.wallet_id=$1 ORDER BY lp.created_at DESC,lp.id LIMIT $2`, walletID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminLedgerEntry, 0)
	for rows.Next() {
		var item AdminLedgerEntry
		if err := rows.Scan(&item.JournalEntryID, &item.EntryType, &item.BusinessRef, &item.Description, &item.DeltaMicrocredit, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminPaymentOrder(ctx context.Context, orderID uuid.UUID) (AdminPaymentOrder, error) {
	var item AdminPaymentOrder
	err := r.pool.QueryRow(ctx, `SELECT id,merchant_order_no,requester_id,wallet_id,channel,amount_fen,credit_microcredits,status,fulfillment_status,paid_at,created_at,updated_at FROM payment_orders WHERE id=$1`, orderID).Scan(&item.ID, &item.MerchantOrderNo, &item.RequesterID, &item.WalletID, &item.Channel, &item.AmountFen, &item.CreditMicrocredits, &item.Status, &item.FulfillmentStatus, &item.PaidAt, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminPaymentOrder{}, ErrNotFound
	}
	return item, err
}

func (r *PostgresRepository) AdminReleaseUsageHold(ctx context.Context, holdID uuid.UUID, reason, actor string) (AdminHold, error) {
	reason, actor = strings.TrimSpace(reason), strings.TrimSpace(actor)
	if reason == "" || actor == "" {
		return AdminHold{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return AdminHold{}, err
	}
	defer tx.Rollback(ctx)
	var hold AdminHold
	err = tx.QueryRow(ctx, `SELECT id,usage_request_id,wallet_id,status,max_microcredits,actual_microcredits,expires_at,updated_at FROM usage_holds WHERE id=$1 FOR UPDATE`, holdID).Scan(&hold.ID, &hold.UsageRequestID, &hold.WalletID, &hold.Status, &hold.MaxMicrocredits, &hold.ActualMicrocredits, &hold.ExpiresAt, &hold.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminHold{}, ErrNotFound
	}
	if err != nil {
		return AdminHold{}, err
	}
	if hold.Status != "ACTIVE" && hold.Status != "REVIEW" {
		return AdminHold{}, ErrUsageAlreadyTerminal
	}
	now := r.now()
	command, err := tx.Exec(ctx, `UPDATE wallets SET reserved_microcredits=reserved_microcredits-$2,version=version+1,updated_at=$3 WHERE id=$1 AND reserved_microcredits >= $2`, hold.WalletID, hold.MaxMicrocredits, now)
	if err != nil || command.RowsAffected() != 1 {
		if err != nil {
			return AdminHold{}, err
		}
		return AdminHold{}, ErrConflict
	}
	snapshot, _ := json.Marshal(map[string]string{"admin_release_reason": reason, "admin_actor": actor})
	if _, err := tx.Exec(ctx, `UPDATE usage_holds SET status='RELEASED',usage_snapshot=COALESCE(usage_snapshot,'{}'::jsonb) || $2::jsonb,terminal_at=$3,updated_at=$3 WHERE id=$1`, holdID, snapshot, now); err != nil {
		return AdminHold{}, err
	}
	hold.Status, hold.UpdatedAt = "RELEASED", now
	return hold, tx.Commit(ctx)
}

func (r *PostgresRepository) AdminReconciliationRuns(ctx context.Context, limit int) ([]ReconciliationRun, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,channel,business_date,status,provider_row_count,case_count,started_at,COALESCE(completed_at,started_at) FROM reconciliation_runs ORDER BY started_at DESC,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ReconciliationRun, 0)
	for rows.Next() {
		var item ReconciliationRun
		var businessDate time.Time
		if err := rows.Scan(&item.ID, &item.Channel, &businessDate, &item.Status, &item.ProviderRowCount, &item.CaseCount, &item.StartedAt, &item.CompletedAt); err != nil {
			return nil, err
		}
		item.BusinessDate = businessDate.Format("2006-01-02")
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminReconciliationCases(ctx context.Context, runID uuid.UUID, limit int) ([]AdminReconciliationCase, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,reconciliation_run_id,payment_order_id,merchant_order_no,case_type,status,provider_snapshot,internal_snapshot,resolution,created_at,resolved_at FROM reconciliation_cases WHERE reconciliation_run_id=$1 ORDER BY created_at,id LIMIT $2`, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminReconciliationCase, 0)
	for rows.Next() {
		var item AdminReconciliationCase
		if err := rows.Scan(&item.ID, &item.RunID, &item.PaymentOrderID, &item.MerchantOrderNo, &item.CaseType, &item.Status, &item.ProviderSnapshot, &item.InternalSnapshot, &item.Resolution, &item.CreatedAt, &item.ResolvedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
