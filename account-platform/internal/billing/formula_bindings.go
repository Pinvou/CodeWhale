package billing

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
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type MeteringUnitVersionReference struct {
	UnitCode         string    `json:"unit_code"`
	UnitDefinitionID uuid.UUID `json:"unit_definition_id"`
	Version          int64     `json:"version"`
	Status           string    `json:"status"`
}

type FormulaDraftValidation struct {
	VerifiedCount      int                            `json:"verified_count"`
	UnitVersions       []MeteringUnitVersionReference `json:"unit_versions"`
	ObservedAt         time.Time                      `json:"observed_at"`
	UnitSetFingerprint string                         `json:"unit_set_fingerprint"`
}

type FormulaBindingReceipt struct {
	FormulaVersionID                  uuid.UUID   `json:"formula_version_id"`
	Status                            string      `json:"status"`
	ProtocolCode                      string      `json:"protocol_code"`
	RequiredMeteringUnitDefinitionIDs []uuid.UUID `json:"required_metering_unit_definition_ids"`
	FormulaSnapshotSHA256             string      `json:"formula_snapshot_sha256"`
	PublishedAt                       time.Time   `json:"published_at"`
}

type formulaDraftUnitFact struct {
	MeteringUnitVersionReference
	MinimumPrecision int64      `json:"minimum_precision"`
	PriceDenominator int64      `json:"price_denominator"`
	RoundingRule     string     `json:"rounding_rule"`
	DeactivatedAt    *time.Time `json:"deactivated_at"`
}

func (r *PostgresRepository) ValidateFormulaDraftUnits(ctx context.Context, required map[string]uuid.UUID) (FormulaDraftValidation, error) {
	if len(required) == 0 || len(required) > 32 {
		return FormulaDraftValidation{}, ErrPriceMissing
	}
	ids := make([]uuid.UUID, 0, len(required))
	expected := make(map[uuid.UUID]string, len(required))
	for rawCode, id := range required {
		code := strings.ToUpper(strings.TrimSpace(rawCode))
		if code == "" || id == uuid.Nil || expected[id] != "" {
			return FormulaDraftValidation{}, ErrPriceMissing
		}
		expected[id] = code
		ids = append(ids, id)
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return FormulaDraftValidation{}, err
	}
	defer tx.Rollback(ctx)
	result := FormulaDraftValidation{UnitVersions: make([]MeteringUnitVersionReference, 0, len(required))}
	facts := make([]formulaDraftUnitFact, 0, len(required))
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&result.ObservedAt); err != nil {
		return FormulaDraftValidation{}, err
	}
	rows, err := tx.Query(ctx, `SELECT id,unit_code,version,status,minimum_precision,price_denominator,rounding_rule,deactivated_at FROM metering_unit_definitions WHERE id=ANY($1) AND status='ACTIVE'`, ids)
	if err != nil {
		return FormulaDraftValidation{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var fact formulaDraftUnitFact
		if err := rows.Scan(&fact.UnitDefinitionID, &fact.UnitCode, &fact.Version, &fact.Status, &fact.MinimumPrecision, &fact.PriceDenominator, &fact.RoundingRule, &fact.DeactivatedAt); err != nil {
			return FormulaDraftValidation{}, err
		}
		fact.UnitCode = strings.ToUpper(strings.TrimSpace(fact.UnitCode))
		if expected[fact.UnitDefinitionID] != fact.UnitCode {
			return FormulaDraftValidation{}, ErrPriceMissing
		}
		result.UnitVersions = append(result.UnitVersions, fact.MeteringUnitVersionReference)
		facts = append(facts, fact)
	}
	if err := rows.Err(); err != nil {
		return FormulaDraftValidation{}, err
	}
	if len(result.UnitVersions) != len(required) {
		return FormulaDraftValidation{}, ErrPriceMissing
	}
	sort.Slice(result.UnitVersions, func(i, j int) bool { return result.UnitVersions[i].UnitCode < result.UnitVersions[j].UnitCode })
	sort.Slice(facts, func(i, j int) bool {
		if facts[i].UnitCode != facts[j].UnitCode {
			return facts[i].UnitCode < facts[j].UnitCode
		}
		return facts[i].UnitDefinitionID.String() < facts[j].UnitDefinitionID.String()
	})
	result.VerifiedCount = len(result.UnitVersions)
	factJSON, err := json.Marshal(facts)
	if err != nil {
		return FormulaDraftValidation{}, err
	}
	digest := sha256.Sum256(factJSON)
	result.UnitSetFingerprint = hex.EncodeToString(digest[:])
	if err := tx.Commit(ctx); err != nil {
		return FormulaDraftValidation{}, err
	}
	return result, nil
}

