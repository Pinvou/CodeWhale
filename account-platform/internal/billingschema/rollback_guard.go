package billingschema

import (
	"context"
	"errors"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/jackc/pgx/v5"
)

// DownOne preserves original SQL identities and checks only the known unsafe
// rollback paths. The high-numbered fences retain their existing SQL refusal.
func DownOne(ctx context.Context, conn *pgx.Conn, input []migrations.Migration) error {
	ordered, err := Ordered(input)
	if err != nil {
		return err
	}
	return migrations.DownOneWithGuard(ctx, conn, ordered, retainedRollbackGuard)
}

func retainedRollbackGuard(ctx context.Context, tx pgx.Tx, migration migrations.Migration) error {
	switch migration.Version {
	case 23:
		// Serialize both inputs to the frozen Down's SKU projection. Original
		// 010 excludes empty SKUs; 023 Down's unique index includes them.
		if _, err := tx.Exec(ctx, `LOCK TABLE recharge_products,payment_product_mappings IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		var duplicate bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM recharge_products product
 LEFT JOIN payment_product_mappings mapping
 ON mapping.provider_id='h3c_mall' AND mapping.provider_version='h3c-mall-v1'
 AND mapping.mapping_version=1 AND mapping.product_id=product.product_id
 WHERE product.enabled
 GROUP BY COALESCE(mapping.external_sku,product.sku_code) HAVING count(*)>1
)`).Scan(&duplicate); err != nil {
			return err
		}
		if duplicate {
			return errors.New("payment provider catalog rollback would create duplicate enabled SKUs")
		}
	case 25:
		// Frozen 025 Down does not restore original 013's EXPIRE and
		// REPAIR_CREDIT journal types. Do not rewrite historical SQL or allow
		// a successful rollback to advertise an incompatible prior schema.
		return errors.New("receivable rollback does not restore the prior journal entry contract")
	case 29:
		if _, err := tx.Exec(ctx, `LOCK TABLE manual_review_cases,manual_review_case_events IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		var recorded bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(
 SELECT 1 FROM manual_review_case_events event
 JOIN manual_review_cases review ON review.id=event.case_id
 WHERE review.domain='BILLING' AND review.case_type IN ('PAYMENT_ANOMALY','RECONCILIATION_DIFFERENCE')
)`).Scan(&recorded); err != nil {
			return err
		}
		if recorded {
			return errors.New("payment anomaly rollback would delete immutable manual review events")
		}
	}
	return nil
}
