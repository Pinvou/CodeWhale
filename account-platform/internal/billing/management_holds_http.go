package billing

import (
	"context"
	"encoding/json"

	"net/http"

	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"

	"github.com/google/uuid"
)

func (s *Service) reserve(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(managementHoldRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "HOLD_UNAVAILABLE", "hold service is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var body struct {
		UsageRequestID    uuid.UUID       `json:"usage_request_id"`
		WalletID          uuid.UUID       `json:"wallet_id"`
		Maximum           int64           `json:"maximum_microcredits"`
		PriceVersionID    *uuid.UUID      `json:"price_version_id"`
		LeaseSeconds      int             `json:"lease_seconds"`
		OrganizationID    *uuid.UUID      `json:"organization_id"`
		MemberUserID      *uuid.UUID      `json:"member_user_id"`
		MembershipVersion int64           `json:"membership_version"`
		ModelID           string          `json:"model_id"`
		ProtocolCode      string          `json:"protocol_code"`
		FormulaVersionID  uuid.UUID       `json:"formula_version_id"`
		PricingSnapshot   json.RawMessage `json:"pricing_snapshot"`
		RequestSnapshot   json.RawMessage `json:"request_snapshot"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.UsageRequestID == uuid.Nil || body.WalletID == uuid.Nil || body.Maximum < 0 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid credit reservation", httpserver.RequestID(r.Context()))
		return
	}
	if body.LeaseSeconds == 0 {
		body.LeaseSeconds = 300
	}
	if body.LeaseSeconds < 30 || body.LeaseSeconds > 1800 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "hold lease must be between 30 and 1800 seconds", httpserver.RequestID(r.Context()))
		return
	}
	var hold Hold
	var err error
	var pricing *HoldPricingContext
	if strings.TrimSpace(body.ModelID) != "" || strings.TrimSpace(body.ProtocolCode) != "" || body.FormulaVersionID != uuid.Nil || len(body.PricingSnapshot) > 0 || len(body.RequestSnapshot) > 0 {
		pricing = &HoldPricingContext{ModelID: strings.TrimSpace(body.ModelID), ProtocolCode: strings.TrimSpace(body.ProtocolCode), FormulaVersionID: body.FormulaVersionID, PricingSnapshot: body.PricingSnapshot, RequestSnapshot: body.RequestSnapshot}
		if pricing.ModelID == "" || pricing.ProtocolCode == "" || pricing.FormulaVersionID == uuid.Nil || !json.Valid(pricing.PricingSnapshot) || !json.Valid(pricing.RequestSnapshot) {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "incomplete hold pricing context", httpserver.RequestID(r.Context()))
			return
		}
	}
	if body.OrganizationID != nil || body.MemberUserID != nil {
		member, ok := s.repository.(interface {
			ReserveMemberQuota(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, int64, int64, *uuid.UUID, time.Time) (Hold, error)
			ReserveMemberQuotaPriced(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, int64, int64, *uuid.UUID, time.Time, *HoldPricingContext) (Hold, error)
		})
		if !ok || body.OrganizationID == nil || *body.OrganizationID == uuid.Nil || body.MemberUserID == nil || *body.MemberUserID == uuid.Nil || body.MembershipVersion < 1 {
			api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid member quota reservation", httpserver.RequestID(r.Context()))
			return
		}
		if pricing != nil {
			hold, err = member.ReserveMemberQuotaPriced(r.Context(), body.UsageRequestID, body.WalletID, *body.OrganizationID, *body.MemberUserID, body.MembershipVersion, body.Maximum, body.PriceVersionID, s.now().Add(time.Duration(body.LeaseSeconds)*time.Second), pricing)
		} else {
			hold, err = member.ReserveMemberQuota(r.Context(), body.UsageRequestID, body.WalletID, *body.OrganizationID, *body.MemberUserID, body.MembershipVersion, body.Maximum, body.PriceVersionID, s.now().Add(time.Duration(body.LeaseSeconds)*time.Second))
		}
	} else if pricing != nil {
		hold, err = repository.ReservePriced(r.Context(), body.UsageRequestID, body.WalletID, body.Maximum, body.PriceVersionID, s.now().Add(time.Duration(body.LeaseSeconds)*time.Second), pricing)
	} else {
		hold, err = repository.Reserve(r.Context(), body.UsageRequestID, body.WalletID, body.Maximum, body.PriceVersionID, s.now().Add(time.Duration(body.LeaseSeconds)*time.Second))
	}

	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, hold)
}

func (s *Service) release(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(managementHoldRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "HOLD_UNAVAILABLE", "hold service is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	var body struct {
		UsageRequestID uuid.UUID `json:"usage_request_id"`
		HoldID         uuid.UUID `json:"hold_id"`
		Reason         string    `json:"reason"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.UsageRequestID == uuid.Nil || body.HoldID == uuid.Nil || strings.TrimSpace(body.Reason) == "" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid hold release", httpserver.RequestID(r.Context()))
		return
	}
	hold, err := repository.Release(r.Context(), body.UsageRequestID, body.HoldID, body.Reason)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, hold)
}

type managementHoldRepository interface {
	Reserve(context.Context, uuid.UUID, uuid.UUID, int64, *uuid.UUID, time.Time) (Hold, error)
	ReservePriced(context.Context, uuid.UUID, uuid.UUID, int64, *uuid.UUID, time.Time, *HoldPricingContext) (Hold, error)
	Release(context.Context, uuid.UUID, uuid.UUID, string) (Hold, error)
}
