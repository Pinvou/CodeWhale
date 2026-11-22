package token

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"time"
)

type AdminUsageRequest struct {
	ID                          uuid.UUID       `json:"usage_request_id"`
	UserID                      uuid.UUID       `json:"user_id"`
	SessionID                   string          `json:"session_id"`
	OrganizationID              *uuid.UUID      `json:"organization_id,omitempty"`
	TokenAccountID              uuid.UUID       `json:"token_account_id"`
	CredentialID                uuid.UUID       `json:"credential_id"`
	WalletID                    uuid.UUID       `json:"wallet_id"`
	PriceVersionID              *uuid.UUID      `json:"price_version_id,omitempty"`
	HoldID                      *uuid.UUID      `json:"hold_id,omitempty"`
	Model                       string          `json:"model"`
	ModelID                     *uuid.UUID      `json:"model_id,omitempty"`
	CatalogVersion              *int64          `json:"catalog_version,omitempty"`
	CapabilityVersionID         *uuid.UUID      `json:"model_capability_version_id,omitempty"`
	ProtocolDefinitionID        *uuid.UUID      `json:"protocol_definition_id,omitempty"`
	RouteGeneration             *int64          `json:"route_generation,omitempty"`
	Status                      string          `json:"status"`
	RequestFingerprint          string          `json:"request_fingerprint"`
	ExecutionStatus             string          `json:"execution_status"`
	DeliveryStatus              string          `json:"delivery_status"`
	CancelStatus                string          `json:"cancel_status"`
	SettlementStatus            string          `json:"settlement_status"`
	MaximumMicrocredits         int64           `json:"maximum_microcredits"`
	ActualMicrocredits          *int64          `json:"actual_microcredits,omitempty"`
	ErrorClass                  *string         `json:"error_class,omitempty"`
	ProviderID                  *string         `json:"provider_id,omitempty"`
	AdapterVersion              *string         `json:"adapter_version,omitempty"`
	UpstreamRequestID           *string         `json:"upstream_request_id,omitempty"`
	AttemptStatus               *string         `json:"attempt_status,omitempty"`
	InputTokens                 *int64          `json:"input_tokens,omitempty"`
	OutputTokens                *int64          `json:"output_tokens,omitempty"`
	CachedInputTokens           *int64          `json:"cached_input_tokens,omitempty"`
	ReconciliationAttempts      int             `json:"reconciliation_attempts"`
	ReconciliationLastError     *string         `json:"reconciliation_last_error,omitempty"`
	ReconciliationStoppedAt     *time.Time      `json:"reconciliation_stopped_at,omitempty"`
	CancelRequestedAt           *time.Time      `json:"cancel_requested_at,omitempty"`
	ProviderTerminalAt          *time.Time      `json:"provider_terminal_at,omitempty"`
	FinalUsageObservedAt        *time.Time      `json:"final_usage_observed_at,omitempty"`
	LastPersistedSequence       int64           `json:"last_persisted_sequence"`
	LastClientConfirmedSequence int64           `json:"last_client_confirmed_sequence"`
	CancellationEvidence        json.RawMessage `json:"cancellation_evidence"`
	ConvergenceEvidence         json.RawMessage `json:"convergence_evidence"`
	StartedAt                   time.Time       `json:"started_at"`
	UpdatedAt                   time.Time       `json:"updated_at"`
}

