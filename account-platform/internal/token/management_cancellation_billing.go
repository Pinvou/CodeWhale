package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"io"
	"time"

	"math"

	"net/http"

	"github.com/google/uuid"
)

type BillingHold struct {
	ID             uuid.UUID `json:"hold_id"`
	UsageRequestID uuid.UUID `json:"usage_request_id"`
	Status         string    `json:"status"`
}

type HoldPricingContext struct {
	ModelID          string
	ProtocolCode     string
	FormulaVersionID uuid.UUID
	PricingSnapshot  json.RawMessage
	RequestSnapshot  json.RawMessage
}

func Cost(price PriceVersion, usage Usage) (int64, error) {
	if usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.CachedInputTokens < 0 {
		return 0, ErrForbidden
	}
	if len(price.Units) == 0 {
		input, err := tokenCost(usage.InputTokens, price.InputPerMillion)
		if err != nil {
			return 0, err
		}
		cached, err := tokenCost(usage.CachedInputTokens, price.CachedInputPerMillion)
		if err != nil || input > math.MaxInt64-cached {
			return 0, ErrForbidden
		}
		output, err := tokenCost(usage.OutputTokens, price.OutputPerMillion)
		if err != nil || input+cached > math.MaxInt64-output {
			return 0, ErrForbidden
		}
		return input + cached + output, nil
	}
	quantities := map[string]int64{"INPUT_TOKEN": usage.InputTokens, "CACHED_INPUT_TOKEN": usage.CachedInputTokens, "OUTPUT_TOKEN": usage.OutputTokens}
	var total int64
	for _, unit := range price.Units {
		quantity, ok := quantities[unit.UnitCode]
		if !ok || unit.MinimumPrecision <= 0 || unit.PriceDenominator <= 0 || unit.ResolvedPrice < 0 {
			return 0, ErrForbidden
		}
		component, err := unitCost(quantity, unit)
		if err != nil || total > math.MaxInt64-component {
			return 0, ErrForbidden
		}
		total += component
		delete(quantities, unit.UnitCode)
	}
	if len(quantities) != 0 {
		return 0, ErrForbidden
	}
	return total, nil
}

func unitCost(quantity int64, unit ResolvedUnitPrice) (int64, error) {
	if quantity < 0 || unit.ResolvedPrice < 0 || unit.PriceDenominator <= 0 || unit.MinimumPrecision <= 0 || quantity > math.MaxInt64/unit.MinimumPrecision {
		return 0, ErrForbidden
	}
	precisionUnits := quantity / unit.MinimumPrecision
	if quantity%unit.MinimumPrecision != 0 {
		precisionUnits++
	}
	if unit.ResolvedPrice != 0 && precisionUnits > math.MaxInt64/unit.ResolvedPrice {
		return 0, ErrForbidden
	}
	numerator := precisionUnits * unit.ResolvedPrice
	switch unit.RoundingRule {
	case "CEILING":
		if numerator > math.MaxInt64-(unit.PriceDenominator-1) {
			return 0, ErrForbidden
		}
		return (numerator + unit.PriceDenominator - 1) / unit.PriceDenominator, nil
	case "FLOOR":
		return numerator / unit.PriceDenominator, nil
	case "HALF_UP":
		if numerator > math.MaxInt64-unit.PriceDenominator/2 {
			return 0, ErrForbidden
		}
		return (numerator + unit.PriceDenominator/2) / unit.PriceDenominator, nil
	default:
		return 0, ErrForbidden
	}
}

func (c *HTTPBillingClient) Reserve(ctx context.Context, usageID, walletID uuid.UUID, maximum int64, priceID uuid.UUID) (BillingHold, error) {
	return c.reserveHold(ctx, usageID, map[string]any{"usage_request_id": usageID, "wallet_id": walletID, "maximum_microcredits": maximum, "price_version_id": priceID, "lease_seconds": 900})
}

func (c *HTTPBillingClient) ReservePriced(ctx context.Context, usageID, walletID uuid.UUID, maximum int64, priceID uuid.UUID, pricing HoldPricingContext) (BillingHold, error) {
	return c.reserveHold(ctx, usageID, holdReservationBody(usageID, walletID, maximum, priceID, pricing))
}

func (c *HTTPBillingClient) reserveHold(ctx context.Context, usageID uuid.UUID, body map[string]any) (BillingHold, error) {
	var hold BillingHold
	if err := c.call(ctx, http.MethodPost, "/internal/v1/holds/reserve", body, &hold); err != nil {
		return BillingHold{}, err
	}
	if usageID == uuid.Nil || hold.ID == uuid.Nil || hold.UsageRequestID != usageID {
		return BillingHold{}, errors.New("invalid Billing reservation acknowledgement")
	}
	// Idempotent recovery can return a terminal hold. Its identity is evidence;
	// this client must not reinterpret it as a newly ACTIVE reservation.
	return hold, nil
}

