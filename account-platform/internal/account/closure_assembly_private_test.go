package account

import (
	"context"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"net/http/httptest"
	"strings"
	"testing"
)

type closureAssemblyRepository struct {
	Repository
	allow                                 bool
	calls                                 int
	permission, key, confirmation, reason string
	id                                    uuid.UUID
}

func (f *closureAssemblyRepository) GetMe(context.Context, identity.Principal) (Me, error) {
	return Me{}, errors.New("controlled context header unavailable")
}
func (f *closureAssemblyRepository) Authorize(_ context.Context, _ identity.Principal, p string, _ *uuid.UUID, _ *uuid.UUID) (AuthorizationDecision, error) {
	f.permission = p
	return AuthorizationDecision{Allowed: f.allow}, nil
}
func (f *closureAssemblyRepository) CreateAccountClosure(_ context.Context, p identity.Principal, key, confirmation string) (AccountClosureRequest, error) {
	f.calls++
	f.key = key
	f.confirmation = confirmation
	return AccountClosureRequest{ID: f.id, Status: "IN_PROGRESS", Stage: "CLOSURE_REQUESTED", Blockers: []AccountClosureBlocker{}}, nil
}
func (f *closureAssemblyRepository) AccountClosureForPrincipal(context.Context, identity.Principal) (AccountClosureRequest, error) {
	f.calls++
	return AccountClosureRequest{ID: f.id, Status: "IN_PROGRESS", Stage: "CLOSURE_REQUESTED", Blockers: []AccountClosureBlocker{}}, nil
}
func (f *closureAssemblyRepository) ListAccountClosureRepairs(context.Context, AccountClosureRepairFilter) ([]AccountClosureRequest, error) {
	f.calls++
	return []AccountClosureRequest{}, nil
}
func (f *closureAssemblyRepository) RetryAccountClosure(_ context.Context, _ identity.Principal, id uuid.UUID, key, reason string) (AccountClosureRequest, error) {
	f.calls++
	f.key = key
	f.reason = reason
	if id != f.id {
		panic("wrong target")
	}
	return AccountClosureRequest{ID: f.id, Status: "RETRY", Stage: "QUIESCING", Blockers: []AccountClosureBlocker{}}, nil
}
func TestClosureActualPersonalAndRepairRouter(t *testing.T) {
	id := uuid.New()
	for _, tc := range []struct {
		name, method, path, body, permission string
		auth, allow                          bool
		status, calls                        int
	}{
		{"request", "POST", "/v1/me/account-closure-requests", `{"confirmation":"CLOSE_MY_ACCOUNT"}`, PermissionPersonalAccountClosureRequest, true, true, 202, 1},
		{"read", "GET", "/v1/me/account-closure-request", "", PermissionPersonalAccountClosureRead, true, true, 200, 1},
		{"head-read", "HEAD", "/v1/me/account-closure-request", "", PermissionPersonalAccountClosureRead, true, true, 200, 1},
		{"list", "GET", "/admin/v1/account-closure-requests", "", PermissionPlatformAccountClosuresRead, true, true, 200, 1},
		{"retry", "POST", "/admin/v1/account-closure-requests/" + id.String() + "/actions/retry", `{"reason":"owned repair reason"}`, PermissionPlatformAccountClosuresRepair, true, true, 200, 1},
		{"malformed", "POST", "/v1/me/account-closure-requests", `{"confirmation":`, PermissionPersonalAccountClosureRequest, true, true, 400, 0},
		{"trailing", "POST", "/v1/me/account-closure-requests", `{"confirmation":"CLOSE_MY_ACCOUNT"}{}`, PermissionPersonalAccountClosureRequest, true, true, 400, 0},
		{"unknownfield", "POST", "/v1/me/account-closure-requests", `{"confirmation":"CLOSE_MY_ACCOUNT","force":true}`, PermissionPersonalAccountClosureRequest, true, true, 400, 0},
		{"unauthenticated", "POST", "/v1/me/account-closure-requests", `{}`, "", false, true, 401, 0},
		{"permission-denied", "POST", "/v1/me/account-closure-requests", `{}`, PermissionPersonalAccountClosureRequest, true, false, 403, 0},
		{"no-admin-delete", "DELETE", "/admin/v1/users/" + id.String(), "", "", true, true, 404, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &closureAssemblyRepository{allow: tc.allow, id: id}
			h := NewService(f, nil).Handler(func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "https://owned.invalid", Subject: "closure-actor", SessionID: "closure-session"}, nil
			})
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			r.Header.Set("Idempotency-Key", "original-closure-key")
			if tc.auth {
				r.Header.Set("Authorization", "Bearer controlled")
			}
			out := httptest.NewRecorder()
			h.ServeHTTP(out, r)
			if out.Code != tc.status || f.calls != tc.calls || f.permission != tc.permission {
				t.Fatalf("status=%d calls=%d permission=%s", out.Code, f.calls, f.permission)
			}
			if tc.name == "request" && (f.key != "original-closure-key" || f.confirmation != "CLOSE_MY_ACCOUNT") {
				t.Fatal("request facts lost")
			}
			if tc.name == "retry" && (f.key != "original-closure-key" || f.reason != "owned repair reason") {
				t.Fatal("repair facts lost")
			}
		})
	}
}
