package billing

import (
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"math"
	"testing"
)

func TestUsageRepairFrozenPricing(t *testing.T) {
	id, formula := uuid.New(), uuid.New()
	price := PriceVersion{ID: id, Model: "chat-frozen", ProtocolCode: "CHAT", FormulaVersionID: formula, CreditUnit: "MICROCREDIT", InputPerMillion: 1_000_000, OutputPerMillion: 2_000_000, CachedInputPerMillion: 500_000}
	usage := []byte(`{"input_tokens":7,"output_tokens":3,"cached_input_tokens":2}`)
	for _, c := range []struct {
		name   string
		change func(*PriceVersion, *[]byte)
		allow  bool
	}{
		{"legacy", func(*PriceVersion, *[]byte) {}, true},
		{"foreign-price", func(p *PriceVersion, _ *[]byte) { p.ID = uuid.New() }, false},
		{"foreign-model", func(p *PriceVersion, _ *[]byte) { p.Model = "another" }, false},
		{"foreign-protocol", func(p *PriceVersion, _ *[]byte) { p.ProtocolCode = "IMAGE" }, false},
		{"foreign-formula", func(p *PriceVersion, _ *[]byte) { p.FormulaVersionID = uuid.New() }, false},
		{"negative", func(_ *PriceVersion, u *[]byte) {
			*u = []byte(`{"input_tokens":-1,"output_tokens":3,"cached_input_tokens":2}`)
		}, false},
		{"overflow", func(p *PriceVersion, _ *[]byte) { p.InputPerMillion = math.MaxInt64 }, false},
		{"missing-usage", func(_ *PriceVersion, u *[]byte) { *u = []byte(`{"input_tokens":7,"output_tokens":3}`) }, false},
		{"fraction-usage", func(_ *PriceVersion, u *[]byte) {
			*u = []byte(`{"input_tokens":7.1,"output_tokens":3,"cached_input_tokens":2}`)
		}, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := price
			u := usage
			c.change(&p, &u)
			raw, _ := json.Marshal(p)
			got, err := usageRepairFrozenPricing(&id, price.Model, price.ProtocolCode, &formula, raw, u)
			if (err == nil) != c.allow {
				t.Fatalf("allow=%v got=%d err=%v", c.allow, got, err)
			}
			if c.allow && got != 14 {
				t.Fatalf("cost=%d", got)
			}
		})
	}
	raw, _ := json.Marshal(price)
	if _, err := usageRepairFrozenPricing(nil, price.Model, price.ProtocolCode, &formula, raw, usage); !errors.Is(err, ErrApprovalSnapshotMismatch) {
		t.Fatalf("missing hold price accepted: %v", err)
	}
	var keys map[string]json.RawMessage
	json.Unmarshal(raw, &keys)
	delete(keys, "cached_input_per_million")
	missing, _ := json.Marshal(keys)
	if _, err := usageRepairFrozenPricing(&id, price.Model, price.ProtocolCode, &formula, missing, usage); err == nil {
		t.Fatal("missing legacy rate accepted")
	}
	if _, err := usageRepairFrozenPricing(&id, price.Model, price.ProtocolCode, &formula, raw, []byte(`{"input_tokens":null,"output_tokens":3,"cached_input_tokens":2}`)); err == nil {
		t.Fatal("null usage accepted")
	}

}

func TestUsageRepairResolvedUnitCost(t *testing.T) {
	p := PriceVersion{Units: []ResolvedUnitPrice{
		{UnitCode: "INPUT_TOKEN", MinimumPrecision: 2, PriceDenominator: 3, ResolvedPrice: 4, RoundingRule: "CEILING"},
		{UnitCode: "CACHED_INPUT_TOKEN", MinimumPrecision: 1, PriceDenominator: 3, ResolvedPrice: 1, RoundingRule: "FLOOR"},
		{UnitCode: "OUTPUT_TOKEN", MinimumPrecision: 1, PriceDenominator: 2, ResolvedPrice: 1, RoundingRule: "HALF_UP"},
	}}
	u := usageRepairUsage{InputTokens: 3, CachedInputTokens: 2, OutputTokens: 3}
	for _, c := range []struct {
		name   string
		change func(*PriceVersion)
		allow  bool
	}{
		{"all-rounding", func(*PriceVersion) {}, true},
		{"incomplete", func(p *PriceVersion) { p.Units = p.Units[:2] }, false},
		{"zero-denominator", func(p *PriceVersion) { p.Units[0].PriceDenominator = 0 }, false},
		{"bad-rounding", func(p *PriceVersion) { p.Units[0].RoundingRule = "OTHER" }, false},
		{"duplicate-unit", func(p *PriceVersion) { p.Units[1].UnitCode = "INPUT_TOKEN" }, false},
		{"negative-price", func(p *PriceVersion) { p.Units[0].ResolvedPrice = -1 }, false},
		{"overflow", func(p *PriceVersion) { p.Units[0].ResolvedPrice = math.MaxInt64 }, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			q := p
			q.Units = append([]ResolvedUnitPrice(nil), p.Units...)
			c.change(&q)
			got, err := usageRepairCost(q, u)
			if (err == nil) != c.allow {
				t.Fatalf("allow=%v cost=%d err=%v", c.allow, got, err)
			}
			if c.allow && got != 5 {
				t.Fatalf("cost=%d", got)
			}
		})
	}
}
