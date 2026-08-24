package billing

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
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
	Wallets                  int64               `json:"wallets"`
	PostedMicrocredits       int64               `json:"posted_microcredits"`
	ReservedMicrocredits     int64               `json:"reserved_microcredits"`
	PendingPaymentOrders     int64               `json:"pending_payment_orders"`
	ExpiredPaymentOrders     int64               `json:"expired_payment_orders"`
	ReviewPaymentOrders      int64               `json:"review_payment_orders"`
	PendingAdjustments       int64               `json:"pending_adjustments"`
	ActiveOrReviewUsageHolds int64               `json:"active_or_review_usage_holds"`
	PaymentCapabilities      PaymentCapabilities `json:"payment_capabilities"`
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

type AdminReconciliationCase struct {
	ID               uuid.UUID       `json:"case_id"`
	RunID            uuid.UUID       `json:"reconciliation_run_id"`
	PaymentOrderID   *uuid.UUID      `json:"payment_order_id,omitempty"`
	MerchantOrderNo  string          `json:"merchant_order_no"`
	CaseType         string          `json:"case_type"`
	Status           string          `json:"status"`
	Severity         string          `json:"severity"`
	OwnerID          *uuid.UUID      `json:"owner_id,omitempty"`
	RepairAction     *string         `json:"repair_action,omitempty"`
	ReviewResult     *string         `json:"review_result,omitempty"`
	ProviderSnapshot json.RawMessage `json:"provider_snapshot,omitempty"`
	InternalSnapshot json.RawMessage `json:"internal_snapshot,omitempty"`
	RefundSnapshot   json.RawMessage `json:"refund_snapshot,omitempty"`
	LedgerSnapshot   json.RawMessage `json:"ledger_snapshot,omitempty"`
	Resolution       *string         `json:"resolution,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	ResolvedAt       *time.Time      `json:"resolved_at,omitempty"`
	UpdatedAt        time.Time       `json:"updated_at"`
	Version          int64           `json:"version"`
}

type ReconciliationCaseCommand struct {
	Action         string
	OwnerID        *uuid.UUID
	RepairAction   string
	ReviewResult   string
	Reason         string
	IdempotencyKey string
	ActorID        uuid.UUID
}

type adminRepository interface {
	AdminOverview(context.Context) (AdminOverview, error)
	AdminWallets(context.Context, int) ([]Wallet, error)
	AdminPaymentOrders(context.Context, AdminPaymentOrderFilter, int) ([]AdminPaymentOrder, error)
	AdminRefunds(context.Context, int) ([]AdminRefundOperation, error)
	AdminAdjustments(context.Context, int) ([]ManualAdjustment, error)
	AdminUsageHolds(context.Context, int) ([]AdminHold, error)
	AdminUsageHold(context.Context, uuid.UUID) (AdminHoldDetail, error)
	AdminPriceVersions(context.Context, int) ([]AdminPriceVersion, error)
	AdminCreatePriceVersion(context.Context, string, int64, int64, int64) (AdminPriceVersion, error)
	AdminWalletLedger(context.Context, uuid.UUID, int) ([]AdminLedgerEntry, error)
	AdminPaymentOrder(context.Context, uuid.UUID) (AdminPaymentOrder, error)
	AdminReleaseUsageHold(context.Context, uuid.UUID, string, string, string) (AdminHold, error)
	AdminReconciliationRuns(context.Context, int) ([]ReconciliationRun, error)
	AdminReconciliationCases(context.Context, uuid.UUID, int) ([]AdminReconciliationCase, error)
	UpdateReconciliationCase(context.Context, uuid.UUID, ReconciliationCaseCommand) (AdminReconciliationCase, error)
	AdminPaymentAnomalies(context.Context, PaymentAnomalyFilter, int) ([]PaymentAnomalyCase, error)
	PaymentAnomalyDetail(context.Context, uuid.UUID) (PaymentAnomalyDetail, error)
	ClaimPaymentAnomaly(context.Context, uuid.UUID, uuid.UUID, string) (PaymentAnomalyCase, error)
	ResolvePaymentAnomaly(context.Context, uuid.UUID, uuid.UUID, string, string, string, string) (PaymentAnomalyCase, error)
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
	if s.payments != nil {
		result.PaymentCapabilities = s.payments.Capabilities()
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminWallets(w http.ResponseWriter, r *http.Request) {
	s.writeAdminCollection(w, r, func(repository adminRepository, limit int) (any, error) {
		return repository.AdminWallets(r.Context(), limit)
	})
}

type adminReceivableRepository interface {
	ListReceivables(context.Context, *uuid.UUID, string, int) ([]Receivable, error)
	ReceivableAllocations(context.Context, uuid.UUID) ([]ReceivableAllocation, error)
}

func (s *Service) adminReceivables(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(adminReceivableRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "receivable administration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var walletID *uuid.UUID
	if value := strings.TrimSpace(r.URL.Query().Get("wallet_id")); value != "" {
		parsed, err := uuid.Parse(value)
		if err != nil {
			api.WriteError(w, http.StatusBadRequest, "INVALID_FILTER", "receivable wallet filter is invalid", httpserver.RequestID(r.Context()))
			return
		}
		walletID = &parsed
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status != "" && status != "OPEN" && status != "PARTIALLY_SETTLED" && status != "SETTLED" && status != "WRITTEN_OFF" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_FILTER", "receivable status filter is invalid", httpserver.RequestID(r.Context()))
		return
	}
	items, err := repository.ListReceivables(r.Context(), walletID, status, billingAdminLimit(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
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
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminPaymentOrders(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	filter := AdminPaymentOrderFilter{Query: strings.TrimSpace(r.URL.Query().Get("q")), Status: strings.TrimSpace(r.URL.Query().Get("status"))}
	if len(filter.Query) > 160 || (filter.Status != "" && !validAdminPaymentStatus(filter.Status)) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_FILTER", "payment order filter is invalid", httpserver.RequestID(r.Context()))
		return
	}
	items, err := repository.AdminPaymentOrders(r.Context(), filter, billingAdminLimit(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminRefunds(w http.ResponseWriter, r *http.Request) {
	s.writeAdminCollection(w, r, func(repository adminRepository, limit int) (any, error) {
		return repository.AdminRefunds(r.Context(), limit)
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
	api.WriteJSON(w, http.StatusOK, item)
}

func validAdminPaymentStatus(status string) bool {
	switch status {
	case "CREATED", "PENDING", "PAID", "PAID_LATE", "FAILED", "CLOSED", "MANUAL_REVIEW":
		return true
	default:
		return false
	}
}

func (s *Service) adminAdjustments(w http.ResponseWriter, r *http.Request) {
	s.writeAdminCollection(w, r, func(repository adminRepository, limit int) (any, error) {
		return repository.AdminAdjustments(r.Context(), limit)
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
	api.WriteJSON(w, http.StatusCreated, adjustment)
}

func (s *Service) adminUsageHolds(w http.ResponseWriter, r *http.Request) {
	s.writeAdminCollection(w, r, func(repository adminRepository, limit int) (any, error) {
		return repository.AdminUsageHolds(r.Context(), limit)
	})
}

func (s *Service) adminUsageHold(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	holdID, err := uuid.Parse(r.PathValue("hold_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid usage hold", httpserver.RequestID(r.Context()))
		return
	}
	detail, err := repository.AdminUsageHold(r.Context(), holdID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, detail)
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
	api.WriteJSON(w, http.StatusOK, adjustment)
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
	api.WriteJSON(w, http.StatusOK, order)
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
	api.WriteJSON(w, http.StatusOK, order)
}

func (s *Service) adminRefundPaymentOrder(w http.ResponseWriter, r *http.Request) {
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
	api.WriteJSON(w, http.StatusCreated, refund)
}

func (s *Service) adminLatePaymentReviews(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	filter := LatePaymentReviewFilter{Status: strings.TrimSpace(r.URL.Query().Get("status")), ProviderID: strings.TrimSpace(r.URL.Query().Get("provider_id")), Overdue: strings.EqualFold(r.URL.Query().Get("overdue"), "true")}
	if raw := strings.TrimSpace(r.URL.Query().Get("owner_id")); raw != "" {
		ownerID, err := uuid.Parse(raw)
		if err != nil {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid review owner", httpserver.RequestID(r.Context()))
			return
		}
		filter.OwnerID = &ownerID
	}
	for raw, target := range map[string]**time.Time{
		"entered_from":       &filter.EnteredFrom,
		"entered_to":         &filter.EnteredTo,
		"next_review_before": &filter.NextReviewBefore,
	} {
		value := strings.TrimSpace(r.URL.Query().Get(raw))
		if value == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, value)
		if err != nil {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", raw+" must use RFC3339", httpserver.RequestID(r.Context()))
			return
		}
		parsed = parsed.UTC()
		*target = &parsed
	}
	items, err := repository.AdminLatePaymentReviews(r.Context(), filter, billingAdminLimit(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
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
	api.WriteJSON(w, http.StatusOK, detail)
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
	api.WriteJSON(w, http.StatusOK, item)
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
	api.WriteJSON(w, http.StatusOK, item)
}

func (s *Service) adminScheduleLatePaymentReview(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	caseID, valid := parseLatePaymentCaseID(w, r)
	var body struct {
		NextReviewAt time.Time `json:"next_review_at"`
		Reason       string    `json:"reason"`
	}
	if !ok || !valid || api.DecodeJSON(w, r, &body) != nil {
		return
	}
	item, err := repository.ScheduleLatePaymentReview(r.Context(), caseID, adminActor(adminPrincipal(r)), body.NextReviewAt, body.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
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
	api.WriteJSON(w, http.StatusOK, item)
}

func (s *Service) adminEscalateProviderBlind(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	caseID, valid := parseLatePaymentCaseID(w, r)
	var body struct {
		Reason string `json:"reason"`
	}
	if !ok || !valid || api.DecodeJSON(w, r, &body) != nil {
		return
	}
	item, err := repository.EscalateProviderBlind(r.Context(), caseID, adminActor(adminPrincipal(r)), body.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
}

func (s *Service) adminImportReconciliation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Channel      string              `json:"channel"`
		BusinessDate string              `json:"business_date"`
		Items        []ProviderBillEntry `json:"items"`
	}
	if s.payments == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "PAYMENT_UNAVAILABLE", "payment operations are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if api.DecodeJSON(w, r, &body) != nil {
		return
	}
	businessDate, err := time.Parse("2006-01-02", body.BusinessDate)
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "business_date must use YYYY-MM-DD", httpserver.RequestID(r.Context()))
		return
	}
	run, err := s.payments.ImportProviderBill(r.Context(), adminActor(adminPrincipal(r)), body.Channel, businessDate, body.Items)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, run)
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
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if decodeErr := api.DecodeJSON(w, r, &body); err != nil || decodeErr != nil || strings.TrimSpace(body.Reason) == "" || len(idempotencyKey) < 8 || len(idempotencyKey) > 120 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "release reason and idempotency key are required", httpserver.RequestID(r.Context()))
		return
	}
	principal := adminPrincipal(r)
	hold, err := repository.AdminReleaseUsageHold(r.Context(), holdID, body.Reason, principal.Issuer+"|"+principal.Subject, idempotencyKey)
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
	run, err := s.payments.RunAdminReconciliation(r.Context(), adminActor(adminPrincipal(r)), body.Channel, date)
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

func (s *Service) adminUpdateReconciliationCase(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	caseID, err := uuid.Parse(r.PathValue("case_id"))
	var body struct {
		Action       string     `json:"action"`
		OwnerID      *uuid.UUID `json:"owner_id"`
		RepairAction string     `json:"repair_action"`
		ReviewResult string     `json:"review_result"`
		Reason       string     `json:"reason"`
	}
	idempotencyKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if !ok || err != nil || api.DecodeJSON(w, r, &body) != nil || len(idempotencyKey) < 8 || len([]rune(strings.TrimSpace(body.Reason))) < 10 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "valid reconciliation command, reason, and idempotency key are required", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.UpdateReconciliationCase(r.Context(), caseID, ReconciliationCaseCommand{Action: body.Action, OwnerID: body.OwnerID, RepairAction: body.RepairAction, ReviewResult: body.ReviewResult, Reason: body.Reason, IdempotencyKey: idempotencyKey, ActorID: adminActor(adminPrincipal(r))})
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
}

func (s *Service) adminPaymentAnomalies(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.requireAdminRepository(w, r)
	if !ok {
		return
	}
	filter := PaymentAnomalyFilter{AnomalyType: r.URL.Query().Get("anomaly_type"), Status: r.URL.Query().Get("status"), Severity: r.URL.Query().Get("severity")}
	if raw := strings.TrimSpace(r.URL.Query().Get("owner_id")); raw != "" {
		owner, err := uuid.Parse(raw)
		if err != nil {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid owner_id", httpserver.RequestID(r.Context()))
			return
		}
		filter.OwnerID = &owner
	}
	items, err := repository.AdminPaymentAnomalies(r.Context(), filter, billingAdminLimit(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
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
	api.WriteJSON(w, http.StatusOK, item)
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
	api.WriteJSON(w, http.StatusOK, item)
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
	detail, err := repository.PaymentAnomalyDetail(r.Context(), caseID)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	resolution := strings.ToUpper(strings.TrimSpace(body.Resolution))
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
			_, err = s.payments.RepairReviewedOrder(r.Context(), *detail.Case.PaymentOrderID, adminActor(adminPrincipal(r)), body.Reason)
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
		s.writeError(w, r, err)
		return
	}
	item, err := repository.ResolvePaymentAnomaly(r.Context(), caseID, adminActor(adminPrincipal(r)), idempotencyKey, resolution, body.Reason, financialStatus)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
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

func (r *PostgresRepository) AdminUsageHolds(ctx context.Context, limit int) ([]AdminHold, error) {
	rows, err := r.pool.Query(ctx, `SELECT hold.id,hold.usage_request_id,hold.wallet_id,hold.price_version_id,hold.status,
CASE WHEN hold.status='REVIEW' THEN 'REVIEW_REQUIRED'
 WHEN hold.status='ACTIVE' AND hold.expires_at<=now() THEN 'LEASE_EXPIRED'
 WHEN hold.status<>'ACTIVE' AND COALESCE(allocation.outstanding,0)<>0 THEN 'TERMINAL_ALLOCATION_REMAINS'
 WHEN hold.status='ACTIVE' AND COALESCE(allocation.outstanding,0)<>hold.max_microcredits THEN 'ALLOCATION_MISMATCH'
 ELSE 'NONE' END,
hold.max_microcredits,hold.actual_microcredits,hold.expires_at,hold.terminal_at,hold.model_id,hold.protocol_code,hold.formula_version_id,hold.pricing_snapshot,hold.request_snapshot,hold.usage_snapshot,hold.organization_id,hold.member_user_id,hold.membership_version,hold.created_at,hold.updated_at
FROM usage_holds hold LEFT JOIN (
 SELECT hold_id,sum(reserved_microcredits-settled_microcredits-released_microcredits) outstanding
 FROM usage_hold_allocations GROUP BY hold_id
) allocation ON allocation.hold_id=hold.id ORDER BY hold.updated_at DESC,hold.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminHold, 0)
	for rows.Next() {
		var item AdminHold
		if err := scanAdminHold(rows, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

type adminHoldScanner interface {
	Scan(...any) error
}

func scanAdminHold(row adminHoldScanner, item *AdminHold) error {
	return row.Scan(&item.ID, &item.UsageRequestID, &item.WalletID, &item.PriceVersionID, &item.Status, &item.AnomalyState, &item.MaxMicrocredits, &item.ActualMicrocredits, &item.ExpiresAt, &item.TerminalAt, &item.ModelID, &item.ProtocolCode, &item.FormulaVersionID, &item.PricingSnapshot, &item.RequestSnapshot, &item.UsageSnapshot, &item.OrganizationID, &item.MemberUserID, &item.MembershipVersion, &item.CreatedAt, &item.UpdatedAt)
}

func (r *PostgresRepository) AdminUsageHold(ctx context.Context, holdID uuid.UUID) (AdminHoldDetail, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return AdminHoldDetail{}, err
	}
	defer tx.Rollback(ctx)
	detail := AdminHoldDetail{
		ComponentAllocations: []AdminHoldComponentAllocation{}, MemberAllocations: []AdminHoldMemberAllocation{},
		LeaseEvents: []AdminHoldLeaseEvent{}, AdminActions: []AdminHoldAction{},
	}
	err = scanAdminHold(tx.QueryRow(ctx, `SELECT hold.id,hold.usage_request_id,hold.wallet_id,hold.price_version_id,hold.status,
CASE WHEN hold.status='REVIEW' THEN 'REVIEW_REQUIRED'
 WHEN hold.status='ACTIVE' AND hold.expires_at<=now() THEN 'LEASE_EXPIRED'
 WHEN hold.status<>'ACTIVE' AND COALESCE(allocation.outstanding,0)<>0 THEN 'TERMINAL_ALLOCATION_REMAINS'
 WHEN hold.status='ACTIVE' AND COALESCE(allocation.outstanding,0)<>hold.max_microcredits THEN 'ALLOCATION_MISMATCH'
 ELSE 'NONE' END,
hold.max_microcredits,hold.actual_microcredits,hold.expires_at,hold.terminal_at,hold.model_id,hold.protocol_code,hold.formula_version_id,hold.pricing_snapshot,hold.request_snapshot,hold.usage_snapshot,hold.organization_id,hold.member_user_id,hold.membership_version,hold.created_at,hold.updated_at
FROM usage_holds hold LEFT JOIN (
 SELECT hold_id,sum(reserved_microcredits-settled_microcredits-released_microcredits) outstanding
 FROM usage_hold_allocations GROUP BY hold_id
) allocation ON allocation.hold_id=hold.id WHERE hold.id=$1`, holdID), &detail.Hold)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminHoldDetail{}, ErrNotFound
	}
	if err != nil {
		return AdminHoldDetail{}, err
	}
	componentRows, err := tx.Query(ctx, `SELECT allocation.package_id,allocation.component_id,component.component_type,component.credit_lot_id,allocation.reserved_microcredits,allocation.settled_microcredits,allocation.released_microcredits,component.available_microcredits,component.reserved_microcredits,component.used_microcredits,component.active_grant_microcredits,component.suspended_grant_microcredits,component.status FROM usage_hold_allocations allocation JOIN entitlement_components component ON component.id=allocation.component_id WHERE allocation.hold_id=$1 ORDER BY component.expires_at,component.component_type,component.id`, holdID)
	if err != nil {
		return AdminHoldDetail{}, err
	}
	for componentRows.Next() {
		var item AdminHoldComponentAllocation
		if err := componentRows.Scan(&item.PackageID, &item.ComponentID, &item.ComponentType, &item.CreditLotID, &item.ReservedMicrocredits, &item.SettledMicrocredits, &item.ReleasedMicrocredits, &item.AvailableMicrocredits, &item.CurrentReserved, &item.UsedMicrocredits, &item.ActiveGrantMicrocredits, &item.SuspendedGrantMicrocredits, &item.Status); err != nil {
			componentRows.Close()
			return AdminHoldDetail{}, err
		}
		detail.ComponentAllocations = append(detail.ComponentAllocations, item)
	}
	if err := componentRows.Err(); err != nil {
		componentRows.Close()
		return AdminHoldDetail{}, err
	}
	componentRows.Close()
	memberRows, err := tx.Query(ctx, `SELECT allocation.grant_id,allocation.component_id,grant.organization_id,grant.member_user_id,allocation.reserved_microcredits,allocation.settled_microcredits,allocation.released_microcredits,grant.status,grant.allocation_version FROM member_quota_hold_allocations allocation JOIN member_quota_grants grant ON grant.id=allocation.grant_id WHERE allocation.hold_id=$1 ORDER BY allocation.component_id,allocation.grant_id`, holdID)
	if err != nil {
		return AdminHoldDetail{}, err
	}
	for memberRows.Next() {
		var item AdminHoldMemberAllocation
		if err := memberRows.Scan(&item.GrantID, &item.ComponentID, &item.OrganizationID, &item.MemberUserID, &item.ReservedMicrocredits, &item.SettledMicrocredits, &item.ReleasedMicrocredits, &item.Status, &item.AllocationVersion); err != nil {
			memberRows.Close()
			return AdminHoldDetail{}, err
		}
		detail.MemberAllocations = append(detail.MemberAllocations, item)
	}
	if err := memberRows.Err(); err != nil {
		memberRows.Close()
		return AdminHoldDetail{}, err
	}
	memberRows.Close()
	leaseRows, err := tx.Query(ctx, `SELECT id,event_type,previous_expires_at,expires_at,previous_max_microcredits,max_microcredits,additional_microcredits,created_at FROM usage_hold_lease_events WHERE hold_id=$1 ORDER BY created_at,id`, holdID)
	if err != nil {
		return AdminHoldDetail{}, err
	}
	for leaseRows.Next() {
		var item AdminHoldLeaseEvent
		if err := leaseRows.Scan(&item.ID, &item.EventType, &item.PreviousExpiresAt, &item.ExpiresAt, &item.PreviousMaxMicrocredits, &item.MaxMicrocredits, &item.AdditionalMicrocredits, &item.CreatedAt); err != nil {
			leaseRows.Close()
			return AdminHoldDetail{}, err
		}
		detail.LeaseEvents = append(detail.LeaseEvents, item)
	}
	if err := leaseRows.Err(); err != nil {
		leaseRows.Close()
		return AdminHoldDetail{}, err
	}
	leaseRows.Close()
	actionRows, err := tx.Query(ctx, `SELECT id,action,reason,actor,before_status,after_status,allocation_snapshot,created_at FROM usage_hold_admin_actions WHERE hold_id=$1 ORDER BY created_at,id`, holdID)
	if err != nil {
		return AdminHoldDetail{}, err
	}
	for actionRows.Next() {
		var item AdminHoldAction
		if err := actionRows.Scan(&item.ID, &item.Action, &item.Reason, &item.Actor, &item.BeforeStatus, &item.AfterStatus, &item.Allocations, &item.CreatedAt); err != nil {
			actionRows.Close()
			return AdminHoldDetail{}, err
		}
		detail.AdminActions = append(detail.AdminActions, item)
	}
	if err := actionRows.Err(); err != nil {
		actionRows.Close()
		return AdminHoldDetail{}, err
	}
	actionRows.Close()
	return detail, tx.Commit(ctx)
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
	if _, err := tx.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,created_at,scope_type,scope_id,protocol_code,formula_version_id,credit_unit,lifecycle_status) VALUES($1,$2,$3,$4,$5,$6,$6,'MODEL',$2,'CHAT',$7,'MICROCREDIT','DRAFT')`, item.ID, model, input, output, cached, now, chatFormulaVersionID); err != nil {
		return AdminPriceVersion{}, err
	}
	for unitID, rate := range map[uuid.UUID]int64{
		uuid.MustParse("00000000-0000-7000-8100-000000000001"): input,
		uuid.MustParse("00000000-0000-7000-8100-000000000002"): cached,
		uuid.MustParse("00000000-0000-7000-8100-000000000003"): output,
	} {
		if _, err := tx.Exec(ctx, `INSERT INTO pricing_version_unit_configs(price_version_id,unit_definition_id,config_type,fixed_price_per_denominator,created_at) VALUES($1,$2,'FIXED_PRICE',$3,$4)`, item.ID, unitID, rate, now); err != nil {
			return AdminPriceVersion{}, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE pricing_versions SET lifecycle_status='PUBLISHED',published_at=$2 WHERE id=$1`, item.ID, now); err != nil {
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

func (r *PostgresRepository) AdminReleaseUsageHold(ctx context.Context, holdID uuid.UUID, reason, actor, idempotencyKey string) (AdminHold, error) {
	reason, actor, idempotencyKey = strings.TrimSpace(reason), strings.TrimSpace(actor), strings.TrimSpace(idempotencyKey)
	if holdID == uuid.Nil || reason == "" || len(reason) > 500 || actor == "" || len(idempotencyKey) < 8 || len(idempotencyKey) > 120 {
		return AdminHold{}, ErrConflict
	}
	fingerprintBytes := sha256.Sum256([]byte(holdID.String() + "\x00" + reason + "\x00" + actor))
	fingerprint := fmt.Sprintf("%x", fingerprintBytes[:])
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return AdminHold{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "usage-hold.admin.safe-release", idempotencyKey); err != nil {
		return AdminHold{}, err
	}
	var existingHoldID uuid.UUID
	var existingFingerprint string
	err = tx.QueryRow(ctx, `SELECT hold_id,request_fingerprint FROM usage_hold_admin_actions WHERE action='SAFE_RELEASE' AND idempotency_key=$1`, idempotencyKey).Scan(&existingHoldID, &existingFingerprint)
	if err == nil {
		if existingHoldID != holdID || existingFingerprint != fingerprint {
			return AdminHold{}, ErrIdempotencyConflict
		}
		return adminHoldByID(ctx, tx, holdID)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AdminHold{}, err
	}
	var hold AdminHold
	err = tx.QueryRow(ctx, `SELECT id,usage_request_id,wallet_id,price_version_id,status,max_microcredits,actual_microcredits,expires_at,terminal_at,model_id,protocol_code,formula_version_id,pricing_snapshot,request_snapshot,usage_snapshot,organization_id,member_user_id,membership_version,created_at,updated_at FROM usage_holds WHERE id=$1 FOR UPDATE`, holdID).Scan(&hold.ID, &hold.UsageRequestID, &hold.WalletID, &hold.PriceVersionID, &hold.Status, &hold.MaxMicrocredits, &hold.ActualMicrocredits, &hold.ExpiresAt, &hold.TerminalAt, &hold.ModelID, &hold.ProtocolCode, &hold.FormulaVersionID, &hold.PricingSnapshot, &hold.RequestSnapshot, &hold.UsageSnapshot, &hold.OrganizationID, &hold.MemberUserID, &hold.MembershipVersion, &hold.CreatedAt, &hold.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminHold{}, ErrNotFound
	}
	if err != nil {
		return AdminHold{}, err
	}
	now := r.now()
	if hold.Status != "REVIEW" && !(hold.Status == "ACTIVE" && !hold.ExpiresAt.After(now)) {
		return AdminHold{}, ErrConflict
	}
	beforeStatus := hold.Status
	if err := releasePackageAllocations(ctx, tx, hold.ID, now); err != nil {
		return AdminHold{}, err
	}
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
	var allocationSnapshot json.RawMessage
	if err := tx.QueryRow(ctx, `SELECT jsonb_build_object(
'component_allocations',COALESCE((SELECT jsonb_agg(jsonb_build_object('package_id',package_id,'component_id',component_id,'reserved_microcredits',reserved_microcredits,'settled_microcredits',settled_microcredits,'released_microcredits',released_microcredits) ORDER BY component_id) FROM usage_hold_allocations WHERE hold_id=$1),'[]'::jsonb),
'member_allocations',COALESCE((SELECT jsonb_agg(jsonb_build_object('grant_id',grant_id,'component_id',component_id,'reserved_microcredits',reserved_microcredits,'settled_microcredits',settled_microcredits,'released_microcredits',released_microcredits) ORDER BY grant_id) FROM member_quota_hold_allocations WHERE hold_id=$1),'[]'::jsonb))`, holdID).Scan(&allocationSnapshot); err != nil {
		return AdminHold{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO usage_hold_admin_actions(id,hold_id,action,idempotency_key,request_fingerprint,reason,actor,before_status,after_status,allocation_snapshot,created_at) VALUES($1,$2,'SAFE_RELEASE',$3,$4,$5,$6,$7,'RELEASED',$8,$9)`, uuid.Must(uuid.NewV7()), holdID, idempotencyKey, fingerprint, reason, actor, beforeStatus, allocationSnapshot, now); err != nil {
		return AdminHold{}, err
	}
	hold.Status, hold.AnomalyState, hold.TerminalAt, hold.UpdatedAt = "RELEASED", "NONE", &now, now
	if err := tx.Commit(ctx); err != nil {
		return AdminHold{}, err
	}
	return hold, nil
}

func adminHoldByID(ctx context.Context, tx pgx.Tx, holdID uuid.UUID) (AdminHold, error) {
	var hold AdminHold
	err := tx.QueryRow(ctx, `SELECT hold.id,hold.usage_request_id,hold.wallet_id,hold.price_version_id,hold.status,
CASE WHEN hold.status='REVIEW' THEN 'REVIEW_REQUIRED'
 WHEN hold.status='ACTIVE' AND hold.expires_at<=now() THEN 'LEASE_EXPIRED'
 WHEN hold.status<>'ACTIVE' AND COALESCE(allocation.outstanding,0)<>0 THEN 'TERMINAL_ALLOCATION_REMAINS'
 WHEN hold.status='ACTIVE' AND COALESCE(allocation.outstanding,0)<>hold.max_microcredits THEN 'ALLOCATION_MISMATCH'
 ELSE 'NONE' END,
hold.max_microcredits,hold.actual_microcredits,hold.expires_at,hold.terminal_at,hold.model_id,hold.protocol_code,hold.formula_version_id,hold.pricing_snapshot,hold.request_snapshot,hold.usage_snapshot,hold.organization_id,hold.member_user_id,hold.membership_version,hold.created_at,hold.updated_at
FROM usage_holds hold LEFT JOIN (
 SELECT hold_id,sum(reserved_microcredits-settled_microcredits-released_microcredits) outstanding
 FROM usage_hold_allocations GROUP BY hold_id
) allocation ON allocation.hold_id=hold.id WHERE hold.id=$1`, holdID).Scan(&hold.ID, &hold.UsageRequestID, &hold.WalletID, &hold.PriceVersionID, &hold.Status, &hold.AnomalyState, &hold.MaxMicrocredits, &hold.ActualMicrocredits, &hold.ExpiresAt, &hold.TerminalAt, &hold.ModelID, &hold.ProtocolCode, &hold.FormulaVersionID, &hold.PricingSnapshot, &hold.RequestSnapshot, &hold.UsageSnapshot, &hold.OrganizationID, &hold.MemberUserID, &hold.MembershipVersion, &hold.CreatedAt, &hold.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminHold{}, ErrNotFound
	}
	return hold, err
}

func (r *PostgresRepository) AdminReconciliationRuns(ctx context.Context, limit int) ([]ReconciliationRun, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,provider_id,provider_version,channel,business_date,source_mode,requested_by,input_digest,status,provider_row_count,case_count,started_at,COALESCE(completed_at,started_at) FROM reconciliation_runs ORDER BY started_at DESC,id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]ReconciliationRun, 0)
	for rows.Next() {
		var item ReconciliationRun
		var businessDate time.Time
		if err := rows.Scan(&item.ID, &item.ProviderID, &item.ProviderVersion, &item.Channel, &businessDate, &item.SourceMode, &item.RequestedBy, &item.InputDigest, &item.Status, &item.ProviderRowCount, &item.CaseCount, &item.StartedAt, &item.CompletedAt); err != nil {
			return nil, err
		}
		item.BusinessDate = businessDate.Format("2006-01-02")
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminReconciliationCases(ctx context.Context, runID uuid.UUID, limit int) ([]AdminReconciliationCase, error) {
	rows, err := r.pool.Query(ctx, reconciliationCaseSelect+` WHERE reconciliation_run_id=$1 ORDER BY created_at,id LIMIT $2`, runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminReconciliationCase, 0)
	for rows.Next() {
		var item AdminReconciliationCase
		if err := scanReconciliationCase(rows, &item); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

const reconciliationCaseSelect = `SELECT id,reconciliation_run_id,payment_order_id,merchant_order_no,case_type,status,severity,owner_id,repair_action,review_result,provider_snapshot,internal_snapshot,refund_snapshot,ledger_snapshot,resolution,created_at,resolved_at,updated_at,version FROM reconciliation_cases`

func scanReconciliationCase(row pgx.Row, item *AdminReconciliationCase) error {
	return row.Scan(&item.ID, &item.RunID, &item.PaymentOrderID, &item.MerchantOrderNo, &item.CaseType, &item.Status, &item.Severity, &item.OwnerID, &item.RepairAction, &item.ReviewResult, &item.ProviderSnapshot, &item.InternalSnapshot, &item.RefundSnapshot, &item.LedgerSnapshot, &item.Resolution, &item.CreatedAt, &item.ResolvedAt, &item.UpdatedAt, &item.Version)
}

func (r *PostgresRepository) UpdateReconciliationCase(ctx context.Context, caseID uuid.UUID, command ReconciliationCaseCommand) (AdminReconciliationCase, error) {
	command.Action = strings.ToUpper(strings.TrimSpace(command.Action))
	command.Reason = strings.TrimSpace(command.Reason)
	command.IdempotencyKey = strings.TrimSpace(command.IdempotencyKey)
	command.RepairAction = strings.TrimSpace(command.RepairAction)
	command.ReviewResult = strings.TrimSpace(command.ReviewResult)
	if caseID == uuid.Nil || command.ActorID == uuid.Nil || len([]rune(command.Reason)) < 10 || len(command.IdempotencyKey) < 8 || len(command.IdempotencyKey) > 120 {
		return AdminReconciliationCase{}, ErrConflict
	}
	requestPayload, _ := json.Marshal(map[string]any{"action": command.Action, "owner_id": command.OwnerID, "repair_action": command.RepairAction, "review_result": command.ReviewResult, "reason": command.Reason})
	digest := sha256.Sum256(requestPayload)
	requestHash := fmt.Sprintf("%x", digest[:])
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return AdminReconciliationCase{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "reconciliation-case.command", caseID.String()+":"+command.IdempotencyKey); err != nil {
		return AdminReconciliationCase{}, err
	}
	var existingHash string
	err = tx.QueryRow(ctx, `SELECT request_hash FROM reconciliation_case_commands WHERE reconciliation_case_id=$1 AND idempotency_key=$2`, caseID, command.IdempotencyKey).Scan(&existingHash)
	if err == nil {
		if existingHash != requestHash {
			return AdminReconciliationCase{}, ErrConflict
		}
		var item AdminReconciliationCase
		if err := scanReconciliationCase(tx.QueryRow(ctx, reconciliationCaseSelect+` WHERE id=$1`, caseID), &item); err != nil {
			return AdminReconciliationCase{}, err
		}
		return item, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return AdminReconciliationCase{}, err
	}
	var item AdminReconciliationCase
	if err := scanReconciliationCase(tx.QueryRow(ctx, reconciliationCaseSelect+` WHERE id=$1 FOR UPDATE`, caseID), &item); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = ErrNotFound
		}
		return AdminReconciliationCase{}, err
	}
	before, _ := json.Marshal(item)
	now := r.now()
	switch command.Action {
	case "CLAIM":
		if item.Status != "OPEN" && item.Status != "IN_REVIEW" {
			return AdminReconciliationCase{}, ErrConflict
		}
		if item.OwnerID != nil && *item.OwnerID != command.ActorID {
			return AdminReconciliationCase{}, ErrConflict
		}
		item.OwnerID = &command.ActorID
		item.Status = "IN_REVIEW"
	case "ASSIGN":
		if command.OwnerID == nil || item.Status == "RESOLVED" || item.Status == "IGNORED" {
			return AdminReconciliationCase{}, ErrConflict
		}
		item.OwnerID = command.OwnerID
		item.Status = "IN_REVIEW"
	case "SET_REPAIR_ACTION":
		if command.RepairAction == "" || item.OwnerID == nil || *item.OwnerID != command.ActorID || item.Status == "RESOLVED" || item.Status == "IGNORED" {
			return AdminReconciliationCase{}, ErrConflict
		}
		item.RepairAction = &command.RepairAction
		item.Status = "IN_REVIEW"
	case "RESOLVE", "IGNORE":
		if command.ReviewResult == "" || item.OwnerID == nil || *item.OwnerID != command.ActorID || (item.Status != "OPEN" && item.Status != "IN_REVIEW") {
			return AdminReconciliationCase{}, ErrConflict
		}
		item.ReviewResult, item.Resolution = &command.ReviewResult, &command.Reason
		item.Status = map[bool]string{true: "RESOLVED", false: "IGNORED"}[command.Action == "RESOLVE"]
		item.ResolvedAt = &now
	case "REOPEN":
		if item.Status != "RESOLVED" && item.Status != "IGNORED" {
			return AdminReconciliationCase{}, ErrConflict
		}
		item.Status, item.ResolvedAt = "OPEN", nil
		item.ReviewResult, item.Resolution = nil, nil
	default:
		return AdminReconciliationCase{}, ErrConflict
	}
	item.UpdatedAt = now
	item.Version++
	_, err = tx.Exec(ctx, `UPDATE reconciliation_cases SET status=$2,severity=$3,owner_id=$4,repair_action=$5,review_result=$6,resolution=$7,resolved_at=$8,updated_at=$9,version=version+1 WHERE id=$1`, item.ID, item.Status, item.Severity, item.OwnerID, item.RepairAction, item.ReviewResult, item.Resolution, item.ResolvedAt, now)
	if err == nil {
		manualStatus := "OPEN"
		if item.Status == "RESOLVED" || item.Status == "IGNORED" {
			manualStatus = "RESOLVED"
		}
		_, err = tx.Exec(ctx, `UPDATE manual_review_cases SET status=$2,owner_id=$3,claimed_at=CASE WHEN $3::uuid IS NULL THEN claimed_at ELSE COALESCE(claimed_at,$4) END,first_response_at=CASE WHEN $3::uuid IS NULL THEN first_response_at ELSE COALESCE(first_response_at,$4) END,mitigation_at=CASE WHEN $5<>'' THEN COALESCE(mitigation_at,$4) ELSE mitigation_at END,resolved_at=$6,updated_at=$4 WHERE id=$1`, item.ID, manualStatus, item.OwnerID, now, command.RepairAction, item.ResolvedAt)
	}
	after, _ := json.Marshal(item)
	if err == nil {
		_, err = tx.Exec(ctx, `INSERT INTO reconciliation_case_commands(id,reconciliation_case_id,actor_id,idempotency_key,request_hash,action,reason,before_snapshot,after_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, uuid.Must(uuid.NewV7()), item.ID, command.ActorID, command.IdempotencyKey, requestHash, command.Action, command.Reason, before, after, now)
	}
	if err == nil {
		err = appendManualReviewEvent(ctx, tx, item.ID, command.Action, &command.ActorID, map[string]any{"reason": command.Reason, "repair_action": command.RepairAction, "review_result": command.ReviewResult}, now)
	}
	if err != nil {
		return AdminReconciliationCase{}, err
	}
	return item, tx.Commit(ctx)
}
