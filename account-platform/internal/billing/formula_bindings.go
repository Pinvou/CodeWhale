package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/google/uuid"
)

func (r *PostgresRepository) RegisterFormulaBinding(ctx context.Context, formulaID uuid.UUID, protocol string, required []uuid.UUID, snapshot json.RawMessage, publishedAt time.Time) error {
	protocol = strings.ToUpper(strings.TrimSpace(protocol))
	if formulaID == uuid.Nil || protocol == "" || len(required) == 0 || !json.Valid(snapshot) || publishedAt.IsZero() {
		return ErrConflict
	}
	var known int
	if err := r.pool.QueryRow(ctx, `SELECT count(*) FROM metering_unit_definitions WHERE id=ANY($1)`, required).Scan(&known); err != nil {
		return err
	}
	if known != len(required) {
		return ErrPriceMissing
	}
	command, err := r.pool.Exec(ctx, `INSERT INTO billing_protocol_formula_bindings(formula_version_id,protocol_code,required_unit_definition_ids,formula_snapshot,published_at) VALUES($1,$2,$3,$4,$5) ON CONFLICT(formula_version_id) DO NOTHING`, formulaID, protocol, required, []byte(snapshot), publishedAt)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		var existingProtocol string
		var existingRequired []uuid.UUID
		var existingSnapshot []byte
		if err := r.pool.QueryRow(ctx, `SELECT protocol_code,required_unit_definition_ids,formula_snapshot FROM billing_protocol_formula_bindings WHERE formula_version_id=$1`, formulaID).Scan(&existingProtocol, &existingRequired, &existingSnapshot); err != nil {
			return err
		}
		encodedRequired, _ := json.Marshal(required)
		existingRequiredJSON, _ := json.Marshal(existingRequired)
		if existingProtocol != protocol || string(encodedRequired) != string(existingRequiredJSON) || !jsonEqual(existingSnapshot, snapshot) {
			return ErrConflict
		}
	}
	return nil
}

func jsonEqual(left, right []byte) bool {
	var a, b any
	return json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil && string(mustCanonicalJSON(a)) == string(mustCanonicalJSON(b))
}
func mustCanonicalJSON(value any) []byte { encoded, _ := json.Marshal(value); return encoded }

func (s *Service) putFormulaBinding(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(interface {
		RegisterFormulaBinding(context.Context, uuid.UUID, string, []uuid.UUID, json.RawMessage, time.Time) error
	})
	if !ok {
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
		return
	}
	if err := repository.RegisterFormulaBinding(r.Context(), formulaID, body.ProtocolCode, body.RequiredUnitDefinitionIDs, body.FormulaSnapshot, body.PublishedAt); err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"formula_version_id": formulaID, "status": "REGISTERED"})
}
