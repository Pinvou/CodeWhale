package account

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type directoryDetailRepository struct {
	overviewRepository
	missing     bool
	target      uuid.UUID
	detailReads int
}

func (r *directoryDetailRepository) AdminUserDetail(_ context.Context, id uuid.UUID) (AdminUser, error) {
	r.detailReads++
	r.target = id
	if r.missing {
		return AdminUser{}, ErrNotFound
	}
	return AdminUser{ID: id, UserCode: "SAMPLE-001", Status: "ACTIVE"}, nil
}
func (r *directoryDetailRepository) AdminOrganizationDetail(_ context.Context, id uuid.UUID) (Organization, error) {
	r.detailReads++
	r.target = id
	if r.missing {
		return Organization{}, ErrNotFound
	}
	return Organization{ID: id, Name: "Controlled organization", Status: "DISABLED", Version: 3}, nil
}

func TestRestoredDirectoryDetailsRequireTheirOwnReadPermission(t *testing.T) {
	id := uuid.New()
	for _, domain := range []struct{ path, permission, idField string }{{"users", PermissionPlatformUsersRead, "user_id"}, {"organizations", PermissionPlatformOrganizationsRead, "organization_id"}} {
		for _, tc := range []struct {
			name, method, target string
			allowed, missing     bool
			status, reads        int
		}{
			{"allowed", "GET", id.String(), true, false, 200, 1},
			{"denied", "GET", id.String(), false, false, 403, 0},
			{"missing", "GET", id.String(), true, true, 404, 1},
			{"invalid", "GET", "invalid", true, false, 400, 0},
			{"no mutation", "POST", id.String(), true, false, 405, 0},
		} {
			t.Run(domain.path+"/"+tc.name, func(t *testing.T) {
				repo := &directoryDetailRepository{overviewRepository: overviewRepository{grants: map[string]bool{domain.permission: tc.allowed}}, missing: tc.missing}
				handler := NewService(repo, nil).Handler(func(context.Context, string) (identity.Principal, error) {
					return identity.Principal{Issuer: "issuer", Subject: "subject"}, nil
				})
				req := httptest.NewRequest(tc.method, "/admin/v1/"+domain.path+"/"+tc.target, nil)
				req.Header.Set("Authorization", "Bearer fixture")
				out := httptest.NewRecorder()
				handler.ServeHTTP(out, req)
				if out.Code != tc.status || repo.detailReads != tc.reads {
					t.Fatalf("status=%d reads=%d", out.Code, repo.detailReads)
				}
				if tc.status == 200 {
					var value map[string]any
					if err := json.Unmarshal(out.Body.Bytes(), &value); err != nil {
						t.Fatal(err)
					}
					if value[domain.idField] != id.String() || repo.target != id || value["email"] != nil || out.Header().Get("Cache-Control") != "no-store" {
						t.Fatal("detail identity or disclosure mismatch")
					}
				}
			})
		}
	}
}
