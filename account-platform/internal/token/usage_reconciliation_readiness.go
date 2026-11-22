package token

import (
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
)

const (
	usageReconciliationReadinessGovernanceUnavailable = "GOVERNANCE_UNAVAILABLE"
	usageReconciliationReadinessGovernanceInactive    = "GOVERNANCE_INACTIVE"
)

type usageReconciliationCommandReadiness struct {
	SchemaVersion     int    `json:"schema_version"`
	CommandType       string `json:"command_type"`
	GovernanceState   string `json:"governance_state"`
	GovernanceEpoch   int64  `json:"governance_epoch"`
	CommandsEnabled   bool   `json:"commands_enabled"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

func (s *Service) adminUsageReconciliationCommandReadiness(w http.ResponseWriter, r *http.Request) {
	projection := usageReconciliationCommandReadiness{
		SchemaVersion:     1,
		CommandType:       "TOKEN_USAGE_RECONCILIATION_RETRY",
		GovernanceState:   "UNAVAILABLE",
		UnavailableReason: usageReconciliationReadinessGovernanceUnavailable,
	}
	if s.commandGovernanceActivation != nil {
		activation, err := s.commandGovernanceActivation.CurrentTokenCommandGovernanceActivation(r.Context())
		if err == nil {
			projection.GovernanceState = activation.State
			projection.GovernanceEpoch = activation.Epoch
			projection.UnavailableReason = usageReconciliationReadinessGovernanceInactive
			if activation.State == "ACTIVE" && activation.Epoch > 0 {
				projection.CommandsEnabled = true
				projection.UnavailableReason = ""
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, projection)
}
