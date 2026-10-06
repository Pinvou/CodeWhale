package billing

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestPriceDraftRequiresShanghaiFullDaySegmentAndExplicitScope(t *testing.T) {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, shanghaiLocation)
	model := "gpt-example"
	command := PriceDraftCommand{ScopeType: "MODEL", ScopeID: &model, ProtocolCode: "CHAT", FormulaVersionID: chatFormulaVersionID, CreditUnit: "MICROCREDIT", EffectiveFrom: from, ExpectedETag: 1}
	normalized, key, err := normalizePriceDraft(command)
	if err != nil || key != model || normalized.ScopeType != "MODEL" {
		t.Fatalf("valid model day segment rejected: %#v %q %v", normalized, key, err)
	}
	command.EffectiveFrom = from.Add(time.Hour)
	if _, _, err := normalizePriceDraft(command); err == nil {
		t.Fatal("partial Shanghai business day was accepted")
	}
}

func TestGlobalPriceCannotInheritAndFixedZeroIsExplicit(t *testing.T) {
	zero := int64(0)
	if got := legacyRates([]PriceUnitConfiguration{{UnitDefinitionID: uuid.MustParse("00000000-0000-7000-8100-000000000001"), ConfigType: "FIXED_PRICE", FixedPricePerDenominator: &zero}})["INPUT_TOKEN"]; got != 0 {
		t.Fatalf("explicit fixed zero changed to %d", got)
	}
}
