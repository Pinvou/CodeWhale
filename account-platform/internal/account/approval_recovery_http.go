package account

import (
	"net/http"
	"strconv"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
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
