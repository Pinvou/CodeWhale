package token

import (
	"encoding/json"
	"math"
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

func TestUnitCostUsesCanonicalQuantityWithVersionedPrecisionMetadata(t *testing.T) {
	for _, test := range []struct {
		name                                    string
		quantity, precision, price, denominator int64
		rounding                                string
		expected                                int64
		rejected                                bool
	}{
		{"precision-is-not-price-scaling", 100, 100, 1000, 100, "CEILING", 1000, false},
		{"large-precision-small-quantity", 128000, math.MaxInt64, 1, 1, "CEILING", 128000, false},
		{"ceiling", 3, 1, 1, 2, "CEILING", 2, false},
		{"floor", 3, 1, 1, 2, "FLOOR", 1, false},
		{"half-up", 3, 1, 1, 2, "HALF_UP", 2, false},
		{"zero-free", math.MaxInt64, 1, 0, 1, "CEILING", 0, false},
		{"multiply-overflow", math.MaxInt64, 1, 2, 1, "CEILING", 0, true},
		{"ceiling-int64-boundary", math.MaxInt64, 1, 1, 2, "CEILING", math.MaxInt64/2 + 1, false},
		{"negative-quantity", -1, 1, 1, 1, "CEILING", 0, true},
		{"missing-precision", 1, 0, 1, 1, "CEILING", 0, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			cost, err := unitCost(test.quantity, ResolvedUnitPrice{MinimumPrecision: test.precision, ResolvedPrice: test.price, PriceDenominator: test.denominator, RoundingRule: test.rounding})
			if (err != nil) != test.rejected || err == nil && cost != test.expected {
				t.Fatalf("cost=%d err=%v want=%d rejected=%v", cost, err, test.expected, test.rejected)
			}
		})
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
	price := versionedTestPrice(PriceVersion{ID: uuid.New()})
	if err := ValidatePriceFormula(price); err != nil {
		t.Fatal(err)
	}
	price.FormulaSnapshot = json.RawMessage(`{"evaluator_version":"JAVASCRIPT","expression":{}}`)
	if err := ValidatePriceFormula(price); err == nil {
		t.Fatal("unsafe evaluator was accepted")
	}
}
