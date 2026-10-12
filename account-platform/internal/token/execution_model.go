package token

import (
	"context"

	"time"

	"github.com/google/uuid"
)

type UpstreamRequestEvidence struct {
	ProviderID     string
	AdapterVersion string
	RequestID      string
}

type ReplayEvent struct {
	Sequence  int64
	EventType string
	Payload   []byte
}

type ReplayResult struct {
	Kind                string
	HTTPStatus          int
	ContentType         string
	Payload             []byte
	ResponseReceived    bool
	ResponseDisposition string
	FinalUsage          *Usage
	ActualMicrocredits  *int64
	SettlementSnapshot  map[string]any
	LastSequence        int64
	ExpiresAt           time.Time
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

type UserUsageTurnRecord struct {
	UsageRequestID       uuid.UUID  `json:"usage_request_id"`
	LogicalTurnID        string     `json:"logical_turn_id,omitempty"`
	UsageRequestCount    int64      `json:"usage_request_count"`
	CancelUsageRequestID *uuid.UUID `json:"cancel_usage_request_id,omitempty"`
	Model                string     `json:"model"`
	DisplayStatus        string     `json:"display_status"`
	ExecutionStatus      string     `json:"execution_status"`
	DeliveryStatus       string     `json:"delivery_status"`
	CancelStatus         string     `json:"cancel_status"`
	SettlementStatus     string     `json:"settlement_status"`
	ActualMicrocredits   *int64     `json:"actual_microcredits"`
	ChargedMicrocredits  *int64     `json:"charged_microcredits"`
	InputTokens          *int64     `json:"input_tokens"`
	OutputTokens         *int64     `json:"output_tokens"`
	CachedInputTokens    *int64     `json:"cached_input_tokens"`
	StartedAt            time.Time  `json:"started_at"`
	CompletedAt          *time.Time `json:"completed_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	cursorStartedAt      time.Time
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
	RecordUpstreamRequest(context.Context, uuid.UUID, UpstreamRequestEvidence) error
	AppendReplayEvent(context.Context, uuid.UUID, string, []byte) (int64, error)
	SaveReplayResult(context.Context, uuid.UUID, ReplayResult) error
	RecordFinalUsage(context.Context, uuid.UUID, string, string, Usage) error
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
