package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TokenCommandPrecheckRequest intentionally excludes reason and idempotency.
// It binds the immutable action payload to a fresh, authoritative resource
// snapshot before the operator can dispatch a governed command.
type TokenCommandPrecheckRequest struct {
	CommandType        string          `json:"command_type"`
	TargetResourceType string          `json:"target_resource_type"`
	TargetResourceID   string          `json:"target_resource_id"`
	Command            json.RawMessage `json:"command"`
}

type TokenCommandPrecheckResult struct {
	ImpactPrecheckResult
	ExpectedVersions json.RawMessage `json:"expected_versions"`
}

type commandPrecheckSnapshot struct {
	LocalEvidence    *ImpactLocalEvidence
	Status           string
	ExpectedVersions map[string]any
	EvidenceValues   []string
	PriceDimensions  []ImpactPriceQuery
	Blockers         []string
	CatalogDiff      *CatalogDifference
}

type runtimeGovernedCommand struct {
	ResourceID            uuid.UUID
	ExpectedVersion       time.Time
	TargetGeneration      int64
	EvidenceHighWatermark int64
}

type modelPublishCommand struct {
	ModelID              uuid.UUID  `json:"model_id"`
	PublishMode          string     `json:"publish_mode,omitempty"`
	LifecyclePlanID      *uuid.UUID `json:"lifecycle_plan_id,omitempty"`
	LifecyclePlanVersion *int64     `json:"lifecycle_plan_version,omitempty"`
	EffectiveAt          *time.Time `json:"effective_at,omitempty"`
}

func parseModelPublishCommand(raw json.RawMessage) (modelPublishCommand, error) {
	var command modelPublishCommand
	if decodeCommand(raw, &command) != nil || command.ModelID == uuid.Nil {
		return modelPublishCommand{}, ErrTokenCommandInvalid
	}
	// The pre-APT-134 shape remains the canonical immediate compatibility
	// request. New callers must state the discriminator explicitly.
	if command.PublishMode == "" {
		command.PublishMode = "IMMEDIATE"
	}
	switch command.PublishMode {
	case "IMMEDIATE":
		if command.LifecyclePlanID != nil || command.LifecyclePlanVersion != nil || command.EffectiveAt != nil {
			return modelPublishCommand{}, ErrTokenCommandInvalid
		}
	case "SCHEDULED":
		if command.LifecyclePlanID == nil || *command.LifecyclePlanID == uuid.Nil || command.LifecyclePlanVersion == nil || *command.LifecyclePlanVersion < 1 || command.EffectiveAt == nil || command.EffectiveAt.IsZero() {
			return modelPublishCommand{}, ErrTokenCommandInvalid
		}
		// Portal Date serialisation uses millisecond precision while Go omits a
		// zero fractional component. Freeze scheduled commands at UTC-second
		// precision before either side hashes or persists the envelope.
		effective := command.EffectiveAt.UTC().Truncate(time.Second)
		command.EffectiveAt = &effective
	default:
		return modelPublishCommand{}, ErrTokenCommandInvalid
	}
	return command, nil
}

var directCommandPrecheckTypes = map[string]string{
	"TOKEN_PROTOCOL_DRAFT_CREATE":     "PROTOCOL",
	"TOKEN_PROTOCOL_PUBLISH":          "PROTOCOL",
	"TOKEN_FORMULA_DRAFT_CREATE":      "FORMULA",
	"TOKEN_FORMULA_PUBLISH":           "FORMULA",
	"TOKEN_MODEL_DRAFT_SAVE":          "MODEL",
	"TOKEN_MODEL_PUBLISH":             "MODEL",
	"TOKEN_MODEL_CATALOG_SYNC":        "MODEL_CATALOG",
	"TOKEN_ACCOUNT_CREATE":            "TOKEN_ACCOUNT",
	"TOKEN_ACCOUNT_STATUS_CHANGE":     "TOKEN_ACCOUNT",
	"TOKEN_ROUTE_UPSERT":              "TOKEN_ROUTE",
	"TOKEN_ROUTE_DELETE":              "TOKEN_ROUTE",
	"TOKEN_ROUTE_PROVISIONING_REPAIR": "PROVISIONING",
	"TOKEN_CREDENTIAL_VALIDATE":       "PROVIDER_CREDENTIAL",
}

