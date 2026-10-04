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

func TestMaximumCostBoundsCachedPricesAndSplitCeilings(t *testing.T) {
	for _, price := range []PriceVersion{{InputPerMillion: 1, CachedInputPerMillion: 1}, {InputPerMillion: 2, CachedInputPerMillion: 10}, {InputPerMillion: 1_000_000, CachedInputPerMillion: 2_000_000, OutputPerMillion: 1_000_000}, {InputPerMillion: 0, CachedInputPerMillion: 100_000}, {InputPerMillion: 100_001, CachedInputPerMillion: 99_999, OutputPerMillion: 20_000}} {
		bound, err := MaximumCost(price, 100, 3)
		if err != nil {
			t.Fatal(err)
		}
		for input := int64(0); input <= 100; input++ {
			for cached := int64(0); cached <= input; cached++ {
				actual, err := Cost(price, Usage{InputTokens: input, CachedInputTokens: cached, OutputTokens: 3})
				if err != nil || actual > bound {
					t.Fatalf("bound=%d actual=%d input=%d cached=%d price=%+v err=%v", bound, actual, input, cached, price, err)
				}
			}
		}
	}
}