func (c *HTTPBillingClient) Release(ctx context.Context, usageID, holdID uuid.UUID, reason string) error {
	return c.call(ctx, http.MethodPost, "/internal/v1/holds/release", map[string]any{"usage_request_id": usageID, "hold_id": holdID, "reason": reason}, nil)
}

// This is the existing general hold release, without personal pricing proof.
// A nullable price is compared exactly, never treated as a wildcard.
func (c *HTTPBillingClient) ReleaseCapturedReference(ctx context.Context, intent capturedReferenceRelease) (capturedReleaseReceipt, error) {
	var receipt capturedReleaseReceipt
	if intent.UsageID == uuid.Nil || intent.HoldID == uuid.Nil || intent.WalletID == uuid.Nil || intent.Maximum < 0 {
		return receipt, ErrForbidden
	}
	raw, err := c.capturedPost200(ctx, "/internal/v1/holds/release", map[string]any{"usage_request_id": intent.UsageID, "hold_id": intent.HoldID, "reason": "trusted usage deadline exceeded"})
	if err != nil {
		return receipt, err
	}
	if !expiryJSONFields(raw, []string{"hold_id", "usage_request_id", "wallet_id", "max_microcredits", "status"}) || json.Unmarshal(raw, &receipt.confirmedReleaseReceipt) != nil || receipt.Maximum < intent.Maximum || receipt.Status != "RELEASED" || receipt.ID != intent.HoldID || receipt.UsageID != intent.UsageID || receipt.WalletID != intent.WalletID || !sameOptionalUUID(receipt.PriceID, intent.PriceID) {
		return receipt, ErrForbidden
	}
	if receipt.Maximum > intent.Maximum {
		proof := &capturedExtensionProof{OperationID: finalUsageReservationOperationID(intent.UsageID, intent.HoldID), LeaseSeconds: 900, Additional: receipt.Maximum - intent.Maximum}
		// Only a verified terminal hold makes an unknown operation harmless:
		// Billing either returns this exact old fingerprint's ACK or refuses it.
		ack, err := c.capturedPost200(ctx, "/internal/v1/holds/extend", map[string]any{"usage_request_id": intent.UsageID, "hold_id": intent.HoldID, "operation_id": proof.OperationID, "lease_seconds": proof.LeaseSeconds, "additional_microcredits": proof.Additional})
		if err != nil {
			return receipt, err
		}
		var extra struct {
			Actual  *int64    `json:"actual_microcredits"`
			Expires time.Time `json:"expires_at"`
		}
		if !expiryJSONFields(ack, []string{"hold_id", "usage_request_id", "wallet_id", "max_microcredits", "status", "expires_at"}) || json.Unmarshal(ack, &proof.Receipt) != nil || json.Unmarshal(ack, &extra) != nil || extra.Actual != nil {
			return receipt, ErrForbidden
		}
		proof.ExpiresAt = extra.Expires
		receipt.Extension = proof
	}
	if !capturedReceiptMatches(receipt, intent) {
		return receipt, ErrForbidden
	}
	return receipt, nil
}

// The two captured confirmation requests require 200, unlike general POST
// callers which also accept 201. Keep the existing client, auth and read bound.
func (c *HTTPBillingClient) capturedPost200(ctx context.Context, path string, input any) (json.RawMessage, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	q, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL.String()+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	if err = serviceauth.SetAuthorization(q, c.serviceToken); err != nil {
		return nil, err
	}
	q.Header.Set("Content-Type", "application/json")
	r, err := c.client.Do(q)
	if err != nil {
		return nil, err
	}
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, (2<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 2<<20 || !json.Valid(b) {
		return nil, errors.New("Billing captured receipt is invalid")
	}
	if r.StatusCode != http.StatusOK {
		var p struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(b, &p)
		return nil, &BillingHTTPError{Status: r.StatusCode, Code: p.Code, RetryAfter: r.Header.Get("Retry-After")}
	}
	return b, nil
}

type SnapshotBillingClient interface {
	ReservePriced(context.Context, uuid.UUID, uuid.UUID, int64, uuid.UUID, HoldPricingContext) (BillingHold, error)
}

type cancellationBillingClient interface {
	Reserve(context.Context, uuid.UUID, uuid.UUID, int64, uuid.UUID) (BillingHold, error)
	Release(context.Context, uuid.UUID, uuid.UUID, string) error
}