// AdminUsageDetail is the Portal-safe detail projection. The full internal
// record contains user/session, credential, provider request, and request
// fingerprint fields that are not needed for evidence reconciliation.
type AdminUsageDetail struct {
	ID                      uuid.UUID  `json:"usage_request_id"`
	OrganizationID          *uuid.UUID `json:"organization_id,omitempty"`
	TokenAccountID          uuid.UUID  `json:"token_account_id"`
	PriceVersionID          *uuid.UUID `json:"price_version_id,omitempty"`
	HoldID                  *uuid.UUID `json:"hold_id,omitempty"`
	Model                   string     `json:"model"`
	ModelID                 *uuid.UUID `json:"model_id,omitempty"`
	CatalogVersion          *int64     `json:"catalog_version,omitempty"`
	CapabilityVersionID     *uuid.UUID `json:"model_capability_version_id,omitempty"`
	ProtocolDefinitionID    *uuid.UUID `json:"protocol_definition_id,omitempty"`
	RouteGeneration         *int64     `json:"route_generation,omitempty"`
	Status                  string     `json:"status"`
	ExecutionStatus         string     `json:"execution_status"`
	DeliveryStatus          string     `json:"delivery_status"`
	CancelStatus            string     `json:"cancel_status"`
	SettlementStatus        string     `json:"settlement_status"`
	MaximumMicrocredits     int64      `json:"maximum_microcredits"`
	ActualMicrocredits      *int64     `json:"actual_microcredits,omitempty"`
	ErrorClass              *string    `json:"error_class,omitempty"`
	AdapterVersion          *string    `json:"adapter_version,omitempty"`
	AttemptStatus           *string    `json:"attempt_status,omitempty"`
	InputTokens             *int64     `json:"input_tokens,omitempty"`
	OutputTokens            *int64     `json:"output_tokens,omitempty"`
	CachedInputTokens       *int64     `json:"cached_input_tokens,omitempty"`
	ReconciliationAttempts  int        `json:"reconciliation_attempts"`
	ReconciliationLastError *string    `json:"reconciliation_last_error,omitempty"`
	ReconciliationStoppedAt *time.Time `json:"reconciliation_stopped_at,omitempty"`
	LastPersistedSequence   int64      `json:"last_persisted_sequence"`
	StartedAt               time.Time  `json:"started_at"`
	UpdatedAt               time.Time  `json:"updated_at"`
}

func redactAdminUsageDetail(item AdminUsageRequest) AdminUsageDetail {
	diagnostic := stableReconciliationDiagnostic(item.ReconciliationLastError)
	return AdminUsageDetail{
		ID: item.ID, OrganizationID: item.OrganizationID, TokenAccountID: item.TokenAccountID,
		PriceVersionID: item.PriceVersionID, HoldID: item.HoldID, Model: item.Model, ModelID: item.ModelID,
		CatalogVersion: item.CatalogVersion, CapabilityVersionID: item.CapabilityVersionID,
		ProtocolDefinitionID: item.ProtocolDefinitionID, RouteGeneration: item.RouteGeneration,
		Status: item.Status, ExecutionStatus: item.ExecutionStatus, DeliveryStatus: item.DeliveryStatus,
		CancelStatus: item.CancelStatus, SettlementStatus: item.SettlementStatus,
		MaximumMicrocredits: item.MaximumMicrocredits, ActualMicrocredits: item.ActualMicrocredits,
		ErrorClass: item.ErrorClass, AdapterVersion: item.AdapterVersion, AttemptStatus: item.AttemptStatus,
		InputTokens: item.InputTokens, OutputTokens: item.OutputTokens, CachedInputTokens: item.CachedInputTokens,
		ReconciliationAttempts: item.ReconciliationAttempts, ReconciliationLastError: diagnostic,
		ReconciliationStoppedAt: item.ReconciliationStoppedAt, LastPersistedSequence: item.LastPersistedSequence,
		StartedAt: item.StartedAt, UpdatedAt: item.UpdatedAt,
	}
}

func redactAdminUsageSummary(item AdminUsageRequest) AdminUsageSummary {
	return AdminUsageSummary{ID: item.ID, OrganizationID: item.OrganizationID,
		TokenAccountID: item.TokenAccountID, Model: item.Model, ModelID: item.ModelID,
		CatalogVersion: item.CatalogVersion, ExecutionStatus: item.ExecutionStatus,
		DeliveryStatus: item.DeliveryStatus, CancelStatus: item.CancelStatus,
		SettlementStatus: item.SettlementStatus, MaximumMicrocredits: item.MaximumMicrocredits,
		ActualMicrocredits: item.ActualMicrocredits, ErrorClass: item.ErrorClass,
		ReconciliationAttempts: item.ReconciliationAttempts, ReconciliationStopped: item.ReconciliationStoppedAt,
		StartedAt: item.StartedAt, UpdatedAt: item.UpdatedAt}
}

