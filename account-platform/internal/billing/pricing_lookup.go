package billing

import (
	"context"
	"errors"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"strings"
)

func (r *PostgresRepository) CurrentPrice(ctx context.Context, model string) (PriceVersion, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return PriceVersion{}, ErrConflict
	}
	now := r.now()
	var formulaID uuid.UUID
	err := r.pool.QueryRow(ctx, `SELECT binding.formula_version_id FROM billing_protocol_formula_bindings binding WHERE binding.protocol_code='CHAT' AND EXISTS(SELECT 1 FROM pricing_versions price WHERE price.protocol_code=binding.protocol_code AND price.formula_version_id=binding.formula_version_id AND price.credit_unit='MICROCREDIT' AND price.lifecycle_status IN ('PUBLISHED','ACTIVE') AND price.published_at IS NOT NULL AND price.cancelled_at IS NULL AND price.effective_from<=$2 AND (price.effective_to IS NULL OR price.effective_to>$2) AND (price.scope_type='GLOBAL' OR price.scope_type='MODEL' AND price.scope_id=$1)) ORDER BY binding.published_at DESC,binding.formula_version_id DESC LIMIT 1`, model, now).Scan(&formulaID)
	if errors.Is(err, pgx.ErrNoRows) {
		return PriceVersion{}, ErrPriceMissing
	}
	if err != nil {
		return PriceVersion{}, err
	}
	return r.ResolveCurrentPrice(ctx, model, "CHAT", formulaID, "MICROCREDIT", now)
}

func (r *PostgresRepository) PriceByID(ctx context.Context, priceID uuid.UUID) (PriceVersion, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return PriceVersion{}, err
	}
	defer tx.Rollback(ctx)
	var candidate priceCandidate
	err = tx.QueryRow(ctx, `SELECT id,scope_type,COALESCE(scope_id,''),protocol_code,formula_version_id,credit_unit,effective_from,effective_to FROM pricing_versions WHERE id=$1 AND published_at IS NOT NULL`, priceID).Scan(&candidate.id, &candidate.scopeType, &candidate.model, &candidate.protocol, &candidate.formulaID, &candidate.creditUnit, &candidate.effectiveFrom, &candidate.effectiveTo)
	if errors.Is(err, pgx.ErrNoRows) {
		return PriceVersion{}, ErrNotFound
	}
	if err != nil {
		return PriceVersion{}, err
	}
	model := candidate.model
	if candidate.scopeType == "GLOBAL" {
		model = "*"
	}
	price, err := resolvePriceCandidate(ctx, tx, candidate, model, candidate.effectiveFrom)
	if err != nil {
		return PriceVersion{}, err
	}
	return price, tx.Commit(ctx)
}
