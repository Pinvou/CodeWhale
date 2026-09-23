package billing

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound                      = errors.New("billing resource not found")
	ErrConflict                      = errors.New("billing resource conflict")
	ErrInsufficientBalance           = errors.New("wallet has insufficient available credits")
	ErrWalletUnavailable             = errors.New("wallet is unavailable")
	ErrUsageAlreadyTerminal          = errors.New("usage hold is already terminal")
	ErrUsagePlatformLossReview       = errors.New("usage arrived after entitlement expiry and requires platform-loss review")
	ErrCommerceCustomerNotReady      = errors.New("commerce customer mapping is not ready")
	ErrInsufficientComponentCapacity = errors.New("entitlement component has insufficient allocatable capacity")
	ErrIdempotencyConflict           = errors.New("idempotency key was used with a different request")
	ErrPriceMissing                  = errors.New("price is missing")
	ErrPriceAmbiguous                = errors.New("price is ambiguous")
	ErrPriceUnavailable              = errors.New("price is unavailable")
	ErrPriceWindowConflict           = errors.New("price window conflicts with a published version")
	ErrPriceETagConflict             = errors.New("price space etag conflict")
	ErrPaymentRateLimited            = errors.New("payment action rate limit reached")
)

type PaymentRateLimitError struct{ RetryAfter time.Duration }

func (e *PaymentRateLimitError) Error() string { return ErrPaymentRateLimited.Error() }
func (e *PaymentRateLimitError) Unwrap() error { return ErrPaymentRateLimited }

type Wallet struct {
	ID                     uuid.UUID `json:"wallet_id"`
	OwnerType              string    `json:"owner_type"`
	OwnerID                uuid.UUID `json:"owner_id"`
	Status                 string    `json:"status"`
	PostedMicrocredits     int64     `json:"posted_microcredits"`
	ReservedMicrocredits   int64     `json:"reserved_microcredits"`
	ReceivableMicrocredits int64     `json:"receivable_microcredits"`
	AvailableMicrocredits  int64     `json:"available_microcredits"`
	FinancialStatus        string    `json:"financial_status"`
	FinancialFreezeReason  *string   `json:"financial_freeze_reason,omitempty"`
	Version                int64     `json:"version"`
}

type Product struct {
	ProductID              string   `json:"product_id"`
	PlanName               string   `json:"plan_name"`
	SKUCode                string   `json:"sku_code,omitempty"`
	AmountFen              int64    `json:"amount_fen"`
	CreditMicrocredits     int64    `json:"credit_microcredits"`
	DisplayPoints          int64    `json:"display_points"`
	BasePoints             int64    `json:"base_points"`
	BonusPoints            int64    `json:"bonus_points"`
	ValidityDays           int      `json:"validity_days"`
	PriceStatus            string   `json:"price_status"`
	InvoiceProductName     string   `json:"invoice_product_name"`
	Currency               string   `json:"currency"`
	Channels               []string `json:"channels"`
	CatalogVersion         int64    `json:"catalog_version"`
	AvailabilityStatus     string   `json:"availability_status"`
	AvailabilityReasonCode string   `json:"availability_reason_code,omitempty"`
}

type Hold struct {
	ID                 uuid.UUID  `json:"hold_id"`
	UsageRequestID     uuid.UUID  `json:"usage_request_id"`
	WalletID           uuid.UUID  `json:"wallet_id"`
	PriceVersionID     *uuid.UUID `json:"price_version_id,omitempty"`
	Status             string     `json:"status"`
	MaxMicrocredits    int64      `json:"max_microcredits"`
	ActualMicrocredits *int64     `json:"actual_microcredits,omitempty"`
	ExpiresAt          time.Time  `json:"expires_at"`
}

type HoldPricingContext struct {
	ModelID          string          `json:"model_id"`
	ProtocolCode     string          `json:"protocol_code"`
	FormulaVersionID uuid.UUID       `json:"formula_version_id"`
	PricingSnapshot  json.RawMessage `json:"pricing_snapshot"`
	RequestSnapshot  json.RawMessage `json:"request_snapshot"`
}

