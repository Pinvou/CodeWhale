package token

import (
	"encoding/json"

	"time"

	"github.com/google/uuid"
)

type Candidate struct {
	ManagementMode    string
	ProviderID        string
	CredentialVersion int
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
	UsageSnapshot               json.RawMessage
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
