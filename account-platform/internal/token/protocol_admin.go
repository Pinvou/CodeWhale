package token

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrProtocolHeadVersionConflict = errors.New("protocol head version conflict")
	ErrFormulaHeadVersionConflict  = errors.New("formula head version conflict")
	ErrProtocolDraftETagMismatch   = errors.New("protocol draft etag mismatch")
	ErrFormulaDraftETagMismatch    = errors.New("formula draft etag mismatch")
	ErrProtocolFormulaPrecheck     = errors.New("protocol or formula precheck failed")
	ErrProtocolFormulaCommandInput = errors.New("protocol or formula command input is invalid")
)

type ProtocolDefinition struct {
	ID            uuid.UUID       `json:"protocol_definition_id"`
	ProtocolCode  string          `json:"protocol_code"`
	Version       int64           `json:"version"`
	Status        string          `json:"status"`
	RequestSchema json.RawMessage `json:"request_schema"`
	ETag          string          `json:"etag"`
	CreatedAt     time.Time       `json:"created_at"`
	PublishedAt   *time.Time      `json:"published_at,omitempty"`
}
type ProtocolFormulaVersion struct {
	ID                   uuid.UUID            `json:"formula_version_id"`
	ProtocolDefinitionID uuid.UUID            `json:"protocol_definition_id"`
	ProtocolCode         string               `json:"protocol_code"`
	Version              int64                `json:"version"`
	Status               string               `json:"status"`
	RequiredUnitVersions map[string]uuid.UUID `json:"required_unit_versions"`
	Expression           map[string]string    `json:"expression"`
	EvaluatorVersion     string               `json:"evaluator_version"`
	ETag                 string               `json:"etag"`
	CreatedAt            time.Time            `json:"created_at"`
	PublishedAt          *time.Time           `json:"published_at,omitempty"`
}

type ProtocolFormulaCommandMetadata struct {
	ActorID              uuid.UUID
	CommandType          string
	Reason               string
	IdempotencyKeyDigest string
	RequestFingerprint   string
	CommandReference     string
	TraceID              string
}

type ProtocolFormulaCommandResult struct {
	CommandReference string                  `json:"command_reference"`
	CommandType      string                  `json:"command_type"`
	PolicyVersion    int64                   `json:"policy_version"`
	Status           string                  `json:"status"`
	Protocol         *ProtocolDefinition     `json:"protocol,omitempty"`
	Formula          *ProtocolFormulaVersion `json:"formula,omitempty"`
	Precheck         map[string]any          `json:"precheck"`
	BillingSync      string                  `json:"billing_sync,omitempty"`
}

type FormulaBindingSync struct {
	FormulaVersionID      uuid.UUID
	BillingStatus         string
	ProtocolCode          string
	RequiredUnitVersions  []uuid.UUID
	FormulaSnapshotSHA256 string
	PublishedAt           time.Time
}

// ProtocolPublicationValidation is immutable evidence produced by the
// service-layer Billing preflight. PublishProtocolCommand recomputes the
// formula-set digest while holding database locks so a concurrent formula
// change cannot reuse stale Billing validation.
type ProtocolPublicationValidation struct {
	ProtocolDefinitionID uuid.UUID
	ProtocolCode         string
	ProtocolVersion      int64
	FormulaSetSHA256     string
	UnitVersions         []MeteringUnitVersionReference
}