func (s *Service) adminTokenCommandPrecheck(w http.ResponseWriter, r *http.Request) {
	var input TokenCommandPrecheckRequest
	if api.DecodeJSON(w, r, &input) != nil {
		return
	}
	originalCommandType, originalResourceType, originalResourceID := input.CommandType, input.TargetResourceType, input.TargetResourceID
	input.CommandType = strings.ToUpper(strings.TrimSpace(input.CommandType))
	input.TargetResourceType = strings.ToUpper(strings.TrimSpace(input.TargetResourceType))
	input.TargetResourceID = strings.TrimSpace(input.TargetResourceID)
	expectedType, supported := directCommandPrecheckTypes[input.CommandType]
	strictCatalogSync := input.CommandType != "TOKEN_MODEL_CATALOG_SYNC" || originalCommandType == "TOKEN_MODEL_CATALOG_SYNC" && originalResourceType == "MODEL_CATALOG" && originalResourceID == "CURRENT"
	strictAccountCreate := input.CommandType != "TOKEN_ACCOUNT_CREATE" || originalCommandType == "TOKEN_ACCOUNT_CREATE"
	if !supported || !strictCatalogSync || !strictAccountCreate || input.TargetResourceType != expectedType || input.TargetResourceID == "" || containsSensitiveCommandField(input.Command) || validateOwnedCommandPrecheckTarget(input.CommandType, input.TargetResourceID, input.Command) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_COMMAND_PRECHECK", "the governed command precheck is invalid", httpserver.RequestID(r.Context()))
		return
	}
	// APT-123-R2 deliberately ships the Saga seam before APT-138 activates its
	// worker. PRE must reject before identity/Account/Billing/database reads so
	// a disabled capability cannot be used as an authority or existence oracle.
	if input.CommandType == "TOKEN_ACCOUNT_CREATE" && !s.tokenAccountCreateReady.Load() {
		api.WriteError(w, http.StatusServiceUnavailable, "TOKEN_ACCOUNT_CREATE_NOT_ACTIVE", "Token Account creation is temporarily unavailable", httpserver.RequestID(r.Context()))
		return
	}
	principal, ok := identity.FromContext(r.Context())
	if !ok {
		api.WriteError(w, http.StatusUnauthorized, "AUTH_REQUIRED", "a valid bearer token is required", httpserver.RequestID(r.Context()))
		return
	}
	if _, _, err := s.authorizedTokenCommandActor(r.Context(), principal, input.CommandType); errors.Is(err, authorization.ErrPermissionDenied) {
		api.WriteError(w, http.StatusForbidden, "PERMISSION_DENIED", "the requested permission is not granted", httpserver.RequestID(r.Context()))
		return
	} else if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "AUTHORIZATION_UNAVAILABLE", "authorization is temporarily unavailable", httpserver.RequestID(r.Context()))
		return
	}
	result, err := s.buildTokenCommandPrecheck(r.Context(), input)
	if err != nil {
		status, code := http.StatusConflict, "COMMAND_PRECHECK_CHANGED"
		if errors.Is(err, ErrTokenAccountPolicyUnavailable) {
			status, code = http.StatusServiceUnavailable, "TOKEN_ACCOUNT_POLICY_UNAVAILABLE"
		} else if errors.Is(err, ErrTokenAccountPolicyConfigMismatch) {
			status, code = http.StatusServiceUnavailable, "TOKEN_ACCOUNT_POLICY_CONFIG_MISMATCH"
		} else if errors.Is(err, ErrNotFound) {
			status, code = http.StatusNotFound, "NOT_FOUND"
		} else if errors.Is(err, ErrForbidden) {
			status, code = http.StatusForbidden, "PERMISSION_DENIED"
		} else if errors.Is(err, ErrTokenCommandInvalid) {
			status, code = http.StatusBadRequest, "INVALID_COMMAND_PRECHECK"
		} else if !errors.Is(err, ErrCatalogVersionConflict) && !errors.Is(err, ErrProtocolFormulaPrecheck) {
			status, code = http.StatusServiceUnavailable, "COMMAND_PRECHECK_UNAVAILABLE"
		}
		api.WriteError(w, status, code, "the governed command precheck could not be established", httpserver.RequestID(r.Context()))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) buildTokenCommandPrecheck(ctx context.Context, input TokenCommandPrecheckRequest) (TokenCommandPrecheckResult, error) {
	repository, ok := s.repository.(*PostgresRepository)
	if !ok {
		return TokenCommandPrecheckResult{}, errors.New("command precheck repository unavailable")
	}
	commandType := strings.ToUpper(strings.TrimSpace(input.CommandType))
	expectedType, supported := directCommandPrecheckTypes[commandType]
	resourceType, resourceID := strings.TrimSpace(input.TargetResourceType), strings.TrimSpace(input.TargetResourceID)
	if !supported || expectedType != strings.ToUpper(resourceType) || resourceID == "" || validateOwnedCommandPrecheckTarget(commandType, resourceID, input.Command) != nil || commandType == "TOKEN_MODEL_CATALOG_SYNC" && (input.CommandType != commandType || input.TargetResourceType != "MODEL_CATALOG" || input.TargetResourceID != "CURRENT") {
		return TokenCommandPrecheckResult{}, ErrTokenCommandInvalid
	}
	if isProtocolFormulaGovernedCommand(commandType) {
		canonicalCommand, err := canonicalizeProtocolFormulaCommand(commandType, input.Command)
		if err != nil {
			return TokenCommandPrecheckResult{}, ErrTokenCommandInvalid
		}
		input.Command = canonicalCommand
	}
	if commandType == "TOKEN_MODEL_PUBLISH" {
		command, err := parseModelPublishCommand(input.Command)
		if err != nil {
			return TokenCommandPrecheckResult{}, ErrTokenCommandInvalid
		}
		input.Command, err = json.Marshal(command)
		if err != nil {
			return TokenCommandPrecheckResult{}, ErrTokenCommandInvalid
		}
		input.Command, err = canonicalJSON(input.Command)
		if err != nil {
			return TokenCommandPrecheckResult{}, ErrTokenCommandInvalid
		}
	}
	if commandType == "TOKEN_MODEL_DRAFT_SAVE" || commandType == "TOKEN_ACCOUNT_CREATE" || commandType == "TOKEN_ACCOUNT_STATUS_CHANGE" {
		canonical, err := canonicalizeModelAccountEnvelope(TokenCommandEnvelope{CommandType: commandType, TargetResourceID: resourceID, Command: input.Command})
		if err != nil {
			return TokenCommandPrecheckResult{}, ErrTokenCommandInvalid
		}
		input.Command = canonical.Command
	}
	if commandType == "TOKEN_ACCOUNT_CREATE" {
		return s.buildAccountCreatePrecheck(ctx, input)
	}
	var snapshot commandPrecheckSnapshot
	var observedAt time.Time
	if commandType == "TOKEN_MODEL_CATALOG_SYNC" {
		if s.catalogSync == nil {
			return TokenCommandPrecheckResult{}, errors.New("catalog synchronizer unavailable")
		}
		preview, err := s.catalogSync.Preview(ctx)
		if err != nil {
			return TokenCommandPrecheckResult{}, err
		}
		observedAt = preview.ObservedAt
		snapshot = catalogSyncCommandSnapshot(preview)
	} else {
		tx, err := repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return TokenCommandPrecheckResult{}, err
		}
		defer tx.Rollback(ctx)
		if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&observedAt); err != nil {
			return TokenCommandPrecheckResult{}, err
		}
		snapshot, err = loadCommandPrecheckSnapshot(ctx, tx, commandType, input.TargetResourceID, input.Command)
		if err != nil {
			return TokenCommandPrecheckResult{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return TokenCommandPrecheckResult{}, err
		}
	}
	billingObservedAt, billingValues, err := s.validateCommandBillingPrecheck(ctx, commandType, input.TargetResourceID, input.Command)
	if err != nil {
		return TokenCommandPrecheckResult{}, err
	}
	snapshot.EvidenceValues = append(snapshot.EvidenceValues, billingValues...)
	var priceCoverage *ImpactPriceSummary
	if len(snapshot.PriceDimensions) > 0 {
		price, priceErr := s.loadImpactPriceCoverage(ctx, snapshot.PriceDimensions)
		if priceErr != nil {
			return TokenCommandPrecheckResult{}, priceErr
		}
		priceCoverage = &price
		billingObserved := price.BillingObservedAt.UTC()
		billingObservedAt = &billingObserved
		snapshot.ExpectedVersions["financial_fact_revision"] = price.FinancialFactRevision
		snapshot.ExpectedVersions["price_set_fingerprint"] = price.PriceSetFingerprint
		snapshot.EvidenceValues = append(snapshot.EvidenceValues, "billing_revision="+strconv.FormatInt(price.FinancialFactRevision, 10), "price_set_fingerprint="+price.PriceSetFingerprint)
		if !price.FullyCovered {
			snapshot.Blockers = append(snapshot.Blockers, "PRICE_COVERAGE_MISSING")
		}
	}
	expectedJSON, fingerprint, err := commandPrecheckFingerprint(commandType, expectedType, input.TargetResourceID, input.Command, snapshot)
	if err != nil {
		return TokenCommandPrecheckResult{}, err
	}
	evidenceDigest := sha256.Sum256([]byte(strings.Join(snapshot.EvidenceValues, "\x00")))
	evidenceHash := hex.EncodeToString(evidenceDigest[:])
	result := TokenCommandPrecheckResult{
		ImpactPrecheckResult: ImpactPrecheckResult{
			Action: commandType, Evidence: ImpactLocalEvidence{
				ResourceType: expectedType, ResourceID: input.TargetResourceID, CurrentStatus: snapshot.Status,
				CurrentVersion: ImpactResourceVersion{EvidenceETag: `"impact-` + evidenceHash + `"`, EvidenceSetSHA256: evidenceHash},
				Dependencies:   ImpactDependencySummary{}, ObservedAt: observedAt,
			}, Blockers: append([]string(nil), snapshot.Blockers...), TokenObservedAt: observedAt.UTC(),
			SnapshotSemantics: "INDEPENDENT_TOKEN_AND_BILLING_SNAPSHOTS", PrecheckFingerprint: fingerprint, RequiresCASRevalidation: true,
		}, ExpectedVersions: expectedJSON,
	}
	result.CatalogDiff = snapshot.CatalogDiff
	if snapshot.LocalEvidence != nil {
		result.Evidence = *snapshot.LocalEvidence
	}
	result.PriceCoverage = priceCoverage
	if billingObservedAt != nil {
		observed := billingObservedAt.UTC()
		result.BillingObservedAt = &observed
	}
	return result, nil
}

