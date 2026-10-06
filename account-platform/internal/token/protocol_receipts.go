package token

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrGovernanceConflict = errors.New("token governance command conflicts with immutable facts")

// Governance commands bind a caller's logical operation to immutable input and
// its original reply. The transaction also contains the corresponding mutation.
func (r *PostgresRepository) beginGovernanceCommand(ctx context.Context, actor, logicalID uuid.UUID, kind string, input any) (pgx.Tx, []byte, error) {
	if actor == uuid.Nil || logicalID == uuid.Nil {
		return nil, nil, ErrForbidden
	}
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, nil, err
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, nil, err
	}
	ok := false
	defer func() {
		if !ok {
			_ = tx.Rollback(ctx)
		}
	}()
	if err := lockTokenKey(ctx, tx, "governance", actor.String()+"|"+kind+"|"+logicalID.String()); err != nil {
		return nil, nil, err
	}
	var matches bool
	var reply []byte
	err = tx.QueryRow(ctx, `SELECT request_snapshot=$4::jsonb,response_snapshot FROM token_governance_commands WHERE actor_id=$1 AND operation_kind=$2 AND logical_id=$3 FOR UPDATE`, actor, kind, logicalID, payload).Scan(&matches, &reply)
	if errors.Is(err, pgx.ErrNoRows) {
		_, err = tx.Exec(ctx, `INSERT INTO token_governance_commands(actor_id,operation_kind,logical_id,request_snapshot,started_at) VALUES($1,$2,$3,$4,$5)`, actor, kind, logicalID, payload, r.now())
	} else if err == nil && !matches {
		return nil, nil, ErrGovernanceConflict
	}
	if err != nil {
		return nil, nil, err
	}
	ok = true
	return tx, reply, nil
}

