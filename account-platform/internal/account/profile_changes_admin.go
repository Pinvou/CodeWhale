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
	if err = s.profiles.retryRepair(r.Context(), mustPrincipal(r.Context()), id); err != nil {
		s.writeProfileError(w, r, err)
		return
	}
	api.WriteJSON(w, http.StatusAccepted, map[string]any{"profile_change_request_id": id, "status": "RETRY"})
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
func (s *ProfileChangeService) retryRepair(ctx context.Context, principal identity.Principal, id uuid.UUID) error {
	return s.store.RetryProfileRepair(ctx, principal, id)
}
func (r *PostgresRepository) ListProfileRepairs(ctx context.Context) ([]profileChangeRepairItem, error) {
	rows, err := r.pool.Query(ctx, `SELECT id,user_id,field,status,attempts,COALESCE(last_error_code,'') FROM profile_change_requests WHERE status IN ('RETRY','REPAIR_REQUIRED') ORDER BY updated_at DESC LIMIT 100`)
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
func (r *PostgresRepository) RetryProfileRepair(ctx context.Context, principal identity.Principal, id uuid.UUID) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	actor, err := activeUserID(ctx, tx, principal)
	if err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `UPDATE profile_change_requests SET status='RETRY',next_attempt_at=$2,last_error_code=NULL,version=version+1,updated_at=$2 WHERE id=$1 AND status IN ('RETRY','REPAIR_REQUIRED')`, id, r.now())
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err = appendAudit(ctx, tx, actor, principal.SessionID, nil, "account.profile_change.repair_retry", "profile_change", id.String(), "SUCCEEDED", map[string]any{"direction": "FORWARD"}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