func catalogSyncCommandSnapshot(preview CatalogSyncPreview) commandPrecheckSnapshot {
	return commandPrecheckSnapshot{Status: "READY", ExpectedVersions: map[string]any{
		"expected_catalog_version": preview.CatalogVersion, "adapter_observed_version": preview.AdapterObservedVersion,
		"catalog_diff_fingerprint": preview.DiffFingerprint,
	}, EvidenceValues: []string{strconv.FormatInt(preview.CatalogVersion, 10), preview.AdapterObservedVersion, preview.DiffFingerprint, strconv.Itoa(preview.Added), strconv.Itoa(preview.Missing)}, CatalogDiff: preview.Difference}
}

func commandPrecheckFingerprint(commandType, resourceType, resourceID string, raw json.RawMessage, snapshot commandPrecheckSnapshot) (json.RawMessage, string, error) {
	expectedJSON, err := json.Marshal(snapshot.ExpectedVersions)
	if err != nil {
		return nil, "", err
	}
	canonicalCommand, err := canonicalJSON(raw)
	if err != nil {
		return nil, "", ErrTokenCommandInvalid
	}
	values := []string{commandType, resourceType, resourceID, string(expectedJSON), string(canonicalCommand)}
	values = append(values, snapshot.EvidenceValues...)
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return expectedJSON, hex.EncodeToString(digest[:]), nil
}

func validateOwnedCommandPrecheck(commandType string, raw json.RawMessage) error {
	switch strings.ToUpper(strings.TrimSpace(commandType)) {
	case "TOKEN_PROTOCOL_DRAFT_CREATE":
		var command struct {
			ProtocolCode  string          `json:"protocol_code"`
			RequestSchema json.RawMessage `json:"request_schema"`
		}
		if decodeCommand(raw, &command) != nil || strings.TrimSpace(command.ProtocolCode) == "" || len(command.RequestSchema) == 0 {
			return ErrTokenCommandInvalid
		}
	case "TOKEN_PROTOCOL_PUBLISH":
		var command struct {
			ExpectedProtocolVersion int64  `json:"expected_protocol_version"`
			DraftETag               string `json:"draft_etag"`
		}
		if decodeCommand(raw, &command) != nil || command.ExpectedProtocolVersion < 1 || !validCommandETag(command.DraftETag) {
			return ErrTokenCommandInvalid
		}
	case "TOKEN_FORMULA_DRAFT_CREATE":
		var command struct {
			RequiredUnitVersions map[string]uuid.UUID `json:"required_unit_versions"`
			Expression           map[string]string    `json:"expression"`
		}
		if decodeCommand(raw, &command) != nil {
			return ErrTokenCommandInvalid
		}
		if _, _, err := normalizeSafeFormula(command.RequiredUnitVersions, command.Expression); err != nil {
			return ErrTokenCommandInvalid
		}
	case "TOKEN_FORMULA_PUBLISH":
		var command struct {
			ExpectedFormulaVersion int64  `json:"expected_formula_version"`
			DraftETag              string `json:"draft_etag"`
		}
		if decodeCommand(raw, &command) != nil || command.ExpectedFormulaVersion < 1 || !validCommandETag(command.DraftETag) {
			return ErrTokenCommandInvalid
		}
	case "TOKEN_MODEL_PUBLISH":
		_, err := parseModelPublishCommand(raw)
		return err
	case "TOKEN_MODEL_DRAFT_SAVE":
		// Target equality is checked by validateOwnedCommandPrecheckTarget.
		var command governedModelDraftCommand
		if decodeCommand(raw, &command) != nil || command.ModelID == uuid.Nil {
			return ErrTokenCommandInvalid
		}
	case "TOKEN_ACCOUNT_CREATE":
		var command governedAccountCreateCommand
		if decodeCommand(raw, &command) != nil || command.OwnerID == uuid.Nil {
			return ErrTokenCommandInvalid
		}
	case "TOKEN_ACCOUNT_STATUS_CHANGE":
		var command governedAccountStatusCommand
		if decodeCommand(raw, &command) != nil {
			return ErrTokenCommandInvalid
		}
	case "TOKEN_MODEL_CATALOG_SYNC":
		var command struct{}
		if decodeCommand(raw, &command) != nil {
			return ErrTokenCommandInvalid
		}
	case "TOKEN_ROUTE_PROVISIONING_REPAIR":
		_, err := parseRuntimeGovernedCommand(commandType, "", raw)
		return err
	case "TOKEN_CREDENTIAL_VALIDATE":
		var command struct {
			TokenAccountID uuid.UUID `json:"token_account_id"`
			CredentialID   uuid.UUID `json:"credential_id"`
		}
		if decodeCommand(raw, &command) != nil || command.TokenAccountID == uuid.Nil || command.CredentialID == uuid.Nil {
			return ErrTokenCommandInvalid
		}
	default:
		return ErrTokenCommandInvalid
	}
	return nil
}

