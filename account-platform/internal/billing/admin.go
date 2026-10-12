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
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type AdminOverview struct {
	Wallets                  int64                      `json:"wallets"`
	PostedMicrocredits       int64                      `json:"posted_microcredits"`
	ReservedMicrocredits     int64                      `json:"reserved_microcredits"`
	ReceivableMicrocredits   int64                      `json:"receivable_microcredits"`
	PendingPaymentOrders     int64                      `json:"pending_payment_orders"`
	ExpiredPaymentOrders     int64                      `json:"expired_payment_orders"`
	ReviewPaymentOrders      int64                      `json:"review_payment_orders"`
	PendingAdjustments       int64                      `json:"pending_adjustments"`
	ActiveOrReviewUsageHolds int64                      `json:"active_or_review_usage_holds"`
	PaymentCapabilities      ProductPaymentCapabilities `json:"payment_capabilities"`
}

type AdminPaymentOrder struct {
	ID                         uuid.UUID            `json:"order_id"`
	MerchantOrderNo            string               `json:"merchant_order_no"`
	ProviderID                 string               `json:"provider_id"`
	ProviderVersion            string               `json:"provider_version"`
	ProductID                  string               `json:"product_id"`
	ProviderSKU                string               `json:"provider_sku"`
	PrimaryProviderTransaction string               `json:"primary_provider_transaction_id"`
	MobileMasked               string               `json:"mobile_masked"`
	RequesterID                uuid.UUID            `json:"requester_id"`
	PayerUserID                uuid.UUID            `json:"payer_user_id"`
	BeneficiaryOrganizationID  *uuid.UUID           `json:"beneficiary_organization_id,omitempty"`
	WalletID                   uuid.UUID            `json:"wallet_id"`
	Channel                    string               `json:"channel"`
	AmountFen                  int64                `json:"amount_fen"`
	CreditMicrocredits         int64                `json:"credit_microcredits"`
	Status                     string               `json:"status"`
	FulfillmentStatus          string               `json:"fulfillment_status"`
	CloseStatus                string               `json:"close_status"`
	PaidAt                     *time.Time           `json:"paid_at,omitempty"`
	PaymentSummary             json.RawMessage      `json:"payment_summary"`
	RefundSummary              json.RawMessage      `json:"refund_summary"`
	ChargebackSummary          json.RawMessage      `json:"chargeback_summary"`
	AttentionSummary           string               `json:"attention_summary"`
	SummaryVersion             int64                `json:"summary_version"`
	Transactions               []PaymentTransaction `json:"transactions,omitempty"`
	Evidence                   []PaymentEvidence    `json:"evidence,omitempty"`
	Packages                   []EntitlementPackage `json:"entitlement_packages,omitempty"`
	CreatedAt                  time.Time            `json:"created_at"`
	UpdatedAt                  time.Time            `json:"updated_at"`
}

