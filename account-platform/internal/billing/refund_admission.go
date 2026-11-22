package billing

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

func ordinaryRefundLegacyOrder(order PaymentOrder, snapshot []byte, mappingID *uuid.UUID) bool {
	var product struct {
		ProductID   string `json:"product_id"`
		Kind        string `json:"kind"`
		PriceStatus string `json:"price_status"`
	}
	if !bytes.HasPrefix(bytes.TrimSpace(snapshot), []byte("{")) || json.Unmarshal(snapshot, &product) != nil || mappingID != nil {
		return false
	}
	if product.ProductID != "" && product.ProductID != order.ProductID {
		return false
	}
	// These are the original pre-plan products. Do not infer historical order
	// identity from the current catalog, missing fields alone, or a name prefix.
	switch order.ProductID {
	case "CNY_1", "CNY_10", "CNY_100", "CNY_200":
		return (product.Kind == "" || product.Kind == "LEGACY") && (product.PriceStatus == "" || product.PriceStatus == "LEGACY")
	default:
		return false
	}
}

func (p *PaymentCoordinator) claimUnstartedRefund(ctx context.Context, refundID uuid.UUID) error {
	// A lease expiry does not grant permission to repeat a possible dispatch.
	// This CAS also fences a worker holding a stale NOT_STARTED preflight.
	claimed, err := p.repository.pool.Exec(ctx, `UPDATE refunds SET dispatch_state='IN_PROGRESS',dispatch_started_at=COALESCE(dispatch_started_at,$2),dispatch_lease_until=$2+interval '2 minutes' WHERE id=$1 AND status='REQUESTED' AND dispatch_state='NOT_STARTED' AND (dispatch_lease_until IS NULL OR dispatch_lease_until<$2)`, refundID, p.now())
	if err != nil {
		return err
	}
	if claimed.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}