func (r *PostgresRepository) ListProtocolsAndFormulas(ctx context.Context) ([]ProtocolDefinition, []ProtocolFormulaVersion, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,protocol_code,version,status,request_schema,created_at,published_at FROM protocol_definitions ORDER BY protocol_code,version DESC`)
	if err != nil {
		return nil, nil, err
	}
	protocols := make([]ProtocolDefinition, 0)
	for rows.Next() {
		var item ProtocolDefinition
		if err := rows.Scan(&item.ID, &item.ProtocolCode, &item.Version, &item.Status, &item.RequestSchema, &item.CreatedAt, &item.PublishedAt); err != nil {
			rows.Close()
			return nil, nil, err
		}
		item.ETag = protocolDefinitionETag(item)
		protocols = append(protocols, item)
	}
	rows.Close()
	rows, err = r.pool.Query(ctx, `SELECT formula.id,formula.protocol_definition_id,protocol.protocol_code,formula.version,formula.status,formula.required_unit_versions,formula.expression,formula.evaluator_version,formula.created_at,formula.published_at FROM protocol_formula_versions formula JOIN protocol_definitions protocol ON protocol.id=formula.protocol_definition_id ORDER BY protocol.protocol_code,formula.version DESC`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	formulas := make([]ProtocolFormulaVersion, 0)
	for rows.Next() {
		var item ProtocolFormulaVersion
		var required, expression []byte
		if err := rows.Scan(&item.ID, &item.ProtocolDefinitionID, &item.ProtocolCode, &item.Version, &item.Status, &required, &expression, &item.EvaluatorVersion, &item.CreatedAt, &item.PublishedAt); err != nil {
			return nil, nil, err
		}
		if json.Unmarshal(required, &item.RequiredUnitVersions) != nil || json.Unmarshal(expression, &item.Expression) != nil {
			return nil, nil, ErrForbidden
		}
		item.ETag = protocolFormulaETag(item)
		formulas = append(formulas, item)
	}
	return protocols, formulas, rows.Err()
}

func (r *PostgresRepository) CreateProtocolDraftCommand(ctx context.Context, metadata ProtocolFormulaCommandMetadata, code string, schema json.RawMessage, expectedHeadVersion int64) (ProtocolFormulaCommandResult, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	canonicalSchema, schemaOK := canonicalJSONObject(schema)
	if err := validateProtocolFormulaCommandMetadata(metadata); err != nil || code == "" || expectedHeadVersion < 0 || !schemaOK {
		return ProtocolFormulaCommandResult{}, ErrProtocolFormulaPrecheck
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTokenKey(ctx, tx, "protocol", code); err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	var headVersion int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version),0) FROM protocol_definitions WHERE protocol_code=$1`, code).Scan(&headVersion); err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	if headVersion != expectedHeadVersion {
		return ProtocolFormulaCommandResult{}, ErrProtocolHeadVersionConflict
	}
	var publishedReferenceCount int64
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM model_capability_versions capability JOIN protocol_definitions protocol ON protocol.id=capability.protocol_definition_id WHERE protocol.protocol_code=$1`, code).Scan(&publishedReferenceCount); err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	item := ProtocolDefinition{ID: uuid.Must(uuid.NewV7()), ProtocolCode: code, Version: headVersion + 1, Status: "DRAFT", RequestSchema: canonicalSchema, CreatedAt: r.protocolCommandNow()}
	item.ETag = protocolDefinitionETag(item)
	if _, err := tx.Exec(ctx, `INSERT INTO protocol_definitions(id,protocol_code,version,status,request_schema,created_at) VALUES($1,$2,$3,'DRAFT',$4,$5)`, item.ID, code, item.Version, []byte(canonicalSchema), item.CreatedAt); err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	precheck := map[string]any{"protocol_code_conflict": false, "published_reference_count": publishedReferenceCount, "expected_protocol_head_version": expectedHeadVersion, "before_status": "NONE", "after_status": "DRAFT"}
	if err := recordProtocolFormulaCommand(ctx, tx, metadata, "PROTOCOL", item.ID, item.Version, item.ETag, precheck, item.CreatedAt); err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	return protocolCommandResult(metadata, &item, nil, precheck, ""), nil
}

func (r *PostgresRepository) CreateFormulaDraftCommand(ctx context.Context, metadata ProtocolFormulaCommandMetadata, protocolID uuid.UUID, expectedProtocolVersion, expectedFormulaHeadVersion int64, required map[string]uuid.UUID, expression map[string]string, validation FormulaDraftValidation) (ProtocolFormulaCommandResult, error) {
	required, expression, mappingErr := normalizeSafeFormula(required, expression)
	if err := validateProtocolFormulaCommandMetadata(metadata); err != nil || protocolID == uuid.Nil || expectedProtocolVersion < 1 || expectedFormulaHeadVersion < 0 || mappingErr != nil || validateFormulaDraftValidation(required, validation) != nil {
		return ProtocolFormulaCommandResult{}, ErrProtocolFormulaPrecheck
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	defer tx.Rollback(ctx)
	var code, status string
	var protocolVersion int64
	if err := tx.QueryRow(ctx, `SELECT protocol_code,version,status FROM protocol_definitions WHERE id=$1 FOR SHARE`, protocolID).Scan(&code, &protocolVersion, &status); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProtocolFormulaCommandResult{}, ErrNotFound
		}
		return ProtocolFormulaCommandResult{}, err
	}
	if status != "PUBLISHED" {
		return ProtocolFormulaCommandResult{}, ErrProtocolFormulaPrecheck
	}
	if protocolVersion != expectedProtocolVersion {
		return ProtocolFormulaCommandResult{}, ErrProtocolHeadVersionConflict
	}
	if err := lockTokenKey(ctx, tx, "formula", protocolID.String()); err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	var headVersion int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version),0) FROM protocol_formula_versions WHERE protocol_definition_id=$1`, protocolID).Scan(&headVersion); err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	if headVersion != expectedFormulaHeadVersion {
		return ProtocolFormulaCommandResult{}, ErrFormulaHeadVersionConflict
	}
	requiredJSON, _ := json.Marshal(required)
	expressionJSON, _ := json.Marshal(expression)
	item := ProtocolFormulaVersion{ID: uuid.Must(uuid.NewV7()), ProtocolDefinitionID: protocolID, ProtocolCode: code, Version: headVersion + 1, Status: "DRAFT", RequiredUnitVersions: required, Expression: expression, EvaluatorVersion: "SAFE_PARAMETER_MAP_V1", CreatedAt: r.protocolCommandNow()}
	item.ETag = protocolFormulaETag(item)
	if _, err := tx.Exec(ctx, `INSERT INTO protocol_formula_versions(id,protocol_definition_id,version,status,required_unit_versions,expression,evaluator_version,created_at) VALUES($1,$2,$3,'DRAFT',$4,$5,'SAFE_PARAMETER_MAP_V1',$6)`, item.ID, protocolID, item.Version, requiredJSON, expressionJSON, item.CreatedAt); err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	precheck := map[string]any{"protocol_version": protocolVersion, "expected_formula_head_version": expectedFormulaHeadVersion, "metering_unit_version_count": validation.VerifiedCount, "metering_unit_versions": validation.UnitVersions, "parameter_mapping_valid": true, "before_status": "NONE", "after_status": "DRAFT"}
	if err := recordProtocolFormulaCommand(ctx, tx, metadata, "PROTOCOL_FORMULA", item.ID, item.Version, item.ETag, precheck, item.CreatedAt); err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	return protocolCommandResult(metadata, nil, &item, precheck, ""), nil
}

func validateSafeFormula(required map[string]uuid.UUID, expression map[string]string) error {
	_, _, err := normalizeSafeFormula(required, expression)
	return err
}

func normalizeSafeFormula(required map[string]uuid.UUID, expression map[string]string) (map[string]uuid.UUID, map[string]string, error) {
	if len(required) == 0 || len(required) > 32 || len(required) != len(expression) {
		return nil, nil, ErrForbidden
	}
	allowed := map[string]bool{"estimated_input_tokens": true, "cached_input_tokens": true, "max_output_tokens": true, "image_count": true, "request_count": true, "document_count": true}
	normalizedRequired := make(map[string]uuid.UUID, len(required))
	normalizedExpression := make(map[string]string, len(expression))
	for rawUnit, id := range required {
		unit := strings.ToUpper(strings.TrimSpace(rawUnit))
		mapping := strings.TrimSpace(expression[rawUnit])
		if unit == "" || id == uuid.Nil || normalizedRequired[unit] != uuid.Nil || !allowed[mapping] {
			return nil, nil, ErrForbidden
		}
		normalizedRequired[unit] = id
		normalizedExpression[unit] = mapping
	}
	for rawUnit := range expression {
		if normalizedRequired[strings.ToUpper(strings.TrimSpace(rawUnit))] == uuid.Nil {
			return nil, nil, ErrForbidden
		}
	}
	return normalizedRequired, normalizedExpression, nil
}

func validateFormulaDraftValidation(required map[string]uuid.UUID, validation FormulaDraftValidation) error {
	if validation.VerifiedCount != len(required) || len(validation.UnitVersions) != len(required) {
		return ErrProtocolFormulaPrecheck
	}
	seen := make(map[string]bool, len(required))
	for _, unit := range validation.UnitVersions {
		code := strings.ToUpper(strings.TrimSpace(unit.UnitCode))
		if code == "" || unit.Status != "ACTIVE" || unit.Version < 1 || required[code] != unit.UnitDefinitionID || seen[code] {
			return ErrProtocolFormulaPrecheck
		}
		seen[code] = true
	}
	return nil
}

