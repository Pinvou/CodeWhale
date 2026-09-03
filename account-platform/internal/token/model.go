package token

import (
	"context"
	"encoding/json"
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
	ErrSecretRotationPending         = errors.New("secret key rotation is pending")
	ErrAccountProvisioning           = errors.New("account is provisioning")
	ErrAccountRepairRequired         = errors.New("account provisioning requires repair")
	ErrAccountClosing                = errors.New("account closure is in progress")
	ErrOrganizationLifecycleConflict = errors.New("organization lifecycle conflicts with current state")
	ErrCancellationRequested         = errors.New("usage cancellation was requested before dispatch")
	ErrRateLimited                   = errors.New("model request rate or concurrency limit reached")
	ErrRouteVersionConflict          = errors.New("token route version conflict")
	ErrRouteReferenceInvalid         = errors.New("token route model or protocol reference is invalid")
)

type RateLimitError struct{ RetryAfter time.Duration }

func (e *RateLimitError) Error() string { return ErrRateLimited.Error() }
func (e *RateLimitError) Unwrap() error { return ErrRateLimited }

type UnitAuthorizationBinding struct {
	UnitID      uuid.UUID `json:"unit_id"`
	Role        string    `json:"role"`
	Permissions []string  `json:"permissions"`
}

type AuthorizationContext struct {
	UserID                    uuid.UUID
	SessionID                 string
	ContextType               string
	OrganizationID            *uuid.UUID
	MembershipVersion         int64
	ContextVersion            int64
	AuthorizationVersion      int64
	UnitIDs                   []uuid.UUID
	AncestorUnitIDs           []uuid.UUID
	Roles                     []string
	Permissions               []string
	UnitAuthorizationBindings []UnitAuthorizationBinding
	Mobile                    *string
	MobileVerified            bool
	Closing                   bool
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
	HealthExpiresAt   *time.Time
	DatabaseNow       time.Time
	CredentialID      uuid.UUID
	ProtocolCode      string
	ModelID           uuid.UUID
	ProtocolVersionID uuid.UUID
	RouteGeneration   int64
}

type RouteRequest struct {
	Model        string
	ProtocolCode string
}

type UpstreamRequestEvidence struct {
	ProviderID     string
	AdapterVersion string
	RequestID      string
}

type UsageRequest struct {
	ID                          uuid.UUID
	IdempotencyKeyDigest        string
	IdempotencyNamespace        string
	CanonicalVersion            int
	RequestFingerprint          string
	UserID                      uuid.UUID
	SessionID                   string
	OrganizationID              *uuid.UUID
	TokenAccountID              uuid.UUID
	CredentialID                uuid.UUID
	WalletID                    uuid.UUID
	PriceVersionID              *uuid.UUID
	HoldID                      *uuid.UUID
	Model                       string
	ModelID                     *uuid.UUID
	CatalogVersion              *int64
	CapabilityVersionID         *uuid.UUID
	ProtocolDefinitionID        *uuid.UUID
	RouteGeneration             *int64
	ModelSnapshot               json.RawMessage
	RequestSnapshot             json.RawMessage
	Status                      string
	ExecutionStatus             string
	ExecutionVersion            int64
	DeliveryStatus              string
	CancelStatus                string
	SettlementStatus            string
	CancelRequestedAt           *time.Time
	UserStoppedAt               *time.Time
	ProviderTerminalAt          *time.Time
	FinalUsageObservedAt        *time.Time
	LastPersistedSequence       int64
	LastClientConfirmedSequence int64
	MaximumMicrocredits         int64
	ActualMicrocredits          *int64
	ErrorClass                  *string
	StartedAt                   time.Time
	UpdatedAt                   time.Time
}

type CancellationResult struct {
	UsageRequestID       uuid.UUID  `json:"usage_request_id"`
	ExecutionStatus      string     `json:"execution_status"`
	CancelStatus         string     `json:"cancel_status"`
	DeliveryStatus       string     `json:"delivery_status"`
	SettlementStatus     string     `json:"settlement_status"`
	CancelRequestedAt    *time.Time `json:"cancel_requested_at,omitempty"`
	ProviderTerminalAt   *time.Time `json:"provider_terminal_at,omitempty"`
	FinalUsageObservedAt *time.Time `json:"final_usage_observed_at,omitempty"`
}

