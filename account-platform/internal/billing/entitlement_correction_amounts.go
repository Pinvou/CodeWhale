package billing

import "math"

type entitlementCorrectionAmounts struct {
	SourceType    string
	TargetType    string
	Amount        int64
	ExpectedBase  int64
	ExpectedBonus int64
	CurrentBase   int64
	CurrentBonus  int64
}

// calculateEntitlementCorrectionAmounts derives the only permitted complete
// reclassification from the original order snapshot. It is not an eligibility
// decision: the authoritative Billing reader must first prove source and zero
// history, and the executor must recheck both inside its transaction.
func calculateEntitlementCorrectionAmounts(total, currentBase, currentBonus, snapshotBasePoints, snapshotBonusPoints int64) (entitlementCorrectionAmounts, error) {
	if currentBase < 0 || currentBonus < 0 || currentBase > math.MaxInt64-currentBonus ||
		currentBase+currentBonus != total {
		return entitlementCorrectionAmounts{}, ErrConflict
	}
	expectedBase, expectedBonus, err := productComponentAmounts(total, snapshotBasePoints, snapshotBonusPoints)
	if err != nil || (expectedBase == currentBase && expectedBonus == currentBonus) {
		return entitlementCorrectionAmounts{}, ErrConflict
	}
	plan := entitlementCorrectionAmounts{
		ExpectedBase: expectedBase, ExpectedBonus: expectedBonus,
		CurrentBase: currentBase, CurrentBonus: currentBonus,
	}
	if currentBase > expectedBase {
		plan.SourceType, plan.TargetType, plan.Amount = "BASE", "BONUS", currentBase-expectedBase
	} else {
		plan.SourceType, plan.TargetType, plan.Amount = "BONUS", "BASE", expectedBase-currentBase
	}
	if plan.Amount <= 0 ||
		(plan.SourceType == "BASE" && currentBase < plan.Amount) ||
		(plan.SourceType == "BONUS" && currentBonus < plan.Amount) {
		return entitlementCorrectionAmounts{}, ErrConflict
	}
	return plan, nil
}