func validateOwnedCommandPrecheckTarget(commandType, targetID string, raw json.RawMessage) error {
	if isRouteGovernedCommand(commandType) {
		_, err := parseGovernedRouteCommand(commandType, targetID, raw)
		return err
	}
	if err := validateOwnedCommandPrecheck(commandType, raw); err != nil {
		return err
	}
	commandType = strings.ToUpper(strings.TrimSpace(commandType))
	if commandType == "TOKEN_ROUTE_PROVISIONING_REPAIR" {
		_, err := parseRuntimeGovernedCommand(commandType, targetID, raw)
		return err
	}
	if commandType == "TOKEN_CREDENTIAL_VALIDATE" {
		var command struct {
			TokenAccountID uuid.UUID `json:"token_account_id"`
			CredentialID   uuid.UUID `json:"credential_id"`
		}
		id, err := uuid.Parse(strings.TrimSpace(targetID))
		if err != nil || decodeCommand(raw, &command) != nil || command.CredentialID != id {
			return ErrTokenCommandInvalid
		}
	}
	if commandType == "TOKEN_MODEL_DRAFT_SAVE" {
		_, err := parseGovernedModelDraft(targetID, raw)
		return err
	}
	if commandType == "TOKEN_ACCOUNT_CREATE" {
		_, err := parseGovernedAccountCreate(targetID, raw)
		return err
	}
	if commandType == "TOKEN_ACCOUNT_STATUS_CHANGE" {
		_, err := parseGovernedAccountStatus(targetID, raw)
		return err
	}
	if commandType != "TOKEN_MODEL_PUBLISH" {
		return nil
	}
	command, parseErr := parseModelPublishCommand(raw)
	modelID, err := uuid.Parse(strings.TrimSpace(targetID))
	if err != nil || parseErr != nil || command.ModelID != modelID {
		return ErrTokenCommandInvalid
	}
	return nil
}

// parseRuntimeGovernedCommand is the only parser used by runtime precheck,
// command persistence and the domain mutation adapter. In particular the
// boolean is a pointer so an omitted replay_model_request cannot be confused
// with the only permitted value, false.
func parseRuntimeGovernedCommand(commandType, targetID string, raw json.RawMessage) (runtimeGovernedCommand, error) {
	var result runtimeGovernedCommand
	canonicalTarget := strings.TrimSpace(targetID)
	switch strings.ToUpper(strings.TrimSpace(commandType)) {
	case "TOKEN_ROUTE_PROVISIONING_REPAIR":
		var command struct {
			ProvisioningID              string `json:"provisioning_id"`
			ExpectedProvisioningVersion string `json:"expected_provisioning_version"`
			TargetGeneration            int64  `json:"target_generation"`
			ReplayModelRequest          *bool  `json:"replay_model_request"`
		}
		if decodeCommand(raw, &command) != nil || command.ReplayModelRequest == nil || *command.ReplayModelRequest || command.TargetGeneration < 1 {
			return runtimeGovernedCommand{}, ErrTokenCommandInvalid
		}
		id, err := uuid.Parse(command.ProvisioningID)
		version, versionErr := time.Parse(time.RFC3339Nano, command.ExpectedProvisioningVersion)
		if err != nil || id == uuid.Nil || command.ProvisioningID != id.String() || versionErr != nil || command.ExpectedProvisioningVersion != version.UTC().Format(time.RFC3339Nano) || canonicalTarget != "" && canonicalTarget != id.String() {
			return runtimeGovernedCommand{}, ErrTokenCommandInvalid
		}
		result = runtimeGovernedCommand{ResourceID: id, ExpectedVersion: version.UTC(), TargetGeneration: command.TargetGeneration}
	default:
		return runtimeGovernedCommand{}, ErrTokenCommandInvalid
	}
	return result, nil
}

func (s *Service) validateCommandBillingPrecheck(ctx context.Context, commandType, targetID string, raw json.RawMessage) (*time.Time, []string, error) {
	unitSets := []map[string]uuid.UUID{}
	switch commandType {
	case "TOKEN_FORMULA_DRAFT_CREATE":
		var command struct {
			RequiredUnitVersions map[string]uuid.UUID `json:"required_unit_versions"`
			Expression           map[string]string    `json:"expression"`
		}
		if decodeCommand(raw, &command) != nil {
			return nil, nil, ErrTokenCommandInvalid
		}
		required, _, err := normalizeSafeFormula(command.RequiredUnitVersions, command.Expression)
		if err != nil {
			return nil, nil, ErrProtocolFormulaPrecheck
		}
		unitSets = append(unitSets, required)
	case "TOKEN_FORMULA_PUBLISH":
		id, err := uuid.Parse(targetID)
		if err != nil {
			return nil, nil, ErrTokenCommandInvalid
		}
		repository, ok := s.repository.(interface {
			FormulaDraft(context.Context, uuid.UUID) (ProtocolFormulaVersion, error)
		})
		if !ok {
			return nil, nil, errors.New("formula repository unavailable")
		}
		formula, err := repository.FormulaDraft(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		unitSets = append(unitSets, formula.RequiredUnitVersions)
	case "TOKEN_PROTOCOL_PUBLISH":
		id, err := uuid.Parse(targetID)
		if err != nil {
			return nil, nil, ErrTokenCommandInvalid
		}
		repository, ok := s.repository.(interface {
			ProtocolPublishDependencies(context.Context, uuid.UUID) (ProtocolDefinition, []ProtocolFormulaVersion, error)
		})
		if !ok {
			return nil, nil, errors.New("protocol repository unavailable")
		}
		_, formulas, err := repository.ProtocolPublishDependencies(ctx, id)
		if err != nil {
			return nil, nil, err
		}
		for _, formula := range formulas {
			unitSets = append(unitSets, formula.RequiredUnitVersions)
		}
	default:
		return nil, nil, nil
	}
	return s.validateFormulaUnitSets(ctx, unitSets)
}

func (s *Service) validateFormulaUnitSets(ctx context.Context, unitSets []map[string]uuid.UUID) (*time.Time, []string, error) {
	combined := map[string]uuid.UUID{}
	for _, required := range unitSets {
		for rawCode, id := range required {
			code := strings.ToUpper(strings.TrimSpace(rawCode))
			if existing, found := combined[code]; code == "" || id == uuid.Nil || found && existing != id {
				return nil, nil, ErrProtocolFormulaPrecheck
			}
			combined[code] = id
		}
	}
	if len(combined) == 0 {
		return nil, nil, nil
	}
	validator, ok := s.billing.(FormulaDraftValidationClient)
	if !ok {
		return nil, nil, errors.New("Billing validation unavailable")
	}
	validation, err := validator.ValidateFormulaDraftUnits(ctx, combined)
	if err != nil {
		return nil, nil, err
	}
	if validateFormulaDraftValidation(combined, validation) != nil || validation.ObservedAt.IsZero() || !validImpactFingerprint(validation.UnitSetFingerprint) {
		return nil, nil, ErrProtocolFormulaPrecheck
	}
	values := stableUnitMappingValues(combined)
	for _, unit := range validation.UnitVersions {
		if unit.Status != "ACTIVE" {
			return nil, nil, ErrProtocolFormulaPrecheck
		}
		values = append(values, strings.ToUpper(strings.TrimSpace(unit.UnitCode))+"="+unit.UnitDefinitionID.String()+":"+strconv.FormatInt(unit.Version, 10)+":"+unit.Status)
	}
	values = append(values, "billing_unit_set_fingerprint="+validation.UnitSetFingerprint)
	sort.Strings(values)
	observedAt := validation.ObservedAt.UTC()
	return &observedAt, values, nil
}

func canonicalJSON(raw json.RawMessage) (json.RawMessage, error) {
	var value any
	if len(raw) == 0 || decodeExactCommandJSON(raw, &value) != nil {
		return nil, ErrTokenCommandInvalid
	}
	return json.Marshal(value)
}

func catalogDraftFingerprint(draft CatalogDraft) string {
	payload, _ := json.Marshal(struct {
		ModelID             uuid.UUID `json:"model_id"`
		BaseCatalogVersion  int64     `json:"base_catalog_version"`
		DraftVersion        int64     `json:"draft_version"`
		DisplayName         string    `json:"display_name"`
		Description         string    `json:"description"`
		Aliases             []string  `json:"aliases"`
		ProtocolCode        string    `json:"protocol_code"`
		ProtocolVersion     int64     `json:"protocol_version"`
		ContextLength       int64     `json:"context_length"`
		MaximumOutputTokens int64     `json:"maximum_output_tokens"`
		SupportedModalities []string  `json:"supported_modalities"`
		TokenizerVersion    string    `json:"tokenizer_version"`
	}{draft.ModelID, draft.BaseCatalogVersion, draft.DraftVersion, draft.DisplayName, draft.Description, draft.Aliases, draft.ProtocolCode, draft.ProtocolVersion, draft.ContextLength, draft.MaximumOutputTokens, draft.SupportedModalities, draft.TokenizerVersion})
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:])
}

