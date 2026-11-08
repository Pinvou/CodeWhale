package billing

import (
	"math"
	"testing"
)

func TestSplitProductComponentsPreservesTotalAndTypes(t *testing.T) {
	components, err := splitProductComponents(20_000_000, 1_500, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(components) != 2 || components[0].kind != "BASE" || components[0].amount != 15_000_000 || components[1].kind != "BONUS" || components[1].amount != 5_000_000 {
		t.Fatalf("unexpected component split: %#v", components)
	}
	if components[0].amount+components[1].amount != 20_000_000 {
		t.Fatal("component split does not conserve the credited amount")
	}
}

func TestSplitProductComponentsAssignsRoundingRemainderToBonus(t *testing.T) {
	components, err := splitProductComponents(10, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if components[0].amount != 6 || components[1].amount != 4 {
		t.Fatalf("expected BASE=6 BONUS=4, got %#v", components)
	}
}

func TestSplitProductComponentsRejectsInvalidWeights(t *testing.T) {
	for _, test := range []struct{ total, base, bonus int64 }{
		{0, 1, 0}, {1, 0, 0}, {1, -1, 2}, {1, math.MaxInt64, 1},
	} {
		if _, err := splitProductComponents(test.total, test.base, test.bonus); err == nil {
			t.Fatalf("expected invalid split to fail: %#v", test)
		}
	}
}
