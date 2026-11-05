package approvalpolicy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestTokenModelFinancialFactsIgnoreOnlyValidObservationTime(t *testing.T) {
	base := `{"billing_price_revision":11,"price_set_fingerprint":"` + strings.Repeat("a", 64) + `","models_checked":2,"models_covered":2,"fully_covered":true,"billing_as_of":"2026-09-06T01:00:00Z"}`
	advanced := strings.Replace(base, "01:00:00Z", "02:00:00Z", 1)
	if !SameTokenModelFinancialFacts(json.RawMessage(base), json.RawMessage(advanced)) {
		t.Fatal("unchanged financial facts rejected because clock advanced")
	}
	if base == advanced {
		t.Fatal("test did not preserve distinct frozen observation evidence")
	}
	for name, raw := range map[string]string{
		"revision":              strings.Replace(base, `"billing_price_revision":11`, `"billing_price_revision":12`, 1),
		"price set":             strings.Replace(base, strings.Repeat("a", 64), strings.Repeat("b", 64), 1),
		"coverage":              strings.Replace(strings.Replace(base, `"models_covered":2`, `"models_covered":1`, 1), `"fully_covered":true`, `"fully_covered":false`, 1),
		"invalid date":          strings.Replace(base, "2026-09-06T01:00:00Z", "not-a-date", 1),
		"zero date":             strings.Replace(base, "2026-09-06T01:00:00Z", "0001-01-01T00:00:00Z", 1),
		"null date":             strings.Replace(base, `"2026-09-06T01:00:00Z"`, `null`, 1),
		"missing date":          strings.Replace(base, `,"billing_as_of":"2026-09-06T01:00:00Z"`, "", 1),
		"unknown":               strings.Replace(base, `{`, `{"extra":true,`, 1),
		"duplicate":             strings.Replace(base, `{`, `{"billing_price_revision":11,`, 1),
		"case alias":            strings.Replace(base, "billing_price_revision", "BILLING_PRICE_REVISION", 1),
		"fraction":              strings.Replace(base, `"billing_price_revision":11`, `"billing_price_revision":11.5`, 1),
		"null count":            strings.Replace(base, `"models_checked":2`, `"models_checked":null`, 1),
		"inconsistent coverage": strings.Replace(base, `"fully_covered":true`, `"fully_covered":false`, 1),
		"trailing value":        base + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			if SameTokenModelFinancialFacts(json.RawMessage(base), json.RawMessage(raw)) || SameTokenModelFinancialFacts(json.RawMessage(raw), json.RawMessage(base)) {
				t.Fatal("invalid or changed financial facts accepted")
			}
		})
	}
}
