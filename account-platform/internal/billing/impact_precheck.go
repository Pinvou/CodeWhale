package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"hash"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const maximumImpactPriceModels = 5000

type ImpactPriceQuery struct {
	ModelID      uuid.UUID `json:"model_id"`
	ProtocolCode string    `json:"protocol_code"`
	CreditUnit   string    `json:"credit_unit"`
}

type ImpactPriceCoverage struct {
	ModelID          uuid.UUID  `json:"model_id"`
	ProtocolCode     string     `json:"protocol_code"`
	FormulaVersionID *uuid.UUID `json:"formula_version_id,omitempty"`
	CreditUnit       string     `json:"credit_unit"`
	Covered          bool       `json:"covered"`
	PriceVersionID   *uuid.UUID `json:"price_version_id,omitempty"`
	PriceVersion     *int64     `json:"price_version,omitempty"`
	EffectiveFrom    *time.Time `json:"effective_from,omitempty"`
	EffectiveTo      *time.Time `json:"effective_to,omitempty"`
}

type impactPriceRepository interface {
	BatchCurrentPriceCoverage(context.Context, []ImpactPriceQuery) ([]ImpactPriceCoverage, time.Time, int64, string, error)
}

func (s *Service) impactPriceCoverage(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Items []ImpactPriceQuery `json:"items"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil {
		return
	}
	if len(body.Items) == 0 || len(body.Items) > maximumImpactPriceModels {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "items must contain between 1 and 5000 unique values", httpserver.RequestID(r.Context()))
		return
	}
	seen := make(map[string]struct{}, len(body.Items))
	items := make([]ImpactPriceQuery, 0, len(body.Items))
	for _, item := range body.Items {
		item.ProtocolCode = strings.ToUpper(strings.TrimSpace(item.ProtocolCode))
		item.CreditUnit = strings.ToUpper(strings.TrimSpace(item.CreditUnit))
		key := impactPriceQueryKey(item)
		if item.ModelID == uuid.Nil || item.ProtocolCode == "" || item.CreditUnit == "" {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "items contains an invalid value", httpserver.RequestID(r.Context()))
			return
		}
		if _, exists := seen[key]; exists {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "items must be unique", httpserver.RequestID(r.Context()))
			return
		}
		seen[key] = struct{}{}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].ModelID != items[j].ModelID {
			return items[i].ModelID.String() < items[j].ModelID.String()
		}
		if items[i].ProtocolCode != items[j].ProtocolCode {
			return items[i].ProtocolCode < items[j].ProtocolCode
		}
		return items[i].CreditUnit < items[j].CreditUnit
	})
	repository := s.impactPricing
	if repository == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "PRICING_UNAVAILABLE", "pricing coverage is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	coverage, asOf, revision, fingerprint, err := repository.BatchCurrentPriceCoverage(r.Context(), items)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, ErrPriceAmbiguous) || errors.Is(err, ErrPriceUnavailable) {
			api.WriteError(w, http.StatusServiceUnavailable, "PRICING_UNAVAILABLE", "pricing coverage is unavailable", httpserver.RequestID(r.Context()))
			return
		}
		api.WriteError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "the pricing coverage request could not be completed", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": coverage, "as_of": asOf.UTC(), "source": "BILLING", "financial_fact_revision": revision, "price_set_fingerprint": fingerprint})
}

type impactPriceCandidate struct {
	query         ImpactPriceQuery
	id            uuid.UUID
	version       int64
	scopeType     string
	formulaID     uuid.UUID
	effectiveFrom time.Time
	effectiveTo   *time.Time
}

type impactUnitConfig struct {
	unitID     uuid.UUID
	configType string
	fixedPrice *int64
}

// BatchCurrentPriceCoverage executes a constant number of SQL statements for
// the complete batch and resolves every dimension from one repeatable-read DB
// snapshot. It intentionally never calls CurrentPrice in an item loop.
func (r *PostgresRepository) BatchCurrentPriceCoverage(ctx context.Context, queries []ImpactPriceQuery) ([]ImpactPriceCoverage, time.Time, int64, string, error) {
	if len(queries) == 0 || len(queries) > maximumImpactPriceModels {
		return nil, time.Time{}, 0, "", ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, time.Time{}, 0, "", err
	}
	defer tx.Rollback(ctx)
	var asOf time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&asOf); err != nil {
		return nil, time.Time{}, 0, "", err
	}
	var revision int64
	if err := tx.QueryRow(ctx, `SELECT revision FROM billing_financial_fact_revisions WHERE singleton`).Scan(&revision); err != nil || revision < 1 {
		if err == nil {
			err = ErrPriceUnavailable
		}
		return nil, time.Time{}, 0, "", err
	}
	models, protocols, credits := make([]string, len(queries)), make([]string, len(queries)), make([]string, len(queries))
	for i, query := range queries {
		models[i], protocols[i], credits[i] = query.ModelID.String(), query.ProtocolCode, query.CreditUnit
	}
	rows, err := tx.Query(ctx, `WITH requested AS (
		SELECT model_id,protocol_code,credit_unit FROM unnest($1::text[],$2::text[],$3::text[]) request(model_id,protocol_code,credit_unit)
	)
	SELECT request.model_id,request.protocol_code,request.credit_unit,price.id,price.version,price.scope_type,price.formula_version_id,price.effective_from,price.effective_to
	FROM requested request
	JOIN pricing_versions price ON price.protocol_code=request.protocol_code AND price.credit_unit=request.credit_unit
	 AND (price.scope_type='GLOBAL' OR price.scope_type='MODEL' AND price.scope_id=request.model_id)
	JOIN billing_protocol_formula_bindings binding ON binding.formula_version_id=price.formula_version_id AND binding.protocol_code=price.protocol_code
	WHERE price.lifecycle_status IN ('PUBLISHED','ACTIVE') AND price.published_at IS NOT NULL AND price.cancelled_at IS NULL
	 AND price.effective_from<=$4 AND (price.effective_to IS NULL OR price.effective_to>$4)
	 AND NOT EXISTS(SELECT 1 FROM pricing_blocks block WHERE block.model_id=request.model_id AND block.protocol_code=request.protocol_code AND block.formula_version_id=price.formula_version_id AND block.credit_unit=request.credit_unit AND block.effective_from<=$4 AND (block.effective_to IS NULL OR block.effective_to>$4))
	ORDER BY request.model_id,request.protocol_code,request.credit_unit,price.formula_version_id,price.scope_type DESC,price.id`, models, protocols, credits, asOf)
	if err != nil {
		return nil, time.Time{}, 0, "", err
	}
	candidates := make(map[string][]impactPriceCandidate, len(queries))
	priceIDs := make([]uuid.UUID, 0)
	for rows.Next() {
		var item impactPriceCandidate
		if err := rows.Scan(&item.query.ModelID, &item.query.ProtocolCode, &item.query.CreditUnit, &item.id, &item.version, &item.scopeType, &item.formulaID, &item.effectiveFrom, &item.effectiveTo); err != nil {
			rows.Close()
			return nil, time.Time{}, 0, "", err
		}
		key := impactPriceQueryKey(item.query)
		candidates[key] = append(candidates[key], item)
		priceIDs = append(priceIDs, item.id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, 0, "", err
	}
	bindings := map[uuid.UUID][]uuid.UUID{}
	rows, err = tx.Query(ctx, `SELECT formula_version_id,required_unit_definition_ids FROM billing_protocol_formula_bindings WHERE protocol_code=ANY($1::text[])`, protocols)
	if err != nil {
		return nil, time.Time{}, 0, "", err
	}
	for rows.Next() {
		var formula uuid.UUID
		var required []uuid.UUID
		if err := rows.Scan(&formula, &required); err != nil {
			rows.Close()
			return nil, time.Time{}, 0, "", err
		}
		bindings[formula] = required
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, 0, "", err
	}
	configs := map[uuid.UUID]map[uuid.UUID]impactUnitConfig{}
	if len(priceIDs) > 0 {
		rows, err = tx.Query(ctx, `SELECT config.price_version_id,config.unit_definition_id,config.config_type,config.fixed_price_per_denominator
		 FROM pricing_version_unit_configs config JOIN metering_unit_definitions unit ON unit.id=config.unit_definition_id
		 WHERE config.price_version_id=ANY($1::uuid[])`, priceIDs)
		if err != nil {
			return nil, time.Time{}, 0, "", err
		}
		for rows.Next() {
			var priceID uuid.UUID
			var config impactUnitConfig
			if err := rows.Scan(&priceID, &config.unitID, &config.configType, &config.fixedPrice); err != nil {
				rows.Close()
				return nil, time.Time{}, 0, "", err
			}
			if configs[priceID] == nil {
				configs[priceID] = map[uuid.UUID]impactUnitConfig{}
			}
			configs[priceID][config.unitID] = config
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, time.Time{}, 0, "", err
		}
	}
	fingerprint, err := impactPriceFactFingerprint(ctx, tx, models, protocols, credits)
	if err != nil {
		return nil, time.Time{}, 0, "", err
	}
	result := make([]ImpactPriceCoverage, 0, len(queries))
	for _, query := range queries {
		item, err := resolveImpactCandidate(query, candidates[impactPriceQueryKey(query)], bindings, configs)
		if err != nil {
			return nil, time.Time{}, 0, "", err
		}
		result = append(result, item)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, time.Time{}, 0, "", err
	}
	return result, asOf.UTC(), revision, fingerprint, nil
}

// impactPriceFactFingerprint binds every Billing fact capable of changing the
// requested price coverage. Rows are streamed in a fixed table/key order from
// the same repeatable-read transaction as the coverage projection.
func impactPriceFactFingerprint(ctx context.Context, tx pgx.Tx, models, protocols, credits []string) (string, error) {
	digest := sha256.New()
	queries := []struct {
		prefix string
		sql    string
		args   []any
	}{
		{"PRICE", `SELECT jsonb_build_object('id',p.id,'scope_type',p.scope_type,'scope_id',p.scope_id,'model',p.model,'protocol_code',p.protocol_code,'formula_version_id',p.formula_version_id,'credit_unit',p.credit_unit,'version',p.version,'space_etag',p.space_etag,'lifecycle_status',p.lifecycle_status,'input_per_million',p.input_per_million,'output_per_million',p.output_per_million,'cached_input_per_million',p.cached_input_per_million,'effective_from',p.effective_from,'effective_to',p.effective_to,'published_at',p.published_at,'cancelled_at',p.cancelled_at,'ended_at',p.ended_at)::text FROM pricing_versions p WHERE p.protocol_code=ANY($2::text[]) AND p.credit_unit=ANY($3::text[]) AND (p.scope_type='GLOBAL' OR p.scope_type='MODEL' AND p.scope_id=ANY($1::text[])) ORDER BY p.scope_type,p.scope_id NULLS FIRST,p.protocol_code,p.formula_version_id,p.credit_unit,p.version,p.id`, []any{models, protocols, credits}},
		{"BINDING", `SELECT jsonb_build_object('formula_version_id',b.formula_version_id,'protocol_code',b.protocol_code,'required_unit_definition_ids',b.required_unit_definition_ids,'formula_snapshot',b.formula_snapshot,'published_at',b.published_at)::text FROM billing_protocol_formula_bindings b WHERE b.protocol_code=ANY($1::text[]) ORDER BY b.protocol_code,b.formula_version_id`, []any{protocols}},
		{"CONFIG", `SELECT jsonb_build_object('price_version_id',c.price_version_id,'unit_definition_id',c.unit_definition_id,'config_type',c.config_type,'fixed_price_per_denominator',c.fixed_price_per_denominator,'created_at',c.created_at)::text FROM pricing_version_unit_configs c JOIN pricing_versions p ON p.id=c.price_version_id WHERE p.protocol_code=ANY($2::text[]) AND p.credit_unit=ANY($3::text[]) AND (p.scope_type='GLOBAL' OR p.scope_type='MODEL' AND p.scope_id=ANY($1::text[])) ORDER BY c.price_version_id,c.unit_definition_id`, []any{models, protocols, credits}},
		{"BLOCK", `SELECT jsonb_build_object('id',b.id,'model_id',b.model_id,'protocol_code',b.protocol_code,'formula_version_id',b.formula_version_id,'credit_unit',b.credit_unit,'effective_from',b.effective_from,'effective_to',b.effective_to,'created_at',b.created_at)::text FROM pricing_blocks b WHERE b.model_id=ANY($1::text[]) AND b.protocol_code=ANY($2::text[]) AND b.credit_unit=ANY($3::text[]) ORDER BY b.model_id,b.protocol_code,b.formula_version_id,b.credit_unit,b.effective_from,b.id`, []any{models, protocols, credits}},
		{"UNIT", `SELECT jsonb_build_object('id',u.id,'unit_code',u.unit_code,'version',u.version,'minimum_precision',u.minimum_precision,'price_denominator',u.price_denominator,'rounding_rule',u.rounding_rule,'status',u.status,'deactivated_at',u.deactivated_at)::text FROM metering_unit_definitions u WHERE u.id IN (SELECT unnest(b.required_unit_definition_ids) FROM billing_protocol_formula_bindings b WHERE b.protocol_code=ANY($1::text[])) ORDER BY u.unit_code,u.version,u.id`, []any{protocols}},
		{"SPACE", `SELECT jsonb_build_object('scope_type',s.scope_type,'scope_key',s.scope_key,'protocol_code',s.protocol_code,'formula_version_id',s.formula_version_id,'credit_unit',s.credit_unit,'etag',s.etag,'updated_at',s.updated_at)::text FROM pricing_space_versions s WHERE s.protocol_code=ANY($2::text[]) AND s.credit_unit=ANY($3::text[]) AND (s.scope_type='GLOBAL' OR s.scope_type='MODEL' AND s.scope_key=ANY($1::text[])) ORDER BY s.scope_type,s.scope_key,s.protocol_code,s.formula_version_id,s.credit_unit`, []any{models, protocols, credits}},
	}
	for _, query := range queries {
		rows, err := tx.Query(ctx, query.sql, query.args...)
		if err != nil {
			return "", err
		}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				rows.Close()
				return "", err
			}
			writeImpactFact(digest, query.prefix, value)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func writeImpactFact(digest hash.Hash, values ...string) {
	for _, value := range values {
		_, _ = digest.Write([]byte(value))
		_, _ = digest.Write([]byte{0})
	}
}

func resolveImpactCandidate(query ImpactPriceQuery, candidates []impactPriceCandidate, bindings map[uuid.UUID][]uuid.UUID, configs map[uuid.UUID]map[uuid.UUID]impactUnitConfig) (ImpactPriceCoverage, error) {
	base := ImpactPriceCoverage{ModelID: query.ModelID, ProtocolCode: query.ProtocolCode, CreditUnit: query.CreditUnit}
	byFormula := map[uuid.UUID][]impactPriceCandidate{}
	for _, candidate := range candidates {
		byFormula[candidate.formulaID] = append(byFormula[candidate.formulaID], candidate)
	}
	resolved := make([]impactPriceCandidate, 0, len(byFormula))
	for formula, formulaCandidates := range byFormula {
		models, globals := []impactPriceCandidate{}, []impactPriceCandidate{}
		for _, candidate := range formulaCandidates {
			if candidate.scopeType == "MODEL" {
				models = append(models, candidate)
			} else {
				globals = append(globals, candidate)
			}
		}
		selected := globals
		if len(models) > 0 {
			selected = models
		}
		if len(selected) > 1 {
			return base, ErrPriceAmbiguous
		}
		if len(selected) == 1 && impactCandidateNeedsGlobal(selected[0], bindings[formula], configs) && len(globals) > 1 {
			return base, ErrPriceAmbiguous
		}
		if len(selected) == 0 || !impactCandidateComplete(selected[0], globals, bindings[formula], configs) {
			continue
		}
		resolved = append(resolved, selected[0])
	}
	if len(resolved) > 1 {
		return base, ErrPriceAmbiguous
	}
	if len(resolved) == 0 {
		return base, nil
	}
	selected := resolved[0]
	from := selected.effectiveFrom.UTC()
	to := selected.effectiveTo
	if to != nil {
		normalized := to.UTC()
		to = &normalized
	}
	base.Covered, base.FormulaVersionID, base.PriceVersionID, base.PriceVersion, base.EffectiveFrom, base.EffectiveTo = true, &selected.formulaID, &selected.id, &selected.version, &from, to
	return base, nil
}

func impactCandidateNeedsGlobal(selected impactPriceCandidate, required []uuid.UUID, configs map[uuid.UUID]map[uuid.UUID]impactUnitConfig) bool {
	for _, unitID := range required {
		if config, ok := configs[selected.id][unitID]; ok && config.configType == "INHERIT_GLOBAL" {
			return true
		}
	}
	return false
}

func impactCandidateComplete(selected impactPriceCandidate, globals []impactPriceCandidate, required []uuid.UUID, configs map[uuid.UUID]map[uuid.UUID]impactUnitConfig) bool {
	if len(required) == 0 || len(configs[selected.id]) != len(required) {
		return false
	}
	for _, unitID := range required {
		config, ok := configs[selected.id][unitID]
		if !ok {
			return false
		}
		if config.configType == "FIXED_PRICE" && config.fixedPrice != nil {
			continue
		}
		if config.configType != "INHERIT_GLOBAL" || selected.scopeType != "MODEL" || len(globals) != 1 {
			return false
		}
		global, ok := configs[globals[0].id][unitID]
		if !ok || global.configType != "FIXED_PRICE" || global.fixedPrice == nil {
			return false
		}
	}
	return true
}

func impactPriceQueryKey(query ImpactPriceQuery) string {
	return query.ModelID.String() + "\x00" + query.ProtocolCode + "\x00" + query.CreditUnit
}
