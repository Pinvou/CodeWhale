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
