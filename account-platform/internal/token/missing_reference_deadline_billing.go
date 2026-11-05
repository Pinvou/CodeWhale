package token

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
)

func (c *HTTPBillingClient) ReleaseMissingReference(ctx context.Context, request missingReferenceReleaseRequest) (confirmedReleaseReceipt, error) {
	var receipt confirmedReleaseReceipt
	intent := request.Intent
	if intent.Version != 1 || intent.UsageID == uuid.Nil || intent.UserID == uuid.Nil || intent.HoldID == uuid.Nil || intent.PriceID == uuid.Nil ||
		intent.WalletID == uuid.Nil || intent.Maximum < 0 || intent.Cause != missingReferenceDeadlineCause {
		return receipt, ErrForbidden
	}
	expected := map[string]any{
		"version": 1, "wallet_id": intent.WalletID, "owner_id": intent.UserID,
		"price_version_id": intent.PriceID, "maximum_microcredits": intent.Maximum,
		"model_id": request.Pricing.ModelID, "protocol_code": request.Pricing.ProtocolCode,
		"formula_version_id": request.Pricing.FormulaVersionID,
		"pricing_snapshot":   request.Pricing.PricingSnapshot, "request_snapshot": request.Pricing.RequestSnapshot,
	}
	// Existing workload identity/scope and installed release endpoint only.
	var payload json.RawMessage
	if err := c.call(ctx, http.MethodPost, "/internal/v1/holds/release", map[string]any{
		"usage_request_id": intent.UsageID, "hold_id": intent.HoldID,
		"reason": missingReferenceDeadlineCause, "expected_original_hold": expected,
	}, &payload); err != nil {
		return receipt, err
	}
	// A successful HTTP status is not a financial acknowledgement. In
	// particular, a missing or null maximum cannot borrow a valid zero budget.
	if !expiryJSONFields(payload, []string{"hold_id", "usage_request_id", "wallet_id", "price_version_id", "max_microcredits", "status"}) || json.Unmarshal(payload, &receipt) != nil {
		return receipt, ErrForbidden
	}
	if !releaseReceiptMatches(receipt, intent) {
		return receipt, ErrForbidden
	}
	return receipt, nil
}