type EntitlementPackage struct {
	ID                     uuid.UUID              `json:"package_id"`
	UserID                 *uuid.UUID             `json:"user_id,omitempty"`
	WalletID               uuid.UUID              `json:"wallet_id"`
	PaymentOrderID         *uuid.UUID             `json:"pinvou_order_id,omitempty"`
	PaymentTransactionID   *uuid.UUID             `json:"source_payment_transaction_id,omitempty"`
	ProviderOrderReference string                 `json:"provider_order_reference"`
	SourceType             string                 `json:"source_type"`
	SourceReference        string                 `json:"source_reference"`
	ProductID              string                 `json:"plan_id"`
	PlanName               string                 `json:"plan_name"`
	SKUCode                string                 `json:"sku_code"`
	AmountFen              int64                  `json:"amount_fen"`
	PointsTotal            int64                  `json:"points_total_microcredits"`
	PointsReserved         int64                  `json:"points_reserved_microcredits"`
	PointsUsed             int64                  `json:"points_used_microcredits"`
	PointsRemaining        int64                  `json:"points_remaining_microcredits"`
	PointsExpired          int64                  `json:"points_expired_microcredits"`
	PointsRevoked          int64                  `json:"points_revoked_microcredits"`
	CreditedAt             time.Time              `json:"credited_at"`
	ValidFrom              time.Time              `json:"valid_from"`
	ValidUntil             time.Time              `json:"valid_until"`
	Status                 string                 `json:"status"`
	Components             []EntitlementComponent `json:"components"`
}

type EntitlementComponent struct {
	ID                            uuid.UUID `json:"component_id"`
	ComponentType                 string    `json:"component_type"`
	OriginalMicrocredits          int64     `json:"original_microcredits"`
	ReclassifiedOutMicrocredits   int64     `json:"reclassified_out_microcredits"`
	EffectiveOriginalMicrocredits int64     `json:"effective_original_microcredits"`
	AvailableMicrocredits         int64     `json:"available_microcredits"`
	ReservedMicrocredits          int64     `json:"reserved_microcredits"`
	UsedMicrocredits              int64     `json:"used_microcredits"`
	ActiveGrantMicrocredits       int64     `json:"active_grant_microcredits"`
	SuspendedGrantMicrocredits    int64     `json:"suspended_grant_microcredits"`
	ExpiredMicrocredits           int64     `json:"expired_microcredits"`
	RevokedMicrocredits           int64     `json:"revoked_microcredits"`
	CreditedAt                    time.Time `json:"credited_at"`
	ExpiresAt                     time.Time `json:"expires_at"`
	Status                        string    `json:"status"`
}

