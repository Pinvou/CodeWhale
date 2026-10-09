package token

import (
	"context"

	"net/http"

	"github.com/google/uuid"
)

func (c *HTTPBillingClient) Extend(ctx context.Context, usageID, holdID uuid.UUID, additional int64) error {
	return c.call(ctx, http.MethodPost, "/internal/v1/holds/extend", map[string]any{"usage_request_id": usageID, "hold_id": holdID, "lease_seconds": 900, "additional_microcredits": additional}, nil)
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
	Extend(context.Context, uuid.UUID, uuid.UUID, int64) error
	Commit(context.Context, uuid.UUID, uuid.UUID, int64, Usage) error
}
type SnapshotRenewalBillingClient interface {
	SnapshotBillingClient
	Extend(context.Context, uuid.UUID, uuid.UUID, int64) error
}
