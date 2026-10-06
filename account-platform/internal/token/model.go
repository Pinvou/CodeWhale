package token

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound                      = errors.New("token account not found")
	ErrForbidden                     = errors.New("token account is not authorized")
	ErrAmbiguousRoute                = errors.New("token route is ambiguous")
	ErrNoEligibleRoute               = errors.New("no eligible token route")
	ErrInvalidSecret                 = errors.New("invalid token secret configuration")
	ErrAccountProvisioning           = errors.New("account is provisioning")
	ErrAccountRepairRequired         = errors.New("account provisioning requires repair")
	ErrOrganizationLifecycleConflict = errors.New("organization lifecycle conflicts with current state")
)

type AuthorizationContext struct {
	UserID               uuid.UUID
	SessionID            string
	ContextType          string
	OrganizationID       *uuid.UUID
	MembershipVersion    int64
	MemberVersion        int64
	ContextVersion       int64
	AuthorizationVersion int64
	UnitIDs              []uuid.UUID
	AncestorUnitIDs      []uuid.UUID
	Roles                []string
	Permissions          []string
	Mobile               *string
	MobileVerified       bool
}

type Candidate struct {
	TokenAccountID    uuid.UUID
	Name              string
	OwnerType         string
	OwnerID           uuid.UUID
	OwnerOrganization *uuid.UUID
	OwnerUnit         *uuid.UUID
	WalletID          uuid.UUID
	Status            string
	ShareScope        string
	AllowedModels     []string
	ExplicitMembers   []uuid.UUID
	DefaultScope      string
	Priority          int
	HealthStatus      string
	CredentialID      uuid.UUID
}

type RouteRequest struct {
	Model                  string
	ExplicitTokenAccountID *uuid.UUID
}

type UsageRequest struct {
	ID                  uuid.UUID
	IdempotencyKey      string
	UserID              uuid.UUID
	SessionID           string
	OrganizationID      *uuid.UUID
	TokenAccountID      uuid.UUID
	CredentialID        uuid.UUID
	WalletID            uuid.UUID
	PriceVersionID      *uuid.UUID
	HoldID              *uuid.UUID
	Model               string
	Status              string
	MaximumMicrocredits int64
	ActualMicrocredits  *int64
	ErrorClass          *string
	StartedAt           time.Time
	UpdatedAt           time.Time
}

type UsageHistoryCursor struct {
	StartedAt time.Time
	ID        uuid.UUID
}

type UserUsageRecord struct {
	UsageRequestID     uuid.UUID  `json:"usage_request_id"`
	Model              string     `json:"model"`
	DisplayStatus      string     `json:"display_status"`
	ActualMicrocredits *int64     `json:"actual_microcredits,string"`
	InputTokens        *int64     `json:"input_tokens"`
	OutputTokens       *int64     `json:"output_tokens"`
	CachedInputTokens  *int64     `json:"cached_input_tokens"`
	StartedAt          time.Time  `json:"started_at"`
	CompletedAt        *time.Time `json:"completed_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	status             string
}

type UsageStore interface {
	GetUsage(context.Context, uuid.UUID) (UsageRequest, error)
	BeginUsage(context.Context, UsageRequest, map[string]any) (UsageRequest, bool, error)
	MarkReserved(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error
	MarkUpstream(context.Context, uuid.UUID) error
	RecordUpstreamRequest(context.Context, uuid.UUID, string) error
	MarkCommitted(context.Context, uuid.UUID, int64, map[string]any) error
	MarkReleased(context.Context, uuid.UUID, string) error
	MarkReview(context.Context, uuid.UUID, string, map[string]any) error
}