func (r *PostgresRepository) RegisterFormulaBinding(ctx context.Context, formulaID uuid.UUID, protocol string, required []uuid.UUID, snapshot json.RawMessage) (FormulaBindingReceipt, error) {
	protocol = strings.ToUpper(strings.TrimSpace(protocol))
	required = normalizedUUIDs(required)
	snapshotDigest, snapshotOK := canonicalJSONDigest(snapshot)
	if formulaID == uuid.Nil || protocol == "" || len(required) == 0 || !snapshotOK {
		return FormulaBindingReceipt{}, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return FormulaBindingReceipt{}, err
	}
	defer tx.Rollback(ctx)
	if existing, found, err := formulaBindingReceipt(ctx, tx, formulaID); err != nil {
		return FormulaBindingReceipt{}, err
	} else if found {
		if existing.ProtocolCode != protocol || existing.FormulaSnapshotSHA256 != snapshotDigest || !uuidListsEqual(existing.RequiredMeteringUnitDefinitionIDs, required) {
			return FormulaBindingReceipt{}, ErrConflict
		}
		return existing, tx.Commit(ctx)
	}
	var known int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM (SELECT id FROM metering_unit_definitions WHERE id=ANY($1) AND status='ACTIVE' ORDER BY id FOR SHARE) active_units`, required).Scan(&known); err != nil {
		return FormulaBindingReceipt{}, err
	}
	if known != len(required) {
		return FormulaBindingReceipt{}, ErrPriceMissing
	}
	command, err := tx.Exec(ctx, `INSERT INTO billing_protocol_formula_bindings(formula_version_id,protocol_code,required_unit_definition_ids,formula_snapshot,published_at) VALUES($1,$2,$3,$4,transaction_timestamp()) ON CONFLICT(formula_version_id) DO NOTHING`, formulaID, protocol, required, []byte(snapshot))
	if err != nil {
		return FormulaBindingReceipt{}, err
	}
	receipt, found, err := formulaBindingReceipt(ctx, tx, formulaID)
	if err != nil || !found {
		return FormulaBindingReceipt{}, err
	}
	if command.RowsAffected() == 0 && (receipt.ProtocolCode != protocol || receipt.FormulaSnapshotSHA256 != snapshotDigest || !uuidListsEqual(receipt.RequiredMeteringUnitDefinitionIDs, required)) {
		return FormulaBindingReceipt{}, ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return FormulaBindingReceipt{}, err
	}
	return receipt, nil
}

type formulaBindingQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func formulaBindingReceipt(ctx context.Context, query formulaBindingQuerier, formulaID uuid.UUID) (FormulaBindingReceipt, bool, error) {
	var receipt FormulaBindingReceipt
	var snapshot []byte
	err := query.QueryRow(ctx, `SELECT formula_version_id,protocol_code,required_unit_definition_ids,formula_snapshot,published_at FROM billing_protocol_formula_bindings WHERE formula_version_id=$1`, formulaID).Scan(&receipt.FormulaVersionID, &receipt.ProtocolCode, &receipt.RequiredMeteringUnitDefinitionIDs, &snapshot, &receipt.PublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return FormulaBindingReceipt{}, false, nil
	}
	if err != nil {
		return FormulaBindingReceipt{}, false, err
	}
	receipt.Status = "REGISTERED"
	receipt.RequiredMeteringUnitDefinitionIDs = normalizedUUIDs(receipt.RequiredMeteringUnitDefinitionIDs)
	receipt.FormulaSnapshotSHA256, _ = canonicalJSONDigest(snapshot)
	return receipt, true, nil
}

func normalizedUUIDs(values []uuid.UUID) []uuid.UUID {
	result := append([]uuid.UUID(nil), values...)
	sort.Slice(result, func(i, j int) bool { return result[i].String() < result[j].String() })
	return result
}

func uuidListsEqual(left, right []uuid.UUID) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func canonicalJSONDigest(raw []byte) (string, bool) {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return "", false
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return "", false
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), true
}

func jsonEqual(left, right []byte) bool {
	var a, b any
	return json.Unmarshal(left, &a) == nil && json.Unmarshal(right, &b) == nil && string(mustCanonicalJSON(a)) == string(mustCanonicalJSON(b))
}
func mustCanonicalJSON(value any) []byte { encoded, _ := json.Marshal(value); return encoded }

func (s *Service) putFormulaBinding(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(interface {
		RegisterFormulaBinding(context.Context, uuid.UUID, string, []uuid.UUID, json.RawMessage) (FormulaBindingReceipt, error)
	})
	if !ok {
		return
	}
	formulaID, err := uuid.Parse(r.PathValue("formula_version_id"))
	var body struct {
		ProtocolCode              string          `json:"protocol_code"`
		RequiredUnitDefinitionIDs []uuid.UUID     `json:"required_unit_definition_ids"`
		FormulaSnapshot           json.RawMessage `json:"formula_snapshot"`
	}
	if err != nil || api.DecodeJSON(w, r, &body) != nil {
		return
	}
	receipt, err := repository.RegisterFormulaBinding(r.Context(), formulaID, body.ProtocolCode, body.RequiredUnitDefinitionIDs, body.FormulaSnapshot)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, receipt)
}

func (s *Service) validateFormulaDraftUnits(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(interface {
		ValidateFormulaDraftUnits(context.Context, map[string]uuid.UUID) (FormulaDraftValidation, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "FORMULA_UNIT_VALIDATION_UNAVAILABLE", "metering unit validation is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var body struct {
		RequiredUnitVersions map[string]uuid.UUID `json:"required_unit_versions"`
	}
	if api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid formula unit validation request", httpserver.RequestID(r.Context()))
		return
	}
	result, err := repository.ValidateFormulaDraftUnits(r.Context(), body.RequiredUnitVersions)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, result)
}