type AdminRefundOperation struct {
	ID                   uuid.UUID `json:"operation_id"`
	OperationType        string    `json:"operation_type"`
	PaymentOrderID       uuid.UUID `json:"payment_order_id"`
	PaymentTransactionID uuid.UUID `json:"payment_transaction_id"`
	EntitlementPackageID uuid.UUID `json:"entitlement_package_id"`
	ProviderRefundID     *string   `json:"provider_refund_id,omitempty"`
	AmountFen            int64     `json:"amount_fen"`
	BaseMicrocredits     int64     `json:"base_microcredits"`
	BonusMicrocredits    int64     `json:"bonus_microcredits"`
	Status               string    `json:"status"`
	Reason               string    `json:"reason"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type AdminPaymentOrderFilter struct {
	Query  string
	Status string
}

type AdminHold struct {
	ID                 uuid.UUID       `json:"hold_id"`
	UsageRequestID     uuid.UUID       `json:"usage_request_id"`
	WalletID           uuid.UUID       `json:"wallet_id"`
	PriceVersionID     *uuid.UUID      `json:"price_version_id,omitempty"`
	Status             string          `json:"status"`
	AnomalyState       string          `json:"anomaly_state"`
	MaxMicrocredits    int64           `json:"max_microcredits"`
	ActualMicrocredits *int64          `json:"actual_microcredits,omitempty"`
	ExpiresAt          time.Time       `json:"expires_at"`
	TerminalAt         *time.Time      `json:"terminal_at,omitempty"`
	ModelID            *string         `json:"model_id,omitempty"`
	ProtocolCode       *string         `json:"protocol_code,omitempty"`
	FormulaVersionID   *uuid.UUID      `json:"formula_version_id,omitempty"`
	PricingSnapshot    json.RawMessage `json:"pricing_snapshot,omitempty"`
	RequestSnapshot    json.RawMessage `json:"request_snapshot,omitempty"`
	UsageSnapshot      json.RawMessage `json:"usage_snapshot,omitempty"`
	OrganizationID     *uuid.UUID      `json:"organization_id,omitempty"`
	MemberUserID       *uuid.UUID      `json:"member_user_id,omitempty"`
	MembershipVersion  *int64          `json:"membership_version,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

type AdminHoldComponentAllocation struct {
	PackageID                  uuid.UUID `json:"package_id"`
	ComponentID                uuid.UUID `json:"component_id"`
	ComponentType              string    `json:"component_type"`
	CreditLotID                uuid.UUID `json:"credit_lot_id"`
	ReservedMicrocredits       int64     `json:"reserved_microcredits"`
	SettledMicrocredits        int64     `json:"settled_microcredits"`
	ReleasedMicrocredits       int64     `json:"released_microcredits"`
	AvailableMicrocredits      int64     `json:"component_available_microcredits"`
	CurrentReserved            int64     `json:"component_reserved_microcredits"`
	UsedMicrocredits           int64     `json:"component_used_microcredits"`
	ActiveGrantMicrocredits    int64     `json:"component_active_grant_microcredits"`
	SuspendedGrantMicrocredits int64     `json:"component_suspended_grant_microcredits"`
	Status                     string    `json:"component_status"`
}

type AdminHoldMemberAllocation struct {
	GrantID              uuid.UUID `json:"grant_id"`
	ComponentID          uuid.UUID `json:"component_id"`
	OrganizationID       uuid.UUID `json:"organization_id"`
	MemberUserID         uuid.UUID `json:"member_user_id"`
	ReservedMicrocredits int64     `json:"reserved_microcredits"`
	SettledMicrocredits  int64     `json:"settled_microcredits"`
	ReleasedMicrocredits int64     `json:"released_microcredits"`
	Status               string    `json:"grant_status"`
	AllocationVersion    int64     `json:"allocation_version"`
}

type AdminHoldLeaseEvent struct {
	ID                      uuid.UUID  `json:"lease_event_id"`
	EventType               string     `json:"event_type"`
	PreviousExpiresAt       *time.Time `json:"previous_expires_at,omitempty"`
	ExpiresAt               time.Time  `json:"expires_at"`
	PreviousMaxMicrocredits int64      `json:"previous_max_microcredits"`
	MaxMicrocredits         int64      `json:"max_microcredits"`
	AdditionalMicrocredits  int64      `json:"additional_microcredits"`
	CreatedAt               time.Time  `json:"created_at"`
}

type AdminHoldAction struct {
	ID           uuid.UUID       `json:"action_id"`
	Action       string          `json:"action"`
	Reason       string          `json:"reason"`
	Actor        string          `json:"actor"`
	BeforeStatus string          `json:"before_status"`
	AfterStatus  string          `json:"after_status"`
	Allocations  json.RawMessage `json:"allocation_snapshot"`
	CreatedAt    time.Time       `json:"created_at"`
}

type AdminHoldDetail struct {
	Hold                 AdminHold                      `json:"hold"`
	ComponentAllocations []AdminHoldComponentAllocation `json:"component_allocations"`
	MemberAllocations    []AdminHoldMemberAllocation    `json:"member_allocations"`
	LeaseEvents          []AdminHoldLeaseEvent          `json:"lease_events"`
	AdminActions         []AdminHoldAction              `json:"admin_actions"`
}

type AdminLedgerEntry struct {
	JournalEntryID         uuid.UUID  `json:"journal_entry_id"`
	PostingID              uuid.UUID  `json:"posting_id"`
	LedgerAccountID        uuid.UUID  `json:"ledger_account_id"`
	LedgerAccountCode      string     `json:"ledger_account_code"`
	LedgerAccountType      string     `json:"ledger_account_type"`
	CreditLotID            *uuid.UUID `json:"credit_lot_id,omitempty"`
	EntitlementPackageID   *uuid.UUID `json:"entitlement_package_id,omitempty"`
	EntitlementComponentID *uuid.UUID `json:"entitlement_component_id,omitempty"`
	ComponentType          *string    `json:"component_type,omitempty"`
	EntryType              string     `json:"entry_type"`
	BusinessRef            string     `json:"business_reference"`
	Description            string     `json:"description"`
	DeltaMicrocredit       int64      `json:"delta_microcredits"`
	JournalBalance         int64      `json:"journal_balance_microcredits"`
	CreatedAt              time.Time  `json:"created_at"`
}

type adminRepository interface {
	AdminOverview(context.Context) (AdminOverview, error)
	AdminWallets(context.Context, int) ([]Wallet, error)
	AdminPaymentOrders(context.Context, AdminPaymentOrderFilter, int) ([]AdminPaymentOrder, error)
	AdminRefunds(context.Context, int) ([]AdminRefundOperation, error)
	AdminAdjustments(context.Context, int) ([]ManualAdjustment, error)
	AdminWalletLedger(context.Context, uuid.UUID, int) ([]AdminLedgerEntry, error)
	AdminPaymentOrder(context.Context, uuid.UUID) (AdminPaymentOrder, error)
	AdminPaymentAnomalies(context.Context, PaymentAnomalyFilter, int) ([]PaymentAnomalyCase, error)
	PaymentAnomalyDetail(context.Context, uuid.UUID) (PaymentAnomalyDetail, error)
	ClaimPaymentAnomaly(context.Context, uuid.UUID, uuid.UUID, string) (PaymentAnomalyCase, error)
	ResolvePaymentAnomaly(context.Context, uuid.UUID, uuid.UUID, string, string, string, string) (PaymentAnomalyCase, error)
	PreparePaymentAnomalyResolution(context.Context, uuid.UUID, uuid.UUID, string, string, string) (PaymentAnomalyCase, bool, error)
	ApplyConfirmedChargeback(context.Context, uuid.UUID, string) (ChargebackCase, error)
	ApplyReviewedForcedRefund(context.Context, uuid.UUID) (ForcedRefundCase, error)
	AdminLatePaymentReviews(context.Context, LatePaymentReviewFilter, int) ([]LatePaymentReviewCase, error)
	LatePaymentReview(context.Context, uuid.UUID) (LatePaymentReviewCase, error)
	LatePaymentReviewDetail(context.Context, uuid.UUID) (LatePaymentReviewDetail, error)
	ClaimLatePaymentReview(context.Context, uuid.UUID, uuid.UUID) (LatePaymentReviewCase, error)
	ScheduleLatePaymentReview(context.Context, uuid.UUID, uuid.UUID, time.Time, string) (LatePaymentReviewCase, error)
	RecordLatePaymentReviewQuery(context.Context, uuid.UUID, uuid.UUID, PaymentOrder, error) (LatePaymentReviewCase, error)
	ArchiveLatePaymentReview(context.Context, uuid.UUID, uuid.UUID) (LatePaymentReviewCase, error)
	EscalateProviderBlind(context.Context, uuid.UUID, uuid.UUID, string) (LatePaymentReviewCase, error)
}

type adminAdjustmentRequester interface {
	RequestAdjustmentWithIdempotency(context.Context, uuid.UUID, int64, string, uuid.UUID, string) (ManualAdjustment, error)
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
	writeAdminFinancialJSON(w, http.StatusOK, map[string]any{"items": items})
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
	if s.payments != nil {
		result.PaymentCapabilities = s.payments.ProductCapabilities()
	}
	writeAdminFinancialJSON(w, http.StatusOK, result)
}

func (s *Service) adminWallets(w http.ResponseWriter, r *http.Request) {
	s.writeAdminSnapshotCollection(w, r, adminPageWallets, "", func(repository adminPaginationItemsRepository, rawIDs []string) (any, error) {
		ids, err := adminUUIDs(rawIDs)
		if err != nil {
			return nil, err
		}
		return repository.AdminWalletsByIDs(r.Context(), ids)
	})
}

type adminReceivableRepository interface {
	ListReceivables(context.Context, *uuid.UUID, string, int) ([]Receivable, error)
	ReceivableAllocations(context.Context, uuid.UUID) ([]ReceivableAllocation, error)
}

func (s *Service) adminReceivables(w http.ResponseWriter, r *http.Request) {
	s.writeAdminSnapshotCollection(w, r, adminPageReceivables, "", func(repository adminPaginationItemsRepository, rawIDs []string) (any, error) {
		ids, err := adminUUIDs(rawIDs)
		if err != nil {
			return nil, err
		}
		return repository.AdminReceivablesByIDs(r.Context(), ids)
	})
}

func (s *Service) adminReceivableAllocations(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(adminReceivableRepository)
	receivableID, err := uuid.Parse(r.PathValue("receivable_id"))
	if !ok || err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid receivable", httpserver.RequestID(r.Context()))
		return
	}
	items, err := repository.ReceivableAllocations(r.Context(), receivableID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeAdminFinancialJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminPaymentOrders(w http.ResponseWriter, r *http.Request) {
	s.writeAdminSnapshotCollection(w, r, adminPagePaymentOrders, "", func(repository adminPaginationItemsRepository, rawIDs []string) (any, error) {
		ids, err := adminUUIDs(rawIDs)
		if err != nil {
			return nil, err
		}
		return repository.AdminPaymentOrdersByIDs(r.Context(), ids)
	})
}

func (s *Service) adminRefunds(w http.ResponseWriter, r *http.Request) {
	s.writeAdminSnapshotCollection(w, r, adminPageRefunds, "", func(repository adminPaginationItemsRepository, rawIDs []string) (any, error) {
		return repository.AdminRefundsByIDs(r.Context(), rawIDs)
	})
}

func (s *Service) adminPaymentOrderDetail(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	orderID, err := uuid.Parse(r.PathValue("order_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid payment order", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.AdminPaymentOrder(r.Context(), orderID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeAdminFinancialJSON(w, http.StatusOK, item)
}

func validAdminPaymentStatus(status string) bool {
	switch status {
	case "CREATED", "PENDING", "PAID", "PAID_LATE", "FAILED", "CLOSED", "EXPIRED", "MANUAL_REVIEW":
		return true
	default:
		return false
	}
}

func (s *Service) adminAdjustments(w http.ResponseWriter, r *http.Request) {
	s.writeAdminSnapshotCollection(w, r, adminPageAdjustments, "", func(repository adminPaginationItemsRepository, rawIDs []string) (any, error) {
		ids, err := adminUUIDs(rawIDs)
		if err != nil {
			return nil, err
		}
		return repository.AdminAdjustmentsByIDs(r.Context(), ids)
	})
}

func (s *Service) adminRequestAdjustment(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(adminAdjustmentRequester)
	if !ok || s.authorizer == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "ADJUSTMENT_REQUEST_UNAVAILABLE", "manual adjustment requests are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var body struct {
		WalletID           uuid.UUID `json:"wallet_id"`
		AmountMicrocredits int64     `json:"amount_microcredits"`
		Reason             string    `json:"reason"`
		IdempotencyKey     string    `json:"idempotency_key"`
	}
	if api.DecodeJSON(w, r, &body) != nil || body.WalletID == uuid.Nil || body.AmountMicrocredits == 0 || len(strings.TrimSpace(body.Reason)) < 10 || len(strings.TrimSpace(body.IdempotencyKey)) < 8 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid manual adjustment request", httpserver.RequestID(r.Context()))
		return
	}
	requesterID, err := s.authorizer.AuthorizedUserID(r.Context(), adminPrincipal(r), "platform.billing.repairs.request")
	if err != nil {
		if errors.Is(err, authorization.ErrPermissionDenied) {
			api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "the adjustment request permission is not granted", httpserver.RequestID(r.Context()))
		} else {
			api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is temporarily unavailable", httpserver.RequestID(r.Context()))
		}
		return
	}
	adjustment, err := repository.RequestAdjustmentWithIdempotency(r.Context(), body.WalletID, body.AmountMicrocredits, body.Reason, requesterID, body.IdempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeAdminFinancialJSON(w, http.StatusCreated, adjustment)
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

func (s *Service) adminWalletLedger(w http.ResponseWriter, r *http.Request) {
	walletID, err := uuid.Parse(r.PathValue("wallet_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid wallet", httpserver.RequestID(r.Context()))
		return
	}
	if strings.TrimSpace(r.URL.Query().Get("cursor")) == "" {
		repository, ok := s.requireAdminRepository(w, r)
		if !ok {
			return
		}
		if _, err = repository.AdminWalletLedger(r.Context(), walletID, 1); err != nil {
			s.writeError(w, r, err)
			return
		}
	}
	s.writeAdminSnapshotCollection(w, r, adminPageWalletLedger, walletID.String(), func(repository adminPaginationItemsRepository, rawIDs []string) (any, error) {
		ids, err := adminUUIDs(rawIDs)
		if err != nil {
			return nil, err
		}
		return repository.AdminWalletLedgerByIDs(r.Context(), walletID, ids)
	})
}

func (s *Service) adminRejectAdjustment(w http.ResponseWriter, r *http.Request) {
	adjustmentID, err := uuid.Parse(r.PathValue("adjustment_id"))
	var body struct {
		Reason string `json:"reason"`
	}
	if decodeErr := api.DecodeJSON(w, r, &body); err != nil || decodeErr != nil || strings.TrimSpace(body.Reason) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "decision reason is required", httpserver.RequestID(r.Context()))
		return
	}
	actor := adminActor(adminPrincipal(r))
	adjustment, err := s.repository.RejectAdjustment(r.Context(), adjustmentID, actor, body.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeAdminFinancialJSON(w, http.StatusOK, adjustment)
}

func (s *Service) adminApproveAdjustment(w http.ResponseWriter, r *http.Request) {
	api.WriteError(w, http.StatusConflict, "APPROVAL_WORKFLOW_REQUIRED", "submit this adjustment through the approval workflow", httpserver.RequestID(r.Context()))
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
	writeAdminFinancialJSON(w, http.StatusOK, order)
}

func (s *Service) adminRepairPaymentOrder(w http.ResponseWriter, r *http.Request) {
	orderID, err := uuid.Parse(r.PathValue("order_id"))
	var body struct {
		Reason string `json:"reason"`
	}
	if decodeErr := api.DecodeJSON(w, r, &body); err != nil || decodeErr != nil || strings.TrimSpace(body.Reason) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "a repair reason is required", httpserver.RequestID(r.Context()))
		return
	}
	if s.payments == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "payment operations are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	order, err := s.payments.RepairReviewedOrder(r.Context(), orderID, adminActor(adminPrincipal(r)), body.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeAdminFinancialJSON(w, http.StatusOK, order)
}

func (s *Service) adminRefundPaymentOrder(w http.ResponseWriter, r *http.Request) {
	if s.payments == nil || !s.payments.NormalRefundCreationEnabled() {
		s.writeError(w, r, ErrNormalRefundCreationDisabled)
		return
	}
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	orderID, err := uuid.Parse(r.PathValue("order_id"))
	var body struct {
		PaymentTransactionID uuid.UUID `json:"payment_transaction_id"`
		EntitlementPackageID uuid.UUID `json:"entitlement_package_id"`
		Reason               string    `json:"reason"`
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if decodeErr := api.DecodeJSON(w, r, &body); err != nil || decodeErr != nil || body.PaymentTransactionID == uuid.Nil || body.EntitlementPackageID == uuid.Nil || strings.TrimSpace(body.Reason) == "" || len(idempotencyKey) < 16 || s.payments == nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "refund target, reason and idempotency key are required", httpserver.RequestID(r.Context()))
		return
	}
	order, err := repository.AdminPaymentOrder(r.Context(), orderID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	refund, _, err := s.payments.CreateRefund(r.Context(), orderID, order.RequesterID, body.PaymentTransactionID, body.EntitlementPackageID, idempotencyKey, body.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeAdminFinancialJSON(w, http.StatusCreated, refund)
}

func (s *Service) adminLatePaymentReviews(w http.ResponseWriter, r *http.Request) {
	s.writeAdminSnapshotCollection(w, r, adminPageLatePaymentReviews, "", func(repository adminPaginationItemsRepository, rawIDs []string) (any, error) {
		ids, err := adminUUIDs(rawIDs)
		if err != nil {
			return nil, err
		}
		return repository.AdminLatePaymentReviewsByIDs(r.Context(), ids)
	})
}

func (s *Service) adminLatePaymentReviewDetail(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	caseID, valid := parseLatePaymentCaseID(w, r)
	if !ok || !valid {
		return
	}
	detail, err := repository.LatePaymentReviewDetail(r.Context(), caseID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeAdminFinancialJSON(w, http.StatusOK, detail)
}

func parseLatePaymentCaseID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	caseID, err := uuid.Parse(r.PathValue("case_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid late payment review", httpserver.RequestID(r.Context()))
		return uuid.Nil, false
	}
	return caseID, true
}

func (s *Service) adminClaimLatePaymentReview(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	caseID, valid := parseLatePaymentCaseID(w, r)
	if !ok || !valid {
		return
	}
	item, err := repository.ClaimLatePaymentReview(r.Context(), caseID, adminActor(adminPrincipal(r)))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeAdminFinancialJSON(w, http.StatusOK, item)
}

func (s *Service) adminQueryLatePaymentReview(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	caseID, valid := parseLatePaymentCaseID(w, r)
	if !ok || !valid {
		return
	}
	if s.payments == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "payment operations are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	actorID := adminActor(adminPrincipal(r))
	target, err := repository.LatePaymentReview(r.Context(), caseID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if target.OwnerID == nil || *target.OwnerID != actorID {
		s.writeError(w, r, ErrConflict)
		return
	}
	order, queryErr := s.payments.SyncReviewedOrder(r.Context(), target.PaymentOrderID)
	item, recordErr := repository.RecordLatePaymentReviewQuery(r.Context(), caseID, actorID, order, queryErr)
	if recordErr != nil {
		s.writeError(w, r, recordErr)
		return
	}
	writeAdminFinancialJSON(w, http.StatusOK, item)
}

func (s *Service) adminScheduleLatePaymentReview(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	caseID, valid := parseLatePaymentCaseID(w, r)
	var body struct {
		NextReviewAt time.Time `json:"next_review_at"`
		Reason       string    `json:"reason"`
	}
	if !ok || !valid {
		return
	}
	if api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid financial request", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.ScheduleLatePaymentReview(r.Context(), caseID, adminActor(adminPrincipal(r)), body.NextReviewAt, body.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeAdminFinancialJSON(w, http.StatusOK, item)
}

func (s *Service) adminArchiveLatePaymentReview(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	caseID, valid := parseLatePaymentCaseID(w, r)
	if !ok || !valid {
		return
	}
	item, err := repository.ArchiveLatePaymentReview(r.Context(), caseID, adminActor(adminPrincipal(r)))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeAdminFinancialJSON(w, http.StatusOK, item)
}

func (s *Service) adminEscalateProviderBlind(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	caseID, valid := parseLatePaymentCaseID(w, r)
	var body struct {
		Reason string `json:"reason"`
	}
	if !ok || !valid {
		return
	}
	if api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid financial request", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.EscalateProviderBlind(r.Context(), caseID, adminActor(adminPrincipal(r)), body.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeAdminFinancialJSON(w, http.StatusOK, item)
}

func (s *Service) adminPaymentAnomalies(w http.ResponseWriter, r *http.Request) {
	s.writeAdminSnapshotCollection(w, r, adminPagePaymentAnomalies, "", func(repository adminPaginationItemsRepository, rawIDs []string) (any, error) {
		ids, err := adminUUIDs(rawIDs)
		if err != nil {
			return nil, err
		}
		return repository.AdminPaymentAnomaliesByIDs(r.Context(), ids)
	})
}

func (s *Service) adminPaymentAnomalyDetail(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	caseID, err := uuid.Parse(r.PathValue("case_id"))
	if !ok || err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid payment anomaly case", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.PaymentAnomalyDetail(r.Context(), caseID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeAdminFinancialJSON(w, http.StatusOK, item)
}

func (s *Service) adminClaimPaymentAnomaly(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	caseID, err := uuid.Parse(r.PathValue("case_id"))
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !ok || err != nil || len(idempotencyKey) < 8 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "valid case and idempotency key are required", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.ClaimPaymentAnomaly(r.Context(), caseID, adminActor(adminPrincipal(r)), idempotencyKey)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	writeAdminFinancialJSON(w, http.StatusOK, item)
}

func (s *Service) adminResolvePaymentAnomaly(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	caseID, parseErr := uuid.Parse(r.PathValue("case_id"))
	var body struct {
		Resolution string `json:"resolution"`
		Reason     string `json:"reason"`
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !ok || parseErr != nil || api.DecodeJSON(w, r, &body) != nil || len(idempotencyKey) < 8 || len([]rune(strings.TrimSpace(body.Reason))) < 10 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "valid resolution, reason, and idempotency key are required", httpserver.RequestID(r.Context()))
		return
	}
	resolution := strings.ToUpper(strings.TrimSpace(body.Resolution))
	prepared, completed, err := repository.PreparePaymentAnomalyResolution(r.Context(), caseID, adminActor(adminPrincipal(r)), idempotencyKey, resolution, body.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if completed {
		writeAdminFinancialJSON(w, http.StatusOK, prepared)
		return
	}
	// The immutable command has been accepted. Preserve its key and facts on
	// every incomplete outcome, including a provider's still-pending result.
	writeUnknown := func() {
		api.WriteError(w, http.StatusServiceUnavailable, "COMMAND_RESULT_UNKNOWN", "the accepted command is incomplete; retry with the same idempotency key and facts", httpserver.RequestID(r.Context()))
	}
	detail, err := repository.PaymentAnomalyDetail(r.Context(), caseID)
	if err != nil {
		writeUnknown()
		return
	}
	financialStatus := "NONE"
	switch resolution {
	case "CONFIRM_CHARGEBACK":
		if detail.Case.AnomalyType != "CHARGEBACK" || detail.Case.PaymentEvidenceID == nil || detail.Case.ChargebackCaseID == nil {
			err = ErrConflict
		} else {
			var chargeback ChargebackCase
			chargeback, err = repository.ApplyConfirmedChargeback(r.Context(), *detail.Case.PaymentEvidenceID, paymentAnomalyProviderReference(detail.Case.ResourceSnapshot, "provider_chargeback_id"))
			_ = chargeback
			financialStatus = "APPLIED"
		}
	case "CONFIRM_FORCED_REFUND":
		if detail.Case.AnomalyType != "FORCED_REFUND" || detail.Case.ForcedRefundCaseID == nil {
			err = ErrConflict
		} else {
			_, err = repository.ApplyReviewedForcedRefund(r.Context(), *detail.Case.ForcedRefundCaseID)
			financialStatus = "APPLIED"
		}
	case "CONFIRM_PAYMENT":
		if detail.Case.PaymentOrderID == nil || s.payments == nil || (detail.Case.AnomalyType != "DUPLICATE_PAYMENT" && detail.Case.AnomalyType != "LATE_PAYMENT_CONFLICT") {
			err = ErrConflict
		} else {
			_, err = s.payments.repairReviewedOrder(r.Context(), *detail.Case.PaymentOrderID, adminActor(adminPrincipal(r)), body.Reason, caseID, idempotencyKey)
			financialStatus = "APPLIED"
		}
	case "CONFIRM_PROVIDER_FAILED":
		if detail.Case.AnomalyType != "ILLEGAL_TRANSITION" || s.payments == nil {
			err = ErrConflict
		} else {
			var item PaymentAnomalyCase
			item, err = s.payments.ConfirmProviderFailed(r.Context(), caseID, adminActor(adminPrincipal(r)), idempotencyKey, body.Reason)
			if err == nil {
				writeAdminFinancialJSON(w, http.StatusOK, item)
				return
			}
		}
	case "RECONCILE_REFUND":
		if detail.Case.AnomalyType != "REFUND_FAILURE" || detail.Case.RefundID == nil || s.payments == nil {
			err = ErrConflict
		} else {
			var refund Refund
			refund, err = s.payments.processRefund(r.Context(), *detail.Case.RefundID, true)
			if err == nil && refund.Status != "SUCCEEDED" && refund.Status != "FAILED" {
				err = ErrConflict
			}
			financialStatus = "APPLIED"
		}
	case "DISMISS":
		if detail.Case.AnomalyType == "DUPLICATE_PAYMENT" || detail.Case.AnomalyType == "LATE_PAYMENT_CONFLICT" || detail.Case.AnomalyType == "ILLEGAL_TRANSITION" {
			err = ErrConflict
		} else {
			financialStatus = "NONE"
		}
	case "ESCALATE_FINANCIAL_REPAIR":
		financialStatus = "PENDING"
	default:
		err = ErrConflict
	}
	if err != nil {
		writeUnknown()
		return
	}
	item, err := repository.ResolvePaymentAnomaly(r.Context(), caseID, adminActor(adminPrincipal(r)), idempotencyKey, resolution, body.Reason, financialStatus)
	if err != nil {
		writeUnknown()
		return
	}
	writeAdminFinancialJSON(w, http.StatusOK, item)
}

func paymentAnomalyProviderReference(snapshot json.RawMessage, key string) string {
	var values map[string]any
	if json.Unmarshal(snapshot, &values) != nil {
		return ""
	}
	value, _ := values[key].(string)
	return value
}

func (r *PostgresRepository) AdminOverview(ctx context.Context) (AdminOverview, error) {
	var result AdminOverview
	err := r.pool.QueryRow(ctx, `
SELECT
  (SELECT count(*) FROM wallets),
  (SELECT COALESCE(sum(posted_microcredits),0) FROM wallets),
  (SELECT COALESCE(sum(reserved_microcredits),0) FROM wallets),
  (SELECT COALESCE(sum(receivable_microcredits),0) FROM wallets),
  (SELECT count(*) FROM payment_orders WHERE status IN ('CREATED','PENDING') AND expires_at > now()),
  (SELECT count(*) FROM payment_orders WHERE status='EXPIRED' OR (status IN ('CREATED','PENDING') AND expires_at <= now())),
  (SELECT count(*) FROM payment_orders WHERE status='MANUAL_REVIEW' OR fulfillment_status='MANUAL_REVIEW'),
  (SELECT count(*) FROM manual_adjustments WHERE status='PENDING_APPROVAL'),
  (SELECT count(*) FROM usage_holds WHERE status IN ('ACTIVE','REVIEW'))`).Scan(
		&result.Wallets, &result.PostedMicrocredits, &result.ReservedMicrocredits, &result.ReceivableMicrocredits,
		&result.PendingPaymentOrders, &result.ExpiredPaymentOrders, &result.ReviewPaymentOrders, &result.PendingAdjustments,
		&result.ActiveOrReviewUsageHolds,
	)
	return result, err
}

func (r *PostgresRepository) AdminWallets(ctx context.Context, limit int) ([]Wallet, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,owner_type,owner_id,status,posted_microcredits,reserved_microcredits,receivable_microcredits,posted_microcredits-reserved_microcredits-receivable_microcredits,financial_status,financial_freeze_reason,version FROM wallets ORDER BY updated_at DESC,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Wallet, 0)
	for rows.Next() {
		var item Wallet
		if err := rows.Scan(&item.ID, &item.OwnerType, &item.OwnerID, &item.Status, &item.PostedMicrocredits, &item.ReservedMicrocredits, &item.ReceivableMicrocredits, &item.AvailableMicrocredits, &item.FinancialStatus, &item.FinancialFreezeReason, &item.Version); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminPaymentOrders(ctx context.Context, filter AdminPaymentOrderFilter, limit int) ([]AdminPaymentOrder, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,merchant_order_no,provider_id,provider_version,product_id,COALESCE(provider_sku,''),COALESCE(primary_provider_transaction_id,''),COALESCE(mobile_e164,''),requester_id,payer_user_id,beneficiary_organization_id,wallet_id,channel,amount_fen,credit_microcredits,status,fulfillment_status,close_status,paid_at,payment_summary,refund_summary,chargeback_summary,attention_summary,summary_version,created_at,updated_at
		FROM payment_orders
		WHERE ($1='' OR id::text ILIKE '%'||$1||'%' OR merchant_order_no ILIKE '%'||$1||'%' OR COALESCE(primary_provider_transaction_id,'') ILIKE '%'||$1||'%' OR EXISTS(SELECT 1 FROM payment_transactions transaction WHERE transaction.payment_order_id=payment_orders.id AND transaction.provider_transaction_id ILIKE '%'||$1||'%') OR COALESCE(mobile_e164,'') ILIKE '%'||$1||'%' OR COALESCE(provider_sku,'') ILIKE '%'||$1||'%')
		AND ($2='' OR status=$2)
		ORDER BY updated_at DESC,id LIMIT $3`, filter.Query, filter.Status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminPaymentOrder, 0)
	for rows.Next() {
		var item AdminPaymentOrder
		var mobile string
		if err := rows.Scan(&item.ID, &item.MerchantOrderNo, &item.ProviderID, &item.ProviderVersion, &item.ProductID, &item.ProviderSKU, &item.PrimaryProviderTransaction, &mobile, &item.RequesterID, &item.PayerUserID, &item.BeneficiaryOrganizationID, &item.WalletID, &item.Channel, &item.AmountFen, &item.CreditMicrocredits, &item.Status, &item.FulfillmentStatus, &item.CloseStatus, &item.PaidAt, &item.PaymentSummary, &item.RefundSummary, &item.ChargebackSummary, &item.AttentionSummary, &item.SummaryVersion, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		item.MobileMasked = maskAdminMobile(mobile)
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminRefunds(ctx context.Context, limit int) ([]AdminRefundOperation, error) {
	rows, err := r.pool.Query(ctx, `SELECT operation_id,operation_type,payment_order_id,payment_transaction_id,entitlement_package_id,provider_refund_id,amount_fen,base_microcredits,bonus_microcredits,status,reason,created_at,updated_at FROM (
		SELECT refund.id operation_id,'NORMAL' operation_type,refund.payment_order_id,refund.payment_transaction_id,refund.entitlement_package_id,refund.channel_refund_id provider_refund_id,refund.amount_fen,refund.base_microcredits,refund.bonus_microcredits,refund.status,refund.reason,refund.created_at,refund.updated_at
		FROM refunds refund
		UNION ALL
		SELECT forced.id,'FORCED',forced.payment_order_id,forced.payment_transaction_id,forced.entitlement_package_id,forced.provider_refund_id,forced.amount_fen,0,0,forced.status,'PROVIDER_INITIATED',forced.created_at,COALESCE(forced.resolved_at,forced.created_at)
		FROM forced_refund_cases forced
	) operation ORDER BY updated_at DESC,operation_id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminRefundOperation, 0)
	for rows.Next() {
		var item AdminRefundOperation
		if err := rows.Scan(&item.ID, &item.OperationType, &item.PaymentOrderID, &item.PaymentTransactionID, &item.EntitlementPackageID, &item.ProviderRefundID, &item.AmountFen, &item.BaseMicrocredits, &item.BonusMicrocredits, &item.Status, &item.Reason, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func maskAdminMobile(mobile string) string {
	if len(mobile) < 7 {
		return mobile
	}
	return mobile[:3] + "****" + mobile[len(mobile)-4:]
}

func (r *PostgresRepository) AdminAdjustments(ctx context.Context, limit int) ([]ManualAdjustment, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,wallet_id,amount_microcredits,reason,requested_by,approved_by,rejected_by,status,version,request_idempotency_key,approval_request_id,execution_idempotency_key,journal_entry_id,decision_reason,created_at,updated_at,decided_at FROM manual_adjustments ORDER BY updated_at DESC,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ManualAdjustment, 0)
	for rows.Next() {
		var item ManualAdjustment
		if err := rows.Scan(&item.ID, &item.WalletID, &item.AmountMicrocredits, &item.Reason, &item.RequestedBy, &item.ApprovedBy, &item.RejectedBy, &item.Status, &item.Version, &item.RequestIdempotencyKey, &item.ApprovalRequestID, &item.ExecutionIdempotencyKey, &item.JournalEntryID, &item.DecisionReason, &item.CreatedAt, &item.UpdatedAt, &item.DecidedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

type billingRowScanner interface {
	Scan(...any) error
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
SELECT je.id,lp.id,la.id,la.code,la.account_type,lp.credit_lot_id,component.package_id,component.id,component.component_type,
je.entry_type,je.business_reference,je.description,lp.delta_microcredits,
(SELECT COALESCE(sum(peer.delta_microcredits),0) FROM ledger_postings peer WHERE peer.journal_entry_id=je.id),lp.created_at
FROM ledger_postings lp JOIN journal_entries je ON je.id=lp.journal_entry_id JOIN ledger_accounts la ON la.id=lp.ledger_account_id
LEFT JOIN entitlement_components component ON component.credit_lot_id=lp.credit_lot_id
WHERE la.wallet_id=$1 ORDER BY lp.created_at DESC,lp.id LIMIT $2`, walletID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminLedgerEntry, 0)
	for rows.Next() {
		var item AdminLedgerEntry
		if err := rows.Scan(&item.JournalEntryID, &item.PostingID, &item.LedgerAccountID, &item.LedgerAccountCode, &item.LedgerAccountType, &item.CreditLotID, &item.EntitlementPackageID, &item.EntitlementComponentID, &item.ComponentType, &item.EntryType, &item.BusinessRef, &item.Description, &item.DeltaMicrocredit, &item.JournalBalance, &item.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminPaymentOrder(ctx context.Context, orderID uuid.UUID) (AdminPaymentOrder, error) {
	var item AdminPaymentOrder
	var mobile string
	err := r.pool.QueryRow(ctx, `SELECT id,merchant_order_no,provider_id,provider_version,product_id,COALESCE(provider_sku,''),COALESCE(primary_provider_transaction_id,''),COALESCE(mobile_e164,''),requester_id,payer_user_id,beneficiary_organization_id,wallet_id,channel,amount_fen,credit_microcredits,status,fulfillment_status,close_status,paid_at,payment_summary,refund_summary,chargeback_summary,attention_summary,summary_version,created_at,updated_at FROM payment_orders WHERE id=$1`, orderID).Scan(&item.ID, &item.MerchantOrderNo, &item.ProviderID, &item.ProviderVersion, &item.ProductID, &item.ProviderSKU, &item.PrimaryProviderTransaction, &mobile, &item.RequesterID, &item.PayerUserID, &item.BeneficiaryOrganizationID, &item.WalletID, &item.Channel, &item.AmountFen, &item.CreditMicrocredits, &item.Status, &item.FulfillmentStatus, &item.CloseStatus, &item.PaidAt, &item.PaymentSummary, &item.RefundSummary, &item.ChargebackSummary, &item.AttentionSummary, &item.SummaryVersion, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminPaymentOrder{}, ErrNotFound
	}
	if err != nil {
		return item, err
	}
	item.MobileMasked = maskAdminMobile(mobile)
	transactionRows, err := r.pool.Query(ctx, `SELECT id,payment_order_id,provider_id,provider_version,provider_transaction_id,channel,status,fulfillment_status,amount_fen,currency,COALESCE(provider_status,''),provider_paid_at,verified_at,fulfilled_at FROM payment_transactions WHERE payment_order_id=$1 ORDER BY created_at,id`, orderID)
	if err != nil {
		return item, err
	}
	defer transactionRows.Close()
	item.Transactions = []PaymentTransaction{}
	for transactionRows.Next() {
		var transaction PaymentTransaction
		if err := transactionRows.Scan(&transaction.ID, &transaction.PaymentOrderID, &transaction.ProviderID, &transaction.ProviderVersion, &transaction.ProviderTransactionID, &transaction.Channel, &transaction.Status, &transaction.FulfillmentStatus, &transaction.AmountFen, &transaction.Currency, &transaction.ProviderStatus, &transaction.PaidAt, &transaction.VerifiedAt, &transaction.FulfilledAt); err != nil {
			return item, err
		}
		item.Transactions = append(item.Transactions, transaction)
	}
	if err := transactionRows.Err(); err != nil {
		return item, err
	}
	packageRows, err := r.pool.Query(ctx, `SELECT ep.id,ep.user_id,ep.wallet_id,ep.payment_order_id,ep.source_payment_transaction_id,COALESCE(po.merchant_order_no,''),ep.source_type,ep.source_reference,ep.product_id,ep.plan_name,ep.sku_code,ep.amount_fen,ep.points_total,ep.points_reserved,ep.points_used,ep.points_remaining,ep.points_expired,ep.points_revoked,ep.credited_at,ep.valid_from,ep.valid_until,ep.status FROM entitlement_packages ep LEFT JOIN payment_orders po ON po.id=ep.payment_order_id WHERE ep.payment_order_id=$1 ORDER BY ep.credited_at,ep.id`, orderID)
	if err != nil {
		return item, err
	}
	item.Packages = []EntitlementPackage{}
	packageIndex := map[uuid.UUID]int{}
	packageIDs := []uuid.UUID{}
	for packageRows.Next() {
		var entitlement EntitlementPackage
		if err := packageRows.Scan(&entitlement.ID, &entitlement.UserID, &entitlement.WalletID, &entitlement.PaymentOrderID, &entitlement.PaymentTransactionID, &entitlement.ProviderOrderReference, &entitlement.SourceType, &entitlement.SourceReference, &entitlement.ProductID, &entitlement.PlanName, &entitlement.SKUCode, &entitlement.AmountFen, &entitlement.PointsTotal, &entitlement.PointsReserved, &entitlement.PointsUsed, &entitlement.PointsRemaining, &entitlement.PointsExpired, &entitlement.PointsRevoked, &entitlement.CreditedAt, &entitlement.ValidFrom, &entitlement.ValidUntil, &entitlement.Status); err != nil {
			packageRows.Close()
			return item, err
		}
		entitlement.Components = []EntitlementComponent{}
		packageIndex[entitlement.ID] = len(item.Packages)
		packageIDs = append(packageIDs, entitlement.ID)
		item.Packages = append(item.Packages, entitlement)
	}
	if err := packageRows.Err(); err != nil {
		packageRows.Close()
		return item, err
	}
	packageRows.Close()
	if len(packageIDs) > 0 {
		componentRows, err := r.pool.Query(ctx, `SELECT package_id,id,component_type,original_microcredits,available_microcredits,reserved_microcredits,used_microcredits,active_grant_microcredits,suspended_grant_microcredits,expired_microcredits,revoked_microcredits,credited_at,expires_at,status FROM entitlement_components WHERE package_id=ANY($1) ORDER BY package_id,component_type,id`, packageIDs)
		if err != nil {
			return item, err
		}
		for componentRows.Next() {
			var packageID uuid.UUID
			var component EntitlementComponent
			if err := componentRows.Scan(&packageID, &component.ID, &component.ComponentType, &component.OriginalMicrocredits, &component.AvailableMicrocredits, &component.ReservedMicrocredits, &component.UsedMicrocredits, &component.ActiveGrantMicrocredits, &component.SuspendedGrantMicrocredits, &component.ExpiredMicrocredits, &component.RevokedMicrocredits, &component.CreditedAt, &component.ExpiresAt, &component.Status); err != nil {
				componentRows.Close()
				return item, err
			}
			component.EffectiveOriginalMicrocredits = component.OriginalMicrocredits
			item.Packages[packageIndex[packageID]].Components = append(item.Packages[packageIndex[packageID]].Components, component)
		}
		if err := componentRows.Err(); err != nil {
			componentRows.Close()
			return item, err
		}
		componentRows.Close()
	}
	evidenceRows, err := r.pool.Query(ctx, `SELECT id,payment_order_id,payment_transaction_id,evidence_type,provider_id,provider_version,evidence_key,provider_transaction_id,amount_fen,COALESCE(currency,''),COALESCE(provider_status,''),provider_event_at,received_at,payload_digest,payload_snapshot,merge_result FROM payment_evidence WHERE payment_order_id=$1 ORDER BY received_at,id`, orderID)
	if err != nil {
		return item, err
	}
	defer evidenceRows.Close()
	item.Evidence = []PaymentEvidence{}
	for evidenceRows.Next() {
		var evidence PaymentEvidence
		if err := evidenceRows.Scan(&evidence.ID, &evidence.PaymentOrderID, &evidence.PaymentTransactionID, &evidence.EvidenceType, &evidence.ProviderID, &evidence.ProviderVersion, &evidence.EvidenceKey, &evidence.ProviderTransactionID, &evidence.AmountFen, &evidence.Currency, &evidence.ProviderStatus, &evidence.ProviderEventAt, &evidence.ReceivedAt, &evidence.PayloadDigest, &evidence.PayloadSnapshot, &evidence.MergeResult); err != nil {
			return item, err
		}
		item.Evidence = append(item.Evidence, evidence)
	}
	return item, evidenceRows.Err()
}

const reconciliationCaseSelect = `SELECT id,reconciliation_run_id,payment_order_id,merchant_order_no,case_type,status,severity,owner_id,repair_action,review_result,provider_snapshot,internal_snapshot,refund_snapshot,ledger_snapshot,resolution,created_at,resolved_at,updated_at,version FROM reconciliation_cases`