type MemberQuotaGrant struct {
	ID                    uuid.UUID `json:"grant_id"`
	OrganizationID        uuid.UUID `json:"organization_id"`
	MemberUserID          uuid.UUID `json:"member_user_id"`
	PackageID             uuid.UUID `json:"package_id"`
	ComponentID           uuid.UUID `json:"component_id"`
	ComponentType         string    `json:"component_type"`
	OriginalMicrocredits  int64     `json:"original_microcredits"`
	AvailableMicrocredits int64     `json:"available_microcredits"`
	ReservedMicrocredits  int64     `json:"reserved_microcredits"`
	UsedMicrocredits      int64     `json:"used_microcredits"`
	ExpiredMicrocredits   int64     `json:"expired_microcredits"`
	RevokedMicrocredits   int64     `json:"revoked_microcredits"`
	Status                string    `json:"status"`
	SuspensionReason      *string   `json:"suspension_reason,omitempty"`
	MembershipVersion     int64     `json:"membership_version"`
	AllocationVersion     int64     `json:"allocation_version"`
	CreatedBy             uuid.UUID `json:"created_by"`
	ExpiresAt             time.Time `json:"expires_at"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

type MemberQuotaAllocation struct {
	OrganizationID     uuid.UUID `json:"organization_id"`
	MemberUserID       uuid.UUID `json:"member_user_id"`
	PackageID          uuid.UUID `json:"package_id"`
	ComponentID        uuid.UUID `json:"component_id"`
	ComponentType      string    `json:"component_type"`
	AmountMicrocredits int64     `json:"amount_microcredits"`
	MembershipVersion  int64     `json:"membership_version"`
	CreatedBy          uuid.UUID `json:"created_by"`
	IdempotencyKey     string    `json:"idempotency_key"`
}

type PriceVersion struct {
	ID                    uuid.UUID           `json:"price_version_id"`
	Model                 string              `json:"model"`
	InputPerMillion       int64               `json:"input_per_million"`
	OutputPerMillion      int64               `json:"output_per_million"`
	CachedInputPerMillion int64               `json:"cached_input_per_million"`
	ScopeType             string              `json:"scope_type"`
	ProtocolCode          string              `json:"protocol_code"`
	FormulaVersionID      uuid.UUID           `json:"formula_version_id"`
	FormulaSnapshot       json.RawMessage     `json:"formula_snapshot"`
	CreditUnit            string              `json:"credit_unit"`
	EffectiveFrom         time.Time           `json:"effective_from"`
	EffectiveTo           *time.Time          `json:"effective_to,omitempty"`
	ResolvedAt            time.Time           `json:"resolved_at"`
	Units                 []ResolvedUnitPrice `json:"units"`
}

type ResolvedUnitPrice struct {
	UnitCode             string    `json:"unit_code"`
	UnitDefinitionID     uuid.UUID `json:"unit_definition_id"`
	UnitVersion          int64     `json:"unit_version"`
	MinimumPrecision     int64     `json:"minimum_precision"`
	PriceDenominator     int64     `json:"price_denominator"`
	RoundingRule         string    `json:"rounding_rule"`
	ResolvedPrice        int64     `json:"resolved_price"`
	ConfigurationType    string    `json:"configuration_type"`
	PriceSourceScope     string    `json:"price_source_scope"`
	SourcePriceVersionID uuid.UUID `json:"source_price_version_id"`
}

type ManualAdjustment struct {
	ID                      uuid.UUID  `json:"adjustment_id"`
	WalletID                uuid.UUID  `json:"wallet_id"`
	AmountMicrocredits      int64      `json:"amount_microcredits"`
	Reason                  string     `json:"reason"`
	RequestedBy             uuid.UUID  `json:"requested_by"`
	ApprovedBy              *uuid.UUID `json:"approved_by,omitempty"`
	RejectedBy              *uuid.UUID `json:"rejected_by,omitempty"`
	Status                  string     `json:"status"`
	Version                 int64      `json:"version"`
	RequestIdempotencyKey   *string    `json:"request_idempotency_key,omitempty"`
	ApprovalRequestID       *uuid.UUID `json:"approval_request_id,omitempty"`
	ExecutionIdempotencyKey *string    `json:"execution_idempotency_key,omitempty"`
	JournalEntryID          *uuid.UUID `json:"journal_entry_id,omitempty"`
	DecisionReason          *string    `json:"decision_reason,omitempty"`
	CreatedAt               time.Time  `json:"created_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
	DecidedAt               *time.Time `json:"decided_at,omitempty"`
}

type Repository interface {
	EnsureWallet(context.Context, string, uuid.UUID) (Wallet, bool, error)
	GetWallet(context.Context, uuid.UUID) (Wallet, error)
	ListProducts(context.Context, string, string) ([]Product, error)
	CurrentPrice(context.Context, string) (PriceVersion, error)
	PriceByID(context.Context, uuid.UUID) (PriceVersion, error)
	Credit(context.Context, uuid.UUID, string, int64, bool) (Wallet, error)
	Reserve(context.Context, uuid.UUID, uuid.UUID, int64, *uuid.UUID, time.Time) (Hold, error)
	Commit(context.Context, uuid.UUID, uuid.UUID, int64, map[string]any) (Hold, error)
	Release(context.Context, uuid.UUID, uuid.UUID, string) (Hold, error)
	ExpireHolds(context.Context, int) (int, error)
	RequestAdjustment(context.Context, uuid.UUID, int64, string, uuid.UUID) (ManualAdjustment, error)
	RejectAdjustment(context.Context, uuid.UUID, uuid.UUID, string) (ManualAdjustment, error)
	ApplyOrganizationUnitLifecycle(context.Context, uuid.UUID, uuid.UUID, string, int64) (OrganizationUnitDeletionAssessment, error)
	ApplyOrganizationMemberLifecycle(context.Context, uuid.UUID, uuid.UUID, string, int64) (OrganizationMemberLifecycleState, error)
	Ping(context.Context) error
	Close()
}
