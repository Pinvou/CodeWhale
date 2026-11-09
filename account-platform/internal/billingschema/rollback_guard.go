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
	case 10:
		// The frozen Up overwrites the four legacy enabled flags, so the
		// default-looking installed state cannot prove the prior catalog.
		return errors.New("plan catalog rollback cannot preserve prior product availability")
	case 14:
		return requireNoHistoricalRollbackFacts(ctx, tx,
			`LOCK TABLE payment_notifications,payment_order_repairs IN SHARE ROW EXCLUSIVE MODE`,
			`SELECT EXISTS(SELECT 1 FROM payment_notifications WHERE result='MANUAL_REVIEW') OR EXISTS(SELECT 1 FROM payment_order_repairs)`,
			"payment repair rollback cannot preserve review and repair facts")
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
	case 27:
		// Up folds historical refund order and fulfillment states into payment
		// success. A remaining paid or credited order cannot prove its old state.
		return requireNoHistoricalRollbackFacts(ctx, tx,
			`LOCK TABLE payment_orders,entitlement_packages,refunds,refund_component_reservations,refund_grant_reservations,forced_refund_cases,payment_evidence,member_quota_grants IN SHARE ROW EXCLUSIVE MODE`,
			`SELECT EXISTS(SELECT 1 FROM payment_orders WHERE status IN ('PAID','PAID_LATE') OR fulfillment_status='CREDITED')
 OR EXISTS(SELECT 1 FROM entitlement_packages WHERE status='REVOKED')
 OR EXISTS(SELECT 1 FROM refunds)
 OR EXISTS(SELECT 1 FROM refund_component_reservations)
 OR EXISTS(SELECT 1 FROM refund_grant_reservations)
 OR EXISTS(SELECT 1 FROM forced_refund_cases)
 OR EXISTS(SELECT 1 FROM payment_evidence WHERE evidence_type='FORCED_REFUND')
 OR EXISTS(SELECT 1 FROM member_quota_grants WHERE suspension_reason IN ('REFUND_PENDING','FORCED_REFUND_REVIEW'))`,
			"targeted refund rollback cannot preserve original payment and refund facts")
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
	case 35, 36, 37:
		// These frozen Downs publish fixed catalog facts rather than recover
		// the configuration that actually preceded their original Up.
		return errors.New("catalog rollback cannot restore the prior payment mappings and prices")
	case 42:
		// Frozen Down republishes constants, not the actual prior catalog.
		return errors.New("single SKU rollback cannot restore the prior payment mappings and prices")
	case 46:
		// Frozen Down is a comment; removing its ledger would advertise a
		// reversal without recovering the cleared external payment instructions.
		return errors.New("terminal payment instruction cleanup is irreversible")
	case 49:
		// A later observation may move an expired order out of EXPIRED without
		// recovering the original state or payment instructions cleared by Up.
		return requireNoHistoricalRollbackFacts(ctx, tx,
			`LOCK TABLE payment_orders IN SHARE ROW EXCLUSIVE MODE`,
			`SELECT EXISTS(SELECT 1 FROM payment_orders)`,
			"local expiry rollback cannot preserve original order state and payment instructions")
	case 51:
		// The comment-only Down preserves repaired concurrency facts. Keep
		// their migration ledger as well instead of advertising a reversal.
		return errors.New("pricing space version repair is irreversible")
	}
	return nil
}

func requireNoHistoricalRollbackFacts(ctx context.Context, tx pgx.Tx, lockSQL, factsSQL, reason string) error {
	if _, err := tx.Exec(ctx, lockSQL); err != nil {
		return err
	}
	var present bool
	if err := tx.QueryRow(ctx, factsSQL).Scan(&present); err != nil {
		return err
	}
	if present {
		return errors.New(reason)
	}
	return nil
}
