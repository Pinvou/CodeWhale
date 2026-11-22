package billing

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const maximumModelPriceProjectionTransitions = 100

var errModelPriceProjectionTooManyTransitions = errors.New("model price projection has too many scheduled transitions")
var modelPriceProjectionDimension = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

type ModelPriceProjectionUnit struct {
	UnitCode             string    `json:"unit_code"`
	UnitDefinitionID     uuid.UUID `json:"unit_definition_id"`
	UnitVersion          int64     `json:"unit_version"`
	MinimumPrecision     int64     `json:"minimum_precision"`
	PriceDenominator     int64     `json:"price_denominator"`
	RoundingRule         string    `json:"rounding_rule"`
	ResolvedPrice        int64     `json:"resolved_price"`
	ConfigurationType    string    `json:"configuration_type"`
	PriceSourceScope     string    `json:"price_source_scope"`
	SourcePriceVersionID uuid.UUID `json:"source_price_version_id"`
}

type ModelPriceProjectionPrice struct {
	PriceVersionID uuid.UUID                  `json:"price_version_id"`
	ScopeType      string                     `json:"scope_type"`
	SpaceETag      int64                      `json:"space_etag"`
	EffectiveFrom  time.Time                  `json:"effective_from"`
	EffectiveTo    *time.Time                 `json:"effective_to"`
	Units          []ModelPriceProjectionUnit `json:"units"`
}

type ModelPriceProjectionPoint struct {
	EffectiveAt    time.Time                  `json:"effective_at"`
	CoverageStatus string                     `json:"coverage_status"`
	Price          *ModelPriceProjectionPrice `json:"price,omitempty"`
}

type ModelPriceProjection struct {
	ModelID           uuid.UUID                  `json:"model_id"`
	ProtocolCode      string                     `json:"protocol_code"`
	FormulaVersionID  uuid.UUID                  `json:"formula_version_id"`
	CreditUnit        string                     `json:"credit_unit"`
	AsOf              time.Time                  `json:"as_of"`
	Current           ModelPriceProjectionPoint  `json:"current"`
	NextChange        *ModelPriceProjectionPoint `json:"next_change"`
	Source            string                     `json:"source"`
	SnapshotSemantics string                     `json:"snapshot_semantics"`
}

type modelPriceProjectionRepository interface {
	AdminModelPriceProjection(context.Context, uuid.UUID, string, uuid.UUID, string) (ModelPriceProjection, error)
}

func (s *Service) WithAdminModelPriceProjection(enabled bool) *Service {
	s.modelPriceProjectionEnabled = enabled
	return s
}