type DispatchEnvelope struct {
	BaseURL       string          `json:"base_url"`
	Provider      string          `json:"provider"`
	Credential    []byte          `json:"credential"`
	RequestBody   json.RawMessage `json:"request_body"`
	Price         PriceVersion    `json:"price"`
	Stream        bool            `json:"stream"`
	MaximumCost   int64           `json:"maximum_cost"`
	MaximumOutput int64           `json:"maximum_output"`
}

type ReplayEvent struct {
	Sequence  int64
	EventType string
	Payload   []byte
}

type ReplayResult struct {
	Kind         string
	HTTPStatus   int
	ContentType  string
	Payload      []byte
	FinalUsage   *Usage
	LastSequence int64
	ExpiresAt    time.Time
}

type ReplaySnapshot struct {
	Usage          UsageRequest
	Events         []ReplayEvent
	Result         *ReplayResult
	ResultExpired  bool
	TombstoneUntil *time.Time
}

type UsageHistoryCursor struct {
	StartedAt time.Time
	ID        uuid.UUID
}

type UserUsageRecord struct {
	UsageRequestID     uuid.UUID  `json:"usage_request_id"`
	LogicalTurnID      string     `json:"logical_turn_id,omitempty"`
	Model              string     `json:"model"`
	ModelID            *uuid.UUID `json:"model_id,omitempty"`
	CatalogVersion     *int64     `json:"catalog_version,omitempty"`
	DisplayStatus      string     `json:"display_status"`
	ExecutionStatus    string     `json:"execution_status"`
	DeliveryStatus     string     `json:"delivery_status"`
	CancelStatus       string     `json:"cancel_status"`
	SettlementStatus   string     `json:"settlement_status"`
	ActualMicrocredits *int64     `json:"actual_microcredits"`
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
	FindUsageByIdempotency(context.Context, uuid.UUID, string) (UsageRequest, error)
	BeginUsage(context.Context, UsageRequest, map[string]any) (UsageRequest, bool, error)
	PrepareDispatch(context.Context, uuid.UUID, DispatchEnvelope) error
	LoadDispatchPreparation(context.Context, uuid.UUID) (DispatchEnvelope, error)
	MarkHoldReserving(context.Context, uuid.UUID) error
	MarkReserved(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error
	CompleteHoldAndDispatch(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error
	PersistDispatchIntent(context.Context, uuid.UUID, DispatchEnvelope) error
	AcquireDispatch(context.Context, uuid.UUID, string, time.Duration) (DispatchEnvelope, bool, error)
	MarkUpstream(context.Context, uuid.UUID) error
	MarkStreaming(context.Context, uuid.UUID) error
	MarkResponseReceived(context.Context, uuid.UUID) error
	RecordUpstreamRequest(context.Context, uuid.UUID, UpstreamRequestEvidence) error
	AppendReplayEvent(context.Context, uuid.UUID, string, []byte) (int64, error)
	SaveReplayResult(context.Context, uuid.UUID, ReplayResult) error
	LoadReplay(context.Context, uuid.UUID, int64) (ReplaySnapshot, error)
	MarkDelivery(context.Context, uuid.UUID, string) error
	RequestCancellation(context.Context, uuid.UUID, uuid.UUID, string, string, string) (CancellationResult, bool, error)
	ConfirmPreDispatchCancellation(context.Context, uuid.UUID) (CancellationResult, error)
	RecordDeliveryConfirmation(context.Context, uuid.UUID, string, int64) error
	CleanupReplayRetention(context.Context) error
	RecoverUncertainDispatches(context.Context) error
	MarkCommitted(context.Context, uuid.UUID, int64, map[string]any) error
	MarkReleased(context.Context, uuid.UUID, string) error
	MarkReview(context.Context, uuid.UUID, string, map[string]any) error
}
