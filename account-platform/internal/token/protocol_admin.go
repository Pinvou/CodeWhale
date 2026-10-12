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

type FormulaBindingSync struct {
	FormulaVersionID      uuid.UUID
	BillingStatus         string
	ProtocolCode          string
	RequiredUnitVersions  []uuid.UUID
	FormulaSnapshotSHA256 string
	PublishedAt           time.Time
}

// ProtocolPublicationValidation is immutable evidence produced by the
// service-layer Billing preflight. The governed transaction adapter recomputes the
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
