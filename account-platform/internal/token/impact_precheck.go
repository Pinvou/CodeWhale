package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/requestdeadline"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrImpactScopeTooLarge      = errors.New("impact precheck scope is too large")
	ErrImpactPricingUnavailable = errors.New("impact pricing snapshot is unavailable")
)

const maximumImpactModels = 5000

type ImpactDependencySummary struct {
	Routes                 int64 `json:"routes"`
	HealthyRoutes          int64 `json:"healthy_routes"`
	Provisionings          int64 `json:"provisionings"`
	ReadyProvisionings     int64 `json:"ready_provisionings"`
	ActiveLeases           int64 `json:"active_leases"`
	InflightRequests       int64 `json:"inflight_requests"`
	ActiveSessions         int64 `json:"active_sessions"`
	AlternativeCredentials int64 `json:"alternative_credentials"`
	AlternativeAccounts    int64 `json:"alternative_accounts"`
	AffectedCoverageTuples int64 `json:"affected_coverage_tuples"`
	RemainingReadyCoverage int64 `json:"remaining_ready_coverage"`
	CoverageGaps           int64 `json:"coverage_gaps"`
}

type ImpactResourceVersion struct {
	CatalogVersion       *int64     `json:"catalog_version,omitempty"`
	CapabilityVersionID  *uuid.UUID `json:"model_capability_version_id,omitempty"`
	ProtocolDefinitionID *uuid.UUID `json:"protocol_definition_id,omitempty"`
	AccountUpdatedAt     *time.Time `json:"account_updated_at,omitempty"`
	RouteGeneration      *int64     `json:"route_generation,omitempty"`
	RouteVersion         *int64     `json:"route_version,omitempty"`
	AccountVersion       *int64     `json:"account_version,omitempty"`
	CredentialVersion    *int       `json:"credential_version,omitempty"`
	EvidenceETag         string     `json:"evidence_etag"`
	EvidenceSetSHA256    string     `json:"evidence_set_sha256"`
}

type ImpactLocalEvidence struct {
	ResourceType                 string                  `json:"resource_type"`
	ResourceID                   string                  `json:"resource_id"`
	CurrentStatus                string                  `json:"current_status"`
	CurrentVersion               ImpactResourceVersion   `json:"current_version"`
	Dependencies                 ImpactDependencySummary `json:"dependencies"`
	ModelIDs                     []uuid.UUID             `json:"-"`
	PriceDimensions              []ImpactPriceQuery      `json:"-"`
	ReplacementModelID           *uuid.UUID              `json:"replacement_model_id,omitempty"`
	ReplacementReady             *bool                   `json:"replacement_model_ready,omitempty"`
	ObservedAt                   time.Time               `json:"-"`
	RouteSetFingerprint          string                  `json:"-"`
	ProvisioningSetFingerprint   string                  `json:"-"`
	InflightSetFingerprint       string                  `json:"-"`
	CatalogDiffFingerprint       string                  `json:"-"`
	CredentialFingerprint        string                  `json:"-"`
	ReplacementCredentialVersion *int                    `json:"-"`
}

type ImpactPriceSummary struct {
	ModelsChecked         int                   `json:"models_checked"`
	ModelsCovered         int                   `json:"models_covered"`
	FullyCovered          bool                  `json:"fully_covered"`
	Versions              []ImpactPriceEvidence `json:"versions"`
	BillingObservedAt     time.Time             `json:"billing_observed_at"`
	FinancialFactRevision int64                 `json:"financial_fact_revision"`
	PriceSetFingerprint   string                `json:"price_set_fingerprint"`
}

type ImpactPriceEvidence struct {
	ModelID          uuid.UUID  `json:"model_id"`
	ProtocolCode     string     `json:"protocol_code"`
	FormulaVersionID *uuid.UUID `json:"formula_version_id,omitempty"`
	CreditUnit       string     `json:"credit_unit"`
	Covered          bool       `json:"covered"`
	PriceVersionID   *uuid.UUID `json:"price_version_id,omitempty"`
	PriceVersion     *int64     `json:"price_version,omitempty"`
	EffectiveFrom    *time.Time `json:"effective_from,omitempty"`
	EffectiveTo      *time.Time `json:"effective_to,omitempty"`
}

type ImpactPrecheckResult struct {
	Action                   string              `json:"action"`
	Evidence                 ImpactLocalEvidence `json:"evidence"`
	PriceCoverage            *ImpactPriceSummary `json:"price_coverage,omitempty"`
	Blockers                 []string            `json:"blockers"`
	TokenObservedAt          time.Time           `json:"token_observed_at"`
	BillingObservedAt        *time.Time          `json:"billing_observed_at,omitempty"`
	SnapshotSemantics        string              `json:"snapshot_semantics"`
	PrecheckFingerprint      string              `json:"precheck_fingerprint"`
	RequiresCASRevalidation  bool                `json:"requires_cas_revalidation"`
	CriticalResourceVersions map[string]any      `json:"critical_resource_versions,omitempty"`
	FinancialSnapshot        map[string]any      `json:"financial_snapshot,omitempty"`
}

type ImpactPrecheckRepository interface {
	ModelLifecycleImpact(context.Context, uuid.UUID, *uuid.UUID) (ImpactLocalEvidence, error)
	CatalogRollbackImpact(context.Context, int64) (ImpactLocalEvidence, error)
	TokenAccountImpact(context.Context, uuid.UUID) (ImpactLocalEvidence, error)
	TokenRouteImpact(context.Context, uuid.UUID, string, string, string) (ImpactLocalEvidence, error)
	TokenCredentialImpact(context.Context, uuid.UUID, *uuid.UUID) (ImpactLocalEvidence, error)
}

type StableRouteImpactInput struct {
	Action            string
	RouteID           uuid.UUID
	ModelID           uuid.UUID
	ProtocolVersionID uuid.UUID
}

func (s *Service) WithImpactPrechecks(enabled bool, repository ImpactPrecheckRepository, billing ImpactPriceCoverageClient) *Service {
	s.impactPrechecksEnabled = enabled
	s.impactPrechecks = repository
	s.impactPricing = billing
	return s
}

func (s *Service) impactPrecheckRepository(w http.ResponseWriter, r *http.Request) (ImpactPrecheckRepository, bool) {
	if !s.impactPrechecksEnabled {
		api.WriteError(w, http.StatusServiceUnavailable, "IMPACT_PRECHECK_DISABLED", "impact prechecks are not enabled", httpserver.RequestID(r.Context()))
		return nil, false
	}
	repository := s.impactPrechecks
	if repository == nil {
		repository, _ = s.repository.(ImpactPrecheckRepository)
	}
	if repository == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "PRECHECK_UNAVAILABLE", "impact precheck is unavailable", httpserver.RequestID(r.Context()))
		return nil, false
	}
	return repository, true
}

func (s *Service) adminModelLifecycleImpact(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.impactPrecheckRepository(w, r)
	if !ok {
		return
	}
	modelID, ok := adminUUID(w, r, "model_id")
	if !ok {
		return
	}
	target := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("target_status")))
	if target != "PUBLISHED" && target != "DEPRECATED" && target != "REMOVED" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "target_status is invalid", httpserver.RequestID(r.Context()))
		return
	}
	var replacement *uuid.UUID
	if raw := strings.TrimSpace(r.URL.Query().Get("replacement_model_id")); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil || parsed == modelID {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "replacement_model_id is invalid", httpserver.RequestID(r.Context()))
			return
		}
		replacement = &parsed
	}
	evidence, err := repository.ModelLifecycleImpact(r.Context(), modelID, replacement)
	if err != nil {
		s.writeImpactError(w, r, err)
		return
	}
	result := ImpactPrecheckResult{Action: "MODEL_LIFECYCLE_" + target, Evidence: evidence, Blockers: []string{}}
	price, ok := s.impactPriceCoverage(w, r, evidence.PriceDimensions)
	if !ok {
		return
	}
	result.PriceCoverage = &price
	if !price.FullyCovered {
		result.Blockers = append(result.Blockers, "PRICE_COVERAGE_MISSING")
	}
	if replacement != nil && (evidence.ReplacementReady == nil || !*evidence.ReplacementReady) {
		result.Blockers = append(result.Blockers, "REPLACEMENT_MODEL_UNAVAILABLE")
	}
	if target == "REMOVED" && evidence.Dependencies.InflightRequests > 0 {
		result.Blockers = append(result.Blockers, "INFLIGHT_REQUESTS_PRESENT")
	}
	replacementValue := any(nil)
	if replacement != nil {
		replacementValue = replacement.String()
	}
	catalogVersion := int64(0)
	if evidence.CurrentVersion.CatalogVersion != nil {
		catalogVersion = *evidence.CurrentVersion.CatalogVersion
	}
	if catalogVersion < 1 || price.FinancialFactRevision < 1 || !validImpactFingerprint(price.PriceSetFingerprint) || !validImpactFingerprint(evidence.RouteSetFingerprint) || !validImpactFingerprint(evidence.ProvisioningSetFingerprint) || !validImpactFingerprint(evidence.InflightSetFingerprint) {
		result.Blockers = append(result.Blockers, "APPROVAL_SNAPSHOT_UNAVAILABLE")
	}
	result.CriticalResourceVersions = map[string]any{
		"model_id": modelID.String(), "catalog_version": catalogVersion,
		"current_lifecycle": evidence.CurrentStatus, "target_lifecycle": target, "lifecycle_plan_version": int64(0),
		"effective_at": evidence.ObservedAt.UTC(), "existing_session_cutoff_at": nil, "replacement_model_id": replacementValue,
		"billing_price_version": price.FinancialFactRevision, "route_set_fingerprint": evidence.RouteSetFingerprint,
		"provisioning_set_fingerprint": evidence.ProvisioningSetFingerprint, "inflight_request_set_fingerprint": evidence.InflightSetFingerprint,
		"policy_version": int64(2),
	}
	result.FinancialSnapshot = financialImpactSnapshot(price)
	finalizeImpactResult(&result, "target_status="+target, "replacement_model_id="+valueUUID(replacement))
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminCatalogRollbackImpact(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.impactPrecheckRepository(w, r)
	if !ok {
		return
	}
	version, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("source_catalog_version")), 10, 64)
	if err != nil || version <= 0 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "source_catalog_version is invalid", httpserver.RequestID(r.Context()))
		return
	}
	evidence, err := repository.CatalogRollbackImpact(r.Context(), version)
	if err != nil {
		s.writeImpactError(w, r, err)
		return
	}
	price, ok := s.impactPriceCoverage(w, r, evidence.PriceDimensions)
	if !ok {
		return
	}
	blockers := []string{}
	if !price.FullyCovered {
		blockers = append(blockers, "PRICE_COVERAGE_MISSING")
	}
	if evidence.Dependencies.InflightRequests > 0 {
		blockers = append(blockers, "INFLIGHT_REQUESTS_PRESENT")
	}
	result := ImpactPrecheckResult{Action: "MODEL_CATALOG_ROLLBACK", Evidence: evidence, PriceCoverage: &price, Blockers: blockers}
	catalogVersion := int64(0)
	if evidence.CurrentVersion.CatalogVersion != nil {
		catalogVersion = *evidence.CurrentVersion.CatalogVersion
	}
	if catalogVersion < 1 || price.FinancialFactRevision < 1 || !validImpactFingerprint(price.PriceSetFingerprint) || !validImpactFingerprint(evidence.CatalogDiffFingerprint) || !validImpactFingerprint(evidence.RouteSetFingerprint) || !validImpactFingerprint(evidence.ProvisioningSetFingerprint) || !validImpactFingerprint(evidence.InflightSetFingerprint) {
		result.Blockers = append(result.Blockers, "APPROVAL_SNAPSHOT_UNAVAILABLE")
	}
	result.CriticalResourceVersions = map[string]any{
		"current_catalog_version": catalogVersion, "source_catalog_version": version,
		"catalog_diff_fingerprint": evidence.CatalogDiffFingerprint, "billing_price_version": price.FinancialFactRevision,
		"route_set_fingerprint": evidence.RouteSetFingerprint, "provisioning_set_fingerprint": evidence.ProvisioningSetFingerprint,
		"inflight_request_set_fingerprint": evidence.InflightSetFingerprint, "policy_version": int64(2),
	}
	result.FinancialSnapshot = financialImpactSnapshot(price)
	finalizeImpactResult(&result, "source_catalog_version="+strconv.FormatInt(version, 10))
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminTokenAccountStatusImpact(w http.ResponseWriter, r *http.Request) {
	target := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("target_status")))
	s.adminTokenLocalImpact(w, r, "TOKEN_ACCOUNT_STATUS_"+target, []string{"target_status=" + target}, func(repository ImpactPrecheckRepository, accountID uuid.UUID) (ImpactLocalEvidence, error) {
		if target != "ACTIVE" && target != "DISABLED" {
			return ImpactLocalEvidence{}, ErrForbidden
		}
		return repository.TokenAccountImpact(r.Context(), accountID)
	})
}

