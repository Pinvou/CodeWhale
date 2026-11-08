package billing

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ids are the unique, bounded page keys selected by the caller in this same
// RepeatableRead transaction. Preserve their order and every unit fact; this
// projection is separate from the single-row writer's FOR UPDATE path.
func loadManagedPriceBatch(ctx context.Context, tx pgx.Tx, ids []uuid.UUID) ([]ManagedPriceVersion, error) {
	items := make([]ManagedPriceVersion, len(ids))
	if len(ids) == 0 {
		return items, nil
	}
	positions := make(map[uuid.UUID]int, len(ids))
	for index, id := range ids {
		positions[id] = index
	}
	seen := make([]bool, len(ids))
	missingSpace := make([]bool, len(ids))
	rows, err := tx.Query(ctx, `SELECT p.id,p.scope_type,p.scope_id,p.protocol_code,p.formula_version_id,p.credit_unit,p.lifecycle_status,p.version,p.space_etag,p.effective_from,p.effective_to,p.published_at,s.etag
FROM pricing_versions p
LEFT JOIN pricing_space_versions s ON s.scope_type=p.scope_type AND s.scope_key=COALESCE(p.scope_id,'*') AND s.protocol_code=p.protocol_code AND s.formula_version_id=p.formula_version_id AND s.credit_unit=p.credit_unit
WHERE p.id=ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var item ManagedPriceVersion
		var current *int64
		if err := rows.Scan(&item.PriceVersionID, &item.ScopeType, &item.ScopeID, &item.ProtocolCode, &item.FormulaVersionID, &item.CreditUnit, &item.LifecycleStatus, &item.Version, &item.SpaceETag, &item.EffectiveFrom, &item.EffectiveTo, &item.PublishedAt, &current); err != nil {
			return nil, err
		}
		index, exists := positions[item.PriceVersionID]
		if !exists || seen[index] {
			return nil, errors.New("managed price batch returned an unexpected identity")
		}
		seen[index], missingSpace[index] = true, current == nil
		if current != nil {
			item.CurrentSpaceETag = *current
		}
		items[index] = item
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, exists := range seen {
		if !exists {
			return nil, ErrNotFound
		}
	}
	units, err := tx.Query(ctx, `SELECT price_version_id,unit_definition_id,config_type,fixed_price_per_denominator FROM pricing_version_unit_configs WHERE price_version_id=ANY($1::uuid[]) ORDER BY price_version_id,unit_definition_id`, ids)
	if err != nil {
		return nil, err
	}
	defer units.Close()
	for units.Next() {
		var priceID uuid.UUID
		var unit PriceUnitConfiguration
		if err := units.Scan(&priceID, &unit.UnitDefinitionID, &unit.ConfigType, &unit.FixedPricePerDenominator); err != nil {
			return nil, err
		}
		index, exists := positions[priceID]
		if !exists {
			return nil, errors.New("managed price batch returned an unexpected unit owner")
		}
		items[index].Units = append(items[index].Units, unit)
	}
	if err := units.Err(); err != nil {
		return nil, err
	}
	// A missing namespace was an error in the original global list, not a
	// default ETag or permission to omit a price. Do not hide it with an inner
	// join or COALESCE. The space-specific caller already read its namespace.
	for _, missing := range missingSpace {
		if missing {
			return nil, pgx.ErrNoRows
		}
	}
	return items, nil
}
