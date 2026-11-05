package account

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type authorizationFailureRepository struct {
	personalBoundaryRepository
	failure      error
	deniedAudits int
	allowed      bool
	ownerCalls   int
}

func (r *authorizationFailureRepository) Authorize(context.Context, identity.Principal, string, *uuid.UUID, *uuid.UUID) (AuthorizationDecision, error) {
	return AuthorizationDecision{Allowed: r.allowed, PersonalAuthorizationVersion: 2, PlatformAuthorizationVersion: 3}, r.failure
}
func (r *authorizationFailureRepository) AdminUsers(context.Context, AdminUserFilter) (AdminUserPage, error) {
	r.ownerCalls++
	return AdminUserPage{Items: []AdminUser{}}, nil
}
func (r *authorizationFailureRepository) RecordPermissionDenial(context.Context, identity.Principal, string, string) error {
	r.deniedAudits++
	return nil
}
func TestF01PermissionFailureClassification(t *testing.T) {
	for _, item := range []struct {
		name           string
		failure        error
		status, audits int
		allowed        bool
		code           string
	}{
		{"database unavailable", errors.New("database unavailable"), 503, 0, false, "AUTHORIZATION_UNAVAILABLE"},
		{"cancelled", context.Canceled, 503, 0, false, "AUTHORIZATION_UNAVAILABLE"},
		{"typed denial", ErrForbidden, 403, 1, false, "PERMISSION_DENIED"},
		{"wrapped denial", fmt.Errorf("principal: %w", ErrForbidden), 403, 1, false, "PERMISSION_DENIED"},
		{"policy denial", nil, 403, 1, false, "PERMISSION_DENIED"},
		{"allowed", nil, 200, 0, true, ""},
	} {
		t.Run(item.name, func(t *testing.T) {
			repo := &authorizationFailureRepository{failure: item.failure, allowed: item.allowed}
			handler := NewService(repo, nil).Handler(func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "issuer", Subject: "subject"}, nil
			})
			req := httptest.NewRequest(http.MethodGet, "/admin/v1/users", nil)
			req.Header.Set("Authorization", "Bearer fixture")
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, req)
			calls := 0
			if item.allowed {
				calls = 1
			}
			if out.Code != item.status || repo.deniedAudits != item.audits || repo.ownerCalls != calls {
				t.Fatalf("status=%d audits=%d owner calls=%d", out.Code, repo.deniedAudits, repo.ownerCalls)
			}
			if item.code != "" && !strings.Contains(out.Body.String(), `"code":"`+item.code+`"`) {
				t.Fatalf("wrong error code: %s", out.Body.String())
			}
			if item.allowed && (out.Header().Get("X-Personal-Authorization-Version") != "2" || out.Header().Get("X-Platform-Authorization-Version") != "3") {
				t.Fatalf("missing authorization versions: %v", out.Header())
			}
		})
	}
}
