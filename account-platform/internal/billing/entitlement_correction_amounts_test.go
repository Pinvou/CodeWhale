package billing

import (
	"errors"
	"math"
	"testing"
)

func TestEntitlementCorrectionAmountsUseOriginalPaymentSplit(t *testing.T) {
	tests := []struct {
		name, source, target                                                  string
		total, currentBase, currentBonus, snapshotBase, snapshotBonus, amount int64
	}{
		{"base-to-bonus", "BASE", "BONUS", 100, 100, 0, 80, 20, 20},
		{"bonus-to-base", "BONUS", "BASE", 100, 60, 40, 80, 20, 20},
		{"fractional-original-rule", "BASE", "BONUS", 10, 10, 0, 2, 1, 4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan, err := calculateEntitlementCorrectionAmounts(test.total, test.currentBase, test.currentBonus, test.snapshotBase, test.snapshotBonus)
			if err != nil || plan.SourceType != test.source || plan.TargetType != test.target || plan.Amount != test.amount ||
				plan.CurrentBase+plan.CurrentBonus != plan.ExpectedBase+plan.ExpectedBonus {
				t.Fatalf("plan=%+v err=%v", plan, err)
			}
		})
	}
}

func TestEntitlementCorrectionAmountsFailClosed(t *testing.T) {
	for _, input := range [][5]int64{
		{100, 80, 20, 80, 20}, // no-op
		{100, 80, 19, 80, 20}, // missing component balance
		{100, -1, 101, 80, 20},
		{100, 100, 0, 0, 0}, // missing source split
		{100, 100, 0, -1, 101},
		{math.MaxInt64, math.MaxInt64, 1, 1, 1}, // current-sum overflow
		{100, 100, 0, math.MaxInt64, 1},         // snapshot-weight overflow
	} {
		if _, err := calculateEntitlementCorrectionAmounts(input[0], input[1], input[2], input[3], input[4]); !errors.Is(err, ErrConflict) {
			t.Fatalf("invalid composition %v accepted: %v", input, err)
		}
	}
}