func (s *Service) adminTokenRouteImpact(w http.ResponseWriter, r *http.Request) {
	action := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("action")))
	model := strings.TrimSpace(r.URL.Query().Get("model"))
	protocol := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("protocol_code")))
	if (action != "UPSERT" && action != "DELETE") || model == "" || len(model) > 200 || (action == "UPSERT" && protocol == "") {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "route action or model is invalid", httpserver.RequestID(r.Context()))
		return
	}
	s.adminTokenLocalImpact(w, r, "TOKEN_ROUTE_"+action, []string{"model=" + model, "protocol_code=" + protocol}, func(repository ImpactPrecheckRepository, accountID uuid.UUID) (ImpactLocalEvidence, error) {
		return repository.TokenRouteImpact(r.Context(), accountID, model, protocol, action)
	})
}

func (s *Service) adminStableTokenRouteImpact(w http.ResponseWriter, r *http.Request) {
	action := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("action")))
	input := StableRouteImpactInput{Action: action}
	var err error
	if action == "UPSERT" {
		input.ModelID, err = uuid.Parse(strings.TrimSpace(r.URL.Query().Get("model_id")))
		if err == nil {
			input.ProtocolVersionID, err = uuid.Parse(strings.TrimSpace(r.URL.Query().Get("protocol_version_id")))
		}
	} else if action == "DELETE" {
		input.RouteID, err = uuid.Parse(strings.TrimSpace(r.URL.Query().Get("route_id")))
	} else {
		err = ErrForbidden
	}
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "stable route identifiers are invalid", httpserver.RequestID(r.Context()))
		return
	}
	parameters := []string{"route_id=" + input.RouteID.String(), "model_id=" + input.ModelID.String(), "protocol_version_id=" + input.ProtocolVersionID.String()}
	s.adminTokenLocalImpact(w, r, "TOKEN_ROUTE_"+action, parameters, func(repository ImpactPrecheckRepository, accountID uuid.UUID) (ImpactLocalEvidence, error) {
		stable, ok := repository.(interface {
			StableTokenRouteImpact(context.Context, uuid.UUID, StableRouteImpactInput) (ImpactLocalEvidence, error)
		})
		if !ok {
			return ImpactLocalEvidence{}, ErrRouteReferenceInvalid
		}
		return stable.StableTokenRouteImpact(r.Context(), accountID, input)
	})
}

func (s *Service) adminTokenCredentialImpact(w http.ResponseWriter, r *http.Request) {
	action := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("action")))
	if action != "ENROLL" && action != "ROTATE" && action != "REVOKE" && action != "VALIDATE" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "credential action is invalid", httpserver.RequestID(r.Context()))
		return
	}
	var credentialID *uuid.UUID
	if raw := strings.TrimSpace(r.URL.Query().Get("credential_id")); raw != "" {
		parsed, err := uuid.Parse(raw)
		if err != nil {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "credential_id is invalid", httpserver.RequestID(r.Context()))
			return
		}
		credentialID = &parsed
	}
	if action != "ENROLL" && credentialID == nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "credential_id is required", httpserver.RequestID(r.Context()))
		return
	}
	credentialParameter := ""
	if credentialID != nil {
		credentialParameter = credentialID.String()
	}
	if action != "REVOKE" {
		s.adminTokenLocalImpact(w, r, "TOKEN_CREDENTIAL_"+action, []string{"credential_id=" + credentialParameter}, func(repository ImpactPrecheckRepository, accountID uuid.UUID) (ImpactLocalEvidence, error) {
			return repository.TokenCredentialImpact(r.Context(), accountID, credentialID)
		})
		return
	}
	repository, ok := s.impactPrecheckRepository(w, r)
	if !ok {
		return
	}
	accountID, ok := adminUUID(w, r, "token_account_id")
	if !ok {
		return
	}
	evidence, err := repository.TokenCredentialImpact(r.Context(), accountID, credentialID)
	if err != nil {
		s.writeImpactError(w, r, err)
		return
	}
	result := credentialRevokeImpactResult(accountID, *credentialID, evidence)
	api.WriteJSON(w, http.StatusOK, result)
}

func credentialRevokeImpactResult(accountID, credentialID uuid.UUID, evidence ImpactLocalEvidence) ImpactPrecheckResult {
	blockers := []string{}
	if evidence.Dependencies.ReadyProvisionings > 0 && evidence.Dependencies.AlternativeCredentials == 0 {
		blockers = append(blockers, "ALTERNATIVE_CREDENTIAL_MISSING")
	}
	if evidence.Dependencies.ActiveLeases > 0 {
		blockers = append(blockers, "ACTIVE_LEASES_PRESENT")
	}
	accountVersion, credentialVersion := int64(0), 0
	if evidence.CurrentVersion.AccountVersion != nil {
		accountVersion = *evidence.CurrentVersion.AccountVersion
	}
	if evidence.CurrentVersion.CredentialVersion != nil {
		credentialVersion = *evidence.CurrentVersion.CredentialVersion
	}
	result := ImpactPrecheckResult{Action: "TOKEN_CREDENTIAL_REVOKE", Evidence: evidence, Blockers: blockers,
		CriticalResourceVersions: map[string]any{
			"token_account_id": accountID.String(), "token_account_version": accountVersion,
			"credential_id": credentialID.String(), "credential_version": credentialVersion,
			"credential_status": evidence.CurrentStatus, "credential_fingerprint": evidence.CredentialFingerprint,
			"active_route_set_fingerprint":       evidence.RouteSetFingerprint,
			"provisioning_lease_set_fingerprint": evidence.ProvisioningSetFingerprint,
			"inflight_request_set_fingerprint":   evidence.InflightSetFingerprint,
			"replacement_credential_version":     evidence.ReplacementCredentialVersion, "policy_version": int64(1),
		}, FinancialSnapshot: map[string]any{}}
	if accountVersion < 1 || credentialVersion < 1 || !validImpactFingerprint(evidence.CredentialFingerprint) || !validImpactFingerprint(evidence.RouteSetFingerprint) || !validImpactFingerprint(evidence.ProvisioningSetFingerprint) || !validImpactFingerprint(evidence.InflightSetFingerprint) {
		result.Blockers = append(result.Blockers, "APPROVAL_SNAPSHOT_UNAVAILABLE")
	}
	finalizeImpactResult(&result, "token_account_id="+accountID.String(), "credential_id="+credentialID.String(), "credential_version="+strconv.Itoa(credentialVersion))
	return result
}

