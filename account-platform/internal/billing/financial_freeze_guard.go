package billing

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// The current dismissal is being committed by the controlled resolution
// transaction; all earlier dismissals require their immutable command receipt.
// RESOLVED alone is not proof that a financial freeze was disproved.
func packageFinancialFreezeBlocked(ctx context.Context, q queryable, packageID, walletID, excludingRefundID, currentDismissalID uuid.UUID) (bool, error) {
	return financialFreezeBlocked(ctx, q, packageID, walletID, excludingRefundID, currentDismissalID, true)
}

func packageFinancialSourceBlocked(ctx context.Context, q queryable, packageID, walletID uuid.UUID) (bool, error) {
	return financialFreezeBlocked(ctx, q, packageID, walletID, uuid.Nil, uuid.Nil, false)
}

func financialFreezeBlocked(ctx context.Context, q queryable, packageID, walletID, excludingRefundID, currentDismissalID uuid.UUID, includeOtherFences bool) (bool, error) {
	var blocked bool
	err := q.QueryRow(ctx, `SELECT
 EXISTS(SELECT 1 FROM chargeback_cases source WHERE source.entitlement_package_id=$1 AND NOT COALESCE((
 source.status='DISMISSED' AND source.resolution_code='DISMISS' AND EXISTS(
 SELECT 1 FROM payment_anomaly_cases anomaly WHERE anomaly.id=source.manual_review_case_id AND anomaly.chargeback_case_id=source.id AND anomaly.status='RESOLVED' AND anomaly.resolution_code='DISMISS' AND anomaly.financial_action_status='NONE'
 AND (anomaly.id=$4 OR EXISTS(SELECT 1 FROM payment_anomaly_commands command WHERE command.payment_anomaly_case_id=anomaly.id AND command.action='DISMISS')))),false))
 OR EXISTS(SELECT 1 FROM forced_refund_cases source WHERE source.entitlement_package_id=$1 AND NOT COALESCE((
 source.status='RESOLVED' AND source.resolution_code='DISMISS' AND EXISTS(
 SELECT 1 FROM payment_anomaly_cases anomaly WHERE anomaly.id=source.manual_review_case_id AND anomaly.forced_refund_case_id=source.id AND anomaly.status='RESOLVED' AND anomaly.resolution_code='DISMISS' AND anomaly.financial_action_status='NONE'
 AND (anomaly.id=$4 OR EXISTS(SELECT 1 FROM payment_anomaly_commands command WHERE command.payment_anomaly_case_id=anomaly.id AND command.action='DISMISS')))),false))
 OR ($5 AND (EXISTS(SELECT 1 FROM refunds WHERE entitlement_package_id=$1 AND id<>$3 AND status IN ('REQUESTED','MANUAL_REVIEW'))
 OR EXISTS(SELECT 1 FROM receivables WHERE wallet_id=$2 AND outstanding_microcredits>0)))`, packageID, walletID, excludingRefundID, currentDismissalID, includeOtherFences).Scan(&blocked)
	return blocked, err
}

func validFinancialFreezeSnapshot(raw json.RawMessage, packageID, walletID uuid.UUID) bool {
	type component struct {
		ID        uuid.UUID `json:"id"`
		PackageID uuid.UUID `json:"package_id"`
		Status    string    `json:"status"`
	}
	var snapshot struct {
		Package struct {
			ID       uuid.UUID `json:"id"`
			WalletID uuid.UUID `json:"wallet_id"`
			Status   string    `json:"status"`
		} `json:"package"`
		Components []component `json:"components"`
		Grants     []struct {
			ID          uuid.UUID `json:"id"`
			PackageID   uuid.UUID `json:"package_id"`
			ComponentID uuid.UUID `json:"component_id"`
			Status      string    `json:"status"`
		} `json:"grants"`
	}
	if json.Unmarshal(raw, &snapshot) != nil || snapshot.Package.ID != packageID || snapshot.Package.WalletID != walletID || snapshot.Package.Status == "" || len(snapshot.Components) == 0 || snapshot.Grants == nil {
		return false
	}
	components := map[uuid.UUID]bool{}
	for _, item := range snapshot.Components {
		if item.ID == uuid.Nil || item.PackageID != packageID || item.Status == "" || components[item.ID] {
			return false
		}
		components[item.ID] = true
	}
	grants := map[uuid.UUID]bool{}
	for _, item := range snapshot.Grants {
		if item.ID == uuid.Nil || item.PackageID != packageID || !components[item.ComponentID] || item.Status == "" || grants[item.ID] {
			return false
		}
		grants[item.ID] = true
	}
	return true
}
