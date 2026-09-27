package account

import (
	"context"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func (s *Service) adminProfileChanges(w http.ResponseWriter, r *http.Request) {
	items, err := s.profiles.listRepair(r.Context())
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Service) adminRetryProfileChange(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("request_id"))
	if err != nil {
		api.WriteError(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid profile change request", httpserver.RequestID(r.Context()))
		return
	}
	result, err := s.profiles.retryRepair(r.Context(), mustPrincipal(r.Context()), id)
	if err != nil {
		s.writeProfileError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusAccepted, result)
}

type profileChangeRepairItem struct {
	ID       uuid.UUID `json:"profile_change_request_id"`
	UserID   uuid.UUID `json:"user_id"`
	Field    string    `json:"field"`
	Status   string    `json:"status"`
	Attempts int       `json:"attempts"`
	Reason   string    `json:"reason,omitempty"`
}

func (s *ProfileChangeService) listRepair(ctx context.Context) ([]profileChangeRepairItem, error) {
	return s.store.ListProfileRepairs(ctx)
}

type profileRepairRetryResult struct {
	RequestID    uuid.UUID `json:"request_id"`
	ResourceType string    `json:"resource_type"`
	Action       string    `json:"action"`
	Status       string    `json:"status"`
}

func (s *ProfileChangeService) retryRepair(ctx context.Context, principal identity.Principal, id uuid.UUID) (profileRepairRetryResult, error) {
	return s.store.RetryProfileRepair(ctx, principal, id)
}
func (r *PostgresRepository) ListProfileRepairs(ctx context.Context) ([]profileChangeRepairItem, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,user_id,field,status,attempts,reason FROM (
SELECT id,user_id,field,status,attempts,COALESCE(last_error_code,'') AS reason,updated_at FROM profile_change_requests WHERE status IN ('RETRY','REPAIR_REQUIRED')
UNION ALL
SELECT id,user_id,'PASSWORD' AS field,status,attempts,COALESCE(last_error_code,'') AS reason,updated_at FROM password_reset_operations WHERE status='REPAIR_REQUIRED'
) repair ORDER BY updated_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []profileChangeRepairItem{}
	for rows.Next() {
		var item profileChangeRepairItem
		if err := rows.Scan(&item.ID, &item.UserID, &item.Field, &item.Status, &item.Attempts, &item.Reason); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
func (r *PostgresRepository) RetryProfileRepair(ctx context.Context, principal identity.Principal, id uuid.UUID) (profileRepairRetryResult, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return profileRepairRetryResult{}, err
	}
	defer tx.Rollback(ctx)
	actor, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return profileRepairRetryResult{}, err
	}
	now := r.now()
	command, err := tx.Exec(ctx, `UPDATE profile_change_requests SET status='RETRY',next_attempt_at=$2,last_error_code=NULL,version=version+1,updated_at=$2 WHERE id=$1 AND status IN ('RETRY','REPAIR_REQUIRED')`, id, now)
	if err != nil {
		return profileRepairRetryResult{}, err
	}
	resourceType := "profile_change"
	action := "RETRY_SCHEDULED"
	auditAction := "account.profile_change.repair_retry_scheduled"
	if command.RowsAffected() == 0 {
		command, err = tx.Exec(ctx, `UPDATE password_reset_operations SET status='RETRY',next_attempt_at=$2,updated_at=$2 WHERE id=$1 AND status='REPAIR_REQUIRED' AND last_error_code<>'LEGACY_SESSION_REVOCATION_UNCERTAIN'`, id, now)
		if err != nil {
			return profileRepairRetryResult{}, err
		}
		if command.RowsAffected() == 0 {
			var legacy bool
			if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM password_reset_operations WHERE id=$1 AND status='REPAIR_REQUIRED' AND last_error_code='LEGACY_SESSION_REVOCATION_UNCERTAIN')`, id).Scan(&legacy); err != nil {
				return profileRepairRetryResult{}, err
			}
			if legacy {
				return profileRepairRetryResult{}, ErrConflict
			}
			return profileRepairRetryResult{}, ErrNotFound
		}
		resourceType = "password_reset"
		action = "ATOMIC_PASSWORD_RECOVERY_SCHEDULED"
		auditAction = "account.password_reset.repair_recovery_scheduled"
	}
	if err = appendAudit(ctx, tx, actor, principal.SessionID, nil, auditAction, resourceType, id.String(), "SUCCEEDED", map[string]any{"direction": "FORWARD", "action": action}); err != nil {
		return profileRepairRetryResult{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return profileRepairRetryResult{}, err
	}
	return profileRepairRetryResult{RequestID: id, ResourceType: resourceType, Action: action, Status: "RETRY"}, nil
}
