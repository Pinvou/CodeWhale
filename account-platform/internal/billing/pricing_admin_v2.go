package billing

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type MeteringUnitDefinition struct {
	ID               uuid.UUID  `json:"unit_definition_id"`
	UnitCode         string     `json:"unit_code"`
	Version          int64      `json:"version"`
	MinimumPrecision int64      `json:"minimum_precision"`
	PriceDenominator int64      `json:"price_denominator"`
	RoundingRule     string     `json:"rounding_rule"`
	Status           string     `json:"status"`
	CreatedAt        time.Time  `json:"created_at"`
	DeactivatedAt    *time.Time `json:"deactivated_at,omitempty"`
}

type PriceUnitConfiguration struct {
	UnitDefinitionID         uuid.UUID `json:"unit_definition_id"`
	ConfigType               string    `json:"config_type"`
	FixedPricePerDenominator *int64    `json:"fixed_price_per_denominator,omitempty"`
}

type PriceDraftCommand struct {
	ScopeType        string                   `json:"scope_type"`
	ScopeID          *string                  `json:"scope_id,omitempty"`
	ProtocolCode     string                   `json:"protocol_code"`
	FormulaVersionID uuid.UUID                `json:"formula_version_id"`
	CreditUnit       string                   `json:"credit_unit"`
	EffectiveFrom    time.Time                `json:"effective_from"`
	EffectiveTo      *time.Time               `json:"effective_to,omitempty"`
	ExpectedETag     int64                    `json:"expected_etag"`
	Units            []PriceUnitConfiguration `json:"units"`
}

type ManagedPriceVersion struct {
	PriceVersionID          uuid.UUID                `json:"price_version_id"`
	ScopeType               string                   `json:"scope_type"`
	ScopeID                 *string                  `json:"scope_id,omitempty"`
	ProtocolCode            string                   `json:"protocol_code"`
	FormulaVersionID        uuid.UUID                `json:"formula_version_id"`
	CreditUnit              string                   `json:"credit_unit"`
	LifecycleStatus         string                   `json:"lifecycle_status"`
	DerivedStatus           string                   `json:"derived_status"`
	Version                 int64                    `json:"version"`
	SpaceETag               int64                    `json:"space_etag"`
	CurrentSpaceETag        int64                    `json:"current_space_etag"`
	EffectiveFrom           time.Time                `json:"effective_from"`
	EffectiveTo             *time.Time               `json:"effective_to,omitempty"`
	PublishedAt             *time.Time               `json:"published_at,omitempty"`
	Units                   []PriceUnitConfiguration `json:"units"`
	SuccessorPriceVersionID *uuid.UUID               `json:"successor_price_version_id,omitempty"`
}

type ManagedPriceSpaceQuery struct {
	ScopeType        string
	ScopeID          *string
	ProtocolCode     string
	FormulaVersionID uuid.UUID
	CreditUnit       string
}

var shanghaiLocation = func() *time.Location {
	location, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		return time.FixedZone("Asia/Shanghai", 8*60*60)
	}
	return location
}()

func normalizePriceDraft(command PriceDraftCommand) (PriceDraftCommand, string, error) {
	command.ScopeType = strings.ToUpper(strings.TrimSpace(command.ScopeType))
	command.ProtocolCode = strings.ToUpper(strings.TrimSpace(command.ProtocolCode))
	command.CreditUnit = strings.ToUpper(strings.TrimSpace(command.CreditUnit))
	if command.ScopeID != nil {
		value := strings.TrimSpace(*command.ScopeID)
		command.ScopeID = &value
	}
	if command.ScopeType != "GLOBAL" && command.ScopeType != "MODEL" || command.ProtocolCode == "" || command.CreditUnit == "" || command.FormulaVersionID == uuid.Nil || command.ExpectedETag < 1 || !isShanghaiBoundary(command.EffectiveFrom) || command.EffectiveTo != nil && (!isShanghaiBoundary(*command.EffectiveTo) || !command.EffectiveTo.After(command.EffectiveFrom)) {
		return command, "", ErrConflict
	}
	if command.ScopeType == "GLOBAL" {
		if command.ScopeID != nil && *command.ScopeID != "" {
			return command, "", ErrConflict
		}
		command.ScopeID = nil
	} else if command.ScopeID == nil || *command.ScopeID == "" || len(*command.ScopeID) > 160 {
		return command, "", ErrConflict
	}
	scopeKey := "*"
	if command.ScopeID != nil {
		scopeKey = *command.ScopeID
	}
	return command, scopeKey, nil
}

func isShanghaiBoundary(value time.Time) bool {
	local := value.In(shanghaiLocation)
	return local.Hour() == 0 && local.Minute() == 0 && local.Second() == 0 && local.Nanosecond() == 0
}

