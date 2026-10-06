package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (r *PostgresRepository) RegisterFormulaBinding(ctx context.Context, formulaID uuid.UUID, protocol string, required []uuid.UUID, snapshot json.RawMessage, publishedAt time.Time) error {
	protocol = strings.ToUpper(strings.TrimSpace(protocol))
	if formulaID == uuid.Nil || protocol == "" || len(required) == 0 || !json.Valid(snapshot) || publishedAt.IsZero() {
		return ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "formula.binding", formulaID.String()); err != nil {
		return err
	}
	var existingProtocol string
	var existingRequired []uuid.UUID
	var sameSnapshot bool
	// JSONB compares exact decimal values and object facts. Decoding to float64
	// would collapse distinct large integers; lexical JSON comparison would
	// incorrectly reject equivalent spellings such as 1e3 and 1000.
	err = tx.QueryRow(ctx, `SELECT protocol_code,required_unit_definition_ids,formula_snapshot=$2::jsonb FROM billing_protocol_formula_bindings WHERE formula_version_id=$1`, formulaID, []byte(snapshot)).Scan(&existingProtocol, &existingRequired, &sameSnapshot)
	if err == nil {
		encodedRequired, _ := json.Marshal(required)
		existingRequiredJSON, _ := json.Marshal(existingRequired)
		if existingProtocol != protocol || string(encodedRequired) != string(existingRequiredJSON) || !sameSnapshot {
			return ErrConflict
		}
		// Replaying an immutable published binding must not reinterpret later
		// metering-unit lifecycle changes as a different publication.
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var formula struct {
		EvaluatorVersion     string               `json:"evaluator_version"`
		Expression           map[string]string    `json:"expression"`
		RequiredUnitVersions map[string]uuid.UUID `json:"required_unit_versions"`
	}
	if json.Unmarshal(snapshot, &formula) != nil || formula.EvaluatorVersion != "SAFE_PARAMETER_MAP_V1" || len(formula.Expression) != len(required) || len(formula.RequiredUnitVersions) != len(required) {
		return ErrConflict
	}
	seenIDs := make(map[uuid.UUID]bool, len(required))
	for _, id := range required {
		if id == uuid.Nil || seenIDs[id] {
			return ErrConflict
		}
		seenIDs[id] = true
	}
	rows, err := tx.Query(ctx, `SELECT id,unit_code FROM metering_unit_definitions WHERE id=ANY($1) FOR SHARE`, required)
	if err != nil {
		return err
	}
	seenCodes := make(map[string]bool, len(required))
	for rows.Next() {
		var id uuid.UUID
		var code string
		if err := rows.Scan(&id, &code); err != nil {
			rows.Close()
			return err
		}
		if seenCodes[code] || strings.TrimSpace(formula.Expression[code]) == "" || formula.RequiredUnitVersions[code] != id {
			rows.Close()
			return ErrConflict
		}
		seenCodes[code] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	if len(seenCodes) != len(required) {
		return ErrPriceMissing
	}
	if _, err := tx.Exec(ctx, `INSERT INTO billing_protocol_formula_bindings(formula_version_id,protocol_code,required_unit_definition_ids,formula_snapshot,published_at) VALUES($1,$2,$3,$4,$5)`, formulaID, protocol, required, []byte(snapshot), publishedAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) putFormulaBinding(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(interface {
		RegisterFormulaBinding(context.Context, uuid.UUID, string, []uuid.UUID, json.RawMessage, time.Time) error
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "PRICING_UNAVAILABLE", "formula binding is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	formulaID, err := uuid.Parse(r.PathValue("formula_version_id"))
	var body struct {
		ProtocolCode              string          `json:"protocol_code"`
		RequiredUnitDefinitionIDs []uuid.UUID     `json:"required_unit_definition_ids"`
		FormulaSnapshot           json.RawMessage `json:"formula_snapshot"`
		PublishedAt               time.Time       `json:"published_at"`
	}
	if err != nil || api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid formula binding", httpserver.RequestID(r.Context()))
		return
	}
	if err := repository.RegisterFormulaBinding(r.Context(), formulaID, body.ProtocolCode, body.RequiredUnitDefinitionIDs, body.FormulaSnapshot, body.PublishedAt); err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"formula_version_id": formulaID, "status": "REGISTERED"})
}
