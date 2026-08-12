package token

import (
	"testing"
)

func TestCostUsesIntegerCeilingAndCachedRate(t *testing.T) {
	price := PriceVersion{InputPerMillion: 1_000_000, OutputPerMillion: 2_000_000, CachedInputPerMillion: 100_000}
	cost, err := Cost(price, Usage{InputTokens: 3, CachedInputTokens: 1, OutputTokens: 2})
	if err != nil {
		t.Fatal(err)
	}
	if cost != 7 {
		t.Fatalf("Cost = %d, want 7", cost)
	}
}

func TestCostRejectsInvalidUsage(t *testing.T) {
	if _, err := Cost(PriceVersion{}, Usage{InputTokens: 1, CachedInputTokens: 2}); err == nil {
		t.Fatal("cached tokens greater than input were accepted")
	}
}
