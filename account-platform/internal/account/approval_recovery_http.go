package account

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type approvalRecoveryControlView struct {
	ControlID               string                  `json:"control_id"`
	SchemaVersion           int                     `json:"schema_version"`
	Mode                    AdminApprovalMode       `json:"mode"`
	ApprovalWireVersion     int                     `json:"approval_wire_version"`
	ProofRequired           bool                    `json:"proof_required"`
	AuthorizationRelayState AdminApprovalRelayState `json:"authorization_relay_state"`
	ApprovalEpoch           int64                   `json:"approval_epoch"`
	AuthorizationRelayEpoch int64                   `json:"authorization_relay_epoch"`
	Version                 int64                   `json:"version"`
	UpdatedAt               time.Time               `json:"updated_at"`
}

type approvalRecoveryPlanView struct {
	SchemaVersion  int                         `json:"schema_version"`
	ObservedAt     time.Time                   `json:"observed_at"`
	Control        approvalRecoveryControlView `json:"control"`
	Candidates     []ApprovalRecoveryCandidate `json:"candidates"`
	MoreCandidates bool                        `json:"more_candidates"`
}

type approvalRecoveryConfirmation struct {
	Candidate ApprovalRecoveryCandidate `json:"candidate"`
	Lease     RecoveryQueryLease        `json:"lease"`
}

type approvalRecoverySuccessConfirmer interface {
	ConfirmOriginalApprovalSuccess(context.Context, ApprovalRecoveryCandidate, RecoveryQueryLease, RecoveryResultObservation) error
}

type approvalRecoveryZeroEffectConfirmer interface {
	InvalidateOriginalApprovalZeroEffect(context.Context, ApprovalRecoveryCandidate, RecoveryQueryLease) error
}