func (s *Service) adminTokenLocalImpact(w http.ResponseWriter, r *http.Request, action string, actionParameters []string, load func(ImpactPrecheckRepository, uuid.UUID) (ImpactLocalEvidence, error)) {
	repository, ok := s.impactPrecheckRepository(w, r)
	if !ok {
		return
	}
	accountID, ok := adminUUID(w, r, "token_account_id")
	if !ok {
		return
	}
	evidence, err := load(repository, accountID)
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "impact precheck parameters are invalid", httpserver.RequestID(r.Context()))
			return
		}
		s.writeImpactError(w, r, err)
		return
	}
	blockers := []string{}
	if strings.HasSuffix(action, "DISABLED") && evidence.Dependencies.InflightRequests > 0 {
		blockers = append(blockers, "INFLIGHT_REQUESTS_PRESENT")
	}
	if strings.HasSuffix(action, "DELETE") && evidence.Dependencies.AffectedCoverageTuples > 0 {
		blockers = append(blockers, "READY_PROVISIONING_DEPENDS_ON_ROUTE")
	}
	if strings.HasSuffix(action, "DELETE") && evidence.Dependencies.CoverageGaps > 0 {
		blockers = append(blockers, "READY_COVERAGE_WOULD_BE_LOST")
	}
	if (strings.HasSuffix(action, "ROTATE") || strings.HasSuffix(action, "REVOKE")) && evidence.Dependencies.ReadyProvisionings > 0 && evidence.Dependencies.AlternativeCredentials == 0 {
		blockers = append(blockers, "ALTERNATIVE_CREDENTIAL_MISSING")
	}
	if strings.HasSuffix(action, "REVOKE") && evidence.Dependencies.ActiveLeases > 0 {
		blockers = append(blockers, "ACTIVE_LEASES_PRESENT")
	}
	result := ImpactPrecheckResult{Action: action, Evidence: evidence, Blockers: blockers}
	finalizeImpactResult(&result, append([]string{"resource_id=" + evidence.ResourceID}, actionParameters...)...)
	api.WriteJSON(w, http.StatusOK, result)
}

func finalizeImpactResult(result *ImpactPrecheckResult, actionParameters ...string) {
	result.TokenObservedAt = result.Evidence.ObservedAt.UTC()
	result.SnapshotSemantics = "INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS"
	result.RequiresCASRevalidation = true
	values := []string{result.Action, result.Evidence.ResourceType, result.Evidence.ResourceID, result.Evidence.CurrentVersion.EvidenceETag, result.TokenObservedAt.Format(time.RFC3339Nano)}
	values = append(values, actionParameters...)
	if result.PriceCoverage != nil {
		billingObservedAt := result.PriceCoverage.BillingObservedAt.UTC()
		result.BillingObservedAt = &billingObservedAt
		values = append(values, "billing_as_of="+billingObservedAt.Format(time.RFC3339Nano))
		values = append(values, "billing_revision="+strconv.FormatInt(result.PriceCoverage.FinancialFactRevision, 10), "price_set_fingerprint="+result.PriceCoverage.PriceSetFingerprint)
		versions := append([]ImpactPriceEvidence(nil), result.PriceCoverage.Versions...)
		sort.Slice(versions, func(i, j int) bool {
			return impactPriceDimensionKey(ImpactPriceQuery{ModelID: versions[i].ModelID, ProtocolCode: versions[i].ProtocolCode, CreditUnit: versions[i].CreditUnit}) < impactPriceDimensionKey(ImpactPriceQuery{ModelID: versions[j].ModelID, ProtocolCode: versions[j].ProtocolCode, CreditUnit: versions[j].CreditUnit})
		})
		for _, version := range versions {
			values = append(values, impactPriceDimensionKey(ImpactPriceQuery{ModelID: version.ModelID, ProtocolCode: version.ProtocolCode, CreditUnit: version.CreditUnit}), strconv.FormatBool(version.Covered), valueUUID(version.FormulaVersionID), valueUUID(version.PriceVersionID), valueInt64(version.PriceVersion))
		}
	}
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	result.PrecheckFingerprint = hex.EncodeToString(digest[:])
}

func (s *Service) impactPriceCoverage(w http.ResponseWriter, r *http.Request, dimensions []ImpactPriceQuery) (ImpactPriceSummary, bool) {
	result, err := s.loadImpactPriceCoverage(r.Context(), dimensions)
	if err != nil {
		writeImpactDownstreamFailure(w, r)
		return ImpactPriceSummary{}, false
	}
	return result, true
}

func (s *Service) loadImpactPriceCoverage(ctx context.Context, dimensions []ImpactPriceQuery) (ImpactPriceSummary, error) {
	client := s.impactPricing
	if client == nil {
		client, _ = s.billing.(ImpactPriceCoverageClient)
	}
	if client == nil || len(dimensions) == 0 {
		return ImpactPriceSummary{}, ErrImpactPricingUnavailable
	}
	coverage, err := client.ImpactPriceCoverage(ctx, dimensions)
	if err != nil {
		return ImpactPriceSummary{}, err
	}
	if coverage.Source != "BILLING" || coverage.AsOf.IsZero() || coverage.FinancialFactRevision < 1 || !validImpactFingerprint(coverage.PriceSetFingerprint) || len(coverage.Items) != len(dimensions) {
		return ImpactPriceSummary{}, ErrImpactPricingUnavailable
	}
	byKey := make(map[string]ImpactPriceCoverage, len(coverage.Items))
	for _, item := range coverage.Items {
		key := impactPriceDimensionKey(ImpactPriceQuery{ModelID: item.ModelID, ProtocolCode: item.ProtocolCode, CreditUnit: item.CreditUnit})
		if item.ModelID == uuid.Nil || item.ProtocolCode == "" || item.ProtocolCode != strings.ToUpper(strings.TrimSpace(item.ProtocolCode)) || item.CreditUnit == "" || item.CreditUnit != strings.ToUpper(strings.TrimSpace(item.CreditUnit)) {
			return ImpactPriceSummary{}, ErrImpactPricingUnavailable
		}
		if _, duplicate := byKey[key]; duplicate || item.Covered != (item.PriceVersionID != nil && item.PriceVersion != nil && *item.PriceVersion > 0 && item.FormulaVersionID != nil && item.EffectiveFrom != nil && !item.EffectiveFrom.IsZero()) || (!item.Covered && (item.PriceVersionID != nil || item.PriceVersion != nil || item.FormulaVersionID != nil || item.EffectiveFrom != nil || item.EffectiveTo != nil)) {
			return ImpactPriceSummary{}, ErrImpactPricingUnavailable
		}
		byKey[key] = item
	}
	versions := make([]ImpactPriceEvidence, 0, len(dimensions))
	covered := 0
	seen := make(map[string]struct{}, len(dimensions))
	for _, dimension := range dimensions {
		key := impactPriceDimensionKey(dimension)
		if _, duplicate := seen[key]; duplicate {
			return ImpactPriceSummary{}, ErrImpactPricingUnavailable
		}
		seen[key] = struct{}{}
		item, exists := byKey[key]
		if !exists {
			return ImpactPriceSummary{}, ErrImpactPricingUnavailable
		}
		if item.Covered {
			covered++
		}
		versions = append(versions, ImpactPriceEvidence{ModelID: dimension.ModelID, ProtocolCode: item.ProtocolCode, FormulaVersionID: item.FormulaVersionID, CreditUnit: item.CreditUnit, Covered: item.Covered, PriceVersionID: item.PriceVersionID, PriceVersion: item.PriceVersion, EffectiveFrom: item.EffectiveFrom, EffectiveTo: item.EffectiveTo})
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].ModelID.String() < versions[j].ModelID.String() })
	return ImpactPriceSummary{ModelsChecked: len(dimensions), ModelsCovered: covered, FullyCovered: covered == len(dimensions), Versions: versions, BillingObservedAt: coverage.AsOf.UTC(), FinancialFactRevision: coverage.FinancialFactRevision, PriceSetFingerprint: coverage.PriceSetFingerprint}, nil
}

func financialImpactSnapshot(price ImpactPriceSummary) map[string]any {
	return map[string]any{
		"billing_price_revision": price.FinancialFactRevision,
		"price_set_fingerprint":  price.PriceSetFingerprint,
		"models_checked":         price.ModelsChecked,
		"models_covered":         price.ModelsCovered,
		"fully_covered":          price.FullyCovered,
		"billing_as_of":          price.BillingObservedAt.UTC(),
	}
}

func impactPriceDimensionKey(item ImpactPriceQuery) string {
	return item.ModelID.String() + "\x00" + strings.ToUpper(strings.TrimSpace(item.ProtocolCode)) + "\x00" + strings.ToUpper(strings.TrimSpace(item.CreditUnit))
}

