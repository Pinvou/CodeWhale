package billing

import "testing"

func TestEffectiveComponentOriginal(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		original, transferred int64
		want                  int64
		invalid               bool
	}{
		{name: "unchanged", original: 100, want: 100},
		{name: "partial transfer", original: 100, transferred: 40, want: 60},
		{name: "complete transfer", original: 100, transferred: 100, want: 0},
		{name: "negative original", original: -1, invalid: true},
		{name: "negative transfer", original: 100, transferred: -1, invalid: true},
		{name: "over transfer", original: 100, transferred: 101, invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := effectiveComponentOriginal(tc.original, tc.transferred)
			if (err != nil) != tc.invalid || (!tc.invalid && got != tc.want) {
				t.Fatalf("effectiveComponentOriginal(%d, %d) = %d, %v; want %d, invalid=%t", tc.original, tc.transferred, got, err, tc.want, tc.invalid)
			}
		})
	}
}
