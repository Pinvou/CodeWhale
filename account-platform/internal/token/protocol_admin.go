package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type ProtocolDefinition struct {
	ID            uuid.UUID       `json:"protocol_definition_id"`
	ProtocolCode  string          `json:"protocol_code"`
	Version       int64           `json:"version"`
	Status        string          `json:"status"`
	RequestSchema json.RawMessage `json:"request_schema"`
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
	CreatedAt            time.Time            `json:"created_at"`
	PublishedAt          *time.Time           `json:"published_at,omitempty"`
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
		formulas = append(formulas, item)
	}
	return protocols, formulas, rows.Err()
}

func (r *PostgresRepository) CreateProtocolDraft(ctx context.Context, code string, schema json.RawMessage) (ProtocolDefinition, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" || !json.Valid(schema) {
		return ProtocolDefinition{}, ErrForbidden
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ProtocolDefinition{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTokenKey(ctx, tx, "protocol", code); err != nil {
		return ProtocolDefinition{}, err
	}
	var version int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version),0)+1 FROM protocol_definitions WHERE protocol_code=$1`, code).Scan(&version); err != nil {
		return ProtocolDefinition{}, err
	}
	item := ProtocolDefinition{ID: uuid.Must(uuid.NewV7()), ProtocolCode: code, Version: version, Status: "DRAFT", RequestSchema: schema, CreatedAt: r.now()}
	if _, err := tx.Exec(ctx, `INSERT INTO protocol_definitions(id,protocol_code,version,status,request_schema,created_at) VALUES($1,$2,$3,'DRAFT',$4,$5)`, item.ID, code, version, []byte(schema), item.CreatedAt); err != nil {
		return ProtocolDefinition{}, err
	}
	return item, tx.Commit(ctx)
}

func (r *PostgresRepository) CreateFormulaDraft(ctx context.Context, protocolID uuid.UUID, required map[string]uuid.UUID, expression map[string]string) (ProtocolFormulaVersion, error) {
	if protocolID == uuid.Nil || validateSafeFormula(required, expression) != nil {
		return ProtocolFormulaVersion{}, ErrForbidden
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ProtocolFormulaVersion{}, err
	}
	defer tx.Rollback(ctx)
	var code, status string
	if err := tx.QueryRow(ctx, `SELECT protocol_code,status FROM protocol_definitions WHERE id=$1`, protocolID).Scan(&code, &status); err != nil {
		return ProtocolFormulaVersion{}, ErrNotFound
	}
	if status != "PUBLISHED" {
		return ProtocolFormulaVersion{}, ErrForbidden
	}
	if err := lockTokenKey(ctx, tx, "formula", protocolID.String()); err != nil {
		return ProtocolFormulaVersion{}, err
	}
	var version int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version),0)+1 FROM protocol_formula_versions WHERE protocol_definition_id=$1`, protocolID).Scan(&version); err != nil {
		return ProtocolFormulaVersion{}, err
	}
	requiredJSON, _ := json.Marshal(required)
	expressionJSON, _ := json.Marshal(expression)
	item := ProtocolFormulaVersion{ID: uuid.Must(uuid.NewV7()), ProtocolDefinitionID: protocolID, ProtocolCode: code, Version: version, Status: "DRAFT", RequiredUnitVersions: required, Expression: expression, EvaluatorVersion: "SAFE_PARAMETER_MAP_V1", CreatedAt: r.now()}
	if _, err := tx.Exec(ctx, `INSERT INTO protocol_formula_versions(id,protocol_definition_id,version,status,required_unit_versions,expression,evaluator_version,created_at) VALUES($1,$2,$3,'DRAFT',$4,$5,'SAFE_PARAMETER_MAP_V1',$6)`, item.ID, protocolID, version, requiredJSON, expressionJSON, item.CreatedAt); err != nil {
		return ProtocolFormulaVersion{}, err
	}
	return item, tx.Commit(ctx)
}

func validateSafeFormula(required map[string]uuid.UUID, expression map[string]string) error {
	if len(required) == 0 || len(required) > 32 || len(required) != len(expression) {
		return ErrForbidden
	}
	allowed := map[string]bool{"estimated_input_tokens": true, "cached_input_tokens": true, "max_output_tokens": true, "image_count": true, "request_count": true, "document_count": true}
	for unit, id := range required {
		if strings.TrimSpace(unit) == "" || id == uuid.Nil || !allowed[expression[unit]] {
			return ErrForbidden
		}
	}
	for unit := range expression {
		if required[unit] == uuid.Nil {
			return ErrForbidden
		}
	}
	return nil
}