func validImpactFingerprint(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func writeImpactDownstreamFailure(w http.ResponseWriter, r *http.Request) {
	api.WriteError(w, http.StatusGatewayTimeout, "PRECHECK_TIMEOUT", "the impact precheck could not establish complete downstream evidence", httpserver.RequestID(r.Context()))
}

func (s *Service) writeImpactError(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		if requestdeadline.WriteContextDeadlineError(w, r) {
			return
		}
		writeImpactDownstreamFailure(w, r)
		return
	}
	if errors.Is(err, ErrImpactScopeTooLarge) {
		api.WriteError(w, http.StatusUnprocessableEntity, "PRECHECK_FILTER_REQUIRED", "impact precheck scope is too large", httpserver.RequestID(r.Context()))
		return
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrModelUnavailable) {
		api.WriteError(w, http.StatusNotFound, "RESOURCE_NOT_FOUND", "the requested resource is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteError(w, http.StatusServiceUnavailable, "PRECHECK_UNAVAILABLE", "impact precheck is unavailable", httpserver.RequestID(r.Context()))
}

func impactETag(values ...string) string {
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return `"impact-` + hex.EncodeToString(digest[:]) + `"`
}

const canServeCoverageRelation = `(SELECT provision.id provisioning_id,provision.status provisioning_status,provision.scope_type,provision.scope_id,provision.wallet_id,provision.model_id,provision.upstream_model_ref,provision.protocol_code,provision.protocol_version_id,provision.actual_generation,account.id account_id,account.owner_type,account.owner_id,account.owner_organization_id,account.owner_unit_id,account.status account_status,identity.provider_id required_provider,credential.id credential_id,credential.version credential_version,credential.status credential_status,credential.validation_status,credential.provider credential_provider,credential.allowed_models,route.id route_id,route.health_status route_health_status,route.route_generation
FROM route_provisionings provision
JOIN model_identities identity ON identity.id=provision.model_id
JOIN token_accounts account ON account.id=provision.active_token_account_id AND account.management_mode='PLATFORM' AND account.status='ACTIVE' AND account.wallet_id=provision.wallet_id
 AND ((provision.scope_type='PERSONAL' AND account.owner_type='USER' AND account.owner_id=provision.scope_id) OR (provision.scope_type='ORGANIZATION' AND ((account.owner_type='ORG' AND account.owner_id=provision.scope_id) OR (account.owner_type='ORG_UNIT' AND account.owner_organization_id=provision.scope_id))))
JOIN provider_credentials credential ON credential.id=provision.active_credential_id AND credential.token_account_id=account.id AND credential.status='ACTIVE' AND credential.validation_status='VALID' AND credential.provider=identity.provider_id AND provision.upstream_model_ref=ANY(credential.allowed_models)
JOIN routing_policies route ON route.token_account_id=account.id AND route.model_id=provision.model_id AND route.protocol_version_id=provision.protocol_version_id AND route.route_generation=provision.actual_generation AND route.reference_status='RESOLVED' AND route.health_status='HEALTHY' AND route.health_expires_at>transaction_timestamp()
WHERE provision.status='READY')`

const impactAffectedCoverageCTE = `affected AS (
	SELECT DISTINCT scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,protocol_version_id,required_provider
	FROM serving WHERE account_id=$1 AND ($2='' OR upstream_model_ref=$2) AND ($3::uuid IS NULL OR credential_id=$3)
)`

const alternativeCredentialCandidatesCTE = `alternative_credentials AS (
	SELECT candidate.id credential_id,candidate.version credential_version,candidate.status credential_status,candidate.validation_status,candidate.provider,candidate.allowed_models
	FROM provider_credentials candidate
	WHERE $3::uuid IS NOT NULL AND candidate.token_account_id=$1 AND candidate.id<>$3 AND candidate.status IN ('PENDING','RETIRED') AND candidate.validation_status='VALID'
	 AND EXISTS(SELECT 1 FROM affected)
	 AND NOT EXISTS(SELECT 1 FROM affected WHERE candidate.provider<>affected.required_provider OR NOT affected.upstream_model_ref=ANY(candidate.allowed_models))
)`

const alternativeAccountCandidatesCTE = `alternative_accounts AS (
	SELECT DISTINCT candidate.account_id
	FROM serving candidate
	WHERE candidate.account_id<>$1 AND EXISTS(SELECT 1 FROM affected)
	 AND NOT EXISTS(SELECT 1 FROM affected WHERE NOT EXISTS(SELECT 1 FROM serving exact_candidate WHERE exact_candidate.account_id=candidate.account_id AND exact_candidate.scope_type=affected.scope_type AND exact_candidate.scope_id=affected.scope_id AND exact_candidate.wallet_id=affected.wallet_id AND exact_candidate.model_id=affected.model_id AND exact_candidate.upstream_model_ref=affected.upstream_model_ref AND exact_candidate.protocol_code=affected.protocol_code AND exact_candidate.protocol_version_id=affected.protocol_version_id))
)`

func canServeCoverageCount(ctx context.Context, tx pgx.Tx, modelIDs []uuid.UUID, accountID *uuid.UUID, model string, credentialID, excludeAccountID *uuid.UUID) (int64, error) {
	var count int64
	err := tx.QueryRow(ctx, `SELECT count(*) FROM `+canServeCoverageRelation+` coverage WHERE ($1::uuid[] IS NULL OR coverage.model_id=ANY($1)) AND ($2::uuid IS NULL OR coverage.account_id=$2) AND ($3='' OR coverage.upstream_model_ref=$3) AND ($4::uuid IS NULL OR coverage.credential_id=$4) AND ($5::uuid IS NULL OR coverage.account_id<>$5)`, modelIDs, accountID, model, credentialID, excludeAccountID).Scan(&count)
	return count, err
}

func alternativeCoverageCounts(ctx context.Context, tx pgx.Tx, accountID uuid.UUID, model string, credentialID *uuid.UUID) (int64, int64, error) {
	var credentials, accounts int64
	err := tx.QueryRow(ctx, `WITH serving AS `+canServeCoverageRelation+`,`+impactAffectedCoverageCTE+`,`+alternativeCredentialCandidatesCTE+`,`+alternativeAccountCandidatesCTE+`
		SELECT (SELECT count(*) FROM alternative_credentials),(SELECT count(*) FROM alternative_accounts)`, accountID, model, credentialID).Scan(&credentials, &accounts)
	return credentials, accounts, err
}

func stableRouteDeleteCoverage(ctx context.Context, tx pgx.Tx, accountID, routeID, protocolVersionID uuid.UUID) (int64, int64, int64, error) {
	var affected, covered, gaps int64
	err := tx.QueryRow(ctx, `WITH serving AS `+canServeCoverageRelation+`, affected AS (
		SELECT DISTINCT scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,protocol_version_id
		FROM serving WHERE account_id=$1 AND route_id=$2 AND protocol_version_id=$3
	), coverage AS (
		SELECT affected.*,EXISTS(SELECT 1 FROM serving candidate WHERE candidate.account_id<>$1 AND candidate.scope_type=affected.scope_type AND candidate.scope_id=affected.scope_id AND candidate.wallet_id=affected.wallet_id AND candidate.model_id=affected.model_id AND candidate.upstream_model_ref=affected.upstream_model_ref AND candidate.protocol_code=affected.protocol_code AND candidate.protocol_version_id=$3) covered
		FROM affected
	)
	SELECT count(*),count(*) FILTER (WHERE covered),count(*) FILTER (WHERE NOT covered) FROM coverage`, accountID, routeID, protocolVersionID).Scan(&affected, &covered, &gaps)
	return affected, covered, gaps, err
}

func routeDeleteCoverage(ctx context.Context, tx pgx.Tx, accountID uuid.UUID, model, protocol string) (int64, int64, int64, error) {
	var affected, covered, gaps int64
	err := tx.QueryRow(ctx, `WITH serving AS `+canServeCoverageRelation+`, affected AS (
		SELECT DISTINCT scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,protocol_version_id
		FROM serving WHERE account_id=$1 AND upstream_model_ref=$2 AND ($3='' OR protocol_code=$3)
	), coverage AS (
		SELECT affected.*,EXISTS(SELECT 1 FROM serving candidate WHERE candidate.account_id<>$1 AND candidate.scope_type=affected.scope_type AND candidate.scope_id=affected.scope_id AND candidate.wallet_id=affected.wallet_id AND candidate.model_id=affected.model_id AND candidate.upstream_model_ref=affected.upstream_model_ref AND candidate.protocol_code=affected.protocol_code AND candidate.protocol_version_id=affected.protocol_version_id) covered
		FROM affected
	)
	SELECT count(*),count(*) FILTER (WHERE covered),count(*) FILTER (WHERE NOT covered) FROM coverage`, accountID, model, protocol).Scan(&affected, &covered, &gaps)
	return affected, covered, gaps, err
}

func (r *PostgresRepository) ModelLifecycleImpact(ctx context.Context, modelID uuid.UUID, replacementID *uuid.UUID) (ImpactLocalEvidence, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	defer tx.Rollback(ctx)
	evidence, err := modelImpactInTx(ctx, tx, modelID, replacementID)
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.ReplacementModelID = replacementID
	if replacementID != nil {
		var ready bool
		var replacementProtocol string
		err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_catalog_entries WHERE catalog_version=$1 AND model_id=$2 AND lifecycle_status='PUBLISHED' AND source_available),COALESCE((SELECT protocol.protocol_code FROM model_catalog_entries entry JOIN protocol_definitions protocol ON protocol.id=entry.protocol_definition_id WHERE entry.catalog_version=$1 AND entry.model_id=$2),'')`, *evidence.CurrentVersion.CatalogVersion, *replacementID).Scan(&ready, &replacementProtocol)
		if err != nil {
			return ImpactLocalEvidence{}, err
		}
		evidence.ReplacementReady = &ready
		evidence.ModelIDs = append(evidence.ModelIDs, *replacementID)
		if replacementProtocol != "" {
			evidence.PriceDimensions = append(evidence.PriceDimensions, ImpactPriceQuery{ModelID: *replacementID, ProtocolCode: replacementProtocol, CreditUnit: "MICROCREDIT"})
		}
	}
	evidence.ModelIDs = uniqueUUIDs(evidence.ModelIDs)
	evidence.CurrentVersion.EvidenceETag = localImpactETag(evidence)
	return evidence, commitReadOnly(ctx, tx)
}

func (r *PostgresRepository) CatalogRollbackImpact(ctx context.Context, sourceVersion int64) (ImpactLocalEvidence, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	defer tx.Rollback(ctx)
	var observedAt time.Time
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&observedAt); err != nil {
		return ImpactLocalEvidence{}, err
	}
	var current int64
	if err = tx.QueryRow(ctx, `SELECT max(version) FROM model_catalog_versions`).Scan(&current); err != nil || current == 0 {
		return ImpactLocalEvidence{}, ErrModelUnavailable
	}
	rows, err := tx.Query(ctx, `SELECT entry.model_id,protocol.protocol_code FROM model_catalog_entries entry JOIN protocol_definitions protocol ON protocol.id=entry.protocol_definition_id WHERE entry.catalog_version=$1 AND entry.lifecycle_status<>'REMOVED' ORDER BY entry.model_id LIMIT $2`, sourceVersion, maximumImpactModels+1)
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	ids := []uuid.UUID{}
	dimensions := []ImpactPriceQuery{}
	for rows.Next() {
		var id uuid.UUID
		var protocol string
		if scanErr := rows.Scan(&id, &protocol); scanErr != nil {
			rows.Close()
			return ImpactLocalEvidence{}, scanErr
		}
		ids = append(ids, id)
		dimensions = append(dimensions, ImpactPriceQuery{ModelID: id, ProtocolCode: protocol, CreditUnit: "MICROCREDIT"})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return ImpactLocalEvidence{}, err
	}
	if len(ids) == 0 {
		return ImpactLocalEvidence{}, ErrModelUnavailable
	}
	if len(ids) > maximumImpactModels {
		return ImpactLocalEvidence{}, ErrImpactScopeTooLarge
	}
	var dependencies ImpactDependencySummary
	if err := tx.QueryRow(ctx, `SELECT
		count(DISTINCT route.id),count(DISTINCT route.id) FILTER (WHERE route.reference_status='RESOLVED' AND route.health_status='HEALTHY' AND route.health_expires_at>transaction_timestamp()),
		count(DISTINCT provision.id),0,
		count(DISTINCT provision.id) FILTER (WHERE provision.lease_until>transaction_timestamp()),
		count(DISTINCT request.id) FILTER (WHERE request.execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING')),
		count(DISTINCT request.session_id) FILTER (WHERE request.execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING'))
		FROM model_catalog_entries entry
		LEFT JOIN model_identities identity ON identity.id=entry.model_id
		LEFT JOIN routing_policies route ON route.model IN (entry.model_id::text,identity.upstream_model_ref)
		LEFT JOIN route_provisionings provision ON provision.model_id=entry.model_id
		LEFT JOIN usage_requests request ON request.model_id=entry.model_id
		WHERE entry.catalog_version=$1 AND entry.lifecycle_status<>'REMOVED'`, sourceVersion).Scan(&dependencies.Routes, &dependencies.HealthyRoutes, &dependencies.Provisionings, &dependencies.ReadyProvisionings, &dependencies.ActiveLeases, &dependencies.InflightRequests, &dependencies.ActiveSessions); err != nil {
		return ImpactLocalEvidence{}, err
	}
	dependencies.ReadyProvisionings, err = canServeCoverageCount(ctx, tx, ids, nil, "", nil, nil)
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	version := current
	evidence := ImpactLocalEvidence{ResourceType: "MODEL_CATALOG", ResourceID: strconv.FormatInt(sourceVersion, 10), CurrentStatus: "PUBLISHED", CurrentVersion: ImpactResourceVersion{CatalogVersion: &version}, Dependencies: dependencies, ModelIDs: ids, PriceDimensions: dimensions, ObservedAt: observedAt.UTC()}
	evidence.CurrentVersion.EvidenceSetSHA256, err = catalogImpactKeySet(ctx, tx, sourceVersion, current, ids)
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.CatalogDiffFingerprint, err = impactRowsFingerprint(ctx, tx, `SELECT jsonb_build_array(catalog_version,model_id,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available,replacement_model_id,new_session_cutoff_at,continuation_cutoff_at,force_cancel_inflight)::text FROM model_catalog_entries WHERE catalog_version=ANY($1::bigint[]) ORDER BY catalog_version,model_id`, []int64{sourceVersion, current})
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.RouteSetFingerprint, err = impactRowsFingerprint(ctx, tx, `SELECT jsonb_build_array(route.id,route.model_id,route.protocol_version_id,route.route_version,route.route_generation,route.reference_status,route.health_status)::text FROM routing_policies route WHERE route.model_id=ANY($1::uuid[]) ORDER BY route.id`, ids)
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.ProvisioningSetFingerprint, err = impactRowsFingerprint(ctx, tx, `SELECT jsonb_build_array(id,status,desired_generation,actual_generation,lease_until,active_token_account_id,active_credential_id,protocol_version_id)::text FROM route_provisionings WHERE model_id=ANY($1::uuid[]) ORDER BY id`, ids)
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.InflightSetFingerprint, err = impactRowsFingerprint(ctx, tx, `SELECT jsonb_build_array(id,session_id,status,execution_status,execution_version)::text FROM usage_requests WHERE model_id=ANY($1::uuid[]) AND execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING') ORDER BY id`, ids)
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.CurrentVersion.EvidenceETag = localImpactETag(evidence)
	return evidence, commitReadOnly(ctx, tx)
}

func modelImpactInTx(ctx context.Context, tx pgx.Tx, modelID uuid.UUID, replacementID *uuid.UUID) (ImpactLocalEvidence, error) {
	var evidence ImpactLocalEvidence
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&evidence.ObservedAt); err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.ObservedAt = evidence.ObservedAt.UTC()
	var version int64
	var upstream string
	var protocol string
	err := tx.QueryRow(ctx, `SELECT entry.catalog_version,entry.lifecycle_status,entry.model_capability_version_id,entry.protocol_definition_id,identity.upstream_model_ref,protocol.protocol_code
		FROM model_catalog_entries entry JOIN model_identities identity ON identity.id=entry.model_id JOIN protocol_definitions protocol ON protocol.id=entry.protocol_definition_id
		WHERE entry.catalog_version=(SELECT max(version) FROM model_catalog_versions) AND entry.model_id=$1`, modelID).Scan(&version, &evidence.CurrentStatus, &evidence.CurrentVersion.CapabilityVersionID, &evidence.CurrentVersion.ProtocolDefinitionID, &upstream, &protocol)
	if errors.Is(err, pgx.ErrNoRows) {
		return ImpactLocalEvidence{}, ErrModelUnavailable
	}
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.ResourceType, evidence.ResourceID, evidence.CurrentVersion.CatalogVersion = "MODEL", modelID.String(), &version
	evidence.ModelIDs = []uuid.UUID{modelID}
	evidence.PriceDimensions = []ImpactPriceQuery{{ModelID: modelID, ProtocolCode: protocol, CreditUnit: "MICROCREDIT"}}
	err = tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM routing_policies WHERE model IN ($1,$2)),
		(SELECT count(*) FROM routing_policies WHERE (model_id::text=$1 OR model=$2) AND reference_status='RESOLVED' AND health_status='HEALTHY' AND health_expires_at>transaction_timestamp()),
		(SELECT count(*) FROM route_provisionings WHERE model_id=$3),
		0,
		(SELECT count(*) FROM route_provisionings WHERE model_id=$3 AND lease_until>transaction_timestamp()),
		(SELECT count(*) FROM usage_requests WHERE model_id=$3 AND execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING')),
		(SELECT count(DISTINCT session_id) FROM usage_requests WHERE model_id=$3 AND execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING'))`, modelID.String(), upstream, modelID).Scan(&evidence.Dependencies.Routes, &evidence.Dependencies.HealthyRoutes, &evidence.Dependencies.Provisionings, &evidence.Dependencies.ReadyProvisionings, &evidence.Dependencies.ActiveLeases, &evidence.Dependencies.InflightRequests, &evidence.Dependencies.ActiveSessions)
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.Dependencies.ReadyProvisionings, err = canServeCoverageCount(ctx, tx, []uuid.UUID{modelID}, nil, "", nil, nil)
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.CurrentVersion.EvidenceSetSHA256, err = modelImpactKeySet(ctx, tx, modelID, upstream, replacementID)
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.RouteSetFingerprint, err = impactRowsFingerprint(ctx, tx, `SELECT jsonb_build_array(id,model_id,protocol_version_id,route_version,route_generation,reference_status,health_status)::text FROM routing_policies WHERE model_id=$1 OR model IN ($1::text,$2) ORDER BY id`, modelID, upstream)
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.ProvisioningSetFingerprint, err = impactRowsFingerprint(ctx, tx, `SELECT jsonb_build_array(id,status,desired_generation,actual_generation,lease_until,active_token_account_id,active_credential_id,protocol_version_id)::text FROM route_provisionings WHERE model_id=$1 ORDER BY id`, modelID)
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.InflightSetFingerprint, err = impactRowsFingerprint(ctx, tx, `SELECT jsonb_build_array(id,session_id,status,execution_status,execution_version)::text FROM usage_requests WHERE model_id=$1 AND execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING') ORDER BY id`, modelID)
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	return evidence, nil
}

func (r *PostgresRepository) TokenAccountImpact(ctx context.Context, accountID uuid.UUID) (ImpactLocalEvidence, error) {
	return r.tokenAccountScopedImpact(ctx, accountID, "", "", "", nil)
}

func (r *PostgresRepository) TokenRouteImpact(ctx context.Context, accountID uuid.UUID, model, protocol, action string) (ImpactLocalEvidence, error) {
	return r.tokenAccountScopedImpact(ctx, accountID, strings.TrimSpace(model), strings.ToUpper(strings.TrimSpace(protocol)), strings.ToUpper(strings.TrimSpace(action)), nil)
}

func (r *PostgresRepository) StableTokenRouteImpact(ctx context.Context, accountID uuid.UUID, input StableRouteImpactInput) (ImpactLocalEvidence, error) {
	input.Action = strings.ToUpper(strings.TrimSpace(input.Action))
	if accountID == uuid.Nil || input.Action != "UPSERT" && input.Action != "DELETE" || input.Action == "UPSERT" && (input.ModelID == uuid.Nil || input.ProtocolVersionID == uuid.Nil) || input.Action == "DELETE" && input.RouteID == uuid.Nil {
		return ImpactLocalEvidence{}, ErrForbidden
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	defer tx.Rollback(ctx)
	var evidence ImpactLocalEvidence
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&evidence.ObservedAt); err != nil {
		return ImpactLocalEvidence{}, err
	}
	var accountUpdated time.Time
	var accountVersion int64
	if err = tx.QueryRow(ctx, `SELECT updated_at,management_version FROM token_accounts WHERE id=$1 AND management_mode='PLATFORM'`, accountID).Scan(&accountUpdated, &accountVersion); errors.Is(err, pgx.ErrNoRows) {
		return ImpactLocalEvidence{}, ErrNotFound
	} else if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.CurrentVersion.AccountUpdatedAt, evidence.CurrentVersion.AccountVersion = &accountUpdated, &accountVersion
	var reference stableRouteReference
	var routeID uuid.UUID
	var routeGeneration, routeVersion *int64
	if input.Action == "UPSERT" {
		reference, err = resolveStableRouteReference(ctx, tx, input.ModelID, input.ProtocolVersionID)
		if err != nil {
			return ImpactLocalEvidence{}, err
		}
		evidence.CurrentVersion.CatalogVersion = &reference.CatalogVersion
		evidence.CurrentVersion.CapabilityVersionID = &reference.CapabilityVersionID
		evidence.CurrentVersion.ProtocolDefinitionID = &reference.ProtocolVersionID
		var generation, version int64
		err = tx.QueryRow(ctx, `SELECT id,route_generation,route_version FROM routing_policies WHERE token_account_id=$1 AND model_id=$2 AND protocol_version_id=$3 AND reference_status='RESOLVED'`, accountID, input.ModelID, input.ProtocolVersionID).Scan(&routeID, &generation, &version)
		if errors.Is(err, pgx.ErrNoRows) {
			err = nil
			evidence.CurrentStatus = "CREATE"
			zero := int64(0)
			routeGeneration, routeVersion = &zero, &zero
		} else if err == nil {
			evidence.CurrentStatus = "PRESENT"
			routeGeneration, routeVersion = &generation, &version
		}
	} else {
		var generation, version int64
		err = tx.QueryRow(ctx, `SELECT route.model_id,route.protocol_version_id,route.route_generation,route.route_version,identity.upstream_model_ref,protocol.protocol_code,identity.provider_id,identity.adapter_version FROM routing_policies route JOIN model_identities identity ON identity.id=route.model_id JOIN protocol_definitions protocol ON protocol.id=route.protocol_version_id WHERE route.id=$1 AND route.token_account_id=$2 AND route.reference_status='RESOLVED'`, input.RouteID, accountID).Scan(&reference.ModelID, &reference.ProtocolVersionID, &generation, &version, &reference.UpstreamModel, &reference.ProtocolCode, &reference.Provider, &reference.AdapterVersion)
		if errors.Is(err, pgx.ErrNoRows) {
			return ImpactLocalEvidence{}, ErrNotFound
		}
		if err == nil {
			routeID, routeGeneration, routeVersion, evidence.CurrentStatus = input.RouteID, &generation, &version, "PRESENT"
			evidence.CurrentVersion.ProtocolDefinitionID = &reference.ProtocolVersionID
			if err = tx.QueryRow(ctx, `SELECT catalog_version,model_capability_version_id FROM model_catalog_entries WHERE model_id=$1 AND protocol_definition_id=$2 ORDER BY catalog_version DESC LIMIT 1`, reference.ModelID, reference.ProtocolVersionID).Scan(&reference.CatalogVersion, &reference.CapabilityVersionID); err != nil {
				return ImpactLocalEvidence{}, err
			}
			evidence.CurrentVersion.CatalogVersion = &reference.CatalogVersion
			evidence.CurrentVersion.CapabilityVersionID = &reference.CapabilityVersionID
		}
	}
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.ResourceType = "TOKEN_ROUTE"
	if routeID != uuid.Nil {
		evidence.ResourceID = routeID.String()
	} else {
		evidence.ResourceID = accountID.String() + ":" + reference.ModelID.String() + ":" + reference.ProtocolVersionID.String()
	}
	evidence.CurrentVersion.RouteGeneration, evidence.CurrentVersion.RouteVersion = routeGeneration, routeVersion
	err = tx.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM routing_policies WHERE token_account_id=$1 AND model_id=$2 AND protocol_version_id=$3 AND reference_status='RESOLVED'),
 (SELECT count(*) FROM routing_policies WHERE token_account_id=$1 AND model_id=$2 AND protocol_version_id=$3 AND reference_status='RESOLVED' AND health_status='HEALTHY' AND health_expires_at>transaction_timestamp()),
 (SELECT count(*) FROM route_provisionings WHERE active_token_account_id=$1 AND model_id=$2 AND protocol_version_id=$3),
 (SELECT count(*) FROM route_provisionings WHERE active_token_account_id=$1 AND model_id=$2 AND protocol_version_id=$3 AND status='READY'),
 (SELECT count(*) FROM route_provisionings WHERE active_token_account_id=$1 AND model_id=$2 AND protocol_version_id=$3 AND lease_until>transaction_timestamp()),
 (SELECT count(*) FROM usage_requests WHERE token_account_id=$1 AND model_id=$2 AND protocol_definition_id=$3 AND execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING')),
 (SELECT count(DISTINCT session_id) FROM usage_requests WHERE token_account_id=$1 AND model_id=$2 AND protocol_definition_id=$3 AND execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING'))`, accountID, reference.ModelID, reference.ProtocolVersionID).Scan(&evidence.Dependencies.Routes, &evidence.Dependencies.HealthyRoutes, &evidence.Dependencies.Provisionings, &evidence.Dependencies.ReadyProvisionings, &evidence.Dependencies.ActiveLeases, &evidence.Dependencies.InflightRequests, &evidence.Dependencies.ActiveSessions)
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	if input.Action == "DELETE" {
		evidence.Dependencies.AffectedCoverageTuples, evidence.Dependencies.RemainingReadyCoverage, evidence.Dependencies.CoverageGaps, err = stableRouteDeleteCoverage(ctx, tx, accountID, routeID, reference.ProtocolVersionID)
		if err != nil {
			return ImpactLocalEvidence{}, err
		}
	}
	evidence.CurrentVersion.EvidenceSetSHA256, err = stableRouteImpactKeySet(ctx, tx, accountID, routeID, reference)
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.CurrentVersion.EvidenceETag = localImpactETag(evidence)
	return evidence, commitReadOnly(ctx, tx)
}

func (r *PostgresRepository) TokenCredentialImpact(ctx context.Context, accountID uuid.UUID, credentialID *uuid.UUID) (ImpactLocalEvidence, error) {
	return r.tokenAccountScopedImpact(ctx, accountID, "", "", "", credentialID)
}

func (r *PostgresRepository) tokenAccountScopedImpact(ctx context.Context, accountID uuid.UUID, model, routeProtocol, routeAction string, credentialID *uuid.UUID) (ImpactLocalEvidence, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	defer tx.Rollback(ctx)
	var evidence ImpactLocalEvidence
	if err = tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&evidence.ObservedAt); err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.ObservedAt = evidence.ObservedAt.UTC()
	var updated time.Time
	var accountVersion int64
	err = tx.QueryRow(ctx, `SELECT status,updated_at,management_version FROM token_accounts WHERE id=$1 AND management_mode='PLATFORM'`, accountID).Scan(&evidence.CurrentStatus, &updated, &accountVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		return ImpactLocalEvidence{}, ErrNotFound
	}
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.ResourceType, evidence.ResourceID = "TOKEN_ACCOUNT", accountID.String()
	updated = updated.UTC()
	evidence.CurrentVersion.AccountUpdatedAt = &updated
	evidence.CurrentVersion.AccountVersion = &accountVersion
	if model != "" && routeAction == "UPSERT" {
		var published bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM model_catalog_entries entry JOIN model_identities identity ON identity.id=entry.model_id JOIN protocol_definitions protocol ON protocol.id=entry.protocol_definition_id WHERE entry.catalog_version=(SELECT max(version) FROM model_catalog_versions) AND entry.lifecycle_status='PUBLISHED' AND entry.source_available AND (entry.model_id::text=$1 OR identity.upstream_model_ref=$1) AND protocol.protocol_code=$2 AND protocol.status='PUBLISHED')`, model, routeProtocol).Scan(&published); err != nil {
			return ImpactLocalEvidence{}, err
		}
		if !published {
			return ImpactLocalEvidence{}, ErrModelUnavailable
		}
	}
	modelPredicate := model == ""
	var routeGeneration *int64
	if model == "" {
		err = tx.QueryRow(ctx, `SELECT max(route_generation) FROM routing_policies WHERE token_account_id=$1`, accountID).Scan(&routeGeneration)
	} else {
		var storedProtocol string
		var generation int64
		err = tx.QueryRow(ctx, `SELECT route_generation,protocol_code FROM routing_policies WHERE token_account_id=$1 AND model=$2`, accountID, model).Scan(&generation, &storedProtocol)
		if err == nil {
			routeGeneration = &generation
			if routeAction == "DELETE" {
				routeProtocol = storedProtocol
			}
		} else if errors.Is(err, pgx.ErrNoRows) {
			err = nil
		}
	}
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.CurrentVersion.RouteGeneration = routeGeneration
	if model != "" {
		evidence.ResourceType, evidence.ResourceID = "TOKEN_ROUTE", accountID.String()+":"+model
		if routeGeneration == nil {
			if routeAction == "DELETE" {
				return ImpactLocalEvidence{}, ErrNotFound
			}
			zero := int64(0)
			evidence.CurrentVersion.RouteGeneration = &zero
			evidence.CurrentStatus = "CREATE"
		} else {
			evidence.CurrentStatus = "PRESENT"
		}
	}
	if credentialID != nil {
		var version int
		err = tx.QueryRow(ctx, `SELECT version,status,fingerprint FROM provider_credentials WHERE token_account_id=$1 AND id=$2`, accountID, *credentialID).Scan(&version, &evidence.CurrentStatus, &evidence.CredentialFingerprint)
		if errors.Is(err, pgx.ErrNoRows) {
			return ImpactLocalEvidence{}, ErrNotFound
		}
		if err != nil {
			return ImpactLocalEvidence{}, err
		}
		evidence.ResourceType, evidence.ResourceID, evidence.CurrentVersion.CredentialVersion = "PROVIDER_CREDENTIAL", credentialID.String(), &version
	}
	credentialPredicate := credentialID == nil
	var credential uuid.UUID
	if credentialID != nil {
		credential = *credentialID
	}
	err = tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM routing_policies WHERE token_account_id=$1 AND ($2 OR model=$3)),
		(SELECT count(*) FROM routing_policies WHERE token_account_id=$1 AND ($2 OR model_id::text=$3 OR model=$3) AND reference_status='RESOLVED' AND health_status='HEALTHY' AND health_expires_at>transaction_timestamp()),
		(SELECT count(*) FROM route_provisionings WHERE active_token_account_id=$1 AND ($2 OR upstream_model_ref=$3) AND ($4 OR active_credential_id=$5)),
		0,
		(SELECT count(*) FROM route_provisionings WHERE active_token_account_id=$1 AND ($2 OR upstream_model_ref=$3) AND ($4 OR active_credential_id=$5) AND lease_until>transaction_timestamp()),
		(SELECT count(*) FROM usage_requests WHERE token_account_id=$1 AND ($2 OR model=$3) AND ($4 OR credential_id=$5) AND execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING')),
		(SELECT count(DISTINCT session_id) FROM usage_requests WHERE token_account_id=$1 AND ($2 OR model=$3) AND ($4 OR credential_id=$5) AND execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING')),
		0`, accountID, modelPredicate, model, credentialPredicate, credential).Scan(&evidence.Dependencies.Routes, &evidence.Dependencies.HealthyRoutes, &evidence.Dependencies.Provisionings, &evidence.Dependencies.ReadyProvisionings, &evidence.Dependencies.ActiveLeases, &evidence.Dependencies.InflightRequests, &evidence.Dependencies.ActiveSessions, &evidence.Dependencies.AlternativeCredentials)
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.Dependencies.ReadyProvisionings, err = canServeCoverageCount(ctx, tx, nil, &accountID, model, credentialID, nil)
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.Dependencies.AlternativeCredentials, evidence.Dependencies.AlternativeAccounts, err = alternativeCoverageCounts(ctx, tx, accountID, model, credentialID)
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	if credentialID != nil {
		var replacementVersion *int
		err = tx.QueryRow(ctx, `WITH serving AS `+canServeCoverageRelation+`,`+impactAffectedCoverageCTE+`,`+alternativeCredentialCandidatesCTE+`
			SELECT max(credential_version) FROM alternative_credentials`, accountID, model, credentialID).Scan(&replacementVersion)
		if err != nil {
			return ImpactLocalEvidence{}, err
		}
		evidence.ReplacementCredentialVersion = replacementVersion
		evidence.RouteSetFingerprint, err = impactRowsFingerprint(ctx, tx, `SELECT jsonb_build_array(id,model_id,protocol_version_id,route_version,route_generation,reference_status,health_status)::text FROM routing_policies WHERE token_account_id=$1 ORDER BY id`, accountID)
		if err != nil {
			return ImpactLocalEvidence{}, err
		}
		evidence.ProvisioningSetFingerprint, err = impactRowsFingerprint(ctx, tx, `SELECT jsonb_build_array(id,status,desired_generation,actual_generation,lease_until,active_token_account_id,active_credential_id,protocol_version_id)::text FROM route_provisionings WHERE active_token_account_id=$1 AND active_credential_id=$2 AND lease_until>transaction_timestamp() ORDER BY id`, accountID, *credentialID)
		if err != nil {
			return ImpactLocalEvidence{}, err
		}
		evidence.InflightSetFingerprint, err = impactRowsFingerprint(ctx, tx, `SELECT jsonb_build_array(id,session_id,status,execution_status,execution_version)::text FROM usage_requests WHERE token_account_id=$1 AND credential_id=$2 AND execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING') ORDER BY id`, accountID, *credentialID)
		if err != nil {
			return ImpactLocalEvidence{}, err
		}
	}
	if model != "" && routeAction == "DELETE" {
		evidence.Dependencies.AffectedCoverageTuples, evidence.Dependencies.RemainingReadyCoverage, evidence.Dependencies.CoverageGaps, err = routeDeleteCoverage(ctx, tx, accountID, model, routeProtocol)
		if err != nil {
			return ImpactLocalEvidence{}, err
		}
	}
	evidence.CurrentVersion.EvidenceSetSHA256, err = tokenAccountImpactKeySet(ctx, tx, accountID, model, credentialID)
	if err != nil {
		return ImpactLocalEvidence{}, err
	}
	evidence.CurrentVersion.EvidenceETag = localImpactETag(evidence)
	return evidence, commitReadOnly(ctx, tx)
}

func localImpactETag(evidence ImpactLocalEvidence) string {
	version := evidence.CurrentVersion
	return impactETag(evidence.ResourceType, evidence.ResourceID, evidence.CurrentStatus, valueInt64(version.CatalogVersion), valueUUID(version.CapabilityVersionID), valueUUID(version.ProtocolDefinitionID), valueTime(version.AccountUpdatedAt), valueInt64(version.AccountVersion), valueInt64(version.RouteGeneration), valueInt64(version.RouteVersion), valueInt(version.CredentialVersion), version.EvidenceSetSHA256, strconv.FormatInt(evidence.Dependencies.Routes, 10), strconv.FormatInt(evidence.Dependencies.HealthyRoutes, 10), strconv.FormatInt(evidence.Dependencies.Provisionings, 10), strconv.FormatInt(evidence.Dependencies.ReadyProvisionings, 10), strconv.FormatInt(evidence.Dependencies.InflightRequests, 10), strconv.FormatInt(evidence.Dependencies.ActiveSessions, 10), strconv.FormatInt(evidence.Dependencies.ActiveLeases, 10), strconv.FormatInt(evidence.Dependencies.AlternativeCredentials, 10), strconv.FormatInt(evidence.Dependencies.AlternativeAccounts, 10), strconv.FormatInt(evidence.Dependencies.AffectedCoverageTuples, 10), strconv.FormatInt(evidence.Dependencies.RemainingReadyCoverage, 10), strconv.FormatInt(evidence.Dependencies.CoverageGaps, 10))
}

func appendImpactKeyRows(ctx context.Context, tx pgx.Tx, digest hash.Hash, query string, args ...any) error {
	rows, err := tx.Query(ctx, query, args...)
	if err != nil {
		return err
	}
	return appendImpactKeyRowsIterator(ctx, digest, rows)
}

type impactKeyRowsIterator interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}

func appendImpactKeyRowsIterator(ctx context.Context, digest hash.Hash, rows impactKeyRowsIterator) error {
	defer rows.Close()
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		var value string
		if err := rows.Scan(&value); err != nil {
			return err
		}
		writeImpactKey(digest, value)
	}
	return rows.Err()
}

func writeImpactKey(digest hash.Hash, value string) {
	_, _ = digest.Write([]byte(value))
	_, _ = digest.Write([]byte{0})
}

func impactRowsFingerprint(ctx context.Context, tx pgx.Tx, query string, args ...any) (string, error) {
	digest := sha256.New()
	if err := appendImpactKeyRows(ctx, tx, digest, query, args...); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func modelImpactKeySet(ctx context.Context, tx pgx.Tx, modelID uuid.UUID, upstream string, replacementID *uuid.UUID) (string, error) {
	digest := sha256.New()
	replacement := uuid.Nil
	if replacementID != nil {
		replacement = *replacementID
	}
	queries := []struct {
		sql  string
		args []any
	}{
		{`SELECT 'catalog|'||jsonb_build_array(entry.catalog_version,entry.model_id,entry.lifecycle_status,entry.model_capability_version_id,entry.protocol_definition_id,entry.source_available,entry.replacement_model_id,entry.new_session_cutoff_at,entry.continuation_cutoff_at,entry.force_cancel_inflight)::text FROM model_catalog_entries entry WHERE entry.catalog_version=(SELECT max(version) FROM model_catalog_versions) AND entry.model_id=$1`, []any{modelID}},
		{`SELECT 'route|'||jsonb_build_array(id,model,protocol_code,route_generation,'PRESENT',health_status)::text FROM routing_policies WHERE model IN ($1,$2) ORDER BY id`, []any{modelID.String(), upstream}},
		{`SELECT 'provisioning|'||jsonb_build_array(id,status,desired_generation,actual_generation,lease_until,active_token_account_id,active_credential_id,upstream_model_ref,protocol_code,protocol_version_id)::text FROM route_provisionings WHERE model_id=$1 ORDER BY id`, []any{modelID}},
		{`SELECT 'request|'||jsonb_build_array(id,session_id,status,execution_status)::text FROM usage_requests WHERE model_id=$1 AND execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING') ORDER BY id`, []any{modelID}},
		{`SELECT DISTINCT 'credential|'||jsonb_build_array(credential.id,credential.version,credential.status,credential.validation_status,credential.provider,(SELECT jsonb_agg(value ORDER BY value) FROM unnest(credential.allowed_models) value),route.model,route.protocol_code)::text FROM routing_policies route JOIN provider_credentials credential ON credential.token_account_id=route.token_account_id WHERE route.model IN ($1,$2) ORDER BY 1`, []any{modelID.String(), upstream}},
		{`SELECT 'replacement|'||jsonb_build_array(entry.model_id,entry.lifecycle_status,entry.model_capability_version_id,entry.protocol_definition_id,entry.source_available)::text FROM model_catalog_entries entry WHERE $1<>$2 AND entry.catalog_version=(SELECT max(version) FROM model_catalog_versions) AND entry.model_id=$1`, []any{replacement, uuid.Nil}},
	}
	for _, item := range queries {
		if err := appendImpactKeyRows(ctx, tx, digest, item.sql, item.args...); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func stableRouteImpactKeySet(ctx context.Context, tx pgx.Tx, accountID, routeID uuid.UUID, reference stableRouteReference) (string, error) {
	digest := sha256.New()
	queries := []struct {
		sql  string
		args []any
	}{
		{`SELECT 'account|'||jsonb_build_array(id,status,management_version,updated_at)::text FROM token_accounts WHERE id=$1`, []any{accountID}},
		{`SELECT 'target-route|'||jsonb_build_array(id,token_account_id,model_id,protocol_version_id,priority,default_scope,reference_status,route_version,route_generation,health_version,health_status,health_diagnostic_code,health_observed_at,health_expires_at,updated_at)::text FROM routing_policies WHERE token_account_id=$1 AND model_id=$2 AND protocol_version_id=$3 AND ($4::uuid='00000000-0000-0000-0000-000000000000' OR id=$4) ORDER BY id`, []any{accountID, reference.ModelID, reference.ProtocolVersionID, routeID}},
		{`SELECT 'candidate-route|'||jsonb_build_array(id,token_account_id,model_id,protocol_version_id,priority,default_scope,reference_status,route_version,route_generation,health_version,health_status,health_diagnostic_code,health_observed_at,health_expires_at,updated_at)::text FROM routing_policies WHERE model_id=$1 AND protocol_version_id=$2 AND reference_status='RESOLVED' ORDER BY token_account_id,id`, []any{reference.ModelID, reference.ProtocolVersionID}},
		{`SELECT 'provisioning|'||jsonb_build_array(id,scope_type,scope_id,wallet_id,model_id,protocol_version_id,status,stage,desired_generation,actual_generation,lease_until,active_token_account_id,active_credential_id,updated_at)::text FROM route_provisionings WHERE model_id=$1 AND protocol_version_id=$2 ORDER BY id`, []any{reference.ModelID, reference.ProtocolVersionID}},
		{`SELECT 'request|'||jsonb_build_array(id,token_account_id,credential_id,session_id,protocol_definition_id,route_generation,status,execution_status,execution_version,updated_at)::text FROM usage_requests WHERE model_id=$1 AND protocol_definition_id=$2 AND execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING') ORDER BY id`, []any{reference.ModelID, reference.ProtocolVersionID}},
		{`SELECT 'credential|'||jsonb_build_array(id,token_account_id,provider,version,status,validation_status,fingerprint,(SELECT jsonb_agg(value ORDER BY value) FROM unnest(allowed_models) value),validated_at)::text FROM provider_credentials WHERE provider=$1 AND $2=ANY(allowed_models) ORDER BY token_account_id,id`, []any{reference.Provider, reference.UpstreamModel}},
		{`SELECT 'coverage|'||jsonb_build_array(provisioning_id,provisioning_status,scope_type,scope_id,wallet_id,model_id,protocol_version_id,actual_generation,account_id,account_status,required_provider,credential_id,credential_version,route_id,route_health_status,route_generation)::text FROM ` + canServeCoverageRelation + ` serving WHERE model_id=$1 AND protocol_version_id=$2 ORDER BY scope_type,scope_id,wallet_id,account_id,credential_id,route_id,provisioning_id`, []any{reference.ModelID, reference.ProtocolVersionID}},
	}
	for _, item := range queries {
		if err := appendImpactKeyRows(ctx, tx, digest, item.sql, item.args...); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func catalogImpactKeySet(ctx context.Context, tx pgx.Tx, sourceVersion, currentVersion int64, modelIDs []uuid.UUID) (string, error) {
	digest := sha256.New()
	queries := []struct {
		sql  string
		args []any
	}{
		{`SELECT 'catalog-diff|'||jsonb_build_array(catalog_version,model_id,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available,replacement_model_id,new_session_cutoff_at,continuation_cutoff_at,force_cancel_inflight)::text FROM model_catalog_entries WHERE catalog_version=ANY($1::bigint[]) ORDER BY catalog_version,model_id`, []any{[]int64{sourceVersion, currentVersion}}},
		{`SELECT 'route|'||jsonb_build_array(route.id,route.model,route.protocol_code,route.route_generation,'PRESENT',route.health_status)::text FROM routing_policies route JOIN model_identities identity ON route.model IN (identity.id::text,identity.upstream_model_ref) WHERE identity.id=ANY($1::uuid[]) ORDER BY route.id`, []any{modelIDs}},
		{`SELECT 'provisioning|'||jsonb_build_array(id,status,desired_generation,actual_generation,lease_until,active_token_account_id,active_credential_id,upstream_model_ref,protocol_code,protocol_version_id)::text FROM route_provisionings WHERE model_id=ANY($1::uuid[]) ORDER BY id`, []any{modelIDs}},
		{`SELECT 'request|'||jsonb_build_array(id,session_id,status,execution_status)::text FROM usage_requests WHERE model_id=ANY($1::uuid[]) AND execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING') ORDER BY id`, []any{modelIDs}},
		{`SELECT DISTINCT 'credential|'||jsonb_build_array(credential.id,credential.version,credential.status,credential.validation_status,credential.provider,(SELECT jsonb_agg(value ORDER BY value) FROM unnest(credential.allowed_models) value),route.model,route.protocol_code)::text FROM routing_policies route JOIN model_identities identity ON route.model IN (identity.id::text,identity.upstream_model_ref) JOIN provider_credentials credential ON credential.token_account_id=route.token_account_id WHERE identity.id=ANY($1::uuid[]) ORDER BY 1`, []any{modelIDs}},
	}
	for _, item := range queries {
		if err := appendImpactKeyRows(ctx, tx, digest, item.sql, item.args...); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func tokenAccountImpactKeySet(ctx context.Context, tx pgx.Tx, accountID uuid.UUID, model string, credentialID *uuid.UUID) (string, error) {
	digest := sha256.New()
	modelAll := model == ""
	credentialAll := credentialID == nil
	credential := uuid.Nil
	if credentialID != nil {
		credential = *credentialID
	}
	queries := []struct {
		sql  string
		args []any
	}{
		{`SELECT 'account|'||jsonb_build_array(id,status,updated_at,owner_type,owner_id)::text FROM token_accounts WHERE id=$1`, []any{accountID}},
		{`SELECT 'route|'||jsonb_build_array(id,model,protocol_code,route_generation,'PRESENT',health_status)::text FROM routing_policies WHERE token_account_id=$1 AND ($2 OR model=$3) ORDER BY id`, []any{accountID, modelAll, model}},
		{`SELECT 'provisioning|'||jsonb_build_array(id,status,desired_generation,actual_generation,lease_until,active_token_account_id,active_credential_id,upstream_model_ref,protocol_code,protocol_version_id)::text FROM route_provisionings WHERE active_token_account_id=$1 AND ($2 OR upstream_model_ref=$3) AND ($4 OR active_credential_id=$5) ORDER BY id`, []any{accountID, modelAll, model, credentialAll, credential}},
		{`SELECT 'request|'||jsonb_build_array(id,session_id,status,execution_status)::text FROM usage_requests WHERE token_account_id=$1 AND ($2 OR model=$3) AND ($4 OR credential_id=$5) AND execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING') ORDER BY id`, []any{accountID, modelAll, model, credentialAll, credential}},
		{`SELECT 'credential|'||jsonb_build_array(id,version,status,validation_status,provider,(SELECT jsonb_agg(value ORDER BY value) FROM unnest(allowed_models) value))::text FROM provider_credentials WHERE token_account_id=$1 ORDER BY id`, []any{accountID}},
		{`WITH serving AS ` + canServeCoverageRelation + `,` + impactAffectedCoverageCTE + `,` + alternativeAccountCandidatesCTE + `
		 SELECT 'qualifying-coverage|'||jsonb_build_array(serving.provisioning_id,serving.provisioning_status,serving.scope_type,serving.scope_id,serving.wallet_id,serving.model_id,serving.upstream_model_ref,serving.protocol_code,serving.protocol_version_id,serving.actual_generation,serving.account_id,serving.owner_type,serving.owner_id,serving.owner_organization_id,serving.owner_unit_id,serving.account_status,serving.required_provider,serving.credential_id,serving.credential_version,serving.credential_status,serving.validation_status,serving.credential_provider,(SELECT jsonb_agg(value ORDER BY value) FROM unnest(serving.allowed_models) value),serving.route_id,serving.route_health_status,serving.route_generation)::text
		 FROM serving WHERE (serving.account_id=$1 OR serving.account_id IN (SELECT account_id FROM alternative_accounts)) AND EXISTS(SELECT 1 FROM affected WHERE serving.scope_type=affected.scope_type AND serving.scope_id=affected.scope_id AND serving.wallet_id=affected.wallet_id AND serving.model_id=affected.model_id AND serving.upstream_model_ref=affected.upstream_model_ref AND serving.protocol_code=affected.protocol_code AND serving.protocol_version_id=affected.protocol_version_id)
		 ORDER BY serving.scope_type,serving.scope_id,serving.wallet_id,serving.model_id,serving.protocol_version_id,serving.account_id,serving.credential_id,serving.route_id,serving.provisioning_id`, []any{accountID, model, credentialID}},
	}
	for _, item := range queries {
		if err := appendImpactKeyRows(ctx, tx, digest, item.sql, item.args...); err != nil {
			return "", err
		}
	}
	if credentialID != nil {
		if err := appendImpactKeyRows(ctx, tx, digest, `WITH serving AS `+canServeCoverageRelation+`,`+impactAffectedCoverageCTE+`,`+alternativeCredentialCandidatesCTE+`
			SELECT 'alternative-credential|'||jsonb_build_array(candidate.credential_id,candidate.credential_version,candidate.credential_status,candidate.validation_status,candidate.provider,(SELECT jsonb_agg(value ORDER BY value) FROM unnest(candidate.allowed_models) value))::text FROM alternative_credentials candidate ORDER BY candidate.credential_id`, accountID, model, credentialID); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func uniqueUUIDs(values []uuid.UUID) []uuid.UUID {
	seen := map[uuid.UUID]struct{}{}
	result := make([]uuid.UUID, 0, len(values))
	for _, value := range values {
		if value != uuid.Nil {
			if _, exists := seen[value]; !exists {
				seen[value] = struct{}{}
				result = append(result, value)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].String() < result[j].String() })
	return result
}

func commitReadOnly(ctx context.Context, tx pgx.Tx) error { return tx.Commit(ctx) }
func valueInt64(value *int64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatInt(*value, 10)
}
func valueInt(value *int) string {
	if value == nil {
		return ""
	}
	return strconv.Itoa(*value)
}
func valueUUID(value *uuid.UUID) string {
	if value == nil {
		return ""
	}
	return value.String()
}
func valueTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}
