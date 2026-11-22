package account

import (
	"context"
	"errors"
	"net/http"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type adminUserDetailRepository interface {
	AdminUserDetail(context.Context, uuid.UUID) (AdminUser, error)
}

func (s *Service) adminUserDetail(w http.ResponseWriter, r *http.Request) {
	repository, available := s.repository.(adminUserDetailRepository)
	if !available {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "user details are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	id, valid := pathUUID(w, r, "user_id")
	if !valid {
		return
	}
	item, err := repository.AdminUserDetail(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, item)
}

func (r *PostgresRepository) AdminUserDetail(ctx context.Context, id uuid.UUID) (AdminUser, error) {
	var item AdminUser
	err := r.pool.QueryRow(ctx, `SELECT u.id,u.user_code,u.username,u.display_name,u.status,
 (SELECT max(s.last_seen_at) FROM user_sessions s WHERE s.user_id=u.id),u.created_at,u.updated_at
 FROM users u WHERE u.id=$1 AND u.status<>'DELETED'`, id).Scan(
		&item.ID, &item.UserCode, &item.Username, &item.DisplayName, &item.Status, &item.LastSeenAt, &item.CreatedAt, &item.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return AdminUser{}, ErrNotFound
	}
	if err != nil {
		return AdminUser{}, err
	}
	item.UserCode = FormatUserCode(item.UserCode)
	return item, nil
}