func (r *PostgresRepository) PublishProtocol(ctx context.Context, id uuid.UUID) (ProtocolDefinition, error) {
	now := r.now()
	var item ProtocolDefinition
	err := r.pool.QueryRow(ctx, `UPDATE protocol_definitions SET status='PUBLISHED',published_at=$2 WHERE id=$1 AND status='DRAFT' RETURNING id,protocol_code,version,status,request_schema,created_at,published_at`, id, now).Scan(&item.ID, &item.ProtocolCode, &item.Version, &item.Status, &item.RequestSchema, &item.CreatedAt, &item.PublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, ErrNotFound
	}
	return item, err
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
	return item, nil
}
func (r *PostgresRepository) PublishFormula(ctx context.Context, id uuid.UUID) (ProtocolFormulaVersion, error) {
	now := r.now()
	var item ProtocolFormulaVersion
	var required, expression []byte
	err := r.pool.QueryRow(ctx, `UPDATE protocol_formula_versions formula SET status='PUBLISHED',published_at=$2 FROM protocol_definitions protocol WHERE formula.id=$1 AND formula.status='DRAFT' AND protocol.id=formula.protocol_definition_id AND protocol.status='PUBLISHED' RETURNING formula.id,formula.protocol_definition_id,protocol.protocol_code,formula.version,formula.status,formula.required_unit_versions,formula.expression,formula.evaluator_version,formula.created_at,formula.published_at`, id, now).Scan(&item.ID, &item.ProtocolDefinitionID, &item.ProtocolCode, &item.Version, &item.Status, &required, &expression, &item.EvaluatorVersion, &item.CreatedAt, &item.PublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, ErrNotFound
	}
	if err != nil {
		return item, err
	}
	_ = json.Unmarshal(required, &item.RequiredUnitVersions)
	_ = json.Unmarshal(expression, &item.Expression)
	return item, nil
}

func lockTokenKey(ctx context.Context, tx pgx.Tx, namespace, key string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, namespace+"|"+key)
	return err
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
		CreateProtocolDraft(context.Context, string, json.RawMessage) (ProtocolDefinition, error)
	})
	if !ok {
		return
	}
	var body struct {
		ProtocolCode  string          `json:"protocol_code"`
		RequestSchema json.RawMessage `json:"request_schema"`
	}
	if api.DecodeJSON(w, r, &body) != nil {
		return
	}
	item, err := repo.CreateProtocolDraft(r.Context(), body.ProtocolCode, body.RequestSchema)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, item)
}
func (s *Service) adminCreateFormula(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repository.(interface {
		CreateFormulaDraft(context.Context, uuid.UUID, map[string]uuid.UUID, map[string]string) (ProtocolFormulaVersion, error)
	})
	if !ok {
		return
	}
	protocolID, err := uuid.Parse(r.PathValue("protocol_definition_id"))
	var body struct {
		RequiredUnitVersions map[string]uuid.UUID `json:"required_unit_versions"`
		Expression           map[string]string    `json:"expression"`
	}
	if err != nil || api.DecodeJSON(w, r, &body) != nil {
		return
	}
	item, err := repo.CreateFormulaDraft(r.Context(), protocolID, body.RequiredUnitVersions, body.Expression)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, item)
}
func (s *Service) adminPublishProtocol(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repository.(interface {
		PublishProtocol(context.Context, uuid.UUID) (ProtocolDefinition, error)
	})
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("protocol_definition_id"))
	if err != nil {
		return
	}
	item, err := repo.PublishProtocol(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
}
func (s *Service) adminPublishFormula(w http.ResponseWriter, r *http.Request) {
	repo, ok := s.repository.(interface {
		FormulaDraft(context.Context, uuid.UUID) (ProtocolFormulaVersion, error)
		PublishFormula(context.Context, uuid.UUID) (ProtocolFormulaVersion, error)
	})
	if !ok {
		return
	}
	id, err := uuid.Parse(r.PathValue("formula_version_id"))
	if err != nil {
		return
	}
	draft, err := repo.FormulaDraft(r.Context(), id)
	if err != nil || draft.Status != "DRAFT" {
		s.writeError(w, r, err)
		return
	}
	client, ok := s.billing.(FormulaBindingClient)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "FORMULA_SYNC_UNAVAILABLE", "billing formula synchronization is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	required := make([]uuid.UUID, 0, len(draft.RequiredUnitVersions))
	for _, unitID := range draft.RequiredUnitVersions {
		required = append(required, unitID)
	}
	sort.Slice(required, func(i, j int) bool { return required[i].String() < required[j].String() })
	snapshot, _ := json.Marshal(map[string]any{"evaluator_version": draft.EvaluatorVersion, "expression": draft.Expression})
	now := time.Now().UTC()
	if err := client.RegisterFormulaBinding(r.Context(), id, draft.ProtocolCode, required, snapshot, now); err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "FORMULA_SYNC_FAILED", "the formula could not be synchronized to billing", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repo.PublishFormula(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
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
