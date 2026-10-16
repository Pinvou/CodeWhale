package token

import (
	"context"
	"errors"
	"time"

	"net/http"

	"github.com/google/uuid"
)

// Positive extensions have a caller-owned logical operation identity. Zero-cost
// lease renewal retains its original wire contract and does not create a receipt.
func (c *HTTPBillingClient) Extend(ctx context.Context, usageID, holdID uuid.UUID, additional int64, operationID uuid.UUID) error {
	if usageID == uuid.Nil || holdID == uuid.Nil || additional < 0 ||
		(additional == 0 && operationID != uuid.Nil) || (additional > 0 && operationID == uuid.Nil) {
		return ErrForbidden
	}
	body := map[string]any{"usage_request_id": usageID, "hold_id": holdID, "lease_seconds": 900, "additional_microcredits": additional}
	if additional == 0 {
		return c.call(ctx, http.MethodPost, "/internal/v1/holds/extend", body, nil)
	}
	body["operation_id"] = operationID
	var hold struct {
		ID             uuid.UUID  `json:"hold_id"`
		UsageRequestID uuid.UUID  `json:"usage_request_id"`
		WalletID       uuid.UUID  `json:"wallet_id"`
		PriceVersionID *uuid.UUID `json:"price_version_id"`
		Status         string     `json:"status"`
		Maximum        *int64     `json:"max_microcredits"`
		Actual         *int64     `json:"actual_microcredits"`
		ExpiresAt      *time.Time `json:"expires_at"`
	}
	if err := c.call(ctx, http.MethodPost, "/internal/v1/holds/extend", body, &hold); err != nil {
		return err
	}
	// This is an acknowledgement of this operation, not current expiry or release
	// evidence. A durable success receipt can legitimately have a past expiry.
	if hold.ID != holdID || hold.UsageRequestID != usageID || hold.WalletID == uuid.Nil ||
		hold.Status != "ACTIVE" || hold.Maximum == nil || *hold.Maximum < additional ||
		hold.ExpiresAt == nil || hold.ExpiresAt.IsZero() ||
		(hold.PriceVersionID != nil && *hold.PriceVersionID == uuid.Nil) ||
		hold.Actual != nil {
		return errors.New("invalid Billing extension acknowledgement")
	}
	return nil
}

func (c *HTTPBillingClient) Commit(ctx context.Context, usageID, holdID uuid.UUID, actual int64, usage Usage) error {
	return c.call(ctx, http.MethodPost, "/internal/v1/holds/commit", map[string]any{"usage_request_id": usageID, "hold_id": holdID, "actual_microcredits": actual, "usage_snapshot": usage}, nil)
}

// ModelExecutionBillingClient is the completed personal invocation money path.
// Management discovery and the existing finance BFF retain their own interfaces.
type ModelExecutionBillingClient interface {
	cancellationBillingClient
	SnapshotBillingClient
	Price(context.Context, string) (PriceVersion, error)
	PriceByID(context.Context, uuid.UUID) (PriceVersion, error)
	Extend(context.Context, uuid.UUID, uuid.UUID, int64, uuid.UUID) error
	Commit(context.Context, uuid.UUID, uuid.UUID, int64, Usage) error
}
type SnapshotRenewalBillingClient interface {
	SnapshotBillingClient
	Extend(context.Context, uuid.UUID, uuid.UUID, int64, uuid.UUID) error
}
