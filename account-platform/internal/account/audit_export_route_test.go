package account

import (
	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"net/http/httptest"
	"strings"
	"testing"
)

// This route contract deliberately does not fake the concrete PostgreSQL export
// owner. It proves that each installed entry rejects before reaching that owner.
func TestF17AuditRouterPermissionBeforeOwner(t *testing.T) {
	id := uuid.NewString()
	for _, tc := range []struct{ name, method, path, permission string }{
		{"list", "GET", "/admin/v1/audit-logs", PermissionPlatformAuditRead},
		{"detail", "GET", "/admin/v1/audit-logs/" + id, PermissionPlatformAuditRead},
		{"create", "POST", "/admin/v1/audit-exports", PermissionPlatformAuditExport},
		{"status", "GET", "/admin/v1/audit-exports/" + id, PermissionPlatformAuditExport},
		{"ticket", "POST", "/admin/v1/audit-exports/" + id + "/download-tickets", PermissionPlatformAuditExport},
		{"download", "GET", "/admin/v1/audit-exports/" + id + "/download", PermissionPlatformAuditExport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &closureAssemblyRepository{}
			exports, err := NewAuditExportService(&PostgresRepository{}, make([]byte, 32), "test")
			if err != nil {
				t.Fatal(err)
			}
			h := NewService(f, nil).WithAuditExports(exports).Handler(func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "https://owned.invalid", Subject: "audit-actor", SessionID: "audit-session"}, nil
			})
			r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{}`))
			r.Header.Set("Authorization", "Bearer controlled")
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != 403 || f.permission != tc.permission || f.calls != 0 {
				t.Fatalf("permission boundary status=%d permission=%s", w.Code, f.permission)
			}
		})
	}
}

// The existing nil-service route contract is retained: export is unavailable,
// while startup can still assemble the Account router without an export key.
func TestAuditExportDisabledRoutesDoNotReachOwner(t *testing.T) {
	for _, path := range []string{"/admin/v1/audit-exports", "/admin/v1/audit-exports/00000000-0000-4000-8000-000000000001", "/admin/v1/audit-exports/00000000-0000-4000-8000-000000000001/download-tickets", "/admin/v1/audit-exports/00000000-0000-4000-8000-000000000001/download"} {
		for _, method := range []string{"GET", "POST"} {
			t.Run(method+path, func(t *testing.T) {
				repository := &closureAssemblyRepository{}
				handler := NewService(repository, nil).WithAuditExports(nil).Handler(func(context.Context, string) (identity.Principal, error) {
					return identity.Principal{Issuer: "https://owned.invalid", Subject: "audit-actor", SessionID: "audit-session"}, nil
				})
				request := httptest.NewRequest(method, path, strings.NewReader(`{}`))
				request.Header.Set("Authorization", "Bearer controlled")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != 404 || repository.calls != 0 {
					t.Fatal("disabled export dispatched or changed the existing unavailable-route contract")
				}
			})
		}
	}
}
