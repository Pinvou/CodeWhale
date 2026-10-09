package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"io"

	"net/http"
	"net/url"

	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type PriceVersion struct {
	ID                    uuid.UUID           `json:"price_version_id"`
	Model                 string              `json:"model"`
	InputPerMillion       int64               `json:"input_per_million"`
	OutputPerMillion      int64               `json:"output_per_million"`
	CachedInputPerMillion int64               `json:"cached_input_per_million"`
	ScopeType             string              `json:"scope_type"`
	ProtocolCode          string              `json:"protocol_code"`
	FormulaVersionID      uuid.UUID           `json:"formula_version_id"`
	FormulaSnapshot       json.RawMessage     `json:"formula_snapshot"`
	CreditUnit            string              `json:"credit_unit"`
	EffectiveFrom         time.Time           `json:"effective_from"`
	EffectiveTo           *time.Time          `json:"effective_to,omitempty"`
	ResolvedAt            time.Time           `json:"resolved_at"`
	Units                 []ResolvedUnitPrice `json:"units"`
}

type ResolvedUnitPrice struct {
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

type Usage struct {
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
}

type FormulaBindingClient interface {
	RegisterFormulaBinding(context.Context, uuid.UUID, string, []uuid.UUID, json.RawMessage) (FormulaBindingReceipt, error)
}

type FormulaBindingReceipt struct {
	FormulaVersionID                  uuid.UUID   `json:"formula_version_id"`
	Status                            string      `json:"status"`
	ProtocolCode                      string      `json:"protocol_code"`
	RequiredMeteringUnitDefinitionIDs []uuid.UUID `json:"required_metering_unit_definition_ids"`
	FormulaSnapshotSHA256             string      `json:"formula_snapshot_sha256"`
	PublishedAt                       time.Time   `json:"published_at"`
}

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

type FormulaDraftValidationClient interface {
	ValidateFormulaDraftUnits(context.Context, map[string]uuid.UUID) (FormulaDraftValidation, error)
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

type ImpactPriceQuery struct {
	ModelID      uuid.UUID `json:"model_id"`
	ProtocolCode string    `json:"protocol_code"`
	CreditUnit   string    `json:"credit_unit"`
}

type ImpactPriceCoverageResult struct {
	Items                 []ImpactPriceCoverage `json:"items"`
	AsOf                  time.Time             `json:"as_of"`
	Source                string                `json:"source"`
	FinancialFactRevision int64                 `json:"financial_fact_revision"`
	PriceSetFingerprint   string                `json:"price_set_fingerprint"`
}

type ImpactPriceCoverageClient interface {
	ImpactPriceCoverage(context.Context, []ImpactPriceQuery) (ImpactPriceCoverageResult, error)
}

func (c *HTTPBillingClient) Price(ctx context.Context, model string) (PriceVersion, error) {
	var price PriceVersion
	err := c.call(ctx, http.MethodGet, "/internal/v1/pricing/current?model="+url.QueryEscape(model), nil, &price)
	return price, err
}

func (c *HTTPBillingClient) PriceByID(ctx context.Context, priceID uuid.UUID) (PriceVersion, error) {
	var price PriceVersion
	err := c.call(ctx, http.MethodGet, "/internal/v1/pricing/"+priceID.String(), nil, &price)
	return price, err
}

func (c *HTTPBillingClient) ImpactPriceCoverage(ctx context.Context, items []ImpactPriceQuery) (ImpactPriceCoverageResult, error) {
	var result ImpactPriceCoverageResult
	err := c.callStrict(ctx, http.MethodPost, "/internal/v1/pricing/impact-coverage", map[string]any{"items": items}, &result)
	return result, err
}

func (c *HTTPBillingClient) callStrict(ctx context.Context, method, path string, input, output any) error {
	encoded, err := json.Marshal(input)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, method, c.baseURL.String()+path, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	if err := serviceauth.SetAuthorization(request, c.serviceToken); err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		var problem struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(data, &problem)
		return &BillingHTTPError{Status: response.StatusCode, Code: problem.Code}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(data) > 2<<20 {
		return errors.New("invalid bounded Billing response")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("unexpected trailing billing response")
		}
		return err
	}
	return nil
}

func (c *HTTPBillingClient) call(ctx context.Context, method, path string, input, output any) error {
	payload, err := c.raw(ctx, method, path, input, "")
	if err != nil {
		return err
	}
	if output == nil {
		return nil
	}
	return json.Unmarshal(payload, output)
}

func (c *HTTPBillingClient) RegisterFormulaBinding(ctx context.Context, formulaID uuid.UUID, protocol string, required []uuid.UUID, snapshot json.RawMessage) (FormulaBindingReceipt, error) {
	var receipt FormulaBindingReceipt
	err := c.call(ctx, http.MethodPut, "/internal/v1/pricing/formula-bindings/"+formulaID.String(), map[string]any{"protocol_code": protocol, "required_unit_definition_ids": required, "formula_snapshot": snapshot}, &receipt)
	return receipt, err
}

func (c *HTTPBillingClient) ValidateFormulaDraftUnits(ctx context.Context, required map[string]uuid.UUID) (FormulaDraftValidation, error) {
	var result FormulaDraftValidation
	err := c.call(ctx, http.MethodPost, "/internal/v1/pricing/formula-bindings/validate", map[string]any{"required_unit_versions": required}, &result)
	return result, err
}
