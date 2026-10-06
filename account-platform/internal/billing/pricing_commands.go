package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Namespace locks precede price-row locks in every pricing writer. The scope
// identity is immutable; this initial read only identifies the lock, not a CAS.
func lockManagedPriceSpace(ctx context.Context, tx pgx.Tx, id uuid.UUID, now time.Time) (int64, error) {
	var scope, key, protocol, credit string
	var formula uuid.UUID
	err := tx.QueryRow(ctx, `SELECT scope_type,COALESCE(scope_id,'*'),protocol_code,formula_version_id,credit_unit FROM pricing_versions WHERE id=$1`, id).Scan(&scope, &key, &protocol, &formula, &credit)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	return lockPricingSpace(ctx, tx, scope, key, protocol, formula, credit, now)
}

func lockPricingSpace(ctx context.Context, tx pgx.Tx, scope, key, protocol string, formula uuid.UUID, credit string, now time.Time) (int64, error) {
	if err := lockTransactionKey(ctx, tx, "pricing.space", scope+"|"+key+"|"+protocol+"|"+formula.String()+"|"+credit); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO pricing_space_versions(scope_type,scope_key,protocol_code,formula_version_id,credit_unit,etag,updated_at) VALUES($1,$2,$3,$4,$5,1,$6) ON CONFLICT DO NOTHING`, scope, key, protocol, formula, credit, now); err != nil {
		return 0, err
	}
	var etag int64
	err := tx.QueryRow(ctx, `SELECT etag FROM pricing_space_versions WHERE scope_type=$1 AND scope_key=$2 AND protocol_code=$3 AND formula_version_id=$4 AND credit_unit=$5 FOR UPDATE`, scope, key, protocol, formula, credit).Scan(&etag)
	return etag, err
}

func (r *PostgresRepository) beginPricingCommand(ctx context.Context, actor uuid.UUID, key, kind string, facts any) (pgx.Tx, []byte, string, error) {
	if actor == uuid.Nil || !validIdempotencyKey(key, 128) {
		return nil, nil, "", ErrConflict
	}
	encoded, err := json.Marshal(facts)
	if err != nil {
		return nil, nil, "", err
	}
	sum := sha256.Sum256(encoded)
	fingerprint := hex.EncodeToString(sum[:])
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	fail := func(err error) (pgx.Tx, []byte, string, error) { _ = tx.Rollback(ctx); return nil, nil, "", err }
	if err := lockTransactionKey(ctx, tx, "pricing.command", actor.String()+"|"+key); err != nil {
		return fail(err)
	}
	var previousKind, previousFingerprint string
	var reply []byte
	err = tx.QueryRow(ctx, `SELECT operation,request_fingerprint,response_snapshot FROM pricing_operation_commands WHERE actor_user_id=$1 AND idempotency_key=$2`, actor, key).Scan(&previousKind, &previousFingerprint, &reply)
	if err == nil {
		if previousKind != kind || previousFingerprint != fingerprint {
			return fail(ErrIdempotencyConflict)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, nil, "", err
		}
		return nil, reply, fingerprint, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fail(err)
	}
	return tx, nil, fingerprint, nil
}

func (r *PostgresRepository) completePricingCommand(ctx context.Context, tx pgx.Tx, actor uuid.UUID, key, kind, fingerprint string, result any) error {
	reply, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO pricing_operation_commands(actor_user_id,idempotency_key,operation,request_fingerprint,response_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6)`, actor, key, kind, fingerprint, reply, r.now()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
