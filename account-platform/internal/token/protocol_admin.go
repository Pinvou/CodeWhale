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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrProtocolHeadVersionConflict = errors.New("protocol head version conflict")
	ErrFormulaHeadVersionConflict  = errors.New("formula head version conflict")
	ErrProtocolDraftETagMismatch   = errors.New("protocol draft etag mismatch")
	ErrFormulaDraftETagMismatch    = errors.New("formula draft etag mismatch")
	ErrProtocolFormulaPrecheck     = errors.New("protocol or formula precheck failed")
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
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
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
	var protocolStatus string
	var required, expression []byte
	err := r.pool.QueryRow(ctx, `SELECT formula.id,formula.protocol_definition_id,protocol.protocol_code,protocol.status,formula.version,formula.status,formula.required_unit_versions,formula.expression,formula.evaluator_version,formula.created_at,formula.published_at FROM protocol_formula_versions formula JOIN protocol_definitions protocol ON protocol.id=formula.protocol_definition_id WHERE formula.id=$1`, id).Scan(&item.ID, &item.ProtocolDefinitionID, &item.ProtocolCode, &protocolStatus, &item.Version, &item.Status, &required, &expression, &item.EvaluatorVersion, &item.CreatedAt, &item.PublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, ErrNotFound
	}
	if err != nil {
		return item, err
	}
	if protocolStatus != "PUBLISHED" || item.EvaluatorVersion != "SAFE_PARAMETER_MAP_V1" || json.Unmarshal(required, &item.RequiredUnitVersions) != nil || json.Unmarshal(expression, &item.Expression) != nil || validateSafeFormula(item.RequiredUnitVersions, item.Expression) != nil {
		return item, ErrProtocolFormulaPrecheck
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
	if decodeExactCommandJSON(raw, &value) != nil || value == nil {
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

func validProtocolFormulaReason(reason string) bool {
	return utf8.ValidString(reason) && len([]rune(reason)) >= 10 && len([]rune(reason)) <= 2000
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