func finishGovernanceCommand(ctx context.Context, tx pgx.Tx, actor, logicalID uuid.UUID, kind string, result any) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE token_governance_commands SET response_snapshot=$4 WHERE actor_id=$1 AND operation_kind=$2 AND logical_id=$3 AND response_snapshot IS NULL`, actor, kind, logicalID, encoded)
	if err != nil {
		return err
	}
	if command.RowsAffected() != 1 {
		return ErrGovernanceConflict
	}
	var resourceID uuid.UUID
	var resourceType string
	var version int64
	switch item := result.(type) {
	case ProtocolDefinition:
		resourceID, resourceType, version = item.ID, "PROTOCOL", item.Version
	case ProtocolFormulaVersion:
		resourceID, resourceType, version = item.ID, "FORMULA", item.Version
	default:
		return ErrForbidden
	}
	if err := recordTokenAudit(ctx, tx, actor, kind, resourceType, resourceID, map[string]any{"logical_id": logicalID, "version": version}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type FormulaPublication struct {
	Formula   ProtocolFormulaVersion
	StartedAt time.Time
	Completed bool
}

// The intent's time and formula facts are committed before the first Billing
// call. A lost acknowledgement reuses those exact facts after a process restart.
func (r *PostgresRepository) PrepareFormulaPublication(ctx context.Context, actor, logicalID, id uuid.UUID) (FormulaPublication, error) {
	tx, reply, err := r.beginGovernanceCommand(ctx, actor, logicalID, "FORMULA_PUBLISH", map[string]any{"formula_version_id": id})
	if err != nil {
		return FormulaPublication{}, err
	}
	defer tx.Rollback(ctx)
	if len(reply) != 0 {
		var item ProtocolFormulaVersion
		if err := json.Unmarshal(reply, &item); err != nil {
			return FormulaPublication{}, err
		}
		return FormulaPublication{Formula: item, Completed: true}, tx.Commit(ctx)
	}
	item, err := readFormula(ctx, tx, id, true)
	if err != nil {
		return FormulaPublication{}, err
	}
	if item.Status == "PUBLISHED" {
		err = finishGovernanceCommand(ctx, tx, actor, logicalID, "FORMULA_PUBLISH", item)
		return FormulaPublication{Formula: item, Completed: true}, err
	}
	if item.Status != "DRAFT" {
		return FormulaPublication{}, ErrGovernanceConflict
	}
	var parentStatus string
	if err := tx.QueryRow(ctx, `SELECT status FROM protocol_definitions WHERE id=$1 FOR SHARE`, item.ProtocolDefinitionID).Scan(&parentStatus); err != nil {
		return FormulaPublication{}, err
	}
	if parentStatus != "PUBLISHED" {
		return FormulaPublication{}, ErrGovernanceConflict
	}
	if err := validateSafeFormula(item.RequiredUnitVersions, item.Expression); err != nil {
		return FormulaPublication{}, err
	}
	encoded, err := json.Marshal(item)
	if err != nil {
		return FormulaPublication{}, err
	}
	var snapshot []byte
	var startedAt time.Time
	err = tx.QueryRow(ctx, `SELECT formula_snapshot,started_at FROM token_formula_publication_intents WHERE formula_version_id=$1`, id).Scan(&snapshot, &startedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		startedAt, snapshot = r.now(), encoded
		_, err = tx.Exec(ctx, `INSERT INTO token_formula_publication_intents(formula_version_id,formula_snapshot,started_at) VALUES($1,$2,$3)`, id, encoded, startedAt)
	} else if err == nil {
		var matches bool
		err = tx.QueryRow(ctx, `SELECT formula_snapshot=$2::jsonb FROM token_formula_publication_intents WHERE formula_version_id=$1`, id, encoded).Scan(&matches)
		if err == nil && !matches {
			return FormulaPublication{}, ErrGovernanceConflict
		}
	}
	if err != nil {
		return FormulaPublication{}, err
	}
	if err := json.Unmarshal(snapshot, &item); err != nil {
		return FormulaPublication{}, err
	}
	return FormulaPublication{Formula: item, StartedAt: startedAt}, tx.Commit(ctx)
}

func (r *PostgresRepository) CompleteFormulaPublication(ctx context.Context, actor, logicalID, id uuid.UUID) (ProtocolFormulaVersion, error) {
	tx, reply, err := r.beginGovernanceCommand(ctx, actor, logicalID, "FORMULA_PUBLISH", map[string]any{"formula_version_id": id})
	if err != nil {
		return ProtocolFormulaVersion{}, err
	}
	defer tx.Rollback(ctx)
	if len(reply) != 0 {
		var item ProtocolFormulaVersion
		if err := json.Unmarshal(reply, &item); err != nil {
			return item, err
		}
		return item, tx.Commit(ctx)
	}
	item, err := readFormula(ctx, tx, id, true)
	if err != nil {
		return item, err
	}
	if item.Status == "DRAFT" {
		var snapshot []byte
		var startedAt time.Time
		if err := tx.QueryRow(ctx, `SELECT formula_snapshot,started_at FROM token_formula_publication_intents WHERE formula_version_id=$1`, id).Scan(&snapshot, &startedAt); err != nil {
			return item, err
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return item, err
		}
		var matches bool
		if err := tx.QueryRow(ctx, `SELECT formula_snapshot=$2::jsonb FROM token_formula_publication_intents WHERE formula_version_id=$1`, id, encoded).Scan(&matches); err != nil {
			return item, err
		}
		if !matches {
			return item, ErrGovernanceConflict
		}
		command, err := tx.Exec(ctx, `UPDATE protocol_formula_versions f SET status='PUBLISHED',published_at=$2 FROM protocol_definitions p WHERE f.id=$1 AND f.status='DRAFT' AND p.id=f.protocol_definition_id AND p.status='PUBLISHED'`, id, startedAt)
		if err != nil {
			return item, err
		}
		if command.RowsAffected() != 1 {
			return item, ErrGovernanceConflict
		}
		item.Status, item.PublishedAt = "PUBLISHED", &startedAt
	} else if item.Status != "PUBLISHED" {
		return item, ErrGovernanceConflict
	}
	return item, finishGovernanceCommand(ctx, tx, actor, logicalID, "FORMULA_PUBLISH", item)
}

func readFormula(ctx context.Context, tx pgx.Tx, id uuid.UUID, lock bool) (ProtocolFormulaVersion, error) {
	query := `SELECT f.id,f.protocol_definition_id,p.protocol_code,f.version,f.status,f.required_unit_versions,f.expression,f.evaluator_version,f.created_at,f.published_at FROM protocol_formula_versions f JOIN protocol_definitions p ON p.id=f.protocol_definition_id WHERE f.id=$1`
	if lock {
		query += ` FOR UPDATE OF f`
	}
	var item ProtocolFormulaVersion
	var required, expression []byte
	err := tx.QueryRow(ctx, query, id).Scan(&item.ID, &item.ProtocolDefinitionID, &item.ProtocolCode, &item.Version, &item.Status, &required, &expression, &item.EvaluatorVersion, &item.CreatedAt, &item.PublishedAt)
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
