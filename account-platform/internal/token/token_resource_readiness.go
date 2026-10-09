package token

import (
	"context"
	"errors"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
)

const (
	tokenResourceModeSnapshot = "SNAPSHOT"
	tokenResourceModeLegacy   = "LEGACY_BOUNDED"
	tokenResourceModeBlocked  = "BLOCKED"
)

type tokenResourceReadiness struct {
	SchemaVersion                 int    `json:"schema_version"`
	TokenAccountsMode             string `json:"token_accounts_mode"`
	PaginationEnabled             bool   `json:"pagination_enabled"`
	AuthorizationVersionAvailable bool   `json:"authorization_version_available"`
	GovernanceState               string `json:"governance_state"`
	GovernanceEpoch               int64  `json:"governance_epoch"`
	RecoveryQueueAvailable        bool   `json:"recovery_queue_available"`
	RecoveryUnavailableReason     string `json:"recovery_unavailable_reason,omitempty"`
}

func buildTokenResourceReadiness(authorizationVersionAvailable, paginationEnabled bool, paginationErr error, activation TokenCommandGovernanceActivation, activationErr error) tokenResourceReadiness {
	projection := tokenResourceReadiness{
		SchemaVersion:                 1,
		TokenAccountsMode:             tokenResourceModeBlocked,
		AuthorizationVersionAvailable: authorizationVersionAvailable,
		GovernanceState:               "UNAVAILABLE",
		RecoveryUnavailableReason:     "AUTHORIZATION_VERSION_INVALID",
	}
	if !authorizationVersionAvailable {
		return projection
	}
	projection.TokenAccountsMode = tokenResourceModeLegacy
	projection.RecoveryUnavailableReason = "PAGINATION_DISABLED"
	if paginationErr != nil {
		projection.TokenAccountsMode = tokenResourceModeBlocked
		projection.RecoveryUnavailableReason = "PAGINATION_UNAVAILABLE"
	} else if paginationEnabled {
		projection.PaginationEnabled = true
		projection.TokenAccountsMode = tokenResourceModeSnapshot
		projection.RecoveryUnavailableReason = "GOVERNANCE_UNAVAILABLE"
	}
	if activationErr == nil && (activation.State == "PRE_ACTIVATION" || activation.State == "ACTIVE" || activation.State == "SAFE_ROLLBACK") {
		projection.GovernanceState = activation.State
		projection.GovernanceEpoch = activation.Epoch
		known := activation.State == "PRE_ACTIVATION" && activation.Epoch == 0 || (activation.State == "ACTIVE" || activation.State == "SAFE_ROLLBACK") && activation.Epoch > 0
		if projection.PaginationEnabled && known {
			projection.RecoveryQueueAvailable = true
			projection.RecoveryUnavailableReason = ""
		} else if projection.PaginationEnabled {
			projection.RecoveryUnavailableReason = "GOVERNANCE_INACTIVE"
		}
	}
	return projection
}

func (r *PostgresRepository) adminPaginationReadiness(ctx context.Context) (bool, error) {
	if r == nil || !r.adminPagination.enabled || len(r.adminPagination.cursorHMACKey) < 32 {
		return false, nil
	}
	var accepting bool
	err := r.pool.QueryRow(ctx, `SELECT accepting_new_snapshots FROM token_admin_pagination_control WHERE singleton=TRUE`).Scan(&accepting)
	if err != nil {
		return false, err
	}
	if !accepting {
		return false, ErrAdminPaginationSnapshotUnavailable
	}
	return true, nil
}

func (s *Service) adminTokenResourceReadiness(w http.ResponseWriter, r *http.Request) {
	_, authorizationErr := adminAuthorizationVersion(w)
	if authorizationErr != nil {
		w.Header().Set("Cache-Control", "no-store")
		api.WriteJSON(w, http.StatusOK, buildTokenResourceReadiness(false, false, nil, TokenCommandGovernanceActivation{}, errors.New("governance not evaluated")))
		return
	}
	var paginationEnabled bool
	paginationErr := ErrAdminPaginationSnapshotUnavailable
	if repository, ok := s.repository.(*PostgresRepository); ok {
		paginationEnabled, paginationErr = repository.adminPaginationReadiness(r.Context())
	}
	activation := TokenCommandGovernanceActivation{}
	activationErr := errors.New("governance unavailable")
	if s.commandGovernanceActivation != nil {
		activation, activationErr = s.commandGovernanceActivation.CurrentTokenCommandGovernanceActivation(r.Context())
	}
	projection := buildTokenResourceReadiness(true, paginationEnabled, paginationErr, activation, activationErr)
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, projection)
}
