package billing

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var chatFormulaVersionID = uuid.MustParse("00000000-0000-7000-8200-000000000001")

type priceCandidate struct {
	id            uuid.UUID
	scopeType     string
	model         string
	protocol      string
	formulaID     uuid.UUID
	creditUnit    string
	effectiveFrom time.Time
	effectiveTo   *time.Time
}

type unitConfiguration struct {
	unitDefinitionID uuid.UUID
	configType       string
	fixedPrice       *int64
}

func (r *PostgresRepository) ResolveCurrentPrice(ctx context.Context, model, protocol string, formulaVersionID uuid.UUID, creditUnit string, at time.Time) (PriceVersion, error) {
	model, protocol, creditUnit = strings.TrimSpace(model), strings.ToUpper(strings.TrimSpace(protocol)), strings.ToUpper(strings.TrimSpace(creditUnit))
	if model == "" || protocol == "" || formulaVersionID == uuid.Nil || creditUnit == "" {
		return PriceVersion{}, ErrConflict
	}
	if at.IsZero() {
		at = r.now()
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return PriceVersion{}, err
	}
	defer tx.Rollback(ctx)
	var blocked bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pricing_blocks WHERE model_id=$1 AND protocol_code=$2 AND formula_version_id=$3 AND credit_unit=$4 AND effective_from<=$5 AND (effective_to IS NULL OR effective_to>$5))`, model, protocol, formulaVersionID, creditUnit, at).Scan(&blocked); err != nil {
		return PriceVersion{}, err
	}
	if blocked {
		return PriceVersion{}, ErrPriceUnavailable
	}
	selected, found, err := selectUniquePriceCandidate(ctx, tx, "MODEL", model, protocol, formulaVersionID, creditUnit, at)
	if err != nil {
		return PriceVersion{}, err
	}
	if !found {
		selected, found, err = selectUniquePriceCandidate(ctx, tx, "GLOBAL", "", protocol, formulaVersionID, creditUnit, at)
		if err != nil {
			return PriceVersion{}, err
		}
	}
	if !found {
		return PriceVersion{}, ErrPriceMissing
	}
	price, err := resolvePriceCandidate(ctx, tx, selected, model, at)
	if err != nil {
		return PriceVersion{}, err
	}
	return price, tx.Commit(ctx)
}

func selectUniquePriceCandidate(ctx context.Context, tx pgx.Tx, scopeType, scopeID, protocol string, formulaVersionID uuid.UUID, creditUnit string, at time.Time) (priceCandidate, bool, error) {
	rows, err := tx.Query(ctx, `SELECT id,scope_type,COALESCE(scope_id,''),protocol_code,formula_version_id,credit_unit,effective_from,effective_to
FROM pricing_versions
WHERE scope_type=$1 AND (($1='GLOBAL' AND scope_id IS NULL) OR scope_id=$2) AND protocol_code=$3 AND formula_version_id=$4 AND credit_unit=$5
  AND lifecycle_status IN ('PUBLISHED','ACTIVE') AND published_at IS NOT NULL AND cancelled_at IS NULL
  AND effective_from<=$6 AND (effective_to IS NULL OR effective_to>$6)
