package billingschema

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/account"
	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
)

// Original deployment actors, SQL seeds and actual organization creation are
// used here. No permission or role-assignment rows are injected by this test.
func TestOrganizationDeliveredFinanceAuthority(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_SCHEMA_DATABASE_URL")
	if raw == "" {
		t.Skip("exclusive PostgreSQL actor fixture is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	f := newStageFixture(t, ctx, raw)
	c := f.database(t, ctx, "account", "account_service")
	m, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, c, selectVersions(m, []int64{1, 2, 3, 4, 6, 7, 8, 9, 10, 11, 12, 13, 14, 16, 17})); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, stageAccountSeed); err != nil {
		t.Fatal(err)
	}
	if err := accountschema.Up(ctx, c, m); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, "SELECT pg_advisory_unlock_all()"); err != nil {
		t.Fatal(err)
	}
	r, err := account.NewPostgresRepository(ctx, f.serviceURL(t, c, "account_service"))
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	principal := identity.Principal{Issuer: "https://owned.invalid", Subject: "owned-subject", SessionID: "owned-session"}
	owner := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	org, err := r.AdminCreateOrganization(ctx, principal, owner, "owned authority organization", "f06-organization-finance-authority")
	if err != nil {
		t.Fatal(err)
	}
	initial, err := r.GetMe(ctx, principal)
	if err != nil {
		t.Fatal(err)
	}
	selected, err := r.SetActiveContext(ctx, principal, &org.ID, initial.ActiveContext.ContextVersion)
	if err != nil {
		t.Fatal(err)
	}
	handler := account.NewService(r, nil).Handler(func(context.Context, string) (identity.Principal, error) { return principal, nil })
	read := func(path string, output any) {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer owned-fixture")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), output) != nil {
			t.Fatal("actual Account HTTP projection unavailable", response.Code)
		}
	}
	var me account.Me
	var current account.AuthorizationContext
	read("/v1/me", &me)
	read("/v1/me/authorization-context", &current)
	if me.UserID != owner || me.ActiveContext.OrganizationID == nil || *me.ActiveContext.OrganizationID != org.ID || current.UserID != owner || current.OrganizationID == nil || *current.OrganizationID != org.ID || current.OrganizationMembershipVersion == nil || *current.OrganizationMembershipVersion != 1 || !slices.Contains(current.Roles, "OWNER") {
		t.Fatal("real organization owner/context facts lost")
	}
	for _, permission := range []string{"org.wallet.read", "org.entitlements.purchase", "org.quota.read.self", "org.quota.read.all", "org.quota.revoke", "org.roles.read"} {
		t.Run(permission, func(t *testing.T) {
			if !slices.Contains(selected.Permissions, permission) || !slices.Contains(me.ActiveContext.Permissions, permission) || !slices.Contains(current.Permissions, permission) {
				t.Fatal("delivered permission missing from actual context/Me projections")
			}
			decision, err := r.Authorize(ctx, principal, permission, &org.ID, nil)
			if err != nil || !decision.Allowed || decision.UserID != owner || decision.PermissionScope != "ORGANIZATION" || decision.OrganizationID == nil || *decision.OrganizationID != org.ID || decision.AuthorizationVersion != current.OrganizationAuthorizationVersion {
				t.Fatal("actual persisted OWNER cannot authorize delivered organization action", err)
			}
		})
	}
	t.Run("future-storage-grants-not-authority", func(t *testing.T) {
		for _, permission := range []string{"org.audit.export", "org.grants.reclaim_closed", "personal.account.close.request", "personal.account.close.read", "platform.account-closures.repair"} {
			if slices.Contains(selected.Permissions, permission) || slices.Contains(me.ActiveContext.Permissions, permission) || slices.Contains(current.Permissions, permission) || slices.Contains(me.PersonalPermissions, permission) || slices.Contains(me.PlatformPermissions, permission) || slices.Contains(current.PersonalPermissions, permission) || slices.Contains(current.PlatformPermissions, permission) {
				t.Fatal("undelivered permission advertised")
			}
			if _, err := r.Authorize(ctx, principal, permission, &org.ID, nil); !errors.Is(err, account.ErrForbidden) {
				t.Fatal("undelivered permission authorizes", err)
			}
		}
	})
	t.Run("foreign-organization-not-authorized", func(t *testing.T) {
		foreign := uuid.New()
		decision, err := r.Authorize(ctx, principal, "org.entitlements.purchase", &foreign, nil)
		if (err != nil && !errors.Is(err, account.ErrForbidden)) || decision.Allowed {
			t.Fatal("current owner crossed organization target", err)
		}
	})
	t.Run("disabled-organization-not-authorized", func(t *testing.T) {
		if err := r.AdminSetOrganizationStatus(ctx, principal, org.ID, "DISABLED"); err != nil {
			t.Fatal(err)
		}
		decision, err := r.Authorize(ctx, principal, "org.entitlements.purchase", &org.ID, nil)
		if (err != nil && !errors.Is(err, account.ErrForbidden)) || decision.Allowed {
			t.Fatal("disabled organization retained effective purchase authority", err)
		}
		// The actual mutation moves affected users back to PERSONAL. It must
		// preserve that usable context while removing the disabled authority.
		current, err := r.ResolveAuthorizationContext(ctx, principal)
		if err != nil || current.ContextType != "PERSONAL" || current.OrganizationID != nil || slices.Contains(current.Permissions, "org.entitlements.purchase") {
			t.Fatal("disabled organization remained the active authorization context", err)
		}
	})
}
