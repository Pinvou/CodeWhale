package billing

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound             = errors.New("billing resource not found")
	ErrConflict             = errors.New("billing resource conflict")
	ErrInsufficientBalance  = errors.New("wallet has insufficient available credits")
	ErrWalletUnavailable    = errors.New("wallet is unavailable")
	ErrUsageAlreadyTerminal = errors.New("usage hold is already terminal")
)

type Wallet struct {
	ID                    uuid.UUID `json:"wallet_id"`
	OwnerType             string    `json:"owner_type"`
	OwnerID               uuid.UUID `json:"owner_id"`
	Status                string    `json:"status"`
	PostedMicrocredits    int64     `json:"posted_microcredits"`
	ReservedMicrocredits  int64     `json:"reserved_microcredits"`
	AvailableMicrocredits int64     `json:"available_microcredits"`
	Version               int64     `json:"version"`
}

type Product struct {
	ProductID          string   `json:"product_id"`
	PlanName           string   `json:"plan_name"`
	SKUCode            string   `json:"sku_code"`
	AmountFen          int64    `json:"amount_fen"`
	CreditMicrocredits int64    `json:"credit_microcredits"`
	DisplayPoints      int64    `json:"display_points"`
	BasePoints         int64    `json:"base_points"`
	BonusPoints        int64    `json:"bonus_points"`
	ValidityDays       int      `json:"validity_days"`
	PriceStatus        string   `json:"price_status"`
	InvoiceProductName string   `json:"invoice_product_name"`
	Currency           string   `json:"currency"`
	Channels           []string `json:"channels"`
	CatalogVersion     int64    `json:"catalog_version"`
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

type EntitlementPackage struct {
	ID              uuid.UUID `json:"package_id"`
	UserID          uuid.UUID `json:"user_id"`
	WalletID        uuid.UUID `json:"wallet_id"`
	PaymentOrderID  uuid.UUID `json:"pinvou_order_id"`
	MallOrderID     string    `json:"mall_order_id"`
	ProductID       string    `json:"plan_id"`
	PlanName        string    `json:"plan_name"`
	SKUCode         string    `json:"sku_code"`
	AmountFen       int64     `json:"amount_fen,string"`
	PointsTotal     int64     `json:"points_total_microcredits,string"`
	PointsReserved  int64     `json:"points_reserved_microcredits,string"`
	PointsUsed      int64     `json:"points_used_microcredits,string"`
	PointsRemaining int64     `json:"points_remaining_microcredits,string"`
	CreditedAt      time.Time `json:"credited_at"`
	ValidFrom       time.Time `json:"valid_from"`
	ValidUntil      time.Time `json:"valid_until"`
	Status          string    `json:"status"`
}

type PriceVersion struct {
	ID                    uuid.UUID `json:"price_version_id"`
	Model                 string    `json:"model"`
	InputPerMillion       int64     `json:"input_per_million"`
	OutputPerMillion      int64     `json:"output_per_million"`
	CachedInputPerMillion int64     `json:"cached_input_per_million"`
	EffectiveFrom         time.Time `json:"effective_from"`
}

type ManualAdjustment struct {
	ID                 uuid.UUID  `json:"adjustment_id"`
	WalletID           uuid.UUID  `json:"wallet_id"`
	AmountMicrocredits int64      `json:"amount_microcredits"`
	Reason             string     `json:"reason"`
	RequestedBy        uuid.UUID  `json:"requested_by"`
	ApprovedBy         *uuid.UUID `json:"approved_by,omitempty"`
	RejectedBy         *uuid.UUID `json:"rejected_by,omitempty"`
	Status             string     `json:"status"`
	JournalEntryID     *uuid.UUID `json:"journal_entry_id,omitempty"`
	DecisionReason     *string    `json:"decision_reason,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	DecidedAt          *time.Time `json:"decided_at,omitempty"`
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
	ApproveAdjustment(context.Context, uuid.UUID, uuid.UUID, string) (ManualAdjustment, error)
	RejectAdjustment(context.Context, uuid.UUID, uuid.UUID, string) (ManualAdjustment, error)
	Ping(context.Context) error
	Close()
}
