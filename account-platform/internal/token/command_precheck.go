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
	Status           string
	ExpectedVersions map[string]any
	EvidenceValues   []string
	PriceDimensions  []ImpactPriceQuery
	Blockers         []string
}

type runtimeGovernedCommand struct {
	ResourceID            uuid.UUID
	ExpectedVersion       time.Time
	TargetGeneration      int64
	EvidenceHighWatermark int64
}

var directCommandPrecheckTypes = map[string]string{
	"TOKEN_PROTOCOL_DRAFT_CREATE":      "PROTOCOL",
	"TOKEN_PROTOCOL_PUBLISH":           "PROTOCOL",
	"TOKEN_FORMULA_DRAFT_CREATE":       "FORMULA",
	"TOKEN_FORMULA_PUBLISH":            "FORMULA",
	"TOKEN_MODEL_PUBLISH":              "MODEL",
	"TOKEN_MODEL_CATALOG_SYNC":         "MODEL_CATALOG",
	"TOKEN_ROUTE_PROVISIONING_REPAIR":  "PROVISIONING",
	"TOKEN_USAGE_RECONCILIATION_RETRY": "USAGE_REQUEST",
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
	if !supported || !strictCatalogSync || input.TargetResourceType != expectedType || input.TargetResourceID == "" || containsSensitiveCommandField(input.Command) || validateOwnedCommandPrecheckTarget(input.CommandType, input.TargetResourceID, input.Command) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_COMMAND_PRECHECK", "the governed command precheck is invalid", httpserver.RequestID(r.Context()))
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
		if errors.Is(err, ErrNotFound) {
			status, code = http.StatusNotFound, "NOT_FOUND"
		} else if errors.Is(err, ErrForbidden) || errors.Is(err, ErrTokenCommandInvalid) {
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
		snapshot = commandPrecheckSnapshot{Status: "READY", ExpectedVersions: map[string]any{
			"expected_catalog_version": preview.CatalogVersion, "adapter_observed_version": preview.AdapterObservedVersion,
			"catalog_diff_fingerprint": preview.DiffFingerprint,
		}, EvidenceValues: []string{strconv.FormatInt(preview.CatalogVersion, 10), preview.AdapterObservedVersion, preview.DiffFingerprint, strconv.Itoa(preview.Added), strconv.Itoa(preview.Missing)}}
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
	expectedJSON, err := json.Marshal(snapshot.ExpectedVersions)
	if err != nil {
		return TokenCommandPrecheckResult{}, err
	}
	canonicalCommand, err := canonicalJSON(input.Command)
	if err != nil {
		return TokenCommandPrecheckResult{}, ErrTokenCommandInvalid
	}
	values := []string{commandType, expectedType, input.TargetResourceID, string(expectedJSON), string(canonicalCommand)}
	values = append(values, snapshot.EvidenceValues...)
	digest := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	fingerprint := hex.EncodeToString(digest[:])
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
	result.PriceCoverage = priceCoverage
	if billingObservedAt != nil {
		observed := billingObservedAt.UTC()
		result.BillingObservedAt = &observed
	}
	return result, nil
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
		if decodeCommand(raw, &command) != nil || command.ExpectedProtocolVersion < 1 || !validImpactETag(command.DraftETag) {
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
		if decodeCommand(raw, &command) != nil || command.ExpectedFormulaVersion < 1 || !validImpactETag(command.DraftETag) {
			return ErrTokenCommandInvalid
		}
	case "TOKEN_MODEL_PUBLISH":
		var command struct {
			ModelID uuid.UUID `json:"model_id"`
		}
		if decodeCommand(raw, &command) != nil || command.ModelID == uuid.Nil {
			return ErrTokenCommandInvalid
		}
	case "TOKEN_MODEL_CATALOG_SYNC":
		var command struct{}
		if decodeCommand(raw, &command) != nil {
			return ErrTokenCommandInvalid
		}
	case "TOKEN_ROUTE_PROVISIONING_REPAIR", "TOKEN_USAGE_RECONCILIATION_RETRY":
		_, err := parseRuntimeGovernedCommand(commandType, "", raw)
		return err
	default:
		return ErrTokenCommandInvalid
	}
	return nil
}

func validateOwnedCommandPrecheckTarget(commandType, targetID string, raw json.RawMessage) error {
	if err := validateOwnedCommandPrecheck(commandType, raw); err != nil {
		return err
	}
	commandType = strings.ToUpper(strings.TrimSpace(commandType))
	if commandType == "TOKEN_ROUTE_PROVISIONING_REPAIR" || commandType == "TOKEN_USAGE_RECONCILIATION_RETRY" {
		_, err := parseRuntimeGovernedCommand(commandType, targetID, raw)
		return err
	}
	if commandType != "TOKEN_MODEL_PUBLISH" {
		return nil
	}
	var command struct {
		ModelID uuid.UUID `json:"model_id"`
	}
	modelID, err := uuid.Parse(strings.TrimSpace(targetID))
	if err != nil || decodeCommand(raw, &command) != nil || command.ModelID != modelID {
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
	case "TOKEN_USAGE_RECONCILIATION_RETRY":
		var command struct {
			UsageRequestID        string `json:"usage_request_id"`
			ExpectedUsageVersion  string `json:"expected_usage_version"`
			EvidenceHighWatermark int64  `json:"evidence_high_watermark"`
			ReplayModelRequest    *bool  `json:"replay_model_request"`
		}
		if decodeCommand(raw, &command) != nil || command.ReplayModelRequest == nil || *command.ReplayModelRequest || command.EvidenceHighWatermark < 0 {
			return runtimeGovernedCommand{}, ErrTokenCommandInvalid
		}
		id, err := uuid.Parse(command.UsageRequestID)
		version, versionErr := time.Parse(time.RFC3339Nano, command.ExpectedUsageVersion)
		if err != nil || id == uuid.Nil || command.UsageRequestID != id.String() || versionErr != nil || command.ExpectedUsageVersion != version.UTC().Format(time.RFC3339Nano) || canonicalTarget != "" && canonicalTarget != id.String() {
			return runtimeGovernedCommand{}, ErrTokenCommandInvalid
		}
		result = runtimeGovernedCommand{ResourceID: id, ExpectedVersion: version.UTC(), EvidenceHighWatermark: command.EvidenceHighWatermark}
	default:
		return runtimeGovernedCommand{}, ErrTokenCommandInvalid
	}
	return result, nil
}

func validImpactETag(value string) bool {
	value = strings.TrimSpace(value)
	return strings.HasPrefix(value, `"`) && strings.HasSuffix(value, `"`) && len(value) >= 10 && len(value) <= 160
}

func (s *Service) validateCommandBillingPrecheck(ctx context.Context, commandType, targetID string, raw json.RawMessage) (*time.Time, []string, error) {
	validator, ok := s.billing.(FormulaDraftValidationClient)
	if !ok {
		if commandType == "TOKEN_FORMULA_DRAFT_CREATE" || commandType == "TOKEN_FORMULA_PUBLISH" || commandType == "TOKEN_PROTOCOL_PUBLISH" {
			return nil, nil, errors.New("Billing validation unavailable")
		}
		return nil, nil, nil
	}
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
	validation, err := validator.ValidateFormulaDraftUnits(ctx, combined)
	if err != nil || validateFormulaDraftValidation(combined, validation) != nil || validation.ObservedAt.IsZero() || !validImpactFingerprint(validation.UnitSetFingerprint) {
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
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
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
		var protocol ProtocolDefinition
		var formulaDigest string
		err = tx.QueryRow(ctx, `SELECT p.id,p.protocol_code,p.version,p.status,p.request_schema,p.created_at,p.published_at,COALESCE((SELECT encode(digest(string_agg(f.id::text||':'||f.version::text||':'||f.status||':'||encode(digest(f.required_unit_versions::text||f.expression::text||f.evaluator_version,'sha256'),'hex'),E'\\x00' ORDER BY f.id),'sha256'),'hex') FROM protocol_formula_versions f WHERE f.protocol_definition_id=p.id),'') FROM protocol_definitions p WHERE p.id=$1`, id).Scan(&protocol.ID, &protocol.ProtocolCode, &protocol.Version, &protocol.Status, &protocol.RequestSchema, &protocol.CreatedAt, &protocol.PublishedAt, &formulaDigest)
		if errors.Is(err, pgx.ErrNoRows) {
			return commandPrecheckSnapshot{}, ErrNotFound
		}
		if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		etag := protocolDefinitionETag(protocol)
		blockers := []string{}
		if protocol.Status != "DRAFT" || command.ExpectedProtocolVersion != protocol.Version || strings.ToLower(strings.TrimSpace(command.DraftETag)) != etag {
			blockers = append(blockers, "PROTOCOL_DRAFT_CHANGED")
		}
		return commandPrecheckSnapshot{Status: protocol.Status, ExpectedVersions: map[string]any{"protocol_definition_id": id, "expected_protocol_version": protocol.Version, "draft_etag": etag}, EvidenceValues: []string{protocol.ProtocolCode, strconv.FormatInt(protocol.Version, 10), protocol.Status, etag, formulaDigest}, Blockers: blockers}, nil
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
		var formula ProtocolFormulaVersion
		err = tx.QueryRow(ctx, `SELECT id,protocol_definition_id,protocol_code,version,status,required_unit_versions,expression,evaluator_version,created_at,published_at FROM protocol_formula_versions WHERE id=$1`, id).Scan(&formula.ID, &formula.ProtocolDefinitionID, &formula.ProtocolCode, &formula.Version, &formula.Status, &formula.RequiredUnitVersions, &formula.Expression, &formula.EvaluatorVersion, &formula.CreatedAt, &formula.PublishedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return commandPrecheckSnapshot{}, ErrNotFound
		}
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
	case "TOKEN_MODEL_PUBLISH":
		modelID, err := uuid.Parse(targetID)
		if err != nil || modelID == uuid.Nil {
			return commandPrecheckSnapshot{}, ErrTokenCommandInvalid
		}
		var command struct {
			ModelID uuid.UUID `json:"model_id"`
		}
		if decodeCommand(raw, &command) != nil || command.ModelID != modelID {
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
		return commandPrecheckSnapshot{Status: "DRAFT", ExpectedVersions: expectedVersions, EvidenceValues: evidenceValues, PriceDimensions: dimensions, Blockers: blockers}, nil
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
	case "TOKEN_USAGE_RECONCILIATION_RETRY":
		command, err := parseRuntimeGovernedCommand(commandType, targetID, raw)
		if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		var status string
		var highWater int64
		var updatedAt time.Time
		var debitProhibited bool
		err = tx.QueryRow(ctx, `SELECT u.status,u.last_persisted_sequence,u.updated_at,EXISTS(SELECT 1 FROM usage_platform_loss_reviews loss WHERE loss.usage_request_id=u.id AND loss.customer_debit_prohibited) FROM usage_requests u WHERE u.id=$1`, command.ResourceID).Scan(&status, &highWater, &updatedAt, &debitProhibited)
		if errors.Is(err, pgx.ErrNoRows) {
			return commandPrecheckSnapshot{}, ErrNotFound
		}
		if err != nil {
			return commandPrecheckSnapshot{}, err
		}
		if !updatedAt.Equal(command.ExpectedVersion) || highWater != command.EvidenceHighWatermark {
			return commandPrecheckSnapshot{}, ErrCatalogVersionConflict
		}
		blockers := []string{}
		if status != "SETTLEMENT_PENDING" && status != "REVIEW" {
			blockers = append(blockers, "USAGE_NOT_RECONCILABLE")
		}
		if debitProhibited {
			blockers = append(blockers, "CUSTOMER_DEBIT_PROHIBITED")
		}
		version := updatedAt.UTC().Format(time.RFC3339Nano)
		return commandPrecheckSnapshot{Status: status, ExpectedVersions: map[string]any{"usage_request_id": command.ResourceID, "expected_usage_version": version, "evidence_high_watermark": highWater}, EvidenceValues: []string{status, version, strconv.FormatInt(highWater, 10), strconv.FormatBool(debitProhibited)}, Blockers: blockers}, nil
	default:
		return commandPrecheckSnapshot{}, ErrTokenCommandInvalid
	}
}

func tokenCommandExpectedVersionsMatch(actual json.RawMessage, expected json.RawMessage) bool {
	var got, want map[string]any
	if json.Unmarshal(actual, &got) != nil || json.Unmarshal(expected, &want) != nil {
		return false
	}
	delete(got, "precheck_fingerprint")
	delete(got, "request_fingerprint")
	gotJSON, gotErr := json.Marshal(got)
	wantJSON, wantErr := json.Marshal(want)
	return gotErr == nil && wantErr == nil && string(gotJSON) == string(wantJSON)
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
