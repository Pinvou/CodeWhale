package token

import (
	"context"
	"github.com/google/uuid"
	"net/http"
)

type SnapshotOrganizationBillingClient interface {
	ReserveMemberQuotaPriced(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, int64, int64, uuid.UUID, HoldPricingContext) (BillingHold, error)
}

func (c *HTTPBillingClient) ReserveMemberQuota(ctx context.Context, usageID, walletID, organizationID, memberUserID uuid.UUID, membershipVersion, maximum int64, priceID uuid.UUID) (BillingHold, error) {
	var hold BillingHold
	err := c.call(ctx, http.MethodPost, "/internal/v1/holds/reserve", map[string]any{"usage_request_id": usageID, "wallet_id": walletID, "organization_id": organizationID, "member_user_id": memberUserID, "membership_version": membershipVersion, "maximum_microcredits": maximum, "price_version_id": priceID, "lease_seconds": 900}, &hold)
	return hold, err
}
func (c *HTTPBillingClient) ReserveMemberQuotaPriced(ctx context.Context, usageID, walletID, organizationID, memberUserID uuid.UUID, membershipVersion, maximum int64, priceID uuid.UUID, pricing HoldPricingContext) (BillingHold, error) {
	var hold BillingHold
	body := holdReservationBody(usageID, walletID, maximum, priceID, pricing)
	body["organization_id"], body["member_user_id"], body["membership_version"] = organizationID, memberUserID, membershipVersion
	err := c.call(ctx, http.MethodPost, "/internal/v1/holds/reserve", body, &hold)
	return hold, err
}
