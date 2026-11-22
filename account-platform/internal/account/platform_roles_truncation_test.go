package account

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// Called by the existing required platform-role PostgreSQL root, with its
// public migration and runtime actors and owned database cleanup unchanged.
func testPlatformRoleBindingsTruncation(t *testing.T, f *retainedWalletFenceFixture) {
	r := f.repository(t)
	ctx := context.Background()
	admin, _, _ := userCodeActor(t, r, "bounded-role-reader")
	target := seedEntitlementApprovalActor(t, ctx, r.pool, "PERSONAL_USER", "bounded-role-target")
	h := userCodeHandler(r, map[string]identity.Principal{"admin": admin, "target": entitlementApprovalPrincipal(target)})
	check := func(count int, truncated bool) {
		t.Helper()
		listed, err := r.AdminPlatformRoleBindings(ctx, &target.ID)
		want := min(count, 500)
		if err != nil || len(listed.Items) != want || listed.Truncated != truncated {
			t.Fatalf("repository boundary count=%d: items=%d truncated=%t err=%v", count, len(listed.Items), listed.Truncated, err)
		}
		out := platformRoleHTTP(h, "admin", http.MethodGet, "/admin/v1/role-bindings?user_id="+target.ID.String(), nil)
		var wire struct {
			Items     []RoleBinding `json:"items"`
			Truncated *bool         `json:"truncated"`
		}
		if out.Code != http.StatusOK || json.Unmarshal(out.Body.Bytes(), &wire) != nil || wire.Items == nil || len(wire.Items) != want || wire.Truncated == nil || *wire.Truncated != truncated || out.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("HTTP boundary count=%d: status=%d response=%s", count, out.Code, out.Body.String())
		}
		for i, binding := range wire.Items {
			id := uuid.MustParse(fmt.Sprintf("0199a100-0001-7000-8000-%012x", i+1))
			if binding.ID != id || binding.UserID != target.ID || binding.RoleCode != "PLATFORM_MODEL_ADMIN" || binding.Scope != "PLATFORM" || binding.OrganizationID != nil || binding.UnitID != nil {
				t.Fatal("first 500 ordering, exact target or platform scope changed", i, binding)
			}
		}
	}
	check(0, false)
	for i := 1; i <= 501; i++ {
		id := uuid.MustParse(fmt.Sprintf("0199a100-0001-7000-8000-%012x", i))
		// Historical revoked grants are valid production rows and may share a
		// role. Identical sort fields make the final ID tie-break observable.
		if _, err := r.pool.Exec(ctx, `INSERT INTO role_assignments(id,user_id,role_code,source,grant_source,created_at,starts_at,expires_at,revoked_at)
VALUES($1,$2,'PLATFORM_MODEL_ADMIN','ADMIN','ADMIN','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z','2026-01-02T00:00:00Z')`, id, target.ID); err != nil {
			t.Fatal(err)
		}
		if i == 500 || i == 501 {
			check(i, i > 500)
		}
	}
	all, err := r.AdminPlatformRoleBindings(ctx, nil)
	if err != nil || len(all.Items) != 500 || !all.Truncated {
		t.Fatal("unfiltered admin list lost truncation", len(all.Items), all.Truncated, err)
	}
	for _, item := range all.Items {
		if item.Scope != "PLATFORM" || item.OrganizationID != nil || item.UnitID != nil {
			t.Fatal("non-platform association leaked into the admin list")
		}
	}
	for _, boundary := range []struct {
		token  string
		status int
	}{{"", http.StatusUnauthorized}, {"target", http.StatusForbidden}} {
		if out := platformRoleHTTP(h, boundary.token, http.MethodGet, "/admin/v1/role-bindings", nil); out.Code != boundary.status {
			t.Fatal("existing read permission boundary changed", out.Code)
		}
	}
}
