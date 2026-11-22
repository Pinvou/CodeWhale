package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type overviewRepository struct {
	personalBoundaryRepository
	grants             map[string]bool
	authorizationError error
	readError          error
	reads              int
}

func (r *overviewRepository) Authorize(_ context.Context, _ identity.Principal, permission string, _, _ *uuid.UUID) (AuthorizationDecision, error) {
	if r.authorizationError != nil {
		return AuthorizationDecision{}, r.authorizationError
	}
	if !r.grants[permission] {
		return AuthorizationDecision{}, ErrForbidden
	}
	return AuthorizationDecision{Allowed: true, PersonalAuthorizationVersion: 2, PlatformAuthorizationVersion: 3}, nil
}

func (r *overviewRepository) AdminOverview(context.Context) (AdminOverview, error) {
	r.reads++
	return AdminOverview{Users: 7, ActiveUsers: 4, DisabledUsers: 2, Organizations: 3, ActiveMemberships: 5, PendingInvitations: 1}, r.readError
}

func TestRestoredAdminOverviewUsesCurrentPermissionsAndRedactsDomains(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		users, organizations bool
		authError, readError error
		status, reads        int
		fields               int
	}{
		{"users only", true, false, nil, nil, 200, 1, 3},
		{"organizations only", false, true, nil, nil, 200, 1, 3},
		{"both domains", true, true, nil, nil, 200, 1, 6},
		{"denied", false, false, nil, nil, 403, 0, 0},
		{"authorization unavailable", true, true, errors.New("controlled authorization failure"), nil, 503, 0, 0},
		{"read unavailable", true, true, nil, errors.New("controlled read failure"), 500, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &overviewRepository{grants: map[string]bool{PermissionPlatformUsersRead: tc.users, PermissionPlatformOrganizationsRead: tc.organizations}, authorizationError: tc.authError, readError: tc.readError}
			handler := NewService(repo, nil).Handler(func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "issuer", Subject: "subject"}, nil
			})
			req := httptest.NewRequest(http.MethodGet, "/admin/v1/overview", nil)
			req.Header.Set("Authorization", "Bearer fixture")
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, req)
			if out.Code != tc.status || repo.reads != tc.reads {
				t.Fatalf("status=%d reads=%d", out.Code, repo.reads)
			}
			if tc.status != 200 {
				return
			}
			var value map[string]int64
			if err := json.Unmarshal(out.Body.Bytes(), &value); err != nil {
				t.Fatal(err)
			}
			if len(value) != tc.fields {
				t.Fatalf("unexpected fields: %v", value)
			}
			_, users := value["users"]
			_, organizations := value["organizations"]
			if users != tc.users || organizations != tc.organizations || users && value["users"] != 7 || organizations && value["organizations"] != 3 {
				t.Fatal("domain projection mismatch")
			}
			if out.Header().Get("Cache-Control") != "no-store" || out.Header().Get("X-Platform-Authorization-Version") != "3" {
				t.Fatal("missing response authority or cache policy")
			}
		})
	}
}