func (r *PostgresRepository) ListMeteringUnits(ctx context.Context) ([]MeteringUnitDefinition, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,unit_code,version,minimum_precision,price_denominator,rounding_rule,status,created_at,deactivated_at FROM metering_unit_definitions ORDER BY unit_code,version DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]MeteringUnitDefinition, 0)
	for rows.Next() {
		var item MeteringUnitDefinition
		if err := rows.Scan(&item.ID, &item.UnitCode, &item.Version, &item.MinimumPrecision, &item.PriceDenominator, &item.RoundingRule, &item.Status, &item.CreatedAt, &item.DeactivatedAt); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (r *PostgresRepository) CreateMeteringUnitVersion(ctx context.Context, unit MeteringUnitDefinition, actor uuid.UUID, idempotencyKey string) (MeteringUnitDefinition, error) {
	unit.UnitCode, unit.RoundingRule = strings.ToUpper(strings.TrimSpace(unit.UnitCode)), strings.ToUpper(strings.TrimSpace(unit.RoundingRule))
	if unit.UnitCode == "" || unit.MinimumPrecision < 1 || unit.PriceDenominator < 1 || !oneOf(unit.RoundingRule, "CEILING", "FLOOR", "HALF_UP") {
		return MeteringUnitDefinition{}, ErrConflict
	}
	facts := struct {
		Code                   string
		Precision, Denominator int64
		Rounding               string
	}{unit.UnitCode, unit.MinimumPrecision, unit.PriceDenominator, unit.RoundingRule}
	tx, reply, fingerprint, err := r.beginPricingCommand(ctx, actor, idempotencyKey, "UNIT_CREATE", facts)
	if err != nil {
		return MeteringUnitDefinition{}, err
	}
	if reply != nil {
		var previous MeteringUnitDefinition
		if err := json.Unmarshal(reply, &previous); err != nil {
			return MeteringUnitDefinition{}, err
		}
		return previous, nil
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "metering.unit", unit.UnitCode); err != nil {
		return MeteringUnitDefinition{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version),0)+1 FROM metering_unit_definitions WHERE unit_code=$1`, unit.UnitCode).Scan(&unit.Version); err != nil {
		return MeteringUnitDefinition{}, err
	}
	unit.ID, unit.Status, unit.CreatedAt = uuid.Must(uuid.NewV7()), "ACTIVE", r.now()
	if _, err := tx.Exec(ctx, `INSERT INTO metering_unit_definitions(id,unit_code,version,minimum_precision,price_denominator,rounding_rule,status,created_at) VALUES($1,$2,$3,$4,$5,$6,'ACTIVE',$7)`, unit.ID, unit.UnitCode, unit.Version, unit.MinimumPrecision, unit.PriceDenominator, unit.RoundingRule, unit.CreatedAt); err != nil {
		return MeteringUnitDefinition{}, err
	}
	return unit, r.completePricingCommand(ctx, tx, actor, idempotencyKey, "UNIT_CREATE", fingerprint, unit)
}

func (r *PostgresRepository) DeactivateMeteringUnit(ctx context.Context, id uuid.UUID) (MeteringUnitDefinition, error) {
	if id == uuid.Nil {
		return MeteringUnitDefinition{}, ErrConflict
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return MeteringUnitDefinition{}, err
	}
	defer tx.Rollback(ctx)
	var item MeteringUnitDefinition
	err = tx.QueryRow(ctx, `SELECT id,unit_code,version,minimum_precision,price_denominator,rounding_rule,status,created_at,deactivated_at FROM metering_unit_definitions WHERE id=$1 FOR UPDATE`, id).Scan(&item.ID, &item.UnitCode, &item.Version, &item.MinimumPrecision, &item.PriceDenominator, &item.RoundingRule, &item.Status, &item.CreatedAt, &item.DeactivatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return MeteringUnitDefinition{}, ErrNotFound
	}
	if err != nil {
		return MeteringUnitDefinition{}, err
	}
	if item.Status == "INACTIVE" {
		return item, tx.Commit(ctx)
	}
	if item.Status != "ACTIVE" {
		return MeteringUnitDefinition{}, ErrConflict
	}
	now := r.now()
	if err := tx.QueryRow(ctx, `UPDATE metering_unit_definitions SET status='INACTIVE',deactivated_at=$2 WHERE id=$1 AND status='ACTIVE' RETURNING deactivated_at`, id, now).Scan(&item.DeactivatedAt); err != nil {
		return MeteringUnitDefinition{}, err
	}
	item.Status = "INACTIVE"
	return item, tx.Commit(ctx)
}

func (r *PostgresRepository) CreatePriceDraft(ctx context.Context, command PriceDraftCommand, actor uuid.UUID, idempotencyKey string) (ManagedPriceVersion, error) {
	command, scopeKey, err := normalizePriceDraft(command)
	if err != nil || actor == uuid.Nil {
		return ManagedPriceVersion{}, ErrConflict
	}
	tx, reply, fingerprint, err := r.beginPricingCommand(ctx, actor, idempotencyKey, "PRICE_DRAFT_CREATE", command)
	if err != nil {
		return ManagedPriceVersion{}, err
	}
	if reply != nil {
		var previous ManagedPriceVersion
		if err := json.Unmarshal(reply, &previous); err != nil {
			return ManagedPriceVersion{}, err
		}
		return previous, nil
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "pricing.space", command.ScopeType+"|"+scopeKey+"|"+command.ProtocolCode+"|"+command.FormulaVersionID.String()+"|"+command.CreditUnit); err != nil {
		return ManagedPriceVersion{}, err
	}
	var etag int64
	err = tx.QueryRow(ctx, `SELECT etag FROM pricing_space_versions WHERE scope_type=$1 AND scope_key=$2 AND protocol_code=$3 AND formula_version_id=$4 AND credit_unit=$5 FOR UPDATE`, command.ScopeType, scopeKey, command.ProtocolCode, command.FormulaVersionID, command.CreditUnit).Scan(&etag)
	if errors.Is(err, pgx.ErrNoRows) {
		etag = 1
		_, err = tx.Exec(ctx, `INSERT INTO pricing_space_versions(scope_type,scope_key,protocol_code,formula_version_id,credit_unit,etag,updated_at) VALUES($1,$2,$3,$4,$5,1,$6)`, command.ScopeType, scopeKey, command.ProtocolCode, command.FormulaVersionID, command.CreditUnit, r.now())
	}
	if err != nil {
		return ManagedPriceVersion{}, err
	}
	if etag != command.ExpectedETag {
		return ManagedPriceVersion{}, ErrPriceETagConflict
	}
	if err := validatePriceUnitConfigurations(ctx, tx, command.ScopeType, command.ProtocolCode, command.FormulaVersionID, command.Units); err != nil {
		return ManagedPriceVersion{}, err
	}
	var version int64
	if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version),0)+1 FROM pricing_versions WHERE scope_type=$1 AND COALESCE(scope_id,'')=$2 AND protocol_code=$3 AND formula_version_id=$4 AND credit_unit=$5`, command.ScopeType, nullableScopeKey(command.ScopeType, scopeKey), command.ProtocolCode, command.FormulaVersionID, command.CreditUnit).Scan(&version); err != nil {
		return ManagedPriceVersion{}, err
	}
	legacy := legacyRates(command.Units)
	now := r.now()
	id := uuid.Must(uuid.NewV7())
	model := scopeKey
	_, err = tx.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,effective_to,created_at,scope_type,scope_id,protocol_code,formula_version_id,credit_unit,lifecycle_status,version,space_etag,created_by) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,'DRAFT',$14,$15,$16)`, id, model, legacy["INPUT_TOKEN"], legacy["OUTPUT_TOKEN"], legacy["CACHED_INPUT_TOKEN"], command.EffectiveFrom, command.EffectiveTo, now, command.ScopeType, command.ScopeID, command.ProtocolCode, command.FormulaVersionID, command.CreditUnit, version, etag, actor)
	if err != nil {
		return ManagedPriceVersion{}, err
	}
	for _, unit := range command.Units {
		if _, err := tx.Exec(ctx, `INSERT INTO pricing_version_unit_configs(price_version_id,unit_definition_id,config_type,fixed_price_per_denominator,created_at) VALUES($1,$2,$3,$4,$5)`, id, unit.UnitDefinitionID, unit.ConfigType, unit.FixedPricePerDenominator, now); err != nil {
			return ManagedPriceVersion{}, err
		}
	}
	item := ManagedPriceVersion{PriceVersionID: id, ScopeType: command.ScopeType, ScopeID: command.ScopeID, ProtocolCode: command.ProtocolCode, FormulaVersionID: command.FormulaVersionID, CreditUnit: command.CreditUnit, LifecycleStatus: "DRAFT", DerivedStatus: "DRAFT", Version: version, SpaceETag: etag, CurrentSpaceETag: etag, EffectiveFrom: command.EffectiveFrom, EffectiveTo: command.EffectiveTo, Units: command.Units}
	if err := appendPricingAudit(ctx, tx, id, "DRAFT_CREATED", actor, "", nil, item, now); err != nil {
		return ManagedPriceVersion{}, err
	}
	return item, r.completePricingCommand(ctx, tx, actor, idempotencyKey, "PRICE_DRAFT_CREATE", fingerprint, item)
}

func (r *PostgresRepository) ListManagedPrices(ctx context.Context, limit int) ([]ManagedPriceVersion, error) {
	if limit < 1 || limit > 200 {
		return nil, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := tx.Query(ctx, `SELECT id FROM pricing_versions ORDER BY created_at DESC,id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	ids := make([]uuid.UUID, 0)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	items := make([]ManagedPriceVersion, 0, len(ids))
	for _, id := range ids {
		item, scopeKey, err := loadManagedPrice(ctx, tx, id, false)
		if err != nil {
			return nil, err
		}
		if err := tx.QueryRow(ctx, `SELECT etag FROM pricing_space_versions WHERE scope_type=$1 AND scope_key=$2 AND protocol_code=$3 AND formula_version_id=$4 AND credit_unit=$5`, item.ScopeType, scopeKey, item.ProtocolCode, item.FormulaVersionID, item.CreditUnit).Scan(&item.CurrentSpaceETag); err != nil {
			return nil, err
		}
		item.DerivedStatus = derivePriceStatus(item, r.now())
		items = append(items, item)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return items, nil
}

