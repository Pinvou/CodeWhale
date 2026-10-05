package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type WaiverIntent struct {
	UsageID uuid.UUID `json:"usage_request_id"`
	HoldID  uuid.UUID `json:"hold_id"`
	Reason  string    `json:"reason"`
	Actor   string    `json:"actor"`
}

type WaiverAuthority struct {
	Hold struct {
		ID        uuid.UUID `json:"hold_id"`
		UsageID   uuid.UUID `json:"usage_request_id"`
		WalletID  uuid.UUID `json:"wallet_id"`
		Status    string    `json:"status"`
		Maximum   int64     `json:"max_microcredits,string"`
		Actual    *int64    `json:"actual_microcredits,string,omitempty"`
		ExpiresAt time.Time `json:"expires_at"`
		UpdatedAt time.Time `json:"updated_at"`
	} `json:"hold"`
	UsageSnapshot json.RawMessage `json:"usage_snapshot"`
}

type WaiverBilling interface {
	ResolveWaiver(context.Context, uuid.UUID, WaiverIntent) (WaiverAuthority, error)
}

type WaiverStore interface {
	PrepareWaiver(context.Context, WaiverIntent) (uuid.UUID, WaiverIntent, error)
	FinishWaiver(context.Context, uuid.UUID, WaiverIntent, WaiverAuthority) error
}

func (s *Service) WithWaivers(serviceToken string) *Service {
	s.waiverServiceToken = serviceToken
	return s
}

func (s *Service) adminWaiver(w http.ResponseWriter, r *http.Request) {
	var intent WaiverIntent
	if api.DecodeJSON(w, r, &intent) != nil || intent.HoldID == uuid.Nil || intent.UsageID == uuid.Nil || strings.TrimSpace(intent.Reason) == "" || strings.TrimSpace(intent.Actor) == "" {
		api.WriteError(w, 400, "INVALID_REQUEST", "invalid usage waiver", httpserver.RequestID(r.Context()))
		return
	}
	store, ok := s.repository.(WaiverStore)
	billing, billingOK := s.billing.(WaiverBilling)
	if !ok || !billingOK {
		api.WriteError(w, 503, "INTERNAL_ERROR", "usage waiver unavailable", httpserver.RequestID(r.Context()))
		return
	}
	usageID, intent, err := store.PrepareWaiver(r.Context(), intent)
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			api.WriteError(w, 409, "CONFLICT", "usage cannot be waived while active or already terminal", httpserver.RequestID(r.Context()))
			return
		}
		s.writeError(w, r, err)
		return
	}
	authority, err := billing.ResolveWaiver(r.Context(), usageID, intent)
	if err == nil {
		err = store.FinishWaiver(r.Context(), usageID, intent, authority)
	}
	if err != nil {
		api.WriteError(w, 503, "INTERNAL_ERROR", "usage waiver awaiting authoritative resolution", httpserver.RequestID(r.Context()))
		return
	}
	if authority.Hold.Status != "RELEASED" {
		api.WriteError(w, 409, "CONFLICT", "usage was already charged", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, 200, authority)
}

func (c *HTTPBillingClient) ResolveWaiver(ctx context.Context, usageID uuid.UUID, intent WaiverIntent) (WaiverAuthority, error) {
	payload, err := c.raw(ctx, http.MethodPost, "/internal/v1/holds/waiver", map[string]any{"usage_request_id": usageID, "hold_id": intent.HoldID, "reason": intent.Reason, "actor": intent.Actor}, "")
	if err != nil {
		return WaiverAuthority{}, errors.New("Billing waiver authority unavailable")
	}
	var result WaiverAuthority
	if json.Unmarshal(payload, &result) != nil || result.Hold.ID != intent.HoldID || result.Hold.UsageID != usageID || (result.Hold.Status != "RELEASED" && result.Hold.Status != "COMMITTED") {
		return result, errors.New("invalid Billing waiver authority")
	}
	return result, nil
}

