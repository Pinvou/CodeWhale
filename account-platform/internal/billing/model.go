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
	AmountFen          int64    `json:"amount_fen"`
	CreditMicrocredits int64    `json:"credit_microcredits"`
	DisplayPoints      int64    `json:"display_points"`
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
