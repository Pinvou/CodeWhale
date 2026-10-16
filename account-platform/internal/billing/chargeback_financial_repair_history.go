package billing

import (
	"context"

	"github.com/google/uuid"
)

// entitlementCorrectionHasChargebackRepairHistory adds this wave's immutable
// case package identity to the existing zero-financial-history qualification.
// Older installations without this facility retain their existing eligibility;
// failures to read an installed facility are returned to the caller.
func entitlementCorrectionHasChargebackRepairHistory(ctx context.Context, source entitlementCorrectionQuerier, packageID uuid.UUID) (bool, error) {
	var installed bool
	if err := source.QueryRow(ctx, `SELECT to_regclass('public.chargeback_financial_repair_cases') IS NOT NULL`).Scan(&installed); err != nil {
		return false, err
	}
	if !installed {
		return false, nil
	}
	var related bool
	err := source.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM public.chargeback_financial_repair_cases WHERE package_id=$1)`, packageID).Scan(&related)
	return related, err
}
