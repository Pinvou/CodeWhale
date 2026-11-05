package account

import (
	"context"
	"net/http"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/api"
	platformauthorization "github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
)

// This projection restores the original read-only workbench. Permissions are
// evaluated by requireAnyPermission and each domain is independently redacted.
type AdminOverview struct {
	Users              int64
	ActiveUsers        int64
	DisabledUsers      int64
	Organizations      int64
	ActiveMemberships  int64
	PendingInvitations int64
}

type adminOverviewRepository interface {
	AdminOverview(context.Context) (AdminOverview, error)
}

func (s *Service) adminOverview(w http.ResponseWriter, r *http.Request) {
	repository, ok := s.repository.(adminOverviewRepository)
	if !ok {
		api.WriteError(w, http.StatusServiceUnavailable, "ADMIN_UNAVAILABLE", "account overview is unavailable", httpserver.RequestID(r.Context()))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	result, err := repository.AdminOverview(ctx)
	if err != nil {
		s.writeError(w, r, err)
		return
	}
	response := make(map[string]int64, 6)
	if platformauthorization.GrantedPermission(r.Context(), PermissionPlatformUsersRead) {
		response["users"], response["active_users"], response["disabled_users"] = result.Users, result.ActiveUsers, result.DisabledUsers
	}
	if platformauthorization.GrantedPermission(r.Context(), PermissionPlatformOrganizationsRead) {
		response["organizations"], response["active_memberships"], response["pending_invitations"] = result.Organizations, result.ActiveMemberships, result.PendingInvitations
	}
	w.Header().Set("Cache-Control", "no-store")
	api.WriteJSON(w, http.StatusOK, response)
}

func (r *PostgresRepository) AdminOverview(ctx context.Context) (AdminOverview, error) {
	var result AdminOverview
	err := r.pool.QueryRow(ctx, `SELECT
 (SELECT count(*) FROM users WHERE status<>'DELETED'),
 (SELECT count(*) FROM users WHERE status='ACTIVE'),
 (SELECT count(*) FROM users WHERE status='DISABLED'),
 (SELECT count(*) FROM organizations WHERE status<>'DELETED'),
 (SELECT count(*) FROM memberships WHERE status='ACTIVE'),
 (SELECT count(*) FROM invitations WHERE status='PENDING' AND expires_at>now())`).Scan(
		&result.Users, &result.ActiveUsers, &result.DisabledUsers,
		&result.Organizations, &result.ActiveMemberships, &result.PendingInvitations,
	)
	return result, err
}