func adminUsageQuery() string {
	return `SELECT request.id,request.user_id,request.session_id,request.organization_id,request.token_account_id,
request.credential_id,request.wallet_id,request.price_version_id,request.hold_id,COALESCE(request.model_snapshot->>'display_name',request.model),request.model_id,request.catalog_version,request.model_capability_version_id,request.protocol_definition_id,request.route_generation,request.status,request.request_fingerprint,
request.execution_status,request.delivery_status,request.cancel_status,request.settlement_status,
request.maximum_microcredits,request.actual_microcredits,request.error_class,attempt.provider_id,
attempt.adapter_version,attempt.upstream_request_id,attempt.status,attempt.input_tokens,attempt.output_tokens,attempt.cached_input_tokens,
request.reconciliation_attempts,request.reconciliation_last_error,request.reconciliation_stopped_at,
request.cancel_requested_at,request.provider_terminal_at,request.final_usage_observed_at,request.last_persisted_sequence,request.last_client_confirmed_sequence,
COALESCE((SELECT jsonb_agg(jsonb_build_object('evidence_type',evidence.evidence_type,'provider_id',evidence.provider_id,'adapter_version',evidence.adapter_version,'upstream_reference_digest',evidence.upstream_reference_digest,'evidence',evidence.sanitized_evidence,'occurred_at',evidence.occurred_at) ORDER BY evidence.occurred_at) FROM usage_cancellation_evidence evidence WHERE evidence.usage_request_id=request.id),'[]'::jsonb),
COALESCE((SELECT jsonb_agg(jsonb_build_object('evidence_type',evidence.evidence_type,'source_type',evidence.source_type,'decision',evidence.decision,'provider_id',evidence.provider_id,'adapter_version',evidence.adapter_version,'upstream_reference_digest',evidence.upstream_reference_digest,'payload_digest',evidence.payload_digest,'evidence',evidence.sanitized_snapshot,'observed_at',evidence.observed_at) ORDER BY evidence.observed_at,evidence.id) FROM usage_convergence_evidence evidence WHERE evidence.usage_request_id=request.id),'[]'::jsonb),
request.started_at,request.updated_at FROM usage_requests request
LEFT JOIN usage_attempts attempt ON attempt.usage_request_id=request.id`
}

func scanAdminUsage(row interface{ Scan(...any) error }) (AdminUsageRequest, error) {
	var item AdminUsageRequest
	err := row.Scan(&item.ID, &item.UserID, &item.SessionID, &item.OrganizationID, &item.TokenAccountID,
		&item.CredentialID, &item.WalletID, &item.PriceVersionID, &item.HoldID, &item.Model, &item.ModelID, &item.CatalogVersion, &item.CapabilityVersionID, &item.ProtocolDefinitionID, &item.RouteGeneration, &item.Status, &item.RequestFingerprint,
		&item.ExecutionStatus, &item.DeliveryStatus, &item.CancelStatus, &item.SettlementStatus,
		&item.MaximumMicrocredits, &item.ActualMicrocredits, &item.ErrorClass, &item.ProviderID,
		&item.AdapterVersion, &item.UpstreamRequestID, &item.AttemptStatus, &item.InputTokens, &item.OutputTokens, &item.CachedInputTokens,
		&item.ReconciliationAttempts, &item.ReconciliationLastError, &item.ReconciliationStoppedAt,
		&item.CancelRequestedAt, &item.ProviderTerminalAt, &item.FinalUsageObservedAt, &item.LastPersistedSequence, &item.LastClientConfirmedSequence, &item.CancellationEvidence, &item.ConvergenceEvidence, &item.StartedAt, &item.UpdatedAt)
	return item, err
}

func (r *PostgresRepository) AdminUsageRequests(ctx context.Context, limit int) ([]AdminUsageRequest, error) {
	rows, err := r.pool.Query(ctx, adminUsageQuery()+` ORDER BY request.updated_at DESC,request.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]AdminUsageRequest, 0)
	for rows.Next() {
		item, err := scanAdminUsage(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) AdminUsageRequest(ctx context.Context, usageID uuid.UUID) (AdminUsageRequest, error) {
	item, err := scanAdminUsage(r.pool.QueryRow(ctx, adminUsageQuery()+` WHERE request.id=$1`, usageID))
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminUsageRequest{}, ErrNotFound
	}
	return item, err
}