func (r *PostgresRepository) ProtocolPublishDependencies(ctx context.Context, id uuid.UUID) (ProtocolDefinition, []ProtocolFormulaVersion, error) {
	var protocol ProtocolDefinition
	if err := r.pool.QueryRow(ctx, `SELECT id,protocol_code,version,status,request_schema,created_at,published_at FROM protocol_definitions WHERE id=$1`, id).Scan(&protocol.ID, &protocol.ProtocolCode, &protocol.Version, &protocol.Status, &protocol.RequestSchema, &protocol.CreatedAt, &protocol.PublishedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return protocol, nil, ErrNotFound
		}
		return protocol, nil, err
	}
	protocol.ETag = protocolDefinitionETag(protocol)
	rows, err := r.pool.Query(ctx, `SELECT id,protocol_definition_id,$2,version,status,required_unit_versions,expression,evaluator_version,created_at,published_at FROM protocol_formula_versions WHERE protocol_definition_id=$1 AND status IN ('DRAFT','PUBLISHED') ORDER BY id`, id, protocol.ProtocolCode)
	if err != nil {
		return protocol, nil, err
	}
	defer rows.Close()
	formulas := make([]ProtocolFormulaVersion, 0)
	for rows.Next() {
		var item ProtocolFormulaVersion
		var requiredJSON, expressionJSON []byte
		if err := rows.Scan(&item.ID, &item.ProtocolDefinitionID, &item.ProtocolCode, &item.Version, &item.Status, &requiredJSON, &expressionJSON, &item.EvaluatorVersion, &item.CreatedAt, &item.PublishedAt); err != nil {
			return protocol, nil, err
		}
		if json.Unmarshal(requiredJSON, &item.RequiredUnitVersions) != nil || json.Unmarshal(expressionJSON, &item.Expression) != nil || validateSafeFormula(item.RequiredUnitVersions, item.Expression) != nil || item.EvaluatorVersion != "SAFE_PARAMETER_MAP_V1" {
			return protocol, nil, ErrProtocolFormulaPrecheck
		}
		item.ETag = protocolFormulaETag(item)
		formulas = append(formulas, item)
	}
	return protocol, formulas, rows.Err()
}

func (r *PostgresRepository) PublishProtocolCommand(ctx context.Context, metadata ProtocolFormulaCommandMetadata, id uuid.UUID, expectedVersion int64, draftETag string, validation ProtocolPublicationValidation) (ProtocolFormulaCommandResult, error) {
	if err := validateProtocolFormulaCommandMetadata(metadata); err != nil || id == uuid.Nil || expectedVersion < 1 || !validCommandETag(draftETag) {
		return ProtocolFormulaCommandResult{}, ErrProtocolFormulaPrecheck
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	defer tx.Rollback(ctx)
	var item ProtocolDefinition
	if err := tx.QueryRow(ctx, `SELECT id,protocol_code,version,status,request_schema,created_at,published_at FROM protocol_definitions WHERE id=$1 FOR UPDATE`, id).Scan(&item.ID, &item.ProtocolCode, &item.Version, &item.Status, &item.RequestSchema, &item.CreatedAt, &item.PublishedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ProtocolFormulaCommandResult{}, ErrNotFound
		}
		return ProtocolFormulaCommandResult{}, err
	}
	item.ETag = protocolDefinitionETag(item)
	if item.Version != expectedVersion {
		return ProtocolFormulaCommandResult{}, ErrProtocolHeadVersionConflict
	}
	if item.Status != "DRAFT" || item.ETag != strings.ToLower(strings.TrimSpace(draftETag)) || !validJSONObject(item.RequestSchema) {
		return ProtocolFormulaCommandResult{}, ErrProtocolDraftETagMismatch
	}
	modelDraftRows, err := tx.Query(ctx, `SELECT model_id FROM model_catalog_drafts WHERE protocol_code=$1 AND protocol_version=$2 FOR SHARE`, item.ProtocolCode, item.Version)
	if err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	var modelDraftReferences int64
	for modelDraftRows.Next() {
		var modelID uuid.UUID
		if err := modelDraftRows.Scan(&modelID); err != nil {
			modelDraftRows.Close()
			return ProtocolFormulaCommandResult{}, err
		}
		modelDraftReferences++
	}
	if err := modelDraftRows.Err(); err != nil {
		modelDraftRows.Close()
		return ProtocolFormulaCommandResult{}, err
	}
	modelDraftRows.Close()
	formulaRows, err := tx.Query(ctx, `SELECT id,protocol_definition_id,$2,version,status,required_unit_versions,expression,evaluator_version,created_at,published_at FROM protocol_formula_versions WHERE protocol_definition_id=$1 AND status IN ('DRAFT','PUBLISHED') ORDER BY id FOR SHARE`, id, item.ProtocolCode)
	if err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	lockedFormulas := make([]ProtocolFormulaVersion, 0)
	for formulaRows.Next() {
		var formula ProtocolFormulaVersion
		var requiredJSON, expressionJSON []byte
		if err := formulaRows.Scan(&formula.ID, &formula.ProtocolDefinitionID, &formula.ProtocolCode, &formula.Version, &formula.Status, &requiredJSON, &expressionJSON, &formula.EvaluatorVersion, &formula.CreatedAt, &formula.PublishedAt); err != nil {
			formulaRows.Close()
			return ProtocolFormulaCommandResult{}, err
		}
		if json.Unmarshal(requiredJSON, &formula.RequiredUnitVersions) != nil || json.Unmarshal(expressionJSON, &formula.Expression) != nil || formula.EvaluatorVersion != "SAFE_PARAMETER_MAP_V1" || validateSafeFormula(formula.RequiredUnitVersions, formula.Expression) != nil {
			formulaRows.Close()
			return ProtocolFormulaCommandResult{}, ErrProtocolFormulaPrecheck
		}
		lockedFormulas = append(lockedFormulas, formula)
	}
	if err := formulaRows.Err(); err != nil {
		formulaRows.Close()
		return ProtocolFormulaCommandResult{}, err
	}
	formulaRows.Close()
	formulaSetDigest := protocolPublicationFormulaDigest(lockedFormulas)
	if validation.ProtocolDefinitionID != item.ID || validation.ProtocolCode != item.ProtocolCode || validation.ProtocolVersion != item.Version || validation.FormulaSetSHA256 != formulaSetDigest || !protocolPublicationUnitsValid(lockedFormulas, validation.UnitVersions) {
		return ProtocolFormulaCommandResult{}, ErrProtocolFormulaPrecheck
	}
	now := r.protocolCommandNow()
	if _, err := tx.Exec(ctx, `UPDATE protocol_definitions SET status='PUBLISHED',published_at=$2 WHERE id=$1 AND status='DRAFT'`, id, now); err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	item.Status, item.PublishedAt = "PUBLISHED", &now
	item.ETag = protocolDefinitionETag(item)
	precheck := map[string]any{"compatible_formula_count": len(lockedFormulas), "verified_metering_unit_version_count": len(validation.UnitVersions), "formula_set_sha256": formulaSetDigest, "model_draft_reference_count": modelDraftReferences, "protocol_code": item.ProtocolCode, "protocol_version": item.Version, "request_schema_valid": true, "before_status": "DRAFT", "after_status": "PUBLISHED"}
	if err := recordProtocolFormulaCommand(ctx, tx, metadata, "PROTOCOL", item.ID, item.Version, item.ETag, precheck, now); err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	return protocolCommandResult(metadata, &item, nil, precheck, ""), nil
}