func decodeCommand(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return ErrTokenCommandInvalid
		}
		return err
	}
	return nil
}

func loadCommandPrecheckSnapshot(ctx context.Context, tx pgx.Tx, commandType, targetID string, raw json.RawMessage) (commandPrecheckSnapshot, error) {
	if isRouteGovernedCommand(commandType) {
		return routeCommandPrecheckTx(ctx, tx, commandType, targetID, raw)
	}
	switch commandType {
	case "TOKEN_PROTOCOL_DRAFT_CREATE":
		var command struct {
			ProtocolCode  string          `json:"protocol_code"`
			RequestSchema json.RawMessage `json:"request_schema"`
		}
		if decodeCommand(raw, &command) != nil || strings.TrimSpace(command.ProtocolCode) == "" || len(command.RequestSchema) == 0 {
			return commandPrecheckSnapshot{}, ErrTokenCommandInvalid
		}
		code := strings.ToUpper(strings.TrimSpace(command.ProtocolCode))
		if targetID != code {
			return commandPrecheckSnapshot{}, ErrTokenCommandInvalid
		}
		var head int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version),0) FROM protocol_definitions WHERE protocol_code=$1`, code).Scan(&head); err != nil {
			return commandPrecheckSnapshot{}, err
		}
		return commandPrecheckSnapshot{Status: "DRAFTABLE", ExpectedVersions: map[string]any{"protocol_code": code, "expected_protocol_head_version": head}, EvidenceValues: []string{code, strconv.FormatInt(head, 10)}}, nil
	case "TOKEN_PROTOCOL_PUBLISH":
		id, err := uuid.Parse(targetID)
		if err != nil {
			return commandPrecheckSnapshot{}, ErrTokenCommandInvalid
		}
		var command struct {
			ExpectedProtocolVersion int64  `json:"expected_protocol_version"`
			DraftETag               string `json:"draft_etag"`
		}
		if decodeCommand(raw, &command) != nil {
			return commandPrecheckSnapshot{}, ErrTokenCommandInvalid
		}
		facts, err := loadProtocolPublicationFacts(ctx, tx, id)
		if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		protocol := facts.Protocol
		etag := protocolDefinitionETag(protocol)
		blockers := []string{}
		if protocol.Status != "DRAFT" || command.ExpectedProtocolVersion != protocol.Version || strings.ToLower(strings.TrimSpace(command.DraftETag)) != etag {
			blockers = append(blockers, "PROTOCOL_DRAFT_CHANGED")
		}
		return commandPrecheckSnapshot{Status: protocol.Status, ExpectedVersions: map[string]any{"protocol_definition_id": id, "expected_protocol_version": protocol.Version, "draft_etag": etag, "formula_set_sha256": facts.FormulaSetSHA256, "model_draft_set_sha256": facts.ModelDraftSetSHA256, "model_draft_reference_count": facts.ModelDraftReferenceCount}, EvidenceValues: []string{protocol.ProtocolCode, strconv.FormatInt(protocol.Version, 10), protocol.Status, etag, facts.FormulaSetSHA256, facts.ModelDraftSetSHA256, strconv.FormatInt(facts.ModelDraftReferenceCount, 10)}, Blockers: blockers}, nil
	case "TOKEN_FORMULA_DRAFT_CREATE":
		id, err := uuid.Parse(targetID)
		if err != nil {
			return commandPrecheckSnapshot{}, ErrTokenCommandInvalid
		}
		var command struct {
			RequiredUnitVersions map[string]uuid.UUID `json:"required_unit_versions"`
			Expression           map[string]string    `json:"expression"`
		}
		if decodeCommand(raw, &command) != nil || len(command.RequiredUnitVersions) == 0 || len(command.Expression) == 0 {
			return commandPrecheckSnapshot{}, ErrTokenCommandInvalid
		}
		var protocolVersion, formulaHead int64
		var code, status string
		err = tx.QueryRow(ctx, `SELECT p.protocol_code,p.version,p.status,COALESCE((SELECT max(f.version) FROM protocol_formula_versions f WHERE f.protocol_definition_id=p.id),0) FROM protocol_definitions p WHERE p.id=$1`, id).Scan(&code, &protocolVersion, &status, &formulaHead)
		if errors.Is(err, pgx.ErrNoRows) {
			return commandPrecheckSnapshot{}, ErrNotFound
		}
		if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		blockers := []string{}
		if status != "PUBLISHED" {
			blockers = append(blockers, "PROTOCOL_NOT_PUBLISHED")
		}
		return commandPrecheckSnapshot{Status: status, ExpectedVersions: map[string]any{"protocol_definition_id": id, "expected_protocol_version": protocolVersion, "expected_formula_head_version": formulaHead}, EvidenceValues: []string{code, strconv.FormatInt(protocolVersion, 10), strconv.FormatInt(formulaHead, 10)}, Blockers: blockers}, nil
	case "TOKEN_FORMULA_PUBLISH":
		id, err := uuid.Parse(targetID)
		if err != nil {
			return commandPrecheckSnapshot{}, ErrTokenCommandInvalid
		}
		var command struct {
			ExpectedFormulaVersion int64  `json:"expected_formula_version"`
			DraftETag              string `json:"draft_etag"`
		}
		if decodeCommand(raw, &command) != nil {
			return commandPrecheckSnapshot{}, ErrTokenCommandInvalid
		}
		formula, err := loadFormulaPublicationFacts(ctx, tx, id)
		if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		etag := protocolFormulaETag(formula)
		unitDigestBytes, _ := json.Marshal(formula.RequiredUnitVersions)
		expressionDigestBytes, _ := json.Marshal(formula.Expression)
		unitHash := sha256.Sum256(unitDigestBytes)
		expressionHash := sha256.Sum256(expressionDigestBytes)
		blockers := []string{}
		if formula.Status != "DRAFT" || command.ExpectedFormulaVersion != formula.Version || strings.ToLower(strings.TrimSpace(command.DraftETag)) != etag {
			blockers = append(blockers, "FORMULA_DRAFT_CHANGED")
		}
		return commandPrecheckSnapshot{Status: formula.Status, ExpectedVersions: map[string]any{"formula_version_id": id, "expected_formula_version": formula.Version, "draft_etag": etag}, EvidenceValues: []string{formula.ProtocolDefinitionID.String(), formula.ProtocolCode, strconv.FormatInt(formula.Version, 10), formula.Status, etag, hex.EncodeToString(unitHash[:]), hex.EncodeToString(expressionHash[:]), formula.EvaluatorVersion}, Blockers: blockers}, nil
	case "TOKEN_MODEL_DRAFT_SAVE":
		command, err := parseGovernedModelDraft(targetID, raw)
		if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		var current, draftHead int64
		var lifecycle string
		if err = tx.QueryRow(ctx, `SELECT COALESCE(max(version),0) FROM model_catalog_versions`).Scan(&current); err != nil || current < 1 {
			return commandPrecheckSnapshot{}, ErrNotFound
		}
		if err = tx.QueryRow(ctx, `SELECT lifecycle_status FROM model_catalog_entries WHERE catalog_version=$1 AND model_id=$2`, current, command.ModelID).Scan(&lifecycle); errors.Is(err, pgx.ErrNoRows) {
			return commandPrecheckSnapshot{}, ErrNotFound
		} else if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		if err = tx.QueryRow(ctx, `SELECT COALESCE((SELECT draft_version FROM model_catalog_drafts WHERE model_id=$1),0)`, command.ModelID).Scan(&draftHead); err != nil {
			return commandPrecheckSnapshot{}, err
		}
		var protocolID uuid.UUID
		var formulaCount int64
		err = tx.QueryRow(ctx, `SELECT p.id,(SELECT count(*) FROM protocol_formula_versions f WHERE f.protocol_definition_id=p.id AND f.status='PUBLISHED') FROM protocol_definitions p WHERE p.protocol_code=$1 AND p.version=$2 AND p.status='PUBLISHED'`, command.ProtocolCode, command.ProtocolVersion).Scan(&protocolID, &formulaCount)
		if errors.Is(err, pgx.ErrNoRows) {
			return commandPrecheckSnapshot{}, ErrNotFound
		}
		if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		blockers := []string{}
		if command.ExpectedCatalogVersion != current || command.ExpectedDraftVersion != draftHead {
			blockers = append(blockers, "MODEL_DRAFT_CHANGED")
		}
		if lifecycle == "REMOVED" {
			blockers = append(blockers, "MODEL_REMOVED")
		}
		if formulaCount < 1 {
			blockers = append(blockers, "FORMULA_MAPPING_MISSING")
		}
		return commandPrecheckSnapshot{Status: lifecycle, ExpectedVersions: map[string]any{"model_id": command.ModelID, "expected_catalog_version": current, "expected_draft_version": draftHead}, EvidenceValues: []string{command.ModelID.String(), strconv.FormatInt(current, 10), strconv.FormatInt(draftHead, 10), lifecycle, protocolID.String(), strconv.FormatInt(formulaCount, 10), modelAccountEvidenceFingerprint(string(raw))}, Blockers: blockers}, nil
	case "TOKEN_ACCOUNT_STATUS_CHANGE":
		command, err := parseGovernedAccountStatus(targetID, raw)
		if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		accountID, _ := uuid.Parse(targetID)
		var status, mode string
		var version int64
		if err = tx.QueryRow(ctx, `SELECT status,management_mode,management_version FROM token_accounts WHERE id=$1`, accountID).Scan(&status, &mode, &version); errors.Is(err, pgx.ErrNoRows) {
			return commandPrecheckSnapshot{}, ErrNotFound
		} else if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		var routes, provisionings, leases, inflight int64
		if err = tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM routing_policies WHERE token_account_id=$1 AND reference_status='RESOLVED'),(SELECT count(*) FROM route_provisionings WHERE active_token_account_id=$1),(SELECT count(*) FROM route_provisionings WHERE active_token_account_id=$1 AND lease_until>transaction_timestamp()),(SELECT count(*) FROM usage_requests WHERE token_account_id=$1 AND execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING'))`, accountID).Scan(&routes, &provisionings, &leases, &inflight); err != nil {
			return commandPrecheckSnapshot{}, err
		}
		factSet, err := impactRowsFingerprint(ctx, tx, `SELECT value FROM (SELECT jsonb_build_array('ACCOUNT',id,status,management_version,updated_at)::text value FROM token_accounts WHERE id=$1 UNION ALL SELECT jsonb_build_array('ROUTE',id,route_version,route_generation,reference_status)::text FROM routing_policies WHERE token_account_id=$1 UNION ALL SELECT jsonb_build_array('PROVISIONING',id,status,desired_generation,actual_generation,lease_until)::text FROM route_provisionings WHERE active_token_account_id=$1 UNION ALL SELECT jsonb_build_array('USAGE',id,execution_status,execution_version)::text FROM usage_requests WHERE token_account_id=$1 AND execution_status IN ('RECEIVED','HOLD_RESERVING','HOLD_RESERVED','DISPATCH_INTENT_PERSISTED','UPSTREAM_DISPATCHING','STREAMING','SETTLEMENT_PENDING')) facts ORDER BY value`, accountID)
		if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		blockers := []string{}
		if mode != "PLATFORM" || status == "FROZEN" || status == command.TargetStatus {
			blockers = append(blockers, "TOKEN_ACCOUNT_STATUS_NOT_CHANGEABLE")
		}
		return commandPrecheckSnapshot{Status: status, ExpectedVersions: map[string]any{"token_account_id": accountID, "expected_account_version": version, "target_status": command.TargetStatus}, EvidenceValues: []string{accountID.String(), status, strconv.FormatInt(version, 10), command.TargetStatus, factSet, strconv.FormatInt(routes, 10), strconv.FormatInt(provisionings, 10), strconv.FormatInt(leases, 10), strconv.FormatInt(inflight, 10)}, Blockers: blockers}, nil
	case "TOKEN_MODEL_PUBLISH":
		modelID, err := uuid.Parse(targetID)
		if err != nil || modelID == uuid.Nil {
			return commandPrecheckSnapshot{}, ErrTokenCommandInvalid
		}
		command, parseErr := parseModelPublishCommand(raw)
		if parseErr != nil || command.ModelID != modelID {
			return commandPrecheckSnapshot{}, ErrTokenCommandInvalid
		}
		var currentCatalog int64
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version),0) FROM model_catalog_versions`).Scan(&currentCatalog); err != nil || currentCatalog < 1 {
			if err == nil {
				err = ErrNotFound
			}
			return commandPrecheckSnapshot{}, err
		}
		var draft CatalogDraft
		var modalities []byte
		err = tx.QueryRow(ctx, `SELECT model_id,base_catalog_version,draft_version,display_name,description,aliases,protocol_code,protocol_version,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,updated_by,updated_at FROM model_catalog_drafts WHERE model_id=$1`, modelID).Scan(&draft.ModelID, &draft.BaseCatalogVersion, &draft.DraftVersion, &draft.DisplayName, &draft.Description, &draft.Aliases, &draft.ProtocolCode, &draft.ProtocolVersion, &draft.ContextLength, &draft.MaximumOutputTokens, &modalities, &draft.TokenizerVersion, &draft.UpdatedBy, &draft.UpdatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return commandPrecheckSnapshot{}, ErrNotFound
		}
		if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		if err := json.Unmarshal(modalities, &draft.SupportedModalities); err != nil {
			return commandPrecheckSnapshot{}, ErrTokenCommandInvalid
		}
		var protocolID uuid.UUID
		err = tx.QueryRow(ctx, `SELECT id FROM protocol_definitions WHERE protocol_code=$1 AND version=$2 AND status='PUBLISHED'`, draft.ProtocolCode, draft.ProtocolVersion).Scan(&protocolID)
		if errors.Is(err, pgx.ErrNoRows) {
			return commandPrecheckSnapshot{}, ErrNotFound
		}
		if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		rows, err := tx.Query(ctx, `SELECT required_unit_versions FROM protocol_formula_versions WHERE protocol_definition_id=$1 AND status='PUBLISHED' ORDER BY id`, protocolID)
		if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		units := map[string]struct{}{}
		for rows.Next() {
			var required map[string]uuid.UUID
			if err := rows.Scan(&required); err != nil {
				rows.Close()
				return commandPrecheckSnapshot{}, err
			}
			for rawCode, unitID := range required {
				code := strings.ToUpper(strings.TrimSpace(rawCode))
				if code == "" || rawCode != code || unitID == uuid.Nil {
					rows.Close()
					return commandPrecheckSnapshot{}, ErrProtocolFormulaPrecheck
				}
				units[code] = struct{}{}
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return commandPrecheckSnapshot{}, err
		}
		rows.Close()
		if len(units) == 0 {
			return commandPrecheckSnapshot{}, ErrProtocolFormulaPrecheck
		}
		unitCodes := make([]string, 0, len(units))
		for code := range units {
			unitCodes = append(unitCodes, code)
		}
		sort.Strings(unitCodes)
		dimensions := []ImpactPriceQuery{{ModelID: modelID, ProtocolCode: draft.ProtocolCode, CreditUnit: "MICROCREDIT"}}
		draftFingerprint := catalogDraftFingerprint(draft)
		var stableRouteCount, provisioningCount, readyRouteCount int64
		if err := tx.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM routing_policies route WHERE route.model_id=$1 AND route.protocol_version_id=$2 AND route.reference_status='RESOLVED'),
 (SELECT count(*) FROM route_provisionings provision WHERE provision.model_id=$1 AND provision.protocol_version_id=$2),
 (SELECT count(DISTINCT route.id)
    FROM routing_policies route
   WHERE route.model_id=$1 AND route.protocol_version_id=$2 AND route.reference_status='RESOLVED'
     AND EXISTS(
       SELECT 1 FROM route_provisionings provision
        WHERE provision.model_id=route.model_id
          AND provision.protocol_version_id=route.protocol_version_id
          AND provision.active_token_account_id=route.token_account_id
          AND provision.status='READY'
          AND provision.actual_generation=route.route_generation
     ))`, modelID, protocolID).Scan(&stableRouteCount, &provisioningCount, &readyRouteCount); err != nil {
			return commandPrecheckSnapshot{}, err
		}
		routeSetFingerprint, err := impactRowsFingerprint(ctx, tx, `SELECT jsonb_build_array(id,token_account_id,model_id,protocol_version_id,priority,default_scope,reference_status,route_version,route_generation)::text FROM routing_policies WHERE model_id=$1 AND protocol_version_id=$2 AND reference_status='RESOLVED' ORDER BY token_account_id,id`, modelID, protocolID)
		if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		provisioningSetFingerprint, err := impactRowsFingerprint(ctx, tx, `SELECT jsonb_build_array(id,scope_type,scope_id,wallet_id,model_id,protocol_version_id,status,desired_generation,actual_generation,desired_signature,active_token_account_id,active_credential_id,policy_version,updated_at)::text FROM route_provisionings WHERE model_id=$1 AND protocol_version_id=$2 ORDER BY scope_type,scope_id,wallet_id,desired_generation,id`, modelID, protocolID)
		if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		blockers := []string{}
		if draft.BaseCatalogVersion != currentCatalog {
			blockers = append(blockers, "MODEL_DRAFT_CHANGED")
		}
		if stableRouteCount == 0 {
			blockers = append(blockers, "MODEL_ROUTE_COVERAGE_MISSING")
		}
		if provisioningCount == 0 {
			blockers = append(blockers, "MODEL_PROVISIONING_COVERAGE_MISSING")
		} else if readyRouteCount < stableRouteCount {
			blockers = append(blockers, "MODEL_PROVISIONING_NOT_READY")
		}
		expectedVersions := map[string]any{
			"model_id": modelID, "expected_catalog_version": currentCatalog, "expected_draft_version": draft.DraftVersion,
			"draft_fingerprint": draftFingerprint, "protocol_definition_id": protocolID, "protocol_version": draft.ProtocolVersion,
			"stable_route_count": stableRouteCount, "stable_route_set_fingerprint": routeSetFingerprint,
			"provisioning_count": provisioningCount, "ready_route_count": readyRouteCount,
			"provisioning_set_fingerprint": provisioningSetFingerprint,
		}
		evidenceValues := append([]string{
			modelID.String(), strconv.FormatInt(currentCatalog, 10), strconv.FormatInt(draft.DraftVersion, 10), draftFingerprint,
			protocolID.String(), strconv.FormatInt(draft.ProtocolVersion, 10),
			"stable_route_count=" + strconv.FormatInt(stableRouteCount, 10), "stable_route_set_fingerprint=" + routeSetFingerprint,
			"provisioning_count=" + strconv.FormatInt(provisioningCount, 10), "ready_route_count=" + strconv.FormatInt(readyRouteCount, 10),
			"provisioning_set_fingerprint=" + provisioningSetFingerprint,
		}, unitCodes...)
		evidenceValues = append(evidenceValues, "publish_mode="+command.PublishMode)
		if command.PublishMode == "SCHEDULED" {
			evidenceValues = append(evidenceValues, "lifecycle_plan_id="+command.LifecyclePlanID.String(), "lifecycle_plan_version="+strconv.FormatInt(*command.LifecyclePlanVersion, 10), "effective_at="+command.EffectiveAt.UTC().Format(time.RFC3339Nano))
		}
		return commandPrecheckSnapshot{Status: "DRAFT", ExpectedVersions: expectedVersions, EvidenceValues: evidenceValues, PriceDimensions: dimensions, Blockers: blockers}, nil
	case "TOKEN_CREDENTIAL_VALIDATE":
		credentialID, err := uuid.Parse(targetID)
		if err != nil || credentialID == uuid.Nil {
			return commandPrecheckSnapshot{}, ErrTokenCommandInvalid
		}
		var command struct {
			TokenAccountID uuid.UUID `json:"token_account_id"`
			CredentialID   uuid.UUID `json:"credential_id"`
		}
		if decodeCommand(raw, &command) != nil || command.CredentialID != credentialID || command.TokenAccountID == uuid.Nil {
			return commandPrecheckSnapshot{}, ErrTokenCommandInvalid
		}
		var accountVersion int64
		var version int
		var status, validationStatus, fingerprint string
		err = tx.QueryRow(ctx, `SELECT account.management_version,credential.version,credential.status,credential.validation_status,credential.fingerprint FROM token_accounts account JOIN provider_credentials credential ON credential.token_account_id=account.id WHERE account.id=$1 AND account.management_mode='PLATFORM' AND credential.id=$2`, command.TokenAccountID, credentialID).Scan(&accountVersion, &version, &status, &validationStatus, &fingerprint)
		if errors.Is(err, pgx.ErrNoRows) {
			return commandPrecheckSnapshot{}, ErrNotFound
		}
		if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		requestHash := sha256.Sum256([]byte(command.TokenAccountID.String() + "\x00" + credentialID.String() + "\x00" + strconv.Itoa(version)))
		evidenceHash := sha256.Sum256([]byte(strconv.FormatInt(accountVersion, 10) + "\x00" + strconv.Itoa(version) + "\x00" + status + "\x00" + validationStatus + "\x00" + fingerprint))
		blockers := []string{}
		if status != "ACTIVE" {
			blockers = append(blockers, "CREDENTIAL_NOT_ACTIVE")
		}
		return commandPrecheckSnapshot{Status: status, ExpectedVersions: map[string]any{
			"token_account_id": command.TokenAccountID, "expected_account_version": accountVersion,
			"credential_id": credentialID, "credential_version": version,
			"precheck_fingerprint": hex.EncodeToString(evidenceHash[:]), "request_fingerprint": hex.EncodeToString(requestHash[:]),
		}, EvidenceValues: []string{command.TokenAccountID.String(), credentialID.String(), strconv.FormatInt(accountVersion, 10), strconv.Itoa(version), status, validationStatus, fingerprint}, Blockers: blockers}, nil
	case "TOKEN_ROUTE_PROVISIONING_REPAIR":
		command, err := parseRuntimeGovernedCommand(commandType, targetID, raw)
		if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		var status string
		var desiredGeneration int64
		var repairRound int
		var updatedAt time.Time
		err = tx.QueryRow(ctx, `SELECT status,desired_generation,repair_round,updated_at FROM route_provisionings WHERE id=$1`, command.ResourceID).Scan(&status, &desiredGeneration, &repairRound, &updatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return commandPrecheckSnapshot{}, ErrNotFound
		}
		if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		if !updatedAt.Equal(command.ExpectedVersion) || desiredGeneration != command.TargetGeneration {
			return commandPrecheckSnapshot{}, ErrCatalogVersionConflict
		}
		blockers := []string{}
		if status != "FAILED" && status != "REPAIR_REQUIRED" {
			blockers = append(blockers, "PROVISIONING_NOT_REPAIRABLE")
		}
		version := updatedAt.UTC().Format(time.RFC3339Nano)
		return commandPrecheckSnapshot{Status: status, ExpectedVersions: map[string]any{"provisioning_id": command.ResourceID, "expected_provisioning_version": version, "target_generation": desiredGeneration}, EvidenceValues: []string{status, version, strconv.FormatInt(desiredGeneration, 10), strconv.Itoa(repairRound)}, Blockers: blockers}, nil
	default:
		return commandPrecheckSnapshot{}, ErrTokenCommandInvalid
	}
}

func tokenCommandExpectedVersionsMatch(actual json.RawMessage, expected json.RawMessage) bool {
	var got, want map[string]any
	if decodeExactCommandJSON(actual, &got) != nil || decodeExactCommandJSON(expected, &want) != nil || got == nil || want == nil {
		return false
	}
	delete(got, "precheck_fingerprint")
	delete(got, "request_fingerprint")
	return commandJSONValuesEqual(got, want)
}

// stableUnitMappingValues is used by tests and future APT-138 adapters to bind
// Billing unit references without exposing formula expressions.
func stableUnitMappingValues(values map[string]uuid.UUID) []string {
	items := make([]string, 0, len(values))
	for code, id := range values {
		items = append(items, strings.ToUpper(strings.TrimSpace(code))+"="+id.String())
	}
	sort.Strings(items)
	return items
}