func (r *PostgresRepository) PrepareWaiver(ctx context.Context, intent WaiverIntent) (uuid.UUID, WaiverIntent, error) {
	intent.Reason, intent.Actor = strings.TrimSpace(intent.Reason), strings.TrimSpace(intent.Actor)
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, intent, err
	}
	defer tx.Rollback(ctx)
	var usageID uuid.UUID
	var status string
	var snapshot []byte
	var holdID *uuid.UUID
	err = tx.QueryRow(ctx, `SELECT id,status,hold_id,COALESCE(usage_snapshot,'{}'::jsonb) FROM usage_requests WHERE id=$1 FOR UPDATE`, intent.UsageID).Scan(&usageID, &status, &holdID, &snapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, intent, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, intent, err
	}
	var stored struct {
		Intent *WaiverIntent `json:"admin_waiver_intent"`
		HoldID uuid.UUID     `json:"hold_id"`
	}
	if json.Unmarshal(snapshot, &stored) != nil {
		return usageID, intent, ErrForbidden
	}
	if (holdID != nil && *holdID != intent.HoldID) || (holdID == nil && stored.HoldID != intent.HoldID) {
		return usageID, intent, ErrForbidden
	}
	if stored.Intent != nil {
		// The first operator decision owns the immutable reason/actor on replay.
		if stored.Intent.HoldID != intent.HoldID || stored.Intent.UsageID != intent.UsageID {
			return usageID, intent, ErrForbidden
		}
		return usageID, *stored.Intent, tx.Commit(ctx)
	}
	if status == "UPSTREAM_ACTIVE" || status == "CREATED" || status == "COMMITTED" || status == "RELEASED" || intent.HoldID == uuid.Nil || intent.Reason == "" || intent.Actor == "" {
		return usageID, intent, ErrForbidden
	}
	encoded, _ := json.Marshal(map[string]any{"admin_waiver_intent": intent})
	_, err = tx.Exec(ctx, `UPDATE usage_requests SET hold_id=$4,status='REVIEW',error_class='admin_waiver_pending',usage_snapshot=COALESCE(usage_snapshot,'{}'::jsonb)||$2::jsonb,reconciliation_next_at=$3,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,reconciliation_stopped_at=NULL,updated_at=$3 WHERE id=$1`, usageID, encoded, r.now(), intent.HoldID)
	if err != nil {
		return usageID, intent, err
	}
	return usageID, intent, tx.Commit(ctx)
}

func (r *PostgresRepository) FinishWaiver(ctx context.Context, usageID uuid.UUID, intent WaiverIntent, authority WaiverAuthority) error {
	local, err := r.GetUsage(ctx, usageID)
	if err != nil {
		return err
	}
	if authority.Hold.ID != intent.HoldID || authority.Hold.UsageID != usageID || intent.UsageID != usageID || authority.Hold.WalletID != local.WalletID || authority.Hold.Maximum != local.MaximumMicrocredits || authority.Hold.Actual == nil || *authority.Hold.Actual < 0 || *authority.Hold.Actual > local.MaximumMicrocredits {
		return ErrForbidden
	}
	if authority.Hold.Status == "COMMITTED" {
		if authority.Hold.Actual == nil {
			return ErrForbidden
		}
		var snapshot map[string]any
		decoder := json.NewDecoder(strings.NewReader(string(authority.UsageSnapshot)))
		decoder.UseNumber()
		if decoder.Decode(&snapshot) != nil {
			return ErrForbidden
		}
		if _, nested := snapshot["usage"]; !nested {
			snapshot = map[string]any{"usage": snapshot, "reconciled": true}
		}
		return r.MarkCommitted(ctx, usageID, *authority.Hold.Actual, snapshot)
	}
	var proof struct {
		AdminWaiver bool   `json:"admin_waiver"`
		Reason      string `json:"admin_release_reason"`
		Actor       string `json:"admin_actor"`
	}
	if authority.Hold.Status != "RELEASED" || authority.Hold.Actual == nil || *authority.Hold.Actual != 0 || json.Unmarshal(authority.UsageSnapshot, &proof) != nil || !proof.AdminWaiver || proof.Reason != intent.Reason || proof.Actor != intent.Actor {
		return ErrForbidden
	}
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var status string
	var holdID *uuid.UUID
	var snapshot []byte
	if err = tx.QueryRow(ctx, `SELECT status,hold_id,COALESCE(usage_snapshot,'{}'::jsonb) FROM usage_requests WHERE id=$1 FOR UPDATE`, usageID).Scan(&status, &holdID, &snapshot); err != nil {
		return err
	}
	if holdID == nil || *holdID != intent.HoldID || status == "COMMITTED" || status == "UPSTREAM_ACTIVE" {
		return ErrForbidden
	}
	var stored struct {
		Intent *WaiverIntent `json:"admin_waiver_intent"`
	}
	if json.Unmarshal(snapshot, &stored) != nil || stored.Intent == nil || *stored.Intent != intent {
		return ErrForbidden
	}
	if status == "RELEASED" {
		return tx.Commit(ctx)
	}
	metadata, _ := json.Marshal(map[string]any{"admin_waiver": true, "admin_actor": intent.Actor, "admin_release_reason": intent.Reason})
	now := r.now()
	_, err = tx.Exec(ctx, `UPDATE usage_requests SET status='RELEASED',actual_microcredits=0,error_class='admin_waiver',usage_snapshot=COALESCE(usage_snapshot,'{}'::jsonb)||$2::jsonb,terminal_at=$3,updated_at=$3,reconciliation_next_at=NULL,reconciliation_lease_until=NULL,reconciliation_last_error=NULL,reconciliation_stopped_at=NULL WHERE id=$1`, usageID, metadata, now)
	if err != nil {
		return err
	}
	// An operator waiver is not evidence that a model attempt failed. Preserve
	// its observed outcome/token counts and only close unfinished bookkeeping.
	_, err = tx.Exec(ctx, `UPDATE usage_attempts SET ended_at=COALESCE(ended_at,$2) WHERE usage_request_id=$1`, usageID, now)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}
