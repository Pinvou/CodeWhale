package token

import (
	"context"
	"encoding/json"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/google/uuid"
	"net/http"
	"net/url"
)

type OrganizationBillingClient interface {
	CreateOrganizationRechargeOrder(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string, string, string, string) (json.RawMessage, error)
	OrganizationQuotaGrants(context.Context, uuid.UUID, *uuid.UUID) (json.RawMessage, error)
	AllocateOrganizationQuotaBatch(context.Context, uuid.UUID, uuid.UUID, json.RawMessage, string) (json.RawMessage, error)
	RevokeOrganizationQuota(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string) (json.RawMessage, error)
}

func (c *HTTPBillingClient) CreateOrganizationRechargeOrder(ctx context.Context, requesterID, walletID, organizationID uuid.UUID, productID, channel, mobile, commerceCustomerStatus, idempotencyKey string) (json.RawMessage, error) {
	return c.raw(ctx, http.MethodPost, "/internal/v1/recharge/orders", map[string]any{"requester_id": requesterID, "beneficiary_organization_id": organizationID, "wallet_id": walletID, "product_id": productID, "channel": channel, "mobile": mobile, "commerce_customer_status": commerceCustomerStatus}, idempotencyKey)
}

func (c *HTTPBillingClient) OrganizationQuotaGrants(ctx context.Context, organizationID uuid.UUID, memberUserID *uuid.UUID) (json.RawMessage, error) {
	path := "/internal/v1/organizations/" + organizationID.String() + "/quota-grants"
	if memberUserID != nil {
		path += "?member_user_id=" + url.QueryEscape(memberUserID.String())
	}
	return c.raw(ctx, http.MethodGet, path, nil, "")
}

func (c *HTTPBillingClient) AllocateOrganizationQuotaBatch(ctx context.Context, organizationID, _ uuid.UUID, items json.RawMessage, idempotencyKey string) (json.RawMessage, error) {
	var decoded []json.RawMessage
	if err := json.Unmarshal(items, &decoded); err != nil {
		return nil, err
	}
	path := "/internal/v1/organizations/" + organizationID.String() + "/quota-grants:batch"
	return c.rawDelegated(ctx, http.MethodPost, path, map[string]any{"items": decoded}, idempotencyKey, authorization.DelegationRequest{Audience: "billing-service", Permission: "org.quota.allocate", OrganizationID: &organizationID, TargetResourceType: "organization", TargetResourceID: organizationID.String(), HTTPMethod: http.MethodPost, HTTPPath: path})
}

func (c *HTTPBillingClient) RevokeOrganizationQuota(ctx context.Context, organizationID, grantID, _ uuid.UUID, reason, idempotencyKey string) (json.RawMessage, error) {
	path := "/internal/v1/organizations/" + organizationID.String() + "/quota-grants/" + grantID.String() + "/revoke"
	return c.rawDelegated(ctx, http.MethodPost, path, map[string]any{"reason": reason}, idempotencyKey, authorization.DelegationRequest{Audience: "billing-service", Permission: "org.quota.revoke", OrganizationID: &organizationID, TargetResourceType: "member-quota-grant", TargetResourceID: grantID.String(), HTTPMethod: http.MethodPost, HTTPPath: path})
}

type AccountClosedGrantBillingClient interface {
	ReclaimAccountClosedGrant(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, string) (json.RawMessage, error)
}
