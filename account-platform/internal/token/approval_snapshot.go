package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
)

type ApprovalSnapshotRequest struct {
	CommandType        string          `json:"command_type"`
	TargetResourceType string          `json:"target_resource_type"`
	TargetResourceID   string          `json:"target_resource_id"`
	NormalizedCommand  json.RawMessage `json:"normalized_command"`
}

type ApprovalSnapshotProjection struct {
	CommandType              string          `json:"command_type"`
	TargetResourceType       string          `json:"target_resource_type"`
	TargetResourceID         string          `json:"target_resource_id"`
	CriticalResourceVersions json.RawMessage `json:"critical_resource_versions"`
	FinancialSnapshot        json.RawMessage `json:"financial_snapshot"`
	PrecheckFingerprint      string          `json:"precheck_fingerprint"`
	TokenObservedAt          time.Time       `json:"token_observed_at"`
	BillingObservedAt        *time.Time      `json:"billing_observed_at,omitempty"`
}

func (s *Service) internalApprovalCommandPrecheck(w http.ResponseWriter, r *http.Request) {
	var request ApprovalSnapshotRequest
	if api.DecodeJSON(w, r, &request) != nil {
		return
	}
	projection, err := s.approvalCommandPrecheck(r.Context(), request)
	if err != nil {
		status, code := http.StatusConflict, "APPROVAL_SNAPSHOT_CHANGED"
		if errors.Is(err, ErrNotFound) {
			status, code = http.StatusNotFound, "RESOURCE_NOT_FOUND"
		} else if errors.Is(err, ErrTokenCommandInvalid) || errors.Is(err, ErrForbidden) {
			status, code = http.StatusBadRequest, "INVALID_APPROVAL_SNAPSHOT_REQUEST"
		} else if !errors.Is(err, ErrCatalogVersionConflict) {
			status, code = http.StatusServiceUnavailable, "APPROVAL_SNAPSHOT_UNAVAILABLE"
		}
		api.WriteError(w, status, code, "the authoritative approval snapshot could not be established", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, projection)
}

func (s *Service) approvalCommandPrecheck(ctx context.Context, request ApprovalSnapshotRequest) (ApprovalSnapshotProjection, error) {
	request.CommandType = strings.ToUpper(strings.TrimSpace(request.CommandType))
	request.TargetResourceType = strings.ToUpper(strings.TrimSpace(request.TargetResourceType))
	request.TargetResourceID = strings.TrimSpace(request.TargetResourceID)
	repository, ok := s.impactPrechecks.(ImpactPrecheckRepository)
	if !ok || !s.impactPrechecksEnabled {
		return ApprovalSnapshotProjection{}, errors.New("approval impact repository unavailable")
	}
	result := ImpactPrecheckResult{Blockers: []string{}}
	switch request.CommandType {
	case "TOKEN_MODEL_LIFECYCLE_CHANGE":
		if request.TargetResourceType != "MODEL" {
			return ApprovalSnapshotProjection{}, ErrTokenCommandInvalid
		}
		modelID, err := uuid.Parse(request.TargetResourceID)
		if err != nil || modelID == uuid.Nil {
			return ApprovalSnapshotProjection{}, ErrTokenCommandInvalid
		}
		var command struct {
			ModelID                 uuid.UUID  `json:"model_id"`
			TargetLifecycle         string     `json:"target_lifecycle"`
			EffectiveAt             time.Time  `json:"effective_at"`
			ExistingSessionCutoffAt *time.Time `json:"existing_session_cutoff_at"`
			ReplacementModelID      *uuid.UUID `json:"replacement_model_id"`
		}
		if decodeCommand(request.NormalizedCommand, &command) != nil || command.ModelID != modelID || command.EffectiveAt.IsZero() {
			return ApprovalSnapshotProjection{}, ErrTokenCommandInvalid
		}
		command.TargetLifecycle = strings.ToUpper(strings.TrimSpace(command.TargetLifecycle))
		if command.TargetLifecycle != "DEPRECATED" && command.TargetLifecycle != "REMOVED" || command.ReplacementModelID != nil && *command.ReplacementModelID == modelID {
			return ApprovalSnapshotProjection{}, ErrTokenCommandInvalid
		}
		evidence, err := repository.ModelLifecycleImpact(ctx, modelID, command.ReplacementModelID)
		if err != nil {
			return ApprovalSnapshotProjection{}, err
		}
		price, err := s.loadImpactPriceCoverage(ctx, evidence.PriceDimensions)
		if err != nil {
			return ApprovalSnapshotProjection{}, err
		}
		catalogVersion := int64(0)
		if evidence.CurrentVersion.CatalogVersion != nil {
			catalogVersion = *evidence.CurrentVersion.CatalogVersion
		}
		result = ImpactPrecheckResult{Action: "MODEL_LIFECYCLE_" + command.TargetLifecycle, Evidence: evidence, PriceCoverage: &price, Blockers: []string{}, CriticalResourceVersions: map[string]any{
			"model_id": modelID, "catalog_version": catalogVersion, "current_lifecycle": evidence.CurrentStatus,
			"target_lifecycle": command.TargetLifecycle, "lifecycle_plan_version": int64(0), "effective_at": command.EffectiveAt.UTC(),
			"existing_session_cutoff_at": command.ExistingSessionCutoffAt, "replacement_model_id": command.ReplacementModelID,
			"billing_price_version": price.FinancialFactRevision, "route_set_fingerprint": evidence.RouteSetFingerprint,
			"provisioning_set_fingerprint": evidence.ProvisioningSetFingerprint, "inflight_request_set_fingerprint": evidence.InflightSetFingerprint, "policy_version": int64(2),
		}, FinancialSnapshot: financialImpactSnapshot(price)}
		if !price.FullyCovered {
			result.Blockers = append(result.Blockers, "PRICE_COVERAGE_MISSING")
		}
		if command.ReplacementModelID != nil && (evidence.ReplacementReady == nil || !*evidence.ReplacementReady) {
			result.Blockers = append(result.Blockers, "REPLACEMENT_MODEL_UNAVAILABLE")
		}
		if command.TargetLifecycle == "REMOVED" && evidence.Dependencies.InflightRequests > 0 {
			result.Blockers = append(result.Blockers, "INFLIGHT_REQUESTS_PRESENT")
		}
		finalizeImpactResult(&result, "effective_at="+command.EffectiveAt.UTC().Format(time.RFC3339Nano), "replacement_model_id="+valueUUID(command.ReplacementModelID))
	case "TOKEN_MODEL_CATALOG_ROLLBACK":
		if request.TargetResourceType != "MODEL_CATALOG" {
			return ApprovalSnapshotProjection{}, ErrTokenCommandInvalid
		}
		var command struct {
			SourceCatalogVersion int64 `json:"source_catalog_version"`
		}
		if decodeCommand(request.NormalizedCommand, &command) != nil || command.SourceCatalogVersion < 1 || request.TargetResourceID != strconv.FormatInt(command.SourceCatalogVersion, 10) {
			return ApprovalSnapshotProjection{}, ErrTokenCommandInvalid
		}
		evidence, err := repository.CatalogRollbackImpact(ctx, command.SourceCatalogVersion)
		if err != nil {
			return ApprovalSnapshotProjection{}, err
		}
		price, err := s.loadImpactPriceCoverage(ctx, evidence.PriceDimensions)
		if err != nil {
			return ApprovalSnapshotProjection{}, err
		}
		catalogVersion := int64(0)
		if evidence.CurrentVersion.CatalogVersion != nil {
			catalogVersion = *evidence.CurrentVersion.CatalogVersion
		}
		result = ImpactPrecheckResult{Action: "MODEL_CATALOG_ROLLBACK", Evidence: evidence, PriceCoverage: &price, Blockers: []string{}, CriticalResourceVersions: map[string]any{
			"current_catalog_version": catalogVersion, "source_catalog_version": command.SourceCatalogVersion,
			"catalog_diff_fingerprint": evidence.CatalogDiffFingerprint, "billing_price_version": price.FinancialFactRevision,
			"route_set_fingerprint": evidence.RouteSetFingerprint, "provisioning_set_fingerprint": evidence.ProvisioningSetFingerprint,
			"inflight_request_set_fingerprint": evidence.InflightSetFingerprint, "policy_version": int64(2),
		}, FinancialSnapshot: financialImpactSnapshot(price)}
		if !price.FullyCovered {
			result.Blockers = append(result.Blockers, "PRICE_COVERAGE_MISSING")
		}
		if evidence.Dependencies.InflightRequests > 0 {
			result.Blockers = append(result.Blockers, "INFLIGHT_REQUESTS_PRESENT")
		}
		finalizeImpactResult(&result, "source_catalog_version="+strings.TrimSpace(request.TargetResourceID))
	case "TOKEN_CREDENTIAL_REVOKE":
		if request.TargetResourceType != "PROVIDER_CREDENTIAL" {
			return ApprovalSnapshotProjection{}, ErrTokenCommandInvalid
		}
		credentialID, err := uuid.Parse(request.TargetResourceID)
		if err != nil || credentialID == uuid.Nil {
			return ApprovalSnapshotProjection{}, ErrTokenCommandInvalid
		}
		var command struct {
			TokenAccountID         uuid.UUID `json:"token_account_id"`
			ExpectedAccountVersion int64     `json:"expected_account_version"`
			CredentialID           uuid.UUID `json:"credential_id"`
			CredentialVersion      int       `json:"credential_version"`
		}
		if decodeCommand(request.NormalizedCommand, &command) != nil || command.TokenAccountID == uuid.Nil || command.ExpectedAccountVersion < 1 || command.CredentialID != credentialID || command.CredentialVersion < 1 {
			return ApprovalSnapshotProjection{}, ErrTokenCommandInvalid
		}
		evidence, err := repository.TokenCredentialImpact(ctx, command.TokenAccountID, &credentialID)
		if err != nil {
			return ApprovalSnapshotProjection{}, err
		}
		result = credentialRevokeImpactResult(command.TokenAccountID, credentialID, evidence)
		if evidence.CurrentVersion.AccountVersion == nil || *evidence.CurrentVersion.AccountVersion != command.ExpectedAccountVersion || evidence.CurrentVersion.CredentialVersion == nil || *evidence.CurrentVersion.CredentialVersion != command.CredentialVersion {
			result.Blockers = append(result.Blockers, "RESOURCE_VERSION_CHANGED")
		}
	default:
		return ApprovalSnapshotProjection{}, ErrTokenCommandInvalid
	}
	if len(result.Blockers) > 0 || request.CommandType != "TOKEN_CREDENTIAL_REVOKE" && result.BillingObservedAt == nil || !validImpactFingerprint(result.PrecheckFingerprint) {
		return ApprovalSnapshotProjection{}, ErrCatalogVersionConflict
	}
	critical, err := json.Marshal(result.CriticalResourceVersions)
	if err != nil {
		return ApprovalSnapshotProjection{}, err
	}
	financial, err := json.Marshal(result.FinancialSnapshot)
	if err != nil {
		return ApprovalSnapshotProjection{}, err
	}
	return ApprovalSnapshotProjection{CommandType: request.CommandType, TargetResourceType: request.TargetResourceType, TargetResourceID: request.TargetResourceID, CriticalResourceVersions: critical, FinancialSnapshot: financial, PrecheckFingerprint: result.PrecheckFingerprint, TokenObservedAt: result.TokenObservedAt, BillingObservedAt: result.BillingObservedAt}, nil
}
