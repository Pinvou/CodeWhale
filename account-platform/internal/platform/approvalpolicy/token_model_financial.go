package approvalpolicy

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"io"
	"strings"
	"time"
)

// SameTokenModelFinancialFacts compares the closed model-lifecycle/catalog
// financial projection. Observed time is validated but is not a business fact.
// Callers must retain the original approved snapshot for audit and idempotency;
// this comparison is only for a fresh authoritative revalidation.
func SameTokenModelFinancialFacts(left, right json.RawMessage) bool {
	a, ok := decodeTokenModelFinancialFacts(left)
	if !ok {
		return false
	}
	b, ok := decodeTokenModelFinancialFacts(right)
	return ok && a == b
}

type tokenModelFinancialFacts struct {
	Revision     int64
	PriceSet     string
	Checked      int64
	Covered      int64
	FullyCovered bool
}

func decodeTokenModelFinancialFacts(raw json.RawMessage) (tokenModelFinancialFacts, bool) {
	// Unlike ordinary struct decoding, reject duplicate or case-aliased keys as
	// well as missing/unknown fields before comparing an approval proof.
	keys := map[string]bool{"billing_price_revision": false, "price_set_fingerprint": false, "models_checked": false, "models_covered": false, "fully_covered": false, "billing_as_of": false}
	shape := json.NewDecoder(bytes.NewReader(raw))
	if token, err := shape.Token(); err != nil || token != json.Delim('{') {
		return tokenModelFinancialFacts{}, false
	}
	for shape.More() {
		token, err := shape.Token()
		key, ok := token.(string)
		seen, known := keys[key]
		if err != nil || !ok || !known || seen {
			return tokenModelFinancialFacts{}, false
		}
		keys[key] = true
		var value json.RawMessage
		if shape.Decode(&value) != nil {
			return tokenModelFinancialFacts{}, false
		}
	}
	if token, err := shape.Token(); err != nil || token != json.Delim('}') || shape.Decode(new(any)) != io.EOF {
		return tokenModelFinancialFacts{}, false
	}
	for _, seen := range keys {
		if !seen {
			return tokenModelFinancialFacts{}, false
		}
	}
	var wire struct {
		Revision     *int64     `json:"billing_price_revision"`
		PriceSet     *string    `json:"price_set_fingerprint"`
		Checked      *int64     `json:"models_checked"`
		Covered      *int64     `json:"models_covered"`
		FullyCovered *bool      `json:"fully_covered"`
		ObservedAt   *time.Time `json:"billing_as_of"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&wire) != nil || decoder.Decode(new(any)) != io.EOF || wire.Revision == nil || wire.PriceSet == nil || wire.Checked == nil || wire.Covered == nil || wire.FullyCovered == nil || wire.ObservedAt == nil || wire.ObservedAt.IsZero() {
		return tokenModelFinancialFacts{}, false
	}
	if *wire.Revision < 1 || *wire.Checked < 1 || *wire.Covered < 0 || *wire.Covered > *wire.Checked || *wire.FullyCovered != (*wire.Checked == *wire.Covered) || len(*wire.PriceSet) != 64 || *wire.PriceSet != strings.ToLower(*wire.PriceSet) {
		return tokenModelFinancialFacts{}, false
	}
	if _, err := hex.DecodeString(*wire.PriceSet); err != nil {
		return tokenModelFinancialFacts{}, false
	}
	return tokenModelFinancialFacts{*wire.Revision, *wire.PriceSet, *wire.Checked, *wire.Covered, *wire.FullyCovered}, true
}