func (r *PostgresRepository) FormulaDraft(ctx context.Context, id uuid.UUID) (ProtocolFormulaVersion, error) {
	var item ProtocolFormulaVersion
	var required, expression []byte
	err := r.pool.QueryRow(ctx, `SELECT formula.id,formula.protocol_definition_id,protocol.protocol_code,formula.version,formula.status,formula.required_unit_versions,formula.expression,formula.evaluator_version,formula.created_at,formula.published_at FROM protocol_formula_versions formula JOIN protocol_definitions protocol ON protocol.id=formula.protocol_definition_id WHERE formula.id=$1`, id).Scan(&item.ID, &item.ProtocolDefinitionID, &item.ProtocolCode, &item.Version, &item.Status, &required, &expression, &item.EvaluatorVersion, &item.CreatedAt, &item.PublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, ErrNotFound
	}
	if err != nil {
		return item, err
	}
	if json.Unmarshal(required, &item.RequiredUnitVersions) != nil || json.Unmarshal(expression, &item.Expression) != nil {
		return item, ErrForbidden
	}
	item.ETag = protocolFormulaETag(item)
	return item, nil
}

func (r *PostgresRepository) PublishFormulaCommand(ctx context.Context, metadata ProtocolFormulaCommandMetadata, id uuid.UUID, expectedVersion int64, draftETag string, billingSync FormulaBindingSync) (ProtocolFormulaCommandResult, error) {
	if err := validateProtocolFormulaCommandMetadata(metadata); err != nil || id == uuid.Nil || expectedVersion < 1 || !validCommandETag(draftETag) {
		return ProtocolFormulaCommandResult{}, ErrProtocolFormulaPrecheck
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	defer tx.Rollback(ctx)
	var item ProtocolFormulaVersion
	var protocolStatus string
	var required, expression []byte
	err = tx.QueryRow(ctx, `SELECT formula.id,formula.protocol_definition_id,protocol.protocol_code,protocol.status,formula.version,formula.status,formula.required_unit_versions,formula.expression,formula.evaluator_version,formula.created_at,formula.published_at FROM protocol_formula_versions formula JOIN protocol_definitions protocol ON protocol.id=formula.protocol_definition_id WHERE formula.id=$1 FOR UPDATE OF formula`, id).Scan(&item.ID, &item.ProtocolDefinitionID, &item.ProtocolCode, &protocolStatus, &item.Version, &item.Status, &required, &expression, &item.EvaluatorVersion, &item.CreatedAt, &item.PublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProtocolFormulaCommandResult{}, ErrNotFound
	}
	if err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	if json.Unmarshal(required, &item.RequiredUnitVersions) != nil || json.Unmarshal(expression, &item.Expression) != nil || validateSafeFormula(item.RequiredUnitVersions, item.Expression) != nil || protocolStatus != "PUBLISHED" {
		return ProtocolFormulaCommandResult{}, ErrProtocolFormulaPrecheck
	}
	item.ETag = protocolFormulaETag(item)
	if item.Version != expectedVersion {
		return ProtocolFormulaCommandResult{}, ErrFormulaHeadVersionConflict
	}
	if item.Status != "DRAFT" || item.ETag != strings.ToLower(strings.TrimSpace(draftETag)) {
		return ProtocolFormulaCommandResult{}, ErrFormulaDraftETagMismatch
	}
	requiredIDs, snapshotDigest := formulaBindingSnapshot(item)
	if billingSync.FormulaVersionID != item.ID || billingSync.BillingStatus != "REGISTERED" || strings.ToUpper(strings.TrimSpace(billingSync.ProtocolCode)) != item.ProtocolCode || billingSync.PublishedAt.IsZero() || billingSync.FormulaSnapshotSHA256 != snapshotDigest || !uuidSlicesEqual(requiredIDs, billingSync.RequiredUnitVersions) {
		return ProtocolFormulaCommandResult{}, ErrProtocolFormulaPrecheck
	}
	now := billingSync.PublishedAt.UTC()
	if _, err := tx.Exec(ctx, `UPDATE protocol_formula_versions SET status='PUBLISHED',published_at=$2 WHERE id=$1 AND status='DRAFT'`, id, now); err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	item.Status, item.PublishedAt = "PUBLISHED", &now
	item.ETag = protocolFormulaETag(item)
	precheck := map[string]any{"deterministic_preview": true, "metering_unit_version_count": len(item.RequiredUnitVersions), "billing_sync": map[string]any{"formula_version_id": billingSync.FormulaVersionID, "status": billingSync.BillingStatus, "protocol_code": billingSync.ProtocolCode, "required_unit_version_ids": billingSync.RequiredUnitVersions, "formula_snapshot_sha256": billingSync.FormulaSnapshotSHA256, "published_at": billingSync.PublishedAt}, "before_status": "DRAFT", "after_status": "PUBLISHED"}
	if err := recordProtocolFormulaCommand(ctx, tx, metadata, "PROTOCOL_FORMULA", item.ID, item.Version, item.ETag, precheck, now); err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ProtocolFormulaCommandResult{}, err
	}
	return protocolCommandResult(metadata, nil, &item, precheck, "SYNCHRONIZED"), nil
}

