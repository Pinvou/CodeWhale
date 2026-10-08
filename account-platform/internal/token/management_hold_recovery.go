package token

import (
	"context"

	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"net/http"
)

type frozenUsageSnapshot struct {
	OrganizationID                      *uuid.UUID      `json:"organization_id,omitempty"`
	OrganizationMembershipVersion       *int64          `json:"organization_membership_version,omitempty"`
	CanPurchaseOrganizationEntitlements bool            `json:"can_purchase_organization_entitlements,omitempty"`
	WalletOwnerType                     string          `json:"wallet_owner_type"`
	ProviderID                          string          `json:"provider_id"`
	AdapterVersion                      string          `json:"adapter_version"`
	UpstreamModelRef                    string          `json:"upstream_model_ref"`
	ContextType                         string          `json:"context_type"`
	MembershipVersion                   int64           `json:"membership_version"`
	MemberVersion                       int64           `json:"member_version"`
	MaximumOutput                       int64           `json:"maximum_output_tokens"`
	Stream                              bool            `json:"stream"`
	PricingSnapshot                     json.RawMessage `json:"pricing_snapshot"`
}

func (s *Service) ensureUsageHold(ctx context.Context, usage UsageRequest, envelope DispatchEnvelope) (BillingHold, error) {
	var snapshot frozenUsageSnapshot
	if json.Unmarshal(usage.RequestSnapshot, &snapshot) != nil {
		return BillingHold{}, ErrForbidden
	}
	pricingSnapshot, err := json.Marshal(envelope.Price)
	if err != nil {
		return BillingHold{}, err
	}
	pricing := HoldPricingContext{ModelID: usage.Model, ProtocolCode: "CHAT", FormulaVersionID: envelope.Price.FormulaVersionID, PricingSnapshot: pricingSnapshot, RequestSnapshot: usage.RequestSnapshot}
	if snapshot.ContextType == "ORGANIZATION" {
		if usage.OrganizationID == nil || snapshot.OrganizationID == nil || *usage.OrganizationID != *snapshot.OrganizationID || snapshot.OrganizationMembershipVersion == nil || *snapshot.OrganizationMembershipVersion < 1 {
			return BillingHold{}, ErrForbidden
		}
		client, ok := s.billing.(SnapshotOrganizationBillingClient)
		if !ok {
			return BillingHold{}, ErrForbidden
		}
		return client.ReserveMemberQuotaPriced(ctx, usage.ID, usage.WalletID, *usage.OrganizationID, usage.UserID, *snapshot.OrganizationMembershipVersion, usage.MaximumMicrocredits, envelope.Price.ID, pricing)
	}
	if snapshot.ContextType != "PERSONAL" || usage.OrganizationID != nil {
		return BillingHold{}, ErrForbidden
	}
	if client, ok := s.billing.(SnapshotBillingClient); ok {
		return client.ReservePriced(ctx, usage.ID, usage.WalletID, usage.MaximumMicrocredits, envelope.Price.ID, pricing)
	}
	return s.cancellationBilling.Reserve(ctx, usage.ID, usage.WalletID, usage.MaximumMicrocredits, envelope.Price.ID)
}

func definitiveBillingFailure(err error) bool {
	var failure *BillingHTTPError
	if !errors.As(err, &failure) {
		return false
	}
	switch failure.Status {
	case http.StatusConflict:
		return failure.Code == "INSUFFICIENT_COMPONENT_CAPACITY"
	case http.StatusBadRequest, http.StatusPaymentRequired, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusTooManyRequests:
		return true
	default:
		return false
	}
}
