package token

import (
	"bytes"
	"encoding/json"
	"io"
	"math/big"
	"reflect"
	"strings"
)

// Command facts include full int64 versions. Decode the complete value without
// a float64 intermediate; typed destinations still enforce their Go bounds.
func decodeExactCommandJSON(raw json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return ErrTokenCommandInvalid
	}
	return nil
}

// Decimal/exponent spellings retain JSON numeric equivalence without creating
// 10^exponent-sized integers. Coefficients remain digit strings and only the
// exponent is adjusted, so adjacent int64 facts can never round together.
func commandNumberIdentity(number json.Number) (string, string) {
	text := string(number)
	negative := strings.HasPrefix(text, "-")
	text = strings.TrimPrefix(text, "-")
	parts := strings.FieldsFunc(text, func(r rune) bool { return r == 'e' || r == 'E' })
	exponent := new(big.Int)
	if len(parts) == 2 {
		exponent.SetString(parts[1], 10)
	}
	mantissa := parts[0]
	if dot := strings.IndexByte(mantissa, '.'); dot >= 0 {
		exponent.Sub(exponent, big.NewInt(int64(len(mantissa)-dot-1)))
		mantissa = mantissa[:dot] + mantissa[dot+1:]
	}
	digits := strings.TrimLeft(mantissa, "0")
	if digits == "" {
		return "0", "0"
	}
	trimmed := strings.TrimRight(digits, "0")
	exponent.Add(exponent, big.NewInt(int64(len(digits)-len(trimmed))))
	if negative {
		trimmed = "-" + trimmed
	}
	return trimmed, exponent.String()
}

func commandJSONValuesEqual(left, right any) bool {
	switch a := left.(type) {
	case json.Number:
		b, ok := right.(json.Number)
		if !ok {
			return false
		}
		ac, ae := commandNumberIdentity(a)
		bc, be := commandNumberIdentity(b)
		return ac == bc && ae == be
	case map[string]any:
		b, ok := right.(map[string]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for k, v := range a {
			other, present := b[k]
			if !present || !commandJSONValuesEqual(v, other) {
				return false
			}
		}
		return true
	case []any:
		b, ok := right.([]any)
		if !ok || len(a) != len(b) {
			return false
		}
		for i, v := range a {
			if !commandJSONValuesEqual(v, b[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(left, right)
	}
}