// The worker supplies only the original Account binding and its live query
// lease; Account re-reads both owner authorities with its own credentials -
// a worker-supplied verdict is never accepted as terminal authority. Only the
// narrow case of a definite pre-effect failure whose window ended, with an
// ABSENT effect and no positive result, may invalidate.
func (s *Service) internalInvalidateApprovalRecoveryZeroEffect(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	workload, ok := serviceauth.FromContext(r.Context())
	if !ok || workload.ActorType != "SERVICE" || workload.Service != "account-approval-recovery" {
		api.WriteError(w, http.StatusForbidden, "SERVICE_AUTH_FORBIDDEN", "recovery service identity is required", httpserver.RequestID(r.Context()))
		return
	}
	confirmer, ok := s.repository.(approvalRecoveryZeroEffectConfirmer)
	if !ok || s.recoveryResults == nil || s.recoveryEffects == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_RECOVERY_UNAVAILABLE", "approval recovery is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.Header.Get("Content-Type") != "application/json" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_RECOVERY_INVALIDATION", "invalid recovery invalidation", httpserver.RequestID(r.Context()))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, (16<<10)+1))
	if err != nil || len(raw) > 16<<10 || !uniqueRecoveryJSONKeys(raw) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_RECOVERY_INVALIDATION", "invalid recovery invalidation", httpserver.RequestID(r.Context()))
		return
	}
	var body approvalRecoveryConfirmation
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF || !validRecoveryInspectionCandidate(body.Candidate) ||
		body.Candidate.OriginalKey == "" || body.Lease.RequestID != body.Candidate.RequestID || body.Lease.Token == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_RECOVERY_INVALIDATION", "invalid recovery invalidation", httpserver.RequestID(r.Context()))
		return
	}
	// A positive result always wins: it is never invalidated from this surface.
	result, err := s.recoveryResults.ObserveRecoveryResult(r.Context(), body.Candidate)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_RECOVERY_UNAVAILABLE", "approval recovery is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if result.State != "UNCONFIRMED" {
		api.WriteError(w, http.StatusConflict, "APPROVAL_RECOVERY_UNCONFIRMED", "original result is not unconfirmed", httpserver.RequestID(r.Context()))
		return
	}
	observation, err := s.recoveryEffects.Observe(r.Context(), body.Candidate.RequestID, body.Candidate.CommandType, body.Candidate.CommandFingerprint)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_RECOVERY_UNAVAILABLE", "approval recovery is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if observation.State != approvaleffect.StateAbsent || observation.ApprovalRequestID != body.Candidate.RequestID ||
		observation.CommandType != body.Candidate.CommandType {
		api.WriteError(w, http.StatusConflict, "APPROVAL_RECOVERY_EFFECT_OBSERVED", "an effect was observed for the original execution", httpserver.RequestID(r.Context()))
		return
	}
	if err := confirmer.InvalidateOriginalApprovalZeroEffect(r.Context(), body.Candidate, body.Lease); err != nil {
		if errors.Is(err, ErrApprovalRecoveryZeroEffectConflict) {
			api.WriteError(w, http.StatusConflict, "APPROVAL_RECOVERY_CONFLICT", "original recovery binding changed", httpserver.RequestID(r.Context()))
			return
		}
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_RECOVERY_UNAVAILABLE", "approval recovery is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// The worker supplies only the original Account binding and its live query
// lease. Account re-reads the positive result with its own domain credential;
// a worker-supplied result is never accepted as terminal authority.
func (s *Service) internalConfirmApprovalRecovery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	workload, ok := serviceauth.FromContext(r.Context())
	if !ok || workload.ActorType != "SERVICE" || workload.Service != "account-approval-recovery" {
		api.WriteError(w, http.StatusForbidden, "SERVICE_AUTH_FORBIDDEN", "recovery service identity is required", httpserver.RequestID(r.Context()))
		return
	}
	confirmer, ok := s.repository.(approvalRecoverySuccessConfirmer)
	if !ok || s.recoveryResults == nil {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_RECOVERY_UNAVAILABLE", "approval recovery is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery || r.Header.Get("Content-Type") != "application/json" {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_RECOVERY_CONFIRMATION", "invalid recovery confirmation", httpserver.RequestID(r.Context()))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, (16<<10)+1))
	if err != nil || len(raw) > 16<<10 || !uniqueRecoveryJSONKeys(raw) {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_RECOVERY_CONFIRMATION", "invalid recovery confirmation", httpserver.RequestID(r.Context()))
		return
	}
	var body approvalRecoveryConfirmation
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF || !validRecoveryInspectionCandidate(body.Candidate) ||
		body.Candidate.OriginalKey == "" || body.Lease.RequestID != body.Candidate.RequestID || body.Lease.Token == uuid.Nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_RECOVERY_CONFIRMATION", "invalid recovery confirmation", httpserver.RequestID(r.Context()))
		return
	}
	result, err := s.recoveryResults.ObserveRecoveryResult(r.Context(), body.Candidate)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_RECOVERY_UNAVAILABLE", "approval recovery is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if result.State != "SUCCEEDED" {
		api.WriteError(w, http.StatusConflict, "APPROVAL_RECOVERY_UNCONFIRMED", "original result remains unconfirmed", httpserver.RequestID(r.Context()))
		return
	}
	if err := confirmer.ConfirmOriginalApprovalSuccess(r.Context(), body.Candidate, body.Lease, result); err != nil {
		if errors.Is(err, ErrApprovalRecoveryConfirmConflict) {
			api.WriteError(w, http.StatusConflict, "APPROVAL_RECOVERY_CONFLICT", "original recovery binding changed", httpserver.RequestID(r.Context()))
			return
		}
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_RECOVERY_UNAVAILABLE", "approval recovery is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Only the dedicated recovery workload may read the original execution
// references. This endpoint never acquires a lease or changes Account state.
func (s *Service) internalApprovalRecoveryPlan(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	workload, ok := serviceauth.FromContext(r.Context())
	if !ok || workload.ActorType != "SERVICE" || workload.Service != "account-approval-recovery" {
		api.WriteError(w, http.StatusForbidden, "SERVICE_AUTH_FORBIDDEN", "recovery service identity is required", httpserver.RequestID(r.Context()))
		return
	}
	query := r.URL.Query()
	limitValues := query["limit"]
	if len(query) != 1 || len(limitValues) != 1 {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_RECOVERY_PLAN", "invalid recovery plan limit", httpserver.RequestID(r.Context()))
		return
	}
	limit, err := strconv.Atoi(limitValues[0])
	if err != nil || limit < 1 || limit > 200 || strconv.Itoa(limit) != limitValues[0] {
		api.WriteError(w, http.StatusBadRequest, "INVALID_APPROVAL_RECOVERY_PLAN", "invalid recovery plan limit", httpserver.RequestID(r.Context()))
		return
	}
	planner, ok := s.repository.(ApprovalRecoveryPlanner)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_RECOVERY_UNAVAILABLE", "approval recovery is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	plan, err := planner.PlanProofApprovalRecovery(r.Context(), limit)
	if err != nil {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_RECOVERY_UNAVAILABLE", "approval recovery is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	if validateAdminApprovalControl(plan.Control) != nil || plan.ObservedAt.IsZero() || len(plan.Candidates) > limit {
		api.WriteError(w, http.StatusServiceUnavailable, "APPROVAL_RECOVERY_UNAVAILABLE", "approval recovery is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	api.WriteJSON(w, http.StatusOK, approvalRecoveryPlanView{
		SchemaVersion: 1,
		ObservedAt:    plan.ObservedAt,
		Control: approvalRecoveryControlView{
			ControlID:               plan.Control.ControlID,
			SchemaVersion:           plan.Control.SchemaVersion,
			Mode:                    plan.Control.Mode,
			ApprovalWireVersion:     plan.Control.ApprovalWireVersion,
			ProofRequired:           plan.Control.ProofRequired,
			AuthorizationRelayState: plan.Control.AuthorizationRelayState,
			ApprovalEpoch:           plan.Control.ApprovalEpoch,
			AuthorizationRelayEpoch: plan.Control.AuthorizationRelayEpoch,
			Version:                 plan.Control.Version,
			UpdatedAt:               plan.Control.UpdatedAt,
		},
		Candidates:     plan.Candidates,
		MoreCandidates: plan.MoreCandidates,
	})
}