ORDER BY id LIMIT 2`, scopeType, scopeID, protocol, formulaVersionID, creditUnit, at)
	if err != nil {
		return priceCandidate{}, false, err
	}
	defer rows.Close()
	items := make([]priceCandidate, 0, 2)
	for rows.Next() {
		var item priceCandidate
		if err := rows.Scan(&item.id, &item.scopeType, &item.model, &item.protocol, &item.formulaID, &item.creditUnit, &item.effectiveFrom, &item.effectiveTo); err != nil {
			return priceCandidate{}, false, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return priceCandidate{}, false, err
	}
	if len(items) > 1 {
		return priceCandidate{}, false, ErrPriceAmbiguous
	}
	if len(items) == 0 {
		return priceCandidate{}, false, nil
	}
	return items[0], true, nil
}

func resolvePriceCandidate(ctx context.Context, tx pgx.Tx, selected priceCandidate, requestedModel string, at time.Time) (PriceVersion, error) {
	var required []uuid.UUID
	var formulaSnapshot []byte
	if err := tx.QueryRow(ctx, `SELECT required_unit_definition_ids,formula_snapshot FROM billing_protocol_formula_bindings WHERE formula_version_id=$1 AND protocol_code=$2`, selected.formulaID, selected.protocol).Scan(&required, &formulaSnapshot); errors.Is(err, pgx.ErrNoRows) {
		return PriceVersion{}, ErrPriceMissing
	} else if err != nil {
		return PriceVersion{}, err
	}
	configs, err := loadUnitConfigurations(ctx, tx, selected.id)
	if err != nil {
		return PriceVersion{}, err
	}
	if len(configs) != len(required) {
		return PriceVersion{}, ErrPriceMissing
	}
	var global priceCandidate
	globalLoaded := false
	result := PriceVersion{ID: selected.id, Model: requestedModel, ScopeType: selected.scopeType, ProtocolCode: selected.protocol, FormulaVersionID: selected.formulaID, FormulaSnapshot: formulaSnapshot, CreditUnit: selected.creditUnit, EffectiveFrom: selected.effectiveFrom, EffectiveTo: selected.effectiveTo, ResolvedAt: at, Units: make([]ResolvedUnitPrice, 0, len(required))}
	for _, unitID := range required {
		configuration, ok := configs[unitID]
		if !ok {
			return PriceVersion{}, ErrPriceMissing
		}
		source, price := selected, configuration.fixedPrice
		if configuration.configType == "INHERIT_GLOBAL" {
			if selected.scopeType != "MODEL" {
				return PriceVersion{}, ErrPriceMissing
			}
			if !globalLoaded {
				var found bool
				global, found, err = selectUniquePriceCandidate(ctx, tx, "GLOBAL", "", selected.protocol, selected.formulaID, selected.creditUnit, at)
				if err != nil {
					return PriceVersion{}, err
				}
				if !found {
					return PriceVersion{}, ErrPriceMissing
				}
				globalLoaded = true
			}
			globalConfigs, err := loadUnitConfigurations(ctx, tx, global.id)
			if err != nil {
				return PriceVersion{}, err
			}
			globalConfiguration, ok := globalConfigs[unitID]
			if !ok || globalConfiguration.configType != "FIXED_PRICE" || globalConfiguration.fixedPrice == nil {
				return PriceVersion{}, ErrPriceMissing
			}
			source, price = global, globalConfiguration.fixedPrice
		}
		if price == nil {
			return PriceVersion{}, ErrPriceMissing
		}
		var unit ResolvedUnitPrice
		if err := tx.QueryRow(ctx, `SELECT unit_code,version,minimum_precision,price_denominator,rounding_rule FROM metering_unit_definitions WHERE id=$1`, unitID).Scan(&unit.UnitCode, &unit.UnitVersion, &unit.MinimumPrecision, &unit.PriceDenominator, &unit.RoundingRule); err != nil {
			return PriceVersion{}, ErrPriceMissing
		}
		unit.UnitDefinitionID, unit.ResolvedPrice, unit.ConfigurationType, unit.PriceSourceScope, unit.SourcePriceVersionID = unitID, *price, configuration.configType, source.scopeType, source.id
		result.Units = append(result.Units, unit)
		switch unit.UnitCode {
		case "INPUT_TOKEN":
			result.InputPerMillion = scaleLegacyMillion(unit.ResolvedPrice, unit.PriceDenominator)
		case "CACHED_INPUT_TOKEN":
			result.CachedInputPerMillion = scaleLegacyMillion(unit.ResolvedPrice, unit.PriceDenominator)
		case "OUTPUT_TOKEN":
			result.OutputPerMillion = scaleLegacyMillion(unit.ResolvedPrice, unit.PriceDenominator)
		}
	}
	return result, nil
}

func loadUnitConfigurations(ctx context.Context, tx pgx.Tx, priceVersionID uuid.UUID) (map[uuid.UUID]unitConfiguration, error) {
	rows, err := tx.Query(ctx, `SELECT unit_definition_id,config_type,fixed_price_per_denominator FROM pricing_version_unit_configs WHERE price_version_id=$1`, priceVersionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[uuid.UUID]unitConfiguration)
	for rows.Next() {
		var item unitConfiguration
		if err := rows.Scan(&item.unitDefinitionID, &item.configType, &item.fixedPrice); err != nil {
			return nil, err
		}
		result[item.unitDefinitionID] = item
	}
	return result, rows.Err()
}

func scaleLegacyMillion(price, denominator int64) int64 {
	if denominator == 1_000_000 {
		return price
	}
	return 0
}