func (s *Service) adminModelPriceProjection(w http.ResponseWriter, r *http.Request) {
	startedAt, outcome := time.Now(), "success"
	defer func() {
		if s.modelPriceProjectionMetrics != nil {
			s.modelPriceProjectionMetrics.requests.WithLabelValues(outcome).Inc()
			s.modelPriceProjectionMetrics.duration.WithLabelValues(outcome).Observe(time.Since(startedAt).Seconds())
		}
	}()
	if !s.modelPriceProjectionEnabled {
		outcome = "disabled"
		api.WriteError(w, http.StatusServiceUnavailable, "MODEL_PRICE_PROJECTION_DISABLED", "the model price projection is not enabled", httpserver.RequestID(r.Context()))
		return
	}
	modelID, modelErr := uuid.Parse(r.PathValue("model_id"))
	query := r.URL.Query()
	for key := range query {
		if key != "protocol_code" && key != "formula_version_id" && key != "credit_unit" {
			modelErr = ErrConflict
		}
	}
	protocolValues, formulaValues, creditValues := query["protocol_code"], query["formula_version_id"], query["credit_unit"]
	if len(protocolValues) != 1 || len(formulaValues) != 1 || len(creditValues) != 1 {
		modelErr = ErrConflict
	}
	protocol, creditUnit := "", ""
	formulaID := uuid.Nil
	if len(protocolValues) == 1 {
		protocol = strings.ToUpper(strings.TrimSpace(protocolValues[0]))
	}
	if len(creditValues) == 1 {
		creditUnit = strings.ToUpper(strings.TrimSpace(creditValues[0]))
	}
	if len(formulaValues) == 1 {
		formulaID, _ = uuid.Parse(formulaValues[0])
	}
	if modelErr != nil || modelID == uuid.Nil || len(protocol) > 64 || !modelPriceProjectionDimension.MatchString(protocol) || formulaID == uuid.Nil || len(creditUnit) > 32 || !modelPriceProjectionDimension.MatchString(creditUnit) {
		outcome = "invalid"
		api.WriteError(w, http.StatusBadRequest, "INVALID_PRICE_PROJECTION_QUERY", "model_id and exact pricing dimensions are required", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(modelPriceProjectionRepository)
	if !ok {
		outcome = "unavailable"
		api.WriteError(w, http.StatusServiceUnavailable, "MODEL_PRICE_PROJECTION_UNAVAILABLE", "the model price projection is temporarily unavailable", httpserver.RequestID(r.Context()))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	projection, err := repository.AdminModelPriceProjection(ctx, modelID, protocol, formulaID, creditUnit)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			outcome = "timeout"
			api.WriteError(w, http.StatusGatewayTimeout, "MODEL_PRICE_PROJECTION_TIMEOUT", "the model price projection timed out", httpserver.RequestID(r.Context()))
			return
		}
		outcome = "unavailable"
		api.WriteError(w, http.StatusServiceUnavailable, "MODEL_PRICE_PROJECTION_UNAVAILABLE", "the model price projection is temporarily unavailable", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, projection)
}

func (r *PostgresRepository) AdminModelPriceProjection(ctx context.Context, modelID uuid.UUID, protocol string, formulaID uuid.UUID, creditUnit string) (ModelPriceProjection, error) {
	if modelID == uuid.Nil || protocol == "" || formulaID == uuid.Nil || creditUnit == "" {
		return ModelPriceProjection{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ModelPriceProjection{}, err
	}
	defer tx.Rollback(ctx)
	var asOf time.Time
	if err := tx.QueryRow(ctx, `SELECT transaction_timestamp()`).Scan(&asOf); err != nil {
		return ModelPriceProjection{}, err
	}
	model := modelID.String()
	current, err := projectModelPricePoint(ctx, tx, model, protocol, formulaID, creditUnit, asOf)
	if err != nil {
		return ModelPriceProjection{}, err
	}
	transitions, err := modelPriceTransitionTimes(ctx, tx, model, protocol, formulaID, creditUnit, asOf)
	if err != nil {
		return ModelPriceProjection{}, err
	}
	var next *ModelPriceProjectionPoint
	currentFingerprint := modelPricePointFingerprint(current)
	for _, transition := range transitions {
		candidate, candidateErr := projectModelPricePoint(ctx, tx, model, protocol, formulaID, creditUnit, transition)
		if candidateErr != nil {
			return ModelPriceProjection{}, candidateErr
		}
		if modelPricePointFingerprint(candidate) != currentFingerprint {
			next = &candidate
			break
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return ModelPriceProjection{}, err
	}
	return ModelPriceProjection{
		ModelID: modelID, ProtocolCode: protocol, FormulaVersionID: formulaID, CreditUnit: creditUnit,
		AsOf: asOf.UTC(), Current: current, NextChange: next, Source: "BILLING",
		SnapshotSemantics: "BILLING_REPEATABLE_READ_ONLY; TOKEN_CATALOG_NOT_READ; NO_CROSS_DOMAIN_ATOMICITY",
	}, nil
}

func projectModelPricePoint(ctx context.Context, tx pgx.Tx, model, protocol string, formulaID uuid.UUID, creditUnit string, at time.Time) (ModelPriceProjectionPoint, error) {
	point := ModelPriceProjectionPoint{EffectiveAt: at.UTC(), CoverageStatus: "AVAILABLE"}
	price, err := resolveCurrentPriceTx(ctx, tx, model, protocol, formulaID, creditUnit, at)
	if errors.Is(err, ErrPriceMissing) {
		point.CoverageStatus = "MISSING"
		return point, nil
	}
	if errors.Is(err, ErrPriceUnavailable) {
		point.CoverageStatus = "BLOCKED"
		return point, nil
	}
	if err != nil {
		return ModelPriceProjectionPoint{}, err
	}
	scopeKey := "*"
	if price.ScopeType == "MODEL" {
		scopeKey = model
	}
	var etag int64
	if err := tx.QueryRow(ctx, `SELECT etag FROM pricing_space_versions WHERE scope_type=$1 AND scope_key=$2 AND protocol_code=$3 AND formula_version_id=$4 AND credit_unit=$5`, price.ScopeType, scopeKey, protocol, formulaID, creditUnit).Scan(&etag); err != nil {
		return ModelPriceProjectionPoint{}, err
	}
	units := make([]ModelPriceProjectionUnit, 0, len(price.Units))
	for _, unit := range price.Units {
		units = append(units, ModelPriceProjectionUnit{
			UnitCode: unit.UnitCode, UnitDefinitionID: unit.UnitDefinitionID, UnitVersion: unit.UnitVersion,
			MinimumPrecision: unit.MinimumPrecision, PriceDenominator: unit.PriceDenominator, RoundingRule: unit.RoundingRule,
			ResolvedPrice: unit.ResolvedPrice, ConfigurationType: unit.ConfigurationType,
			PriceSourceScope: unit.PriceSourceScope, SourcePriceVersionID: unit.SourcePriceVersionID,
		})
	}
	effectiveTo := price.EffectiveTo
	if effectiveTo != nil {
		normalized := effectiveTo.UTC()
		effectiveTo = &normalized
	}
	point.Price = &ModelPriceProjectionPrice{PriceVersionID: price.ID, ScopeType: price.ScopeType, SpaceETag: etag, EffectiveFrom: price.EffectiveFrom.UTC(), EffectiveTo: effectiveTo, Units: units}
	return point, nil
}

func modelPriceTransitionTimes(ctx context.Context, tx pgx.Tx, model, protocol string, formulaID uuid.UUID, creditUnit string, after time.Time) ([]time.Time, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT transition_at FROM (
    SELECT effective_from AS transition_at FROM pricing_versions
     WHERE (scope_type='GLOBAL' OR scope_type='MODEL' AND scope_id=$1) AND protocol_code=$2 AND formula_version_id=$3 AND credit_unit=$4
       AND lifecycle_status IN ('PUBLISHED','ACTIVE') AND published_at IS NOT NULL AND cancelled_at IS NULL AND effective_from>$5
    UNION ALL
    SELECT effective_to FROM pricing_versions
     WHERE (scope_type='GLOBAL' OR scope_type='MODEL' AND scope_id=$1) AND protocol_code=$2 AND formula_version_id=$3 AND credit_unit=$4
       AND lifecycle_status IN ('PUBLISHED','ACTIVE') AND published_at IS NOT NULL AND cancelled_at IS NULL AND effective_to>$5
    UNION ALL
    SELECT effective_from FROM pricing_blocks
     WHERE model_id=$1 AND protocol_code=$2 AND formula_version_id=$3 AND credit_unit=$4 AND effective_from>$5
    UNION ALL
    SELECT effective_to FROM pricing_blocks
     WHERE model_id=$1 AND protocol_code=$2 AND formula_version_id=$3 AND credit_unit=$4 AND effective_to>$5
) transitions WHERE transition_at IS NOT NULL ORDER BY transition_at LIMIT $6`, model, protocol, formulaID, creditUnit, after, maximumModelPriceProjectionTransitions+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	transitions := make([]time.Time, 0)
	for rows.Next() {
		var transition time.Time
		if err := rows.Scan(&transition); err != nil {
			return nil, err
		}
		transitions = append(transitions, transition.UTC())
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(transitions) > maximumModelPriceProjectionTransitions {
		return nil, errModelPriceProjectionTooManyTransitions
	}
	return transitions, nil
}

func modelPricePointFingerprint(point ModelPriceProjectionPoint) string {
	copyPoint := point
	copyPoint.EffectiveAt = time.Time{}
	encoded, _ := json.Marshal(copyPoint)
	return string(encoded)
}
