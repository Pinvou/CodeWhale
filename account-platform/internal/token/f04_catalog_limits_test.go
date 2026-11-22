package token

import (
	"errors"
	"math"
	"math/big"
	"reflect"
	"testing"
)

func TestF04CatalogRequestLimitBoundaries(t *testing.T) {
	cases := []struct {
		name                            string
		context, maximum, input, output int64
	}{
		{"ordinary-within", 131072, 4096, 1000, 4096},
		{"ordinary-context-exceeded", 131072, 4096, 127000, 4096},
		{"ordinary-output-exceeded", 131072, 4096, 1, 4097},
		{"negative-input", 100, 100, -1, 1},
		{"negative-output", 100, 100, 1, -1},
		{"zero-exact", 0, 0, 0, 0},
		{"context-max-exact", math.MaxInt64, 1, math.MaxInt64 - 1, 1},
		{"input-max-zero", math.MaxInt64, 1, math.MaxInt64, 0},
		{"overflow-one", math.MaxInt64, 1, math.MaxInt64, 1},
		{"overflow-both", math.MaxInt64, math.MaxInt64, math.MaxInt64, math.MaxInt64},
		{"overflow-small-context", 100, math.MaxInt64, math.MaxInt64, 2},
		{"output-max-exact", math.MaxInt64, math.MaxInt64, 0, math.MaxInt64},
		{"output-exceeds-context", 100, math.MaxInt64, 0, math.MaxInt64},
		{"negative-capability-context", math.MinInt64, 0, 0, 0},
		{"negative-capability-output", 100, -1, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := CatalogModel{ContextLength: tc.context, MaximumOutputTokens: tc.maximum}
			before := item.Snapshot()
			// Independent arbitrary-precision oracle for the existing mathematical
			// contract, rather than evaluating the overflowing int64 expression.
			total := new(big.Int).Add(big.NewInt(tc.input), big.NewInt(tc.output))
			reject := tc.input < 0 || tc.output < 0 || tc.output > tc.maximum || total.Cmp(big.NewInt(tc.context)) > 0
			err := item.ValidateRequestLimits(tc.input, tc.output)
			if (err != nil) != reject || (err != nil && !errors.Is(err, ErrModelUnavailable)) {
				t.Fatalf("input=%d output=%d context=%d maximum=%d: got %v; reject=%v", tc.input, tc.output, tc.context, tc.maximum, err, reject)
			}
			if !reflect.DeepEqual(item.Snapshot(), before) {
				t.Fatal("request validation changed frozen model capability")
			}
		})
	}
}
