package token

import (
	"context"

	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

func (s *Service) finishPreDispatchCancellation(ctx context.Context, usageID uuid.UUID) (CancellationResult, error) {
	if s.cancellationStore == nil || s.cancellationBilling == nil {
		return CancellationResult{}, ErrForbidden
	}
	usage, err := s.cancellationStore.GetUsage(ctx, usageID)
	if err != nil {
		return CancellationResult{}, err
	}
	if usage.ExecutionStatus == "RECEIVED" && usage.CanonicalVersion > 0 {
		return s.cancellationStore.ConfirmPreDispatchCancellation(ctx, usageID)
	}
	if usage.CanonicalVersion == 0 && usage.ExecutionStatus == "RECEIVED" {
		// A migrated CREATED row may have lost the acknowledgement of an
		// already-created Billing hold. Its local state cannot prove no hold.
		if _, _, err := legacyCancellationPricing(usage); err != nil {
			return s.unconfirmedCancellation(ctx, usage, ErrForbidden)
		}
		if err := s.cancellationStore.MarkHoldReserving(ctx, usageID); err != nil {
			return cancellationSnapshot(usage), err
		}
		usage.ExecutionStatus = "HOLD_RESERVING"
	}
	if usage.HoldID == nil {
		var hold BillingHold
		var priceID uuid.UUID
		if usage.CanonicalVersion == 0 {
			var pricing HoldPricingContext
			priceID, pricing, err = legacyCancellationPricing(usage)
			if err != nil {
				return cancellationSnapshot(usage), err
			}
			client, supported := s.billing.(SnapshotBillingClient)
			if !supported {
				return cancellationSnapshot(usage), ErrForbidden
			}
			hold, err = client.ReservePriced(ctx, usage.ID, usage.WalletID, usage.MaximumMicrocredits, priceID, pricing)
		} else {
			envelope, preparationErr := s.cancellationStore.LoadDispatchPreparation(ctx, usageID)
			if preparationErr != nil {
				return CancellationResult{}, preparationErr
			}
			priceID = envelope.Price.ID
			hold, err = s.ensureUsageHold(ctx, usage, envelope)
		}
		if err != nil {
			// Reservation already started. Even a definitive rejection of this
			// recovery call cannot establish whether an earlier lost ACK hides
			// a committed hold. Keep the existing uncertainty/review evidence.
			return s.unconfirmedCancellation(ctx, usage, err)
		}
		err = s.cancellationStore.CompleteCancellationHold(ctx, usageID, hold.ID, priceID)
		if err != nil && !errors.Is(err, ErrCancellationRequested) {
			return s.unconfirmedCancellation(ctx, usage, err)
		}
		usage, err = s.cancellationStore.GetUsage(ctx, usageID)
		if err != nil {
			return CancellationResult{}, err
		}
	}
	if usage.ExecutionStatus != "HOLD_RESERVING" && usage.ExecutionStatus != "HOLD_RESERVED" {
		return cancellationSnapshot(usage), nil
	}
	if usage.HoldID != nil {
		if err := s.cancellationBilling.Release(ctx, usageID, *usage.HoldID, "cancelled before dispatch intent"); err != nil {
			return s.unconfirmedCancellation(ctx, usage, err)
		}
	}
	return s.cancellationStore.ConfirmPreDispatchCancellation(ctx, usageID)
}

func legacyCancellationPricing(usage UsageRequest) (uuid.UUID, HoldPricingContext, error) {
	var snapshot struct {
		Model          string          `json:"model"`
		Protocol       string          `json:"protocol_code"`
		ContextType    string          `json:"context_type"`
		OrganizationID *uuid.UUID      `json:"organization_id"`
		WalletID       uuid.UUID       `json:"wallet_id"`
		TokenAccountID uuid.UUID       `json:"token_account_id"`
		CredentialID   uuid.UUID       `json:"credential_id"`
		PriceID        uuid.UUID       `json:"price_version_id"`
		FormulaID      uuid.UUID       `json:"formula_version_id"`
		Pricing        json.RawMessage `json:"pricing_snapshot"`
		Normalization  int             `json:"usage_normalization_version"`
		Input          *int64          `json:"estimated_input_tokens"`
		Output         *int64          `json:"maximum_output_tokens"`
		RequestDigest  string          `json:"request_sha256"`
	}
	var price PriceVersion
	if usage.WalletID == uuid.Nil || usage.MaximumMicrocredits <= 0 || usage.OrganizationID != nil || json.Unmarshal(usage.RequestSnapshot, &snapshot) != nil || snapshot.ContextType != "PERSONAL" || snapshot.OrganizationID != nil || snapshot.Model != usage.Model || snapshot.Protocol != "CHAT" || snapshot.WalletID != usage.WalletID || snapshot.TokenAccountID != usage.TokenAccountID || snapshot.CredentialID != usage.CredentialID || snapshot.PriceID == uuid.Nil || snapshot.FormulaID == uuid.Nil || snapshot.Normalization != 2 || snapshot.Input == nil || snapshot.Output == nil || *snapshot.Input < 0 || *snapshot.Output < 0 || len(snapshot.RequestDigest) != 64 || json.Unmarshal(snapshot.Pricing, &price) != nil || price.ID != snapshot.PriceID || price.FormulaVersionID != snapshot.FormulaID || price.Model != usage.Model {
		return uuid.Nil, HoldPricingContext{}, ErrForbidden
	}
	if _, err := hex.DecodeString(snapshot.RequestDigest); err != nil {
		return uuid.Nil, HoldPricingContext{}, ErrForbidden
	}
	if usage.PriceVersionID != nil && *usage.PriceVersionID != snapshot.PriceID {
		return uuid.Nil, HoldPricingContext{}, ErrForbidden
	}
	maximum, err := Cost(price, Usage{InputTokens: *snapshot.Input, OutputTokens: *snapshot.Output})
	if err != nil || maximum != usage.MaximumMicrocredits {
		return uuid.Nil, HoldPricingContext{}, ErrForbidden
	}
	return snapshot.PriceID, HoldPricingContext{ModelID: usage.Model, ProtocolCode: snapshot.Protocol, FormulaVersionID: snapshot.FormulaID, PricingSnapshot: snapshot.Pricing, RequestSnapshot: usage.RequestSnapshot}, nil
}

func (s *Service) unconfirmedCancellation(ctx context.Context, usage UsageRequest, cause error) (CancellationResult, error) {
	if err := s.cancellationStore.MarkReview(ctx, usage.ID, "cancel_hold_recovery_unconfirmed", map[string]any{"customer_hold_release_unconfirmed": true}); err != nil {
		return cancellationSnapshot(usage), err
	}
	current, err := s.cancellationStore.GetUsage(ctx, usage.ID)
	if err != nil {
		return cancellationSnapshot(usage), err
	}
	return cancellationSnapshot(current), cause
}

type managementCancellationStore interface {
	GetUsage(context.Context, uuid.UUID) (UsageRequest, error)
	ConfirmPreDispatchCancellation(context.Context, uuid.UUID) (CancellationResult, error)
	MarkHoldReserving(context.Context, uuid.UUID) error
	LoadDispatchPreparation(context.Context, uuid.UUID) (DispatchEnvelope, error)
	CompleteCancellationHold(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error
	MarkReview(context.Context, uuid.UUID, string, map[string]any) error
}
