package billing

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"net/http"

	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"

	"github.com/google/uuid"
)

func (s *Service) extendHold(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UsageRequestID         uuid.UUID `json:"usage_request_id"`
		HoldID                 uuid.UUID `json:"hold_id"`
		LeaseSeconds           int       `json:"lease_seconds"`
		AdditionalMicrocredits int64     `json:"additional_microcredits"`
	}
	if err := api.DecodeJSON(w, r, &body); err != nil || body.UsageRequestID == uuid.Nil || body.HoldID == uuid.Nil || body.AdditionalMicrocredits < 0 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid hold extension", httpserver.RequestID(r.Context()))
		return
	}
	if body.LeaseSeconds == 0 {
		body.LeaseSeconds = 900
	}
	if body.LeaseSeconds < 30 || body.LeaseSeconds > 1800 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "hold lease must be between 30 and 1800 seconds", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(interface {
		ExtendHold(context.Context, uuid.UUID, uuid.UUID, time.Time, int64) (Hold, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "HOLD_EXTENSION_UNAVAILABLE", "hold extension is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	hold, err := repository.ExtendHold(r.Context(), body.UsageRequestID, body.HoldID, s.now().Add(time.Duration(body.LeaseSeconds)*time.Second), body.AdditionalMicrocredits)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, hold)
}
func (s *Service) commit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		UsageRequestID uuid.UUID      `json:"usage_request_id"`
		HoldID         uuid.UUID      `json:"hold_id"`
		Actual         int64          `json:"actual_microcredits"`
		UsageSnapshot  map[string]any `json:"usage_snapshot"`
	}
	if err := decodeUsageSettlementJSON(w, r, &body); err != nil || body.UsageRequestID == uuid.Nil || body.HoldID == uuid.Nil || body.Actual < 0 || body.UsageSnapshot == nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid usage settlement", httpserver.RequestID(r.Context()))
		return
	}
	repository, ok := s.repository.(interface {
		Commit(context.Context, uuid.UUID, uuid.UUID, int64, map[string]any) (Hold, error)
	})
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "HOLD_SETTLEMENT_UNAVAILABLE", "hold settlement is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	hold, err := repository.Commit(r.Context(), body.UsageRequestID, body.HoldID, body.Actual, body.UsageSnapshot)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusOK, hold)
}

// Settlement snapshots carry exact supplier integers. Keep the existing body
// limit and strict single-value request contract while avoiding float64 maps.
func decodeUsageSettlementJSON(w http.ResponseWriter, r *http.Request, target any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("request body must contain one JSON value")
		}
		return err
	}
	return nil
}