func normalizeManagedPriceSpaceQuery(query ManagedPriceSpaceQuery) (ManagedPriceSpaceQuery, string, error) {
	query.ScopeType = strings.ToUpper(strings.TrimSpace(query.ScopeType))
	query.ProtocolCode = strings.ToUpper(strings.TrimSpace(query.ProtocolCode))
	query.CreditUnit = strings.ToUpper(strings.TrimSpace(query.CreditUnit))
	if query.ScopeID != nil {
		value := strings.TrimSpace(*query.ScopeID)
		query.ScopeID = &value
	}
	if query.ScopeType == "GLOBAL" {
		if query.ScopeID != nil && *query.ScopeID != "" {
			return query, "", ErrConflict
		}
		query.ScopeID = nil
	} else if query.ScopeType != "MODEL" || query.ScopeID == nil || *query.ScopeID == "" || len(*query.ScopeID) > 160 {
		return query, "", ErrConflict
	}
	if query.ProtocolCode == "" || query.FormulaVersionID == uuid.Nil || query.CreditUnit == "" {
		return query, "", ErrConflict
	}
	scopeKey := "*"
	if query.ScopeID != nil {
		scopeKey = *query.ScopeID
	}
	return query, scopeKey, nil
}

func (r *PostgresRepository) ReadManagedPrice(ctx context.Context, id uuid.UUID) (ManagedPriceVersion, error) {
	if id == uuid.Nil {
		return ManagedPriceVersion{}, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return ManagedPriceVersion{}, err
	}
	defer tx.Rollback(ctx)
	item, scopeKey, err := loadManagedPrice(ctx, tx, id, false)
	if err != nil {
		return ManagedPriceVersion{}, err
	}
	if err := tx.QueryRow(ctx, `SELECT etag FROM pricing_space_versions WHERE scope_type=$1 AND scope_key=$2 AND protocol_code=$3 AND formula_version_id=$4 AND credit_unit=$5`, item.ScopeType, scopeKey, item.ProtocolCode, item.FormulaVersionID, item.CreditUnit).Scan(&item.CurrentSpaceETag); err != nil {
		return ManagedPriceVersion{}, err
	}
	item.DerivedStatus = derivePriceStatus(item, r.now())
	return item, tx.Commit(ctx)
}

func (r *PostgresRepository) ReadManagedPriceSpace(ctx context.Context, raw ManagedPriceSpaceQuery, limit int) (int64, []ManagedPriceVersion, bool, error) {
	query, scopeKey, err := normalizeManagedPriceSpaceQuery(raw)
	if err != nil || limit < 1 || limit > 200 {
		return 0, nil, false, ErrConflict
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return 0, nil, false, err
	}
	defer tx.Rollback(ctx)
	var etag int64
	err = tx.QueryRow(ctx, `SELECT etag FROM pricing_space_versions WHERE scope_type=$1 AND scope_key=$2 AND protocol_code=$3 AND formula_version_id=$4 AND credit_unit=$5`, query.ScopeType, scopeKey, query.ProtocolCode, query.FormulaVersionID, query.CreditUnit).Scan(&etag)
	if errors.Is(err, pgx.ErrNoRows) {
		return 1, []ManagedPriceVersion{}, false, tx.Commit(ctx)
	}
	if err != nil {
		return 0, nil, false, err
	}
	rows, err := tx.Query(ctx, `SELECT id FROM pricing_versions WHERE scope_type=$1 AND COALESCE(scope_id,'')=$2 AND protocol_code=$3 AND formula_version_id=$4 AND credit_unit=$5 ORDER BY created_at DESC,id DESC LIMIT $6`, query.ScopeType, nullableScopeKey(query.ScopeType, scopeKey), query.ProtocolCode, query.FormulaVersionID, query.CreditUnit, limit+1)
	if err != nil {
		return 0, nil, false, err
	}
	ids := make([]uuid.UUID, 0, limit+1)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, nil, false, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, nil, false, err
	}
	hasMore := len(ids) > limit
	if hasMore {
		ids = ids[:limit]
	}
	items := make([]ManagedPriceVersion, 0, len(ids))
	for _, id := range ids {
		item, _, err := loadManagedPrice(ctx, tx, id, false)
		if err != nil {
			return 0, nil, false, err
		}
		item.CurrentSpaceETag = etag
		item.DerivedStatus = derivePriceStatus(item, r.now())
		items = append(items, item)
	}
	return etag, items, hasMore, tx.Commit(ctx)
}