func formulaBindingSnapshot(item ProtocolFormulaVersion) ([]uuid.UUID, string) {
	required := make([]uuid.UUID, 0, len(item.RequiredUnitVersions))
	for _, unitID := range item.RequiredUnitVersions {
		required = append(required, unitID)
	}
	sort.Slice(required, func(i, j int) bool { return required[i].String() < required[j].String() })
	snapshot, _ := json.Marshal(map[string]any{"evaluator_version": item.EvaluatorVersion, "expression": item.Expression})
	digest := sha256.Sum256(snapshot)
	return required, hex.EncodeToString(digest[:])
}

func protocolPublicationFormulaDigest(formulas []ProtocolFormulaVersion) string {
	items := make([]map[string]any, 0, len(formulas))
	for _, formula := range formulas {
		items = append(items, map[string]any{
			"formula_version_id":     formula.ID,
			"protocol_definition_id": formula.ProtocolDefinitionID,
			"version":                formula.Version,
			"status":                 formula.Status,
			"required_unit_versions": formula.RequiredUnitVersions,
			"expression":             formula.Expression,
			"evaluator_version":      formula.EvaluatorVersion,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		return items[i]["formula_version_id"].(uuid.UUID).String() < items[j]["formula_version_id"].(uuid.UUID).String()
	})
	encoded, _ := json.Marshal(items)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func protocolPublicationUnitsValid(formulas []ProtocolFormulaVersion, validated []MeteringUnitVersionReference) bool {
	required := make(map[string]uuid.UUID)
	for _, formula := range formulas {
		for rawCode, id := range formula.RequiredUnitVersions {
			code := strings.ToUpper(strings.TrimSpace(rawCode))
			if previous, exists := required[code]; exists && previous != id {
				return false
			}
			required[code] = id
		}
	}
	if len(required) != len(validated) {
		return false
	}
	seen := make(map[string]bool, len(validated))
	for _, unit := range validated {
		code := strings.ToUpper(strings.TrimSpace(unit.UnitCode))
		if unit.Status != "ACTIVE" || unit.Version < 1 || required[code] != unit.UnitDefinitionID || seen[code] {
			return false
		}
		seen[code] = true
	}
	return true
}

func uuidSlicesEqual(left, right []uuid.UUID) bool {
	if len(left) != len(right) {
		return false
	}
	rightCopy := append([]uuid.UUID(nil), right...)
	sort.Slice(rightCopy, func(i, j int) bool { return rightCopy[i].String() < rightCopy[j].String() })
	for index := range left {
		if left[index] != rightCopy[index] {
			return false
		}
	}
	return true
}

func lockTokenKey(ctx context.Context, tx pgx.Tx, namespace, key string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, namespace+"|"+key)
	return err
}

func (r *PostgresRepository) protocolCommandNow() time.Time {
	if r.now != nil {
		return r.now().UTC()
	}
	return time.Now().UTC()
}

func validateProtocolFormulaCommandMetadata(metadata ProtocolFormulaCommandMetadata) error {
	reason := strings.TrimSpace(metadata.Reason)
	if metadata.ActorID == uuid.Nil || !validProtocolFormulaReason(reason) || len(metadata.IdempotencyKeyDigest) != 64 || len(metadata.RequestFingerprint) != 64 || len(metadata.CommandReference) != 64 || strings.TrimSpace(metadata.CommandType) == "" {
		return ErrProtocolFormulaPrecheck
	}
	return nil
}

func validJSONObject(raw json.RawMessage) bool {
	_, ok := canonicalJSONObject(raw)
	return ok
}

func canonicalJSONObject(raw json.RawMessage) (json.RawMessage, bool) {
	if !json.Valid(raw) {
		return nil, false
	}
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil || value == nil {
		return nil, false
	}
	canonical, err := json.Marshal(value)
	return canonical, err == nil
}

func validCommandETag(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func protocolDefinitionETag(item ProtocolDefinition) string {
	canonical, _ := json.Marshal(map[string]any{
		"protocol_definition_id": item.ID,
		"protocol_code":          strings.ToUpper(strings.TrimSpace(item.ProtocolCode)),
		"version":                item.Version,
		"status":                 item.Status,
		"request_schema":         item.RequestSchema,
		"published_at":           item.PublishedAt,
	})
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

func protocolFormulaETag(item ProtocolFormulaVersion) string {
	canonical, _ := json.Marshal(map[string]any{
		"formula_version_id":     item.ID,
		"protocol_definition_id": item.ProtocolDefinitionID,
		"version":                item.Version,
		"status":                 item.Status,
		"required_unit_versions": item.RequiredUnitVersions,
		"expression":             item.Expression,
		"evaluator_version":      item.EvaluatorVersion,
		"published_at":           item.PublishedAt,
	})
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:])
}

func recordProtocolFormulaCommand(ctx context.Context, tx pgx.Tx, metadata ProtocolFormulaCommandMetadata, resourceType string, resourceID uuid.UUID, resourceVersion int64, resourceETag string, precheck map[string]any, occurredAt time.Time) error {
	traceID := strings.TrimSpace(metadata.TraceID)
	if traceID == "" {
		traceID = metadata.CommandReference
	}
	summary, err := json.Marshal(map[string]any{
		"command_type":           metadata.CommandType,
		"policy_version":         1,
		"permission":             "platform.pricing.manage",
		"reason":                 strings.TrimSpace(metadata.Reason),
		"idempotency_key_digest": metadata.IdempotencyKeyDigest,
		"request_fingerprint":    metadata.RequestFingerprint,
		"command_reference":      metadata.CommandReference,
		"request_id":             traceID,
		"resource_version":       resourceVersion,
		"resource_etag":          resourceETag,
		"precheck":               precheck,
	})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO token_audit_logs(id,actor_user_id,action,resource_type,resource_id,summary,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, uuid.Must(uuid.NewV7()), metadata.ActorID, metadata.CommandType, resourceType, resourceID, summary, occurredAt); err != nil {
		return err
	}
	payload, err := json.Marshal(map[string]any{
		"command_type":        metadata.CommandType,
		"policy_version":      1,
		"permission":          "platform.pricing.manage",
		"command_reference":   metadata.CommandReference,
		"request_id":          traceID,
		"resource_type":       resourceType,
		"resource_id":         resourceID,
		"resource_version":    resourceVersion,
		"resource_etag":       resourceETag,
		"request_fingerprint": metadata.RequestFingerprint,
		"precheck":            precheck,
	})
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO outbox_events(event_id,event_type,aggregate_id,aggregate_version,payload,trace_id,occurred_at) VALUES($1,'token.management-command.executed.v1',$2,$3,$4,$5,$6)`, uuid.Must(uuid.NewV7()), resourceID, resourceVersion, payload, traceID, occurredAt)
	return err
}

func protocolCommandResult(metadata ProtocolFormulaCommandMetadata, protocol *ProtocolDefinition, formula *ProtocolFormulaVersion, precheck map[string]any, billingSync string) ProtocolFormulaCommandResult {
	return ProtocolFormulaCommandResult{
		CommandReference: metadata.CommandReference,
		CommandType:      metadata.CommandType,
		PolicyVersion:    1,
		Status:           "EXECUTED",
		Protocol:         protocol,
		Formula:          formula,
		Precheck:         precheck,
		BillingSync:      billingSync,
	}
}

func protocolFormulaCommandMetadata(r *http.Request, commandType, reason string, body any) (ProtocolFormulaCommandMetadata, error) {
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	reason = strings.TrimSpace(reason)
	if len(key) < 16 || len(key) > 128 || !validProtocolFormulaReason(reason) {
		return ProtocolFormulaCommandMetadata{}, ErrProtocolFormulaCommandInput
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return ProtocolFormulaCommandMetadata{}, ErrProtocolFormulaCommandInput
	}
	actorID := tokenAdminActor(r)
	keyDigest := sha256.Sum256([]byte(key))
	requestDigest := sha256.Sum256([]byte(commandType + "\x00" + r.Method + "\x00" + r.URL.Path + "\x00" + string(payload)))
	referenceDigest := sha256.Sum256([]byte("pinvou-token-command-reference-v1\x00" + actorID.String() + "\x00" + commandType + "\x00" + hex.EncodeToString(keyDigest[:])))
	return ProtocolFormulaCommandMetadata{
		ActorID:              actorID,
		CommandType:          commandType,
		Reason:               reason,
		IdempotencyKeyDigest: hex.EncodeToString(keyDigest[:]),
		RequestFingerprint:   hex.EncodeToString(requestDigest[:]),
		CommandReference:     hex.EncodeToString(referenceDigest[:]),
		TraceID:              httpserver.RequestID(r.Context()),
	}, nil
}

func validProtocolFormulaReason(reason string) bool {
	return utf8.ValidString(reason) && len([]rune(reason)) >= 10 && len([]rune(reason)) <= 2000
}

func requireRecentProtocolFormulaAuthentication(w http.ResponseWriter, r *http.Request, maximumAge time.Duration) bool {
	principal, ok := identity.FromContext(r.Context())
	now := time.Now().UTC()
	age := now.Sub(principal.AuthenticatedAt.UTC())
	if !ok || principal.AuthenticatedAt.IsZero() || age < -time.Minute || age > maximumAge {
		api.WriteError(w, http.StatusUnauthorized, "RECENT_AUTHENTICATION_REQUIRED", "recent authentication is required for publishing", httpserver.RequestID(r.Context()))
		return false
	}
	return true
}

func writeProtocolFormulaCommandError(w http.ResponseWriter, r *http.Request, metadata ProtocolFormulaCommandMetadata, err error) {
	requestID := httpserver.RequestID(r.Context())
	switch {
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		details := map[string]any{"command_reference": metadata.CommandReference}
		if metadata.CommandReference != "" {
			details["result_query"] = "/token/admin/v1/commands/" + metadata.CommandReference
		}
		api.WriteJSON(w, http.StatusGatewayTimeout, api.Error{Code: "COMMAND_RESULT_UNKNOWN", Message: "the command outcome is unknown; query the same command reference", RequestID: requestID, Details: details})
	case errors.Is(err, ErrProtocolFormulaCommandInput):
		api.WriteError(w, http.StatusBadRequest, "INVALID_COMMAND", "reason and Idempotency-Key are required", requestID)
	case errors.Is(err, ErrProtocolHeadVersionConflict):
		api.WriteError(w, http.StatusConflict, "PROTOCOL_VERSION_CONFLICT", "the protocol version has changed", requestID)
	case errors.Is(err, ErrFormulaHeadVersionConflict):
		api.WriteError(w, http.StatusConflict, "FORMULA_VERSION_CONFLICT", "the formula version has changed", requestID)
	case errors.Is(err, ErrProtocolDraftETagMismatch), errors.Is(err, ErrFormulaDraftETagMismatch):
		api.WriteError(w, http.StatusPreconditionFailed, "DRAFT_ETAG_MISMATCH", "the immutable draft snapshot does not match", requestID)
	case errors.Is(err, ErrProtocolFormulaPrecheck):
		api.WriteError(w, http.StatusUnprocessableEntity, "COMMAND_PRECHECK_FAILED", "the protocol or formula command did not pass precheck", requestID)
	case errors.Is(err, ErrNotFound):
		api.WriteError(w, http.StatusNotFound, "PROTOCOL_FORMULA_NOT_FOUND", "the protocol or formula version was not found", requestID)
	default:
		api.WriteError(w, http.StatusInternalServerError, "PROTOCOL_FORMULA_COMMAND_FAILED", "the protocol or formula command could not be completed", requestID)
	}
}

func writeFormulaBillingSyncError(w http.ResponseWriter, r *http.Request, metadata ProtocolFormulaCommandMetadata, err error) {
	var billingError *BillingHTTPError
	if errors.As(err, &billingError) && billingError.Status >= 400 && billingError.Status < 500 {
		api.WriteError(w, http.StatusUnprocessableEntity, "FORMULA_SYNC_REJECTED", "Billing rejected the immutable formula binding", httpserver.RequestID(r.Context()))
		return
	}
	writeProtocolFormulaCommandError(w, r, metadata, context.DeadlineExceeded)
}

func writeFormulaDraftValidationError(w http.ResponseWriter, r *http.Request, err error) {
	var billingError *BillingHTTPError
	if errors.As(err, &billingError) && billingError.Status >= 400 && billingError.Status < 500 {
		api.WriteError(w, http.StatusUnprocessableEntity, "FORMULA_UNIT_VALIDATION_REJECTED", "Billing rejected one or more exact metering unit versions", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteError(w, http.StatusServiceUnavailable, "FORMULA_UNIT_VALIDATION_UNAVAILABLE", "Billing metering unit validation is unavailable", httpserver.RequestID(r.Context()))
}

func (s *Service) adminProtocols(w http.ResponseWriter, r *http.Request) {
	if s.paginatedProtocols(w, r) {
		return
	}
	repo, ok := s.repository.(interface {
		ListProtocolsAndFormulas(context.Context) ([]ProtocolDefinition, []ProtocolFormulaVersion, error)
	})
	if !ok {
		return
	}
	protocols, formulas, err := repo.ListProtocolsAndFormulas(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"protocols": protocols, "formulas": formulas})
}
func (s *Service) adminCreateProtocol(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repository.(interface {
		CreateProtocolDraftCommand(context.Context, ProtocolFormulaCommandMetadata, string, json.RawMessage, int64) (ProtocolFormulaCommandResult, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "PROTOCOL_COMMAND_UNAVAILABLE", "protocol management is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var body struct {
		ProtocolCode                string          `json:"protocol_code"`
		RequestSchema               json.RawMessage `json:"request_schema"`
		ExpectedProtocolHeadVersion *int64          `json:"expected_protocol_head_version"`
		Reason                      string          `json:"reason"`
	}
	if api.DecodeJSON(w, r, &body) != nil || body.ExpectedProtocolHeadVersion == nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_PROTOCOL_COMMAND", "the protocol command is invalid", httpserver.RequestID(r.Context()))
		return
	}
	metadata, err := protocolFormulaCommandMetadata(r, "TOKEN_PROTOCOL_DRAFT_CREATE", body.Reason, body)
	if err != nil {
		writeProtocolFormulaCommandError(w, r, ProtocolFormulaCommandMetadata{}, err)
		return
	}
	result, err := repo.CreateProtocolDraftCommand(r.Context(), metadata, body.ProtocolCode, body.RequestSchema, *body.ExpectedProtocolHeadVersion)
	if err != nil {
		writeProtocolFormulaCommandError(w, r, metadata, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, result)
}
func (s *Service) adminCreateFormula(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repository.(interface {
		CreateFormulaDraftCommand(context.Context, ProtocolFormulaCommandMetadata, uuid.UUID, int64, int64, map[string]uuid.UUID, map[string]string, FormulaDraftValidation) (ProtocolFormulaCommandResult, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "FORMULA_COMMAND_UNAVAILABLE", "formula management is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	protocolID, err := uuid.Parse(r.PathValue("protocol_definition_id"))
	var body struct {
		RequiredUnitVersions       map[string]uuid.UUID `json:"required_unit_versions"`
		Expression                 map[string]string    `json:"expression"`
		ExpectedProtocolVersion    *int64               `json:"expected_protocol_version"`
		ExpectedFormulaHeadVersion *int64               `json:"expected_formula_head_version"`
		Reason                     string               `json:"reason"`
	}
	if err != nil || api.DecodeJSON(w, r, &body) != nil || body.ExpectedProtocolVersion == nil || body.ExpectedFormulaHeadVersion == nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_FORMULA_COMMAND", "the formula command is invalid", httpserver.RequestID(r.Context()))
		return
	}
	metadata, err := protocolFormulaCommandMetadata(r, "TOKEN_FORMULA_DRAFT_CREATE", body.Reason, body)
	if err != nil {
		writeProtocolFormulaCommandError(w, r, ProtocolFormulaCommandMetadata{}, err)
		return
	}
	required, expression, err := normalizeSafeFormula(body.RequiredUnitVersions, body.Expression)
	if err != nil {
		writeProtocolFormulaCommandError(w, r, metadata, ErrProtocolFormulaPrecheck)
		return
	}
	validator, ok := s.billing.(FormulaDraftValidationClient)
	if !ok {
		writeFormulaDraftValidationError(w, r, errors.New("Billing formula validation client is unavailable"))
		return
	}
	validation, err := validator.ValidateFormulaDraftUnits(r.Context(), required)
	if err != nil {
		writeFormulaDraftValidationError(w, r, err)
		return
	}
	if err := validateFormulaDraftValidation(required, validation); err != nil {
		writeFormulaDraftValidationError(w, r, &BillingHTTPError{Status: http.StatusUnprocessableEntity})
		return
	}
	result, err := repo.CreateFormulaDraftCommand(r.Context(), metadata, protocolID, *body.ExpectedProtocolVersion, *body.ExpectedFormulaHeadVersion, required, expression, validation)
	if err != nil {
		writeProtocolFormulaCommandError(w, r, metadata, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, result)
}
func (s *Service) adminPublishProtocol(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repository.(interface {
		ProtocolPublishDependencies(context.Context, uuid.UUID) (ProtocolDefinition, []ProtocolFormulaVersion, error)
		PublishProtocolCommand(context.Context, ProtocolFormulaCommandMetadata, uuid.UUID, int64, string, ProtocolPublicationValidation) (ProtocolFormulaCommandResult, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "PROTOCOL_COMMAND_UNAVAILABLE", "protocol management is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	id, err := uuid.Parse(r.PathValue("protocol_definition_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_PROTOCOL_COMMAND", "the protocol command is invalid", httpserver.RequestID(r.Context()))
		return
	}
	var body struct {
		ExpectedProtocolVersion *int64 `json:"expected_protocol_version"`
		DraftETag               string `json:"draft_etag"`
		Reason                  string `json:"reason"`
	}
	if api.DecodeJSON(w, r, &body) != nil || body.ExpectedProtocolVersion == nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_PROTOCOL_COMMAND", "the protocol command is invalid", httpserver.RequestID(r.Context()))
		return
	}
	metadata, err := protocolFormulaCommandMetadata(r, "TOKEN_PROTOCOL_PUBLISH", body.Reason, body)
	if err != nil {
		writeProtocolFormulaCommandError(w, r, ProtocolFormulaCommandMetadata{}, err)
		return
	}
	if !requireRecentProtocolFormulaAuthentication(w, r, 15*time.Minute) {
		return
	}
	protocol, formulas, err := repo.ProtocolPublishDependencies(r.Context(), id)
	if err != nil {
		writeProtocolFormulaCommandError(w, r, metadata, err)
		return
	}
	validatedUnits := make(map[string]MeteringUnitVersionReference)
	if len(formulas) > 0 {
		validator, ok := s.billing.(FormulaDraftValidationClient)
		if !ok {
			writeFormulaDraftValidationError(w, r, errors.New("Billing formula validation client is unavailable"))
			return
		}
		for _, formula := range formulas {
			validation, validationErr := validator.ValidateFormulaDraftUnits(r.Context(), formula.RequiredUnitVersions)
			if validationErr != nil {
				writeFormulaDraftValidationError(w, r, validationErr)
				return
			}
			if validationErr := validateFormulaDraftValidation(formula.RequiredUnitVersions, validation); validationErr != nil {
				writeFormulaDraftValidationError(w, r, &BillingHTTPError{Status: http.StatusUnprocessableEntity})
				return
			}
			for _, unit := range validation.UnitVersions {
				code := strings.ToUpper(strings.TrimSpace(unit.UnitCode))
				if previous, exists := validatedUnits[code]; exists && previous.UnitDefinitionID != unit.UnitDefinitionID {
					writeProtocolFormulaCommandError(w, r, metadata, ErrProtocolFormulaPrecheck)
					return
				}
				validatedUnits[code] = unit
			}
		}
	}
	unitVersions := make([]MeteringUnitVersionReference, 0, len(validatedUnits))
	for _, unit := range validatedUnits {
		unitVersions = append(unitVersions, unit)
	}
	sort.Slice(unitVersions, func(i, j int) bool { return unitVersions[i].UnitCode < unitVersions[j].UnitCode })
	publicationValidation := ProtocolPublicationValidation{ProtocolDefinitionID: protocol.ID, ProtocolCode: protocol.ProtocolCode, ProtocolVersion: protocol.Version, FormulaSetSHA256: protocolPublicationFormulaDigest(formulas), UnitVersions: unitVersions}
	result, err := repo.PublishProtocolCommand(r.Context(), metadata, id, *body.ExpectedProtocolVersion, body.DraftETag, publicationValidation)
	if err != nil {
		writeProtocolFormulaCommandError(w, r, metadata, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}
func (s *Service) adminPublishFormula(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repository.(interface {
		FormulaDraft(context.Context, uuid.UUID) (ProtocolFormulaVersion, error)
		PublishFormulaCommand(context.Context, ProtocolFormulaCommandMetadata, uuid.UUID, int64, string, FormulaBindingSync) (ProtocolFormulaCommandResult, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "FORMULA_COMMAND_UNAVAILABLE", "formula management is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	id, err := uuid.Parse(r.PathValue("formula_version_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_FORMULA_COMMAND", "the formula command is invalid", httpserver.RequestID(r.Context()))
		return
	}
	var body struct {
		ExpectedFormulaVersion *int64 `json:"expected_formula_version"`
		DraftETag              string `json:"draft_etag"`
		Reason                 string `json:"reason"`
	}
	if api.DecodeJSON(w, r, &body) != nil || body.ExpectedFormulaVersion == nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_FORMULA_COMMAND", "the formula command is invalid", httpserver.RequestID(r.Context()))
		return
	}
	metadata, err := protocolFormulaCommandMetadata(r, "TOKEN_FORMULA_PUBLISH", body.Reason, body)
	if err != nil {
		writeProtocolFormulaCommandError(w, r, ProtocolFormulaCommandMetadata{}, err)
		return
	}
	if !requireRecentProtocolFormulaAuthentication(w, r, 15*time.Minute) {
		return
	}
	draft, err := repo.FormulaDraft(r.Context(), id)
	if err != nil {
		writeProtocolFormulaCommandError(w, r, metadata, err)
		return
	}
	if draft.Version != *body.ExpectedFormulaVersion {
		writeProtocolFormulaCommandError(w, r, metadata, ErrFormulaHeadVersionConflict)
		return
	}
	if draft.Status != "DRAFT" || draft.ETag != strings.ToLower(strings.TrimSpace(body.DraftETag)) {
		writeProtocolFormulaCommandError(w, r, metadata, ErrFormulaDraftETagMismatch)
		return
	}
	client, ok := s.billing.(FormulaBindingClient)
	if !ok {
		writeFormulaBillingSyncError(w, r, metadata, errors.New("billing formula synchronization is unavailable"))
		return
	}
	required := make([]uuid.UUID, 0, len(draft.RequiredUnitVersions))
	for _, unitID := range draft.RequiredUnitVersions {
		required = append(required, unitID)
	}
	sort.Slice(required, func(i, j int) bool { return required[i].String() < required[j].String() })
	snapshot, _ := json.Marshal(map[string]any{"evaluator_version": draft.EvaluatorVersion, "expression": draft.Expression})
	receipt, err := client.RegisterFormulaBinding(r.Context(), id, draft.ProtocolCode, required, snapshot)
	if err != nil {
		writeFormulaBillingSyncError(w, r, metadata, err)
		return
	}
	snapshotDigest := sha256.Sum256(snapshot)
	if receipt.FormulaVersionID != id || receipt.Status != "REGISTERED" || strings.ToUpper(strings.TrimSpace(receipt.ProtocolCode)) != draft.ProtocolCode || !uuidSlicesEqual(required, receipt.RequiredMeteringUnitDefinitionIDs) || receipt.FormulaSnapshotSHA256 != hex.EncodeToString(snapshotDigest[:]) || receipt.PublishedAt.IsZero() {
		writeFormulaBillingSyncError(w, r, metadata, &BillingHTTPError{Status: http.StatusConflict})
		return
	}
	result, err := repo.PublishFormulaCommand(r.Context(), metadata, id, *body.ExpectedFormulaVersion, body.DraftETag, FormulaBindingSync{FormulaVersionID: receipt.FormulaVersionID, BillingStatus: receipt.Status, ProtocolCode: receipt.ProtocolCode, RequiredUnitVersions: receipt.RequiredMeteringUnitDefinitionIDs, FormulaSnapshotSHA256: receipt.FormulaSnapshotSHA256, PublishedAt: receipt.PublishedAt})
	if err != nil {
		if errors.Is(err, ErrProtocolHeadVersionConflict) || errors.Is(err, ErrFormulaHeadVersionConflict) || errors.Is(err, ErrProtocolDraftETagMismatch) || errors.Is(err, ErrFormulaDraftETagMismatch) || errors.Is(err, ErrProtocolFormulaPrecheck) || errors.Is(err, ErrNotFound) {
			writeProtocolFormulaCommandError(w, r, metadata, err)
		} else {
			writeProtocolFormulaCommandError(w, r, metadata, context.DeadlineExceeded)
		}
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}

func (s *Service) adminPreviewFormula(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repository.(interface {
		FormulaDraft(context.Context, uuid.UUID) (ProtocolFormulaVersion, error)
	})
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("formula_version_id"))
	if err != nil {
		return
	}
	item, err := repo.FormulaDraft(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	if err := validateSafeFormula(item.RequiredUnitVersions, item.Expression); err != nil {
		s.writeError(w, r, err)
		return
	}
	sample := make(map[string]int64, len(item.Expression))
	for unit := range item.Expression {
		sample[unit] = 0
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"formula": item, "sample_missing_optional_parameters": sample, "deterministic": true, "external_access": false})
}
