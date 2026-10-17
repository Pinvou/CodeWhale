package billing

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This guard preserves existing provisioning for units without a projection;
// a known terminal projection can never authorize new financial writes.
func rejectDeletedUnitWalletWrite(ctx context.Context, tx pgx.Tx, unitID uuid.UUID) error {
	var deleted bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM organization_unit_lifecycle_states WHERE unit_id=$1 AND status='DELETED')`, unitID).Scan(&deleted); err != nil {
		return err
	}
	if deleted {
		return ErrWalletUnavailable
	}
	return nil
}

func organizationUnitFinancialBlockers(ctx context.Context, queryer organizationUnitQueryer, unitID uuid.UUID) ([]OrganizationUnitBlocker, error) {
	queries := []struct{ code, resourceType, query string }{
		{"WALLET_BALANCE_NOT_ZERO", "wallet", `SELECT w.id::text FROM wallets w WHERE w.owner_type='ORG_UNIT' AND w.owner_id=$1 AND w.posted_microcredits<>0`},
		{"WALLET_RESERVED_BALANCE", "wallet", `SELECT w.id::text FROM wallets w WHERE w.owner_type='ORG_UNIT' AND w.owner_id=$1 AND w.reserved_microcredits<>0`},
		{"WALLET_RECEIVABLE_OPEN", "receivable", `SELECT r.id::text FROM receivables r JOIN wallets w ON w.id=r.wallet_id WHERE w.owner_type='ORG_UNIT' AND w.owner_id=$1 AND r.outstanding_microcredits>0`},
		{"EFFECTIVE_ENTITLEMENT_REMAINS", "entitlement_package", `SELECT DISTINCT p.id::text FROM wallets w JOIN entitlement_packages p ON p.wallet_id=w.id JOIN entitlement_components c ON c.package_id=p.id WHERE w.owner_type='ORG_UNIT' AND w.owner_id=$1 AND (c.available_microcredits+c.reserved_microcredits+c.active_grant_microcredits+c.suspended_grant_microcredits)>0`},
		{"UNSETTLED_USAGE_HOLD", "usage_hold", `SELECT h.id::text FROM wallets w JOIN usage_holds h ON h.wallet_id=w.id WHERE w.owner_type='ORG_UNIT' AND w.owner_id=$1 AND h.status IN ('ACTIVE','REVIEW')`},
		{"UNFINISHED_PAYMENT", "payment_order", `SELECT o.id::text FROM wallets w JOIN payment_orders o ON o.wallet_id=w.id WHERE w.owner_type='ORG_UNIT' AND w.owner_id=$1 AND (o.status IN ('CREATED','PENDING','MANUAL_REVIEW') OR (o.status IN ('PAID','PAID_LATE','PARTIALLY_REFUNDED','REFUNDED') AND o.fulfillment_status<>'CREDITED'))`},
		{"UNFINISHED_REFUND", "refund", `SELECT f.id::text FROM wallets w JOIN payment_orders o ON o.wallet_id=w.id JOIN refunds f ON f.payment_order_id=o.id WHERE w.owner_type='ORG_UNIT' AND w.owner_id=$1 AND f.status IN ('REQUESTED','MANUAL_REVIEW')`},
		{"UNRESOLVED_CHARGEBACK", "chargeback_case", `SELECT c.id::text FROM wallets w JOIN chargeback_cases c ON c.wallet_id=w.id WHERE w.owner_type='ORG_UNIT' AND w.owner_id=$1 AND c.status<>'RESOLVED'`},
		{"UNRESOLVED_FORCED_REFUND", "forced_refund_case", `SELECT c.id::text FROM wallets w JOIN forced_refund_cases c ON c.wallet_id=w.id WHERE w.owner_type='ORG_UNIT' AND w.owner_id=$1 AND c.status='REVIEW_REQUIRED'`},
		{"OPEN_RECONCILIATION_DIFFERENCE", "reconciliation_case", `SELECT c.id::text FROM wallets w JOIN payment_orders o ON o.wallet_id=w.id JOIN reconciliation_cases c ON c.payment_order_id=o.id WHERE w.owner_type='ORG_UNIT' AND w.owner_id=$1 AND c.status IN ('OPEN','IN_REVIEW')`},
		{"PENDING_USAGE_REVIEW", "usage_loss_review", `SELECT review.id::text FROM wallets w JOIN usage_loss_reviews review ON review.wallet_id=w.id WHERE w.owner_type='ORG_UNIT' AND w.owner_id=$1 AND review.status='REVIEW_REQUIRED'`},
		{"PENDING_FINANCIAL_ADJUSTMENT", "manual_adjustment", `SELECT a.id::text FROM wallets w JOIN manual_adjustments a ON a.wallet_id=w.id WHERE w.owner_type='ORG_UNIT' AND w.owner_id=$1 AND a.status IN ('PENDING_APPROVAL','APPROVED')`},
	}
	blockers := make([]OrganizationUnitBlocker, 0)
	for _, item := range queries {
		rows, err := queryer.Query(ctx, item.query, unitID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var resourceID string
			if err := rows.Scan(&resourceID); err != nil {
				rows.Close()
				return nil, err
			}
			blockers = append(blockers, OrganizationUnitBlocker{Code: item.code, ResourceType: item.resourceType, ResourceID: resourceID})
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return blockers, nil
}