func (r *PostgresRepository) UpdatePriceDraft(ctx context.Context, id, actor uuid.UUID, command PriceDraftCommand) (ManagedPriceVersion, error) {
	command, scopeKey, err := normalizePriceDraft(command)
	if err != nil || id == uuid.Nil || actor == uuid.Nil {
		return ManagedPriceVersion{}, ErrConflict
	}
	// The existing actor, draft and observed ETag identify this logical update.
	// Preserve its immutable response before considering a later namespace ETag.
	key := "draft-update:" + id.String() + ":" + strconv.FormatInt(command.ExpectedETag, 10)
	facts := struct {
		ID      uuid.UUID
		Command PriceDraftCommand
	}{id, command}
	tx, reply, fingerprint, err := r.beginPricingCommand(ctx, actor, key, "PRICE_DRAFT_UPDATE", facts)
	if err != nil {
		if errors.Is(err, ErrIdempotencyConflict) {
			return ManagedPriceVersion{}, ErrPriceETagConflict
		}
		return ManagedPriceVersion{}, err
	}
	if reply != nil {
		var previous ManagedPriceVersion
		if err := json.Unmarshal(reply, &previous); err != nil {
			return ManagedPriceVersion{}, err
		}
		return previous, nil
	}
	defer tx.Rollback(ctx)
	etag, err := lockManagedPriceSpace(ctx, tx, id, r.now())
	if err != nil {
		return ManagedPriceVersion{}, err
	}
	before, currentScopeKey, err := loadManagedPriceForUpdate(ctx, tx, id)
	if err != nil {
		return ManagedPriceVersion{}, err
	}
	if before.LifecycleStatus != "DRAFT" || etag != command.ExpectedETag || before.ScopeType != command.ScopeType || currentScopeKey != scopeKey || before.ProtocolCode != command.ProtocolCode || before.FormulaVersionID != command.FormulaVersionID || before.CreditUnit != command.CreditUnit {
		return ManagedPriceVersion{}, ErrPriceETagConflict
	}
	if err := validatePriceUnitConfigurations(ctx, tx, command.ScopeType, command.ProtocolCode, command.FormulaVersionID, command.Units); err != nil {
		return ManagedPriceVersion{}, err
	}
	legacy := legacyRates(command.Units)
	if _, err := tx.Exec(ctx, `UPDATE pricing_versions SET input_per_million=$2,output_per_million=$3,cached_input_per_million=$4,effective_from=$5,effective_to=$6,space_etag=$7 WHERE id=$1`, id, legacy["INPUT_TOKEN"], legacy["OUTPUT_TOKEN"], legacy["CACHED_INPUT_TOKEN"], command.EffectiveFrom, command.EffectiveTo, etag+1); err != nil {
		return ManagedPriceVersion{}, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM pricing_version_unit_configs WHERE price_version_id=$1`, id); err != nil {
		return ManagedPriceVersion{}, err
	}
	for _, unit := range command.Units {
		if _, err := tx.Exec(ctx, `INSERT INTO pricing_version_unit_configs(price_version_id,unit_definition_id,config_type,fixed_price_per_denominator,created_at) VALUES($1,$2,$3,$4,$5)`, id, unit.UnitDefinitionID, unit.ConfigType, unit.FixedPricePerDenominator, r.now()); err != nil {
			return ManagedPriceVersion{}, err
		}
	}
	before.EffectiveFrom, before.EffectiveTo, before.Units = command.EffectiveFrom, command.EffectiveTo, command.Units
	before.SpaceETag = etag + 1
	before.CurrentSpaceETag = etag + 1
	if _, err := tx.Exec(ctx, `UPDATE pricing_space_versions SET etag=$6,updated_at=$7 WHERE scope_type=$1 AND scope_key=$2 AND protocol_code=$3 AND formula_version_id=$4 AND credit_unit=$5`, before.ScopeType, scopeKey, before.ProtocolCode, before.FormulaVersionID, before.CreditUnit, before.SpaceETag, r.now()); err != nil {
		return ManagedPriceVersion{}, err
	}
	if err := appendPricingAudit(ctx, tx, id, "DRAFT_UPDATED", actor, "", nil, before, r.now()); err != nil {
		return ManagedPriceVersion{}, err
	}
	return before, r.completePricingCommand(ctx, tx, actor, key, "PRICE_DRAFT_UPDATE", fingerprint, before)
}

func (r *PostgresRepository) PreviewPriceVersion(ctx context.Context, id uuid.UUID) (PriceVersion, error) {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return PriceVersion{}, err
	}
	defer tx.Rollback(ctx)
	item, scopeKey, err := loadManagedPrice(ctx, tx, id, false)
	if err != nil {
		return PriceVersion{}, err
	}
	model := scopeKey
	if item.ScopeType == "GLOBAL" {
		model = "*"
	}
	candidate := priceCandidate{id: item.PriceVersionID, scopeType: item.ScopeType, model: scopeKey, protocol: item.ProtocolCode, formulaID: item.FormulaVersionID, creditUnit: item.CreditUnit, effectiveFrom: item.EffectiveFrom, effectiveTo: item.EffectiveTo}
	price, err := resolvePriceCandidate(ctx, tx, candidate, model, item.EffectiveFrom)
	if err != nil {
		return PriceVersion{}, err
	}
	return price, tx.Commit(ctx)
}

func validatePriceUnitConfigurations(ctx context.Context, tx pgx.Tx, scopeType, protocol string, formulaID uuid.UUID, units []PriceUnitConfiguration) error {
	var required []uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT required_unit_definition_ids FROM billing_protocol_formula_bindings WHERE formula_version_id=$1 AND protocol_code=$2`, formulaID, protocol).Scan(&required); err != nil {
		return ErrPriceMissing
	}
	configured := make(map[uuid.UUID]PriceUnitConfiguration, len(units))
	for _, unit := range units {
		if unit.UnitDefinitionID == uuid.Nil || configured[unit.UnitDefinitionID].UnitDefinitionID != uuid.Nil || !oneOf(unit.ConfigType, "FIXED_PRICE", "INHERIT_GLOBAL") || unit.ConfigType == "FIXED_PRICE" && (unit.FixedPricePerDenominator == nil || *unit.FixedPricePerDenominator < 0) || unit.ConfigType == "INHERIT_GLOBAL" && (scopeType != "MODEL" || unit.FixedPricePerDenominator != nil) {
			return ErrConflict
		}
		configured[unit.UnitDefinitionID] = unit
	}
	if len(configured) != len(required) {
		return ErrPriceMissing
	}
	for _, id := range required {
		if configured[id].UnitDefinitionID == uuid.Nil {
			return ErrPriceMissing
		}
	}
	return nil
}

