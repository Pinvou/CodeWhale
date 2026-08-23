package token

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestEstimateChatUsageUsesVersionedUTF8ByteRuleAndMissingOutputIsZero(t *testing.T) {
	usage, err := EstimateChatUsage([]byte(`{"messages":[{"content":"hello"},{"content":[{"type":"text","text":"世界"}]}]}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	if usage != (Usage{InputTokens: 3}) {
		t.Fatalf("usage = %#v, want three estimated exclusive input tokens", usage)
	}
}

func TestVersionedUnitCostUsesExactDenominatorAndRounding(t *testing.T) {
	price := PriceVersion{Units: []ResolvedUnitPrice{
		{UnitCode: "INPUT_TOKEN", MinimumPrecision: 1, PriceDenominator: 10, RoundingRule: "CEILING", ResolvedPrice: 3},
		{UnitCode: "CACHED_INPUT_TOKEN", MinimumPrecision: 1, PriceDenominator: 10, RoundingRule: "CEILING", ResolvedPrice: 1},
		{UnitCode: "OUTPUT_TOKEN", MinimumPrecision: 1, PriceDenominator: 10, RoundingRule: "CEILING", ResolvedPrice: 8},
	}}
	cost, err := Cost(price, Usage{InputTokens: 11, CachedInputTokens: 4, OutputTokens: 2})
	if err != nil {
		t.Fatal(err)
	}
	if cost != 7 {
		t.Fatalf("cost = %d, want 7", cost)
	}
}

func TestValidatePriceFormulaRejectsMissingOrUnsafeConfiguration(t *testing.T) {
	price := PriceVersion{ID: uuid.New(), ProtocolCode: "CHAT", FormulaVersionID: uuid.New(), CreditUnit: "MICROCREDIT", FormulaSnapshot: json.RawMessage(`{"evaluator_version":"SAFE_PARAMETER_MAP_V1","expression":{"INPUT_TOKEN":"estimated_input_tokens","CACHED_INPUT_TOKEN":"cached_input_tokens","OUTPUT_TOKEN":"max_output_tokens"}}`), Units: []ResolvedUnitPrice{{UnitCode: "INPUT_TOKEN"}, {UnitCode: "CACHED_INPUT_TOKEN"}, {UnitCode: "OUTPUT_TOKEN"}}}
	if err := ValidatePriceFormula(price); err != nil {
		t.Fatal(err)
	}
	price.FormulaSnapshot = json.RawMessage(`{"evaluator_version":"JAVASCRIPT","expression":{}}`)
	if err := ValidatePriceFormula(price); err == nil {
		t.Fatal("unsafe evaluator was accepted")
	}
}
