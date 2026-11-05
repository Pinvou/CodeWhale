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

type adminOrganizationDetailRepository interface {
	AdminOrganizationDetail(context.Context, uuid.UUID) (Organization, error)
}

func (s *Service) adminOrganizationDetail(w http.ResponseWriter, r *http.Request) {
	repository, available := s.repository.(adminOrganizationDetailRepository)
	if !available {
		api.WriteError(w, http.StatusServiceUnavailable, "ORGANIZATION_ADMIN_UNAVAILABLE", "organization details are unavailable", httpserver.RequestID(r.Context()))
		return
	}
	id, valid := pathUUID(w, r, "organization_id")
	if !valid {
		return
	}
	item, err := repository.AdminOrganizationDetail(r.Context(), id)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, item)
}

func (r *PostgresRepository) AdminOrganizationDetail(ctx context.Context, id uuid.UUID) (Organization, error) {
	var item Organization
	err := r.pool.QueryRow(ctx, `SELECT id,name,status,version FROM organizations WHERE id=$1`, id).Scan(&item.ID, &item.Name, &item.Status, &item.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return Organization{}, ErrNotFound
	}
	return item, err
}