func nullableScopeKey(scopeType, scopeKey string) string {
	if scopeType == "GLOBAL" {
		return ""
	}
	return scopeKey
}
func legacyRates(units []PriceUnitConfiguration) map[string]int64 {
	result := map[string]int64{}
	for _, unit := range units {
		if unit.FixedPricePerDenominator == nil {
			continue
		}
		switch unit.UnitDefinitionID.String() {
		case "00000000-0000-7000-8100-000000000001":
			result["INPUT_TOKEN"] = *unit.FixedPricePerDenominator
		case "00000000-0000-7000-8100-000000000002":
			result["CACHED_INPUT_TOKEN"] = *unit.FixedPricePerDenominator
		case "00000000-0000-7000-8100-000000000003":
			result["OUTPUT_TOKEN"] = *unit.FixedPricePerDenominator
		}
	}
	return result
}
func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

func appendPricingAudit(ctx context.Context, tx pgx.Tx, id uuid.UUID, action string, actor uuid.UUID, reason string, before, after any, now time.Time) error {
	beforeJSON, _ := json.Marshal(before)
	afterJSON, _ := json.Marshal(after)
	_, err := tx.Exec(ctx, `INSERT INTO pricing_audits(id,price_version_id,action,actor_user_id,reason,before_snapshot,after_snapshot,occurred_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, uuid.Must(uuid.NewV7()), id, action, actor, nullableText(reason), nullableJSON(beforeJSON), afterJSON, now)
	return err
}
func nullableText(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
func nullableJSON(value []byte) any {
	if len(value) == 0 || string(value) == "null" {
		return nil
	}
	return value
}

func (r *PostgresRepository) PublishPriceVersion(ctx context.Context, id, actor uuid.UUID, expectedETag int64, idempotencyKey string) (ManagedPriceVersion, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if id == uuid.Nil || actor == uuid.Nil || expectedETag < 1 || !validIdempotencyKey(idempotencyKey, 128) {
		return ManagedPriceVersion{}, ErrConflict
	}
	fingerprintBytes := sha256.Sum256([]byte(id.String() + "|" + strconv.FormatInt(expectedETag, 10)))
	fingerprint := hex.EncodeToString(fingerprintBytes[:])
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ManagedPriceVersion{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "pricing.publish.command", actor.String()+"|"+idempotencyKey); err != nil {
		return ManagedPriceVersion{}, err
	}
	var savedFingerprint string
	var saved []byte
	err = tx.QueryRow(ctx, `SELECT request_fingerprint,response_snapshot FROM pricing_publish_commands WHERE actor_user_id=$1 AND idempotency_key=$2`, actor, idempotencyKey).Scan(&savedFingerprint, &saved)
	if err == nil {
		if savedFingerprint != fingerprint {
			return ManagedPriceVersion{}, ErrIdempotencyConflict
		}
		var item ManagedPriceVersion
		if json.Unmarshal(saved, &item) != nil {
			return ManagedPriceVersion{}, ErrConflict
		}
		return item, tx.Commit(ctx)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return ManagedPriceVersion{}, err
	}
	currentETag, err := lockManagedPriceSpace(ctx, tx, id, r.now())
	if err != nil {
		return ManagedPriceVersion{}, err
	}
	item, scopeKey, err := loadManagedPriceForUpdate(ctx, tx, id)
	if err != nil {
		return ManagedPriceVersion{}, err
	}
	if item.LifecycleStatus != "DRAFT" || currentETag != expectedETag {
		return ManagedPriceVersion{}, ErrPriceETagConflict
	}
	var overlap bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pricing_versions WHERE id<>$1 AND scope_type=$2 AND COALESCE(scope_id,'')=$3 AND protocol_code=$4 AND formula_version_id=$5 AND credit_unit=$6 AND lifecycle_status IN ('PUBLISHED','ACTIVE') AND cancelled_at IS NULL AND effective_from<COALESCE($8::timestamptz,'infinity'::timestamptz) AND COALESCE(effective_to,'infinity')>$7)`, id, item.ScopeType, nullableScopeKey(item.ScopeType, scopeKey), item.ProtocolCode, item.FormulaVersionID, item.CreditUnit, item.EffectiveFrom, item.EffectiveTo).Scan(&overlap); err != nil {
		return ManagedPriceVersion{}, err
	}
	if overlap {
		return ManagedPriceVersion{}, ErrPriceWindowConflict
	}
	now := r.now()
	newETag := expectedETag + 1
	if _, err := tx.Exec(ctx, `UPDATE pricing_versions SET lifecycle_status='PUBLISHED',published_at=$2,publish_idempotency_key=$3,request_fingerprint=$4,space_etag=$5 WHERE id=$1`, id, now, idempotencyKey, fingerprint, newETag); err != nil {
		return ManagedPriceVersion{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE pricing_space_versions SET etag=$6,updated_at=$7 WHERE scope_type=$1 AND scope_key=$2 AND protocol_code=$3 AND formula_version_id=$4 AND credit_unit=$5`, item.ScopeType, scopeKey, item.ProtocolCode, item.FormulaVersionID, item.CreditUnit, newETag, now); err != nil {
		return ManagedPriceVersion{}, err
	}
	item.LifecycleStatus = "PUBLISHED"
	item.DerivedStatus = derivePriceStatus(item, now)
	item.SpaceETag = newETag
	item.CurrentSpaceETag = newETag
	item.PublishedAt = &now
	response, _ := json.Marshal(item)
	if _, err := tx.Exec(ctx, `INSERT INTO pricing_publish_commands(actor_user_id,idempotency_key,request_fingerprint,price_version_id,response_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6)`, actor, idempotencyKey, fingerprint, id, response, now); err != nil {
		return ManagedPriceVersion{}, err
	}
	if err := appendPricingAudit(ctx, tx, id, "PUBLISHED", actor, "", nil, item, now); err != nil {
		return ManagedPriceVersion{}, err
	}
	return item, tx.Commit(ctx)
}

func loadManagedPriceForUpdate(ctx context.Context, tx pgx.Tx, id uuid.UUID) (ManagedPriceVersion, string, error) {
	return loadManagedPrice(ctx, tx, id, true)
}

func loadManagedPrice(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (ManagedPriceVersion, string, error) {
	var item ManagedPriceVersion
	query := `SELECT id,scope_type,scope_id,protocol_code,formula_version_id,credit_unit,lifecycle_status,version,space_etag,effective_from,effective_to,published_at FROM pricing_versions WHERE id=$1`
	if forUpdate {
		query += ` FOR UPDATE`
	}
	err := tx.QueryRow(ctx, query, id).Scan(&item.PriceVersionID, &item.ScopeType, &item.ScopeID, &item.ProtocolCode, &item.FormulaVersionID, &item.CreditUnit, &item.LifecycleStatus, &item.Version, &item.SpaceETag, &item.EffectiveFrom, &item.EffectiveTo, &item.PublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return item, "", ErrNotFound
	}
	if err != nil {
		return item, "", err
	}
	scopeKey := "*"
	if item.ScopeID != nil {
		scopeKey = *item.ScopeID
	}
	rows, err := tx.Query(ctx, `SELECT unit_definition_id,config_type,fixed_price_per_denominator FROM pricing_version_unit_configs WHERE price_version_id=$1 ORDER BY unit_definition_id`, id)
	if err != nil {
		return item, "", err
	}
	defer rows.Close()
	for rows.Next() {
		var unit PriceUnitConfiguration
		if err := rows.Scan(&unit.UnitDefinitionID, &unit.ConfigType, &unit.FixedPricePerDenominator); err != nil {
			return item, "", err
		}
		item.Units = append(item.Units, unit)
	}
	return item, scopeKey, rows.Err()
}

func derivePriceStatus(item ManagedPriceVersion, now time.Time) string {
	return derivePriceLifecycleStatus(item.LifecycleStatus, item.EffectiveFrom, item.EffectiveTo, now)
}

func derivePriceLifecycleStatus(lifecycleStatus string, effectiveFrom time.Time, effectiveTo *time.Time, now time.Time) string {
	if lifecycleStatus == "CANCELLED" || lifecycleStatus == "ENDED" || lifecycleStatus == "DRAFT" {
		return lifecycleStatus
	}
	if now.Before(effectiveFrom) {
		return "PUBLISHED"
	}
	if effectiveTo != nil && !now.Before(*effectiveTo) {
		return "ENDED"
	}
	return "ACTIVE"
}

func (r *PostgresRepository) TransitionPriceVersion(ctx context.Context, id, actor uuid.UUID, action, mode, reason string, replacement *uuid.UUID, idempotencyKey string) (ManagedPriceVersion, error) {
	action, mode, reason = strings.ToUpper(strings.TrimSpace(action)), strings.ToUpper(strings.TrimSpace(mode)), strings.TrimSpace(reason)
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if id == uuid.Nil || actor == uuid.Nil || reason == "" || !validIdempotencyKey(idempotencyKey, 128) {
		return ManagedPriceVersion{}, ErrConflict
	}
	fingerprintInput, _ := json.Marshal(map[string]any{"price_version_id": id, "action": action, "mode": mode, "reason": reason, "replacement_price_version_id": replacement})
	fingerprintBytes := sha256.Sum256(fingerprintInput)
	fingerprint := hex.EncodeToString(fingerprintBytes[:])
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return ManagedPriceVersion{}, err
	}
	defer tx.Rollback(ctx)
	if err := lockTransactionKey(ctx, tx, "pricing.lifecycle.command", actor.String()+"|"+idempotencyKey); err != nil {
		return ManagedPriceVersion{}, err
	}
	var savedFingerprint string
	var saved []byte
	err = tx.QueryRow(ctx, `SELECT request_fingerprint,response_snapshot FROM pricing_lifecycle_commands WHERE actor_user_id=$1 AND idempotency_key=$2`, actor, idempotencyKey).Scan(&savedFingerprint, &saved)
	if err == nil {
		if savedFingerprint != fingerprint {
			return ManagedPriceVersion{}, ErrIdempotencyConflict
		}
		var replay ManagedPriceVersion
		if json.Unmarshal(saved, &replay) != nil {
			return ManagedPriceVersion{}, ErrConflict
		}
		return replay, tx.Commit(ctx)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return ManagedPriceVersion{}, err
	}
	currentETag, err := lockManagedPriceSpace(ctx, tx, id, r.now())
	if err != nil {
		return ManagedPriceVersion{}, err
	}
	item, scopeKey, err := loadManagedPriceForUpdate(ctx, tx, id)
	if err != nil {
		return ManagedPriceVersion{}, err
	}
	now := r.now()
	item.SpaceETag = currentETag
	switch action {
	case "CANCEL":
		if item.LifecycleStatus != "PUBLISHED" || !now.Before(item.EffectiveFrom) {
			return ManagedPriceVersion{}, ErrConflict
		}
		_, err = tx.Exec(ctx, `UPDATE pricing_versions SET lifecycle_status='CANCELLED',cancelled_at=$2,lifecycle_reason=$3 WHERE id=$1`, id, now, reason)
		item.LifecycleStatus = "CANCELLED"
	case "END":
		if derivePriceStatus(item, now) != "ACTIVE" || !oneOf(mode, "FALLBACK_GLOBAL", "BLOCK", "CONTINUATION") {
			return ManagedPriceVersion{}, ErrConflict
		}
		if item.ScopeType != "MODEL" && (mode == "FALLBACK_GLOBAL" || mode == "BLOCK") {
			return ManagedPriceVersion{}, ErrConflict
		}
		if mode == "CONTINUATION" {
			if replacement == nil || *replacement == id {
				return ManagedPriceVersion{}, ErrConflict
			}
			successor, successorScope, loadErr := loadManagedPrice(ctx, tx, *replacement, false)
			if loadErr != nil {
				return ManagedPriceVersion{}, loadErr
			}
			if successor.LifecycleStatus != "DRAFT" || successor.ScopeType != item.ScopeType || successorScope != scopeKey || successor.ProtocolCode != item.ProtocolCode || successor.FormulaVersionID != item.FormulaVersionID || successor.CreditUnit != item.CreditUnit {
				return ManagedPriceVersion{}, ErrConflict
			}
			// Check immutable scope before locking the successor row. Both rows
			// then belong to the namespace already held, preventing reverse locks.
			successor, _, loadErr = loadManagedPriceForUpdate(ctx, tx, *replacement)
			if loadErr != nil {
				return ManagedPriceVersion{}, loadErr
			}
			if successor.EffectiveTo != nil && !successor.EffectiveTo.After(now) {
				return ManagedPriceVersion{}, ErrConflict
			}
			var overlap bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pricing_versions WHERE id<>ALL($1::uuid[]) AND scope_type=$2 AND COALESCE(scope_id,'')=$3 AND protocol_code=$4 AND formula_version_id=$5 AND credit_unit=$6 AND lifecycle_status IN ('PUBLISHED','ACTIVE') AND cancelled_at IS NULL AND effective_from<COALESCE($8::timestamptz,'infinity'::timestamptz) AND COALESCE(effective_to,'infinity')>$7)`, []uuid.UUID{id, *replacement}, item.ScopeType, nullableScopeKey(item.ScopeType, scopeKey), item.ProtocolCode, item.FormulaVersionID, item.CreditUnit, now, successor.EffectiveTo).Scan(&overlap); err != nil {
				return ManagedPriceVersion{}, err
			}
			if overlap {
				return ManagedPriceVersion{}, ErrPriceWindowConflict
			}
			if _, err = tx.Exec(ctx, `UPDATE pricing_versions SET effective_from=$2,lifecycle_status='PUBLISHED',published_at=$2 WHERE id=$1`, *replacement, now); err == nil {
				item.SuccessorPriceVersionID = replacement
			}
			if err != nil {
				return ManagedPriceVersion{}, err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE pricing_versions SET lifecycle_status='ENDED',ended_at=$2,effective_to=$2,lifecycle_reason=$3 WHERE id=$1`, id, now, reason)
		item.LifecycleStatus = "ENDED"
		item.EffectiveTo = &now
		if err == nil && mode == "BLOCK" && item.ScopeType == "MODEL" {
			_, err = tx.Exec(ctx, `INSERT INTO pricing_blocks(id,model_id,protocol_code,formula_version_id,credit_unit,effective_from,reason,created_by,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$6)`, uuid.Must(uuid.NewV7()), scopeKey, item.ProtocolCode, item.FormulaVersionID, item.CreditUnit, now, reason, actor)
		}
		if err == nil && mode == "FALLBACK_GLOBAL" {
			if _, err := lockPricingSpace(ctx, tx, "GLOBAL", "*", item.ProtocolCode, item.FormulaVersionID, item.CreditUnit, now); err != nil {
				return ManagedPriceVersion{}, err
			}
			var required []uuid.UUID
			if err = tx.QueryRow(ctx, `SELECT required_unit_definition_ids FROM billing_protocol_formula_bindings WHERE formula_version_id=$1 AND protocol_code=$2`, item.FormulaVersionID, item.ProtocolCode).Scan(&required); err == nil {
				global, found, resolveErr := selectUniquePriceCandidate(ctx, tx, "GLOBAL", "", item.ProtocolCode, item.FormulaVersionID, item.CreditUnit, now)
				if resolveErr != nil || !found {
					if resolveErr != nil {
						return ManagedPriceVersion{}, resolveErr
					}
					return ManagedPriceVersion{}, ErrPriceMissing
				}
				globalConfigs, loadErr := loadUnitConfigurations(ctx, tx, global.id)
				if loadErr != nil {
					return ManagedPriceVersion{}, loadErr
				}
				for _, unitID := range required {
					config, ok := globalConfigs[unitID]
					if !ok || config.configType != "FIXED_PRICE" || config.fixedPrice == nil {
						return ManagedPriceVersion{}, ErrPriceMissing
					}
				}
				successorID := uuid.Must(uuid.NewV7())
				var version int64
				err = tx.QueryRow(ctx, `SELECT COALESCE(max(version),0)+1 FROM pricing_versions WHERE scope_type='MODEL' AND scope_id=$1 AND protocol_code=$2 AND formula_version_id=$3 AND credit_unit=$4`, scopeKey, item.ProtocolCode, item.FormulaVersionID, item.CreditUnit).Scan(&version)
				var successorEnd *time.Time
				if err == nil {
					nextErr := tx.QueryRow(ctx, `SELECT min(effective_from) FROM pricing_versions WHERE id<>$1 AND scope_type='MODEL' AND scope_id=$2 AND protocol_code=$3 AND formula_version_id=$4 AND credit_unit=$5 AND lifecycle_status IN ('PUBLISHED','ACTIVE') AND cancelled_at IS NULL AND effective_from>$6`, id, scopeKey, item.ProtocolCode, item.FormulaVersionID, item.CreditUnit, now).Scan(&successorEnd)
					if nextErr != nil {
						err = nextErr
					}
				}
				if err == nil {
					_, err = tx.Exec(ctx, `INSERT INTO pricing_versions(id,model,input_per_million,output_per_million,cached_input_per_million,effective_from,effective_to,created_at,scope_type,scope_id,protocol_code,formula_version_id,credit_unit,lifecycle_status,version,space_etag,created_by,lifecycle_reason) VALUES($1,$2,0,0,0,$3,$4,$3,'MODEL',$2,$5,$6,$7,'DRAFT',$8,$9,$10,$11)`, successorID, scopeKey, now, successorEnd, item.ProtocolCode, item.FormulaVersionID, item.CreditUnit, version, item.SpaceETag+1, actor, reason)
				}
				for _, unitID := range required {
					if err != nil {
						break
					}
					_, err = tx.Exec(ctx, `INSERT INTO pricing_version_unit_configs(price_version_id,unit_definition_id,config_type,created_at) VALUES($1,$2,'INHERIT_GLOBAL',$3)`, successorID, unitID, now)
				}
				if err == nil {
					_, err = tx.Exec(ctx, `UPDATE pricing_versions SET lifecycle_status='PUBLISHED',published_at=$2 WHERE id=$1`, successorID, now)
				}
				if err == nil {
					item.SuccessorPriceVersionID = &successorID
				}
			}
		}
	default:
		return ManagedPriceVersion{}, ErrConflict
	}
	if err != nil {
		return ManagedPriceVersion{}, err
	}
	item.DerivedStatus = item.LifecycleStatus
	item.SpaceETag = currentETag + 1
	item.CurrentSpaceETag = currentETag + 1
	if err == nil {
		_, err = tx.Exec(ctx, `UPDATE pricing_space_versions SET etag=etag+1,updated_at=$6 WHERE scope_type=$1 AND scope_key=$2 AND protocol_code=$3 AND formula_version_id=$4 AND credit_unit=$5`, item.ScopeType, scopeKey, item.ProtocolCode, item.FormulaVersionID, item.CreditUnit, now)
	}
	if err != nil {
		return ManagedPriceVersion{}, err
	}
	if err := appendPricingAudit(ctx, tx, id, action, actor, reason, nil, item, now); err != nil {
		return ManagedPriceVersion{}, err
	}
	response, _ := json.Marshal(item)
	if _, err := tx.Exec(ctx, `INSERT INTO pricing_lifecycle_commands(actor_user_id,idempotency_key,request_fingerprint,price_version_id,response_snapshot,created_at) VALUES($1,$2,$3,$4,$5,$6)`, actor, idempotencyKey, fingerprint, id, response, now); err != nil {
		return ManagedPriceVersion{}, err
	}
	return item, tx.Commit(ctx)
}

func (s *Service) adminMeteringUnits(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(interface {
		ListMeteringUnits(context.Context) ([]MeteringUnitDefinition, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "metering administration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	items, err := repository.ListMeteringUnits(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminManagedPrices(w http.ResponseWriter, r *http.Request) {
	if rawID := strings.TrimSpace(r.URL.Query().Get("price_version_id")); rawID != "" {
		repository, ok := s.repository.(interface {
			ReadManagedPrice(context.Context, uuid.UUID) (ManagedPriceVersion, error)
		})
		if !ok {
			api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "pricing administration is unavailable", httpserver.RequestID(r.Context()))
			return
		}
		id, err := uuid.Parse(rawID)
		if err != nil {
			s.writeError(w, r, ErrConflict)
			return
		}
		item, err := repository.ReadManagedPrice(r.Context(), id)
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		api.WriteJSON(w, http.StatusOK, map[string]any{"item": managedPriceResponse(item)})
		return
	}
	if rawScope := strings.TrimSpace(r.URL.Query().Get("scope_type")); rawScope != "" {
		repository, ok := s.repository.(interface {
			ReadManagedPriceSpace(context.Context, ManagedPriceSpaceQuery, int) (int64, []ManagedPriceVersion, bool, error)
		})
		if !ok {
			api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "pricing administration is unavailable", httpserver.RequestID(r.Context()))
			return
		}
		var scopeID *string
		if raw := r.URL.Query().Get("scope_id"); raw != "" {
			scopeID = &raw
		}
		formulaID, err := uuid.Parse(r.URL.Query().Get("formula_version_id"))
		if err != nil {
			s.writeError(w, r, ErrConflict)
			return
		}
		etag, items, hasMore, err := repository.ReadManagedPriceSpace(r.Context(), ManagedPriceSpaceQuery{ScopeType: rawScope, ScopeID: scopeID, ProtocolCode: r.URL.Query().Get("protocol_code"), FormulaVersionID: formulaID, CreditUnit: r.URL.Query().Get("credit_unit")}, billingAdminLimit(r))
		if err != nil {
			s.writeError(w, r, err)
			return
		}
		api.WriteJSON(w, http.StatusOK, map[string]any{"current_space_etag": etag, "items": managedPriceResponses(items), "has_more": hasMore})
		return
	}
	repository, ok := s.repository.(interface {
		ListManagedPrices(context.Context, int) ([]ManagedPriceVersion, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "pricing administration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	items, err := repository.ListManagedPrices(r.Context(), billingAdminLimit(r))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": managedPriceResponses(items)})
}

func (s *Service) adminUpdatePriceDraft(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(interface {
		UpdatePriceDraft(context.Context, uuid.UUID, uuid.UUID, PriceDraftCommand) (ManagedPriceVersion, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "pricing administration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	id, err := uuid.Parse(r.PathValue("price_version_id"))
	var body PriceDraftCommand
	if err != nil || api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid pricing request", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.UpdatePriceDraft(r.Context(), id, adminActor(adminPrincipal(r)), body)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, managedPriceResponse(item))
}

func (s *Service) adminPreviewPrice(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(interface {
		PreviewPriceVersion(context.Context, uuid.UUID) (PriceVersion, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "pricing administration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	id, err := uuid.Parse(r.PathValue("price_version_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid price version", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.PreviewPriceVersion(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, resolvedPriceResponse(item))
}
func (s *Service) adminCreateMeteringUnit(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(interface {
		CreateMeteringUnitVersion(context.Context, MeteringUnitDefinition, uuid.UUID, string) (MeteringUnitDefinition, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "pricing administration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var body MeteringUnitDefinition
	if api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid pricing request", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.CreateMeteringUnitVersion(r.Context(), body, adminActor(adminPrincipal(r)), r.Header.Get("Idempotency-Key"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, item)
}
func (s *Service) adminDeactivateMeteringUnit(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(interface {
		DeactivateMeteringUnit(context.Context, uuid.UUID) (MeteringUnitDefinition, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "pricing administration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	id, err := uuid.Parse(r.PathValue("unit_definition_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid unit definition", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.DeactivateMeteringUnit(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, item)
}
func (s *Service) adminCreatePriceDraftV2(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(interface {
		CreatePriceDraft(context.Context, PriceDraftCommand, uuid.UUID, string) (ManagedPriceVersion, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "pricing administration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var body PriceDraftCommand
	if api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid pricing request", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.CreatePriceDraft(r.Context(), body, adminActor(adminPrincipal(r)), r.Header.Get("Idempotency-Key"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusCreated, managedPriceResponse(item))
}
func (s *Service) adminPublishPrice(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(interface {
		PublishPriceVersion(context.Context, uuid.UUID, uuid.UUID, int64, string) (ManagedPriceVersion, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "pricing administration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	id, err := uuid.Parse(r.PathValue("price_version_id"))
	var body struct {
		ExpectedETag int64 `json:"expected_etag"`
	}
	if err != nil || api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid pricing request", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.PublishPriceVersion(r.Context(), id, adminActor(adminPrincipal(r)), body.ExpectedETag, r.Header.Get("Idempotency-Key"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, managedPriceResponse(item))
}
func (s *Service) adminTransitionPrice(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(interface {
		TransitionPriceVersion(context.Context, uuid.UUID, uuid.UUID, string, string, string, *uuid.UUID, string) (ManagedPriceVersion, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "pricing administration is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	id, err := uuid.Parse(r.PathValue("price_version_id"))
	var body struct {
		Action                    string     `json:"action"`
		Mode                      string     `json:"mode"`
		Reason                    string     `json:"reason"`
		ReplacementPriceVersionID *uuid.UUID `json:"replacement_price_version_id"`
	}
	if err != nil || api.DecodeJSON(w, r, &body) != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid pricing request", httpserver.RequestID(r.Context()))
		return
	}
	item, err := repository.TransitionPriceVersion(r.Context(), id, adminActor(adminPrincipal(r)), body.Action, body.Mode, body.Reason, body.ReplacementPriceVersionID, r.Header.Get("Idempotency-Key"))
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, managedPriceResponse(item))
}
