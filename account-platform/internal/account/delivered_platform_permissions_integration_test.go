package account

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var deliveredPlatformPermissions = []string{
	"platform.registration-bonus.read", "platform.registration-bonus.manage", "platform.billing.holds.release",
}

// This matrix follows the complete public migration chain: migration 34 moved
// safe-release authority from the historical approver role to the operator.
// Migration 87 grants bonus read/manage to operations and read only to auditors.
// No fixture inserts or changes role_permissions.
func TestDeliveredPlatformPermissionsPostgres(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")
	if raw == "" {
		t.Skip("explicit owned PostgreSQL required")
	}
	// The retained fixture creates exclusively absent roles and UUID databases.
	// Restrict its control connection before any setup writes; the dedicated
	// cluster may use either postgres or its own named administrative login.
	config, err := pgx.ParseConfig(raw)
	if err != nil || config.Host != "127.0.0.1" || config.Database != "postgres" || config.Password != "" || config.Port < 1024 {
		t.Fatal("dedicated loopback admin postgres database URL required before fixture writes")
	}
	f := retainedWalletFenceActors(t)
	r := f.repository(t)
	ctx := f.ctx
	var current, session string
	var super bool
	if err := r.pool.QueryRow(ctx, `SELECT current_user,session_user,(SELECT rolsuper FROM pg_roles WHERE rolname=current_user)`).Scan(&current, &session, &super); err != nil || current != "account_service" || session != current || super {
		t.Fatal("genuine Account runtime identity", err)
	}
	t.Logf("public Up/Verify fixture: current_user=%s session_user=%s superuser=%v", current, session, super)

	samples := []struct {
		role string
		want [3]bool
	}{
		{"PLATFORM_SUPER_ADMIN", [3]bool{true, true, true}},
		{"PLATFORM_OPERATIONS_ADMIN", [3]bool{true, true, false}},
		{"PLATFORM_AUDITOR", [3]bool{true, false, false}},
		{"PLATFORM_BILLING_OPERATOR", [3]bool{false, false, true}},
		{"PLATFORM_BILLING_APPROVER", [3]bool{false, false, false}},
		{"PERSONAL_USER", [3]bool{false, false, false}},
	}
	actors := map[string]entitlementFixtureActor{}
	principals := map[string]identity.Principal{}
	for _, sample := range samples {
		actor := seedEntitlementApprovalActor(t, ctx, r.pool, sample.role, "delivered-"+strings.ToLower(sample.role))
		actors[sample.role] = actor
		principals[sample.role] = entitlementApprovalPrincipal(actor)
		if _, created, err := r.Bootstrap(ctx, principals[sample.role]); err != nil || created {
			t.Fatal("initialize existing actor session/context with real Bootstrap", err)
		}
	}

	// Reuse the signed loopback JWKS fixture. The first identity lacks Account
	// check authority; the second has the actual internal.authorization.check.
	// Human identities are controlled, but the repository and authority responses
	// are real, including single/all-permission evaluation and current role facts.
	wrongScopeToken, checkToken := configureEntitlementProofIdentity(t)
	handler := userCodeHandler(r, principals)
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := authorization.NewClient(server.URL, checkToken)
	if err != nil {
		t.Fatal(err)
	}

	for _, sample := range samples {
		t.Run(sample.role, func(t *testing.T) {
			actor, principal := actors[sample.role], principals[sample.role]
			for index, permission := range deliveredPlatformPermissions {
				want := sample.want[index]
				var seeded bool
				if err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM role_permissions WHERE role_code=$1 AND permission_code=$2)`, sample.role, permission).Scan(&seeded); err != nil || seeded != want {
					t.Fatalf("public seed role=%s permission=%s granted=%v want=%v: %v", sample.role, permission, seeded, want, err)
				}
				decision, err := r.Authorize(ctx, principal, permission, nil, nil)
				deliveredPermissionDecision(t, "single", permission, decision, err, want, actor.ID)
				decision, err = r.AuthorizeComposite(ctx, principal, nil, []string{permission})
				deliveredPermissionDecision(t, "composite", permission, decision, err, want, actor.ID)
				allowed, headers, err := client.Allowed(ctx, principal, permission)
				if err != nil || allowed != want || headers.Get("X-Platform-Authorization-Version") == "" {
					t.Errorf("real Account HTTP client single %s allowed=%v want=%v: %v", permission, allowed, want, err)
				}
				id, version, err := client.AuthorizedUserIDAll(ctx, principal, permission)
				if want {
					if err != nil || id != actor.ID || version < 1 {
						t.Errorf("real Account HTTP client all %s: actor=%s version=%d err=%v", permission, id, version, err)
					}
				} else if !errors.Is(err, authorization.ErrPermissionDenied) || id != uuid.Nil {
					t.Errorf("real Account HTTP client all %s must deny: actor=%s err=%v", permission, id, err)
				}
			}
			// A combined request must not borrow a missing manage/release grant
			// from another role. It also exercises the consumer's all-of shape.
			_, _, err := client.AuthorizedUserIDAll(ctx, principal, deliveredPlatformPermissions...)
			if sample.role == "PLATFORM_SUPER_ADMIN" {
				if err != nil {
					t.Errorf("super administrator all-of: %v", err)
				}
			} else if !errors.Is(err, authorization.ErrPermissionDenied) {
				t.Errorf("role without all three grants passed composite: %v", err)
			}
			for _, endpoint := range []struct{ method, path string }{
				{http.MethodPost, "/v1/account/bootstrap"},
				{http.MethodGet, "/v1/me"},
				{http.MethodGet, "/v1/me/authorization-context"},
			} {
				response := platformRoleHTTP(handler, sample.role, endpoint.method, endpoint.path, map[string]any{})
				var projection struct {
					UserID              uuid.UUID `json:"user_id"`
					PlatformPermissions []string  `json:"platform_permissions"`
				}
				if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &projection) != nil || projection.UserID != actor.ID {
					t.Fatalf("actual %s: status=%d body=%s", endpoint.path, response.Code, response.Body.String())
				}
				for index, permission := range deliveredPlatformPermissions {
					if got := slices.Contains(projection.PlatformPermissions, permission); got != sample.want[index] {
						t.Errorf("%s projection %s=%v want=%v", endpoint.path, permission, got, sample.want[index])
					}
				}
			}
		})
	}

	t.Run("actual-role-catalog-preserves-seeded-boundaries", func(t *testing.T) {
		response := platformRoleHTTP(handler, "PLATFORM_SUPER_ADMIN", http.MethodGet, "/admin/v1/roles", nil)
		var catalog struct {
			Items []AdminRoleDefinition `json:"items"`
		}
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &catalog) != nil {
			t.Fatal("actual role catalog", response.Code, response.Body.String())
		}
		for _, sample := range samples[:len(samples)-1] {
			index := slices.IndexFunc(catalog.Items, func(item AdminRoleDefinition) bool { return item.Code == sample.role })
			if index < 0 {
				t.Fatalf("role catalog lost %s", sample.role)
			}
			for j, permission := range deliveredPlatformPermissions {
				if got := slices.Contains(catalog.Items[index].Permissions, permission); got != sample.want[j] {
					t.Errorf("catalog role=%s permission=%s got=%v want=%v", sample.role, permission, got, sample.want[j])
				}
			}
		}
		// Installing these three abilities confers neither role-management
		// authority on operators nor access to another scope's role catalog.
		for _, role := range []string{"PLATFORM_OPERATIONS_ADMIN", "PLATFORM_BILLING_OPERATOR", "PERSONAL_USER"} {
			if denied := platformRoleHTTP(handler, role, http.MethodGet, "/admin/v1/roles", nil); denied.Code != http.StatusForbidden {
				t.Errorf("unrelated role catalog boundary %s: %d", role, denied.Code)
			}
		}
	})

	t.Run("unknown-scope-and-workload-fail-closed", func(t *testing.T) {
		principal := principals["PLATFORM_SUPER_ADMIN"]
		for _, permission := range []string{"platform.registration-bonus.force", "platform.billing.holds.force_release", "platform.unknown.permission"} {
			if _, err := r.Authorize(ctx, principal, permission, nil, nil); !errors.Is(err, ErrForbidden) {
				t.Errorf("unknown permission did not fail closed: %s: %v", permission, err)
			}
			if _, err := r.AuthorizeComposite(ctx, principal, nil, []string{deliveredPlatformPermissions[0], permission}); !errors.Is(err, ErrForbidden) {
				t.Errorf("unknown all-of permission did not fail closed: %s: %v", permission, err)
			}
			if allowed, _, err := client.Allowed(ctx, principal, permission); err != nil || allowed {
				t.Errorf("unknown permission HTTP result allowed=%v err=%v", allowed, err)
			}
		}
		org, unit := uuid.New(), uuid.New()
		for _, permission := range deliveredPlatformPermissions {
			decision, err := r.Authorize(ctx, principal, permission, &org, &unit)
			if err != nil && !errors.Is(err, ErrForbidden) || decision.Allowed {
				t.Errorf("PLATFORM permission escaped to organization/unit: %s: %v", permission, err)
			}
			if _, err := r.Authorize(ctx, principal, permission, nil, &unit); !errors.Is(err, ErrForbidden) {
				t.Errorf("unit without organization accepted: %v", err)
			}
		}
		body := map[string]any{"issuer": principal.Issuer, "subject": principal.Subject, "permission": deliveredPlatformPermissions[0]}
		for _, auth := range []struct {
			token  string
			status int
		}{{"", 401}, {wrongScopeToken, 403}} {
			if response := platformRoleHTTP(handler, auth.token, http.MethodPost, "/internal/v1/authorization/check", body); response.Code != auth.status {
				t.Errorf("internal workload boundary: status=%d want=%d", response.Code, auth.status)
			}
		}
	})

	t.Run("grant-revoke-expiry-and-disabled-are-live", func(t *testing.T) {
		admin := principals["PLATFORM_SUPER_ADMIN"]
		target := seedEntitlementApprovalActor(t, ctx, r.pool, "PERSONAL_USER", "delivered-grant-target")
		principal := entitlementApprovalPrincipal(target)
		binding, err := r.AdminGrantPlatformRoleBinding(ctx, admin, RoleBindingGrant{UserID: target.ID, RoleCode: "PLATFORM_OPERATIONS_ADMIN", IdempotencyKey: uuid.NewString(), Reason: "controlled delivered permission grant"})
		if err != nil {
			t.Fatal("real platform role grant", err)
		}
		if _, _, err := client.AuthorizedUserIDAll(ctx, principal, deliveredPlatformPermissions[:2]...); err != nil {
			t.Errorf("new granted operations authority unavailable: %v", err)
		}
		if _, err := r.AdminRevokePlatformRoleBinding(ctx, admin, binding.ID); err != nil {
			t.Fatal("real platform revoke", err)
		}
		if _, _, err := client.AuthorizedUserIDAll(ctx, principal, deliveredPlatformPermissions[:2]...); !errors.Is(err, authorization.ErrPermissionDenied) {
			t.Errorf("revoked role retained authority: %v", err)
		}
		for _, sample := range []struct{ label, role, permission string }{
			{"expired", "PLATFORM_BILLING_OPERATOR", "platform.billing.holds.release"},
			{"disabled", "PLATFORM_OPERATIONS_ADMIN", "platform.registration-bonus.manage"},
		} {
			actor := seedEntitlementApprovalActor(t, ctx, r.pool, sample.role, "delivered-"+sample.label)
			p := entitlementApprovalPrincipal(actor)
			before, _, err := client.Allowed(ctx, p, sample.permission)
			if err != nil || !before {
				t.Errorf("%s positive control before lifecycle change: %v", sample.label, err)
			}
			if sample.label == "expired" {
				_, err = r.pool.Exec(ctx, `UPDATE role_assignments SET starts_at=clock_timestamp()-interval '2 days',expires_at=clock_timestamp()-interval '1 day' WHERE user_id=$1 AND role_code=$2`, actor.ID, sample.role)
			} else {
				_, err = r.pool.Exec(ctx, `UPDATE users SET status='DISABLED' WHERE id=$1`, actor.ID)
			}
			if err != nil {
				t.Fatal("controlled lifecycle fixture", err)
			}
			decision, err := r.Authorize(ctx, p, sample.permission, nil, nil)
			if err != nil || decision.Allowed {
				t.Errorf("%s single authorization retained authority: %v", sample.label, err)
			}
			if _, _, err := client.AuthorizedUserIDAll(ctx, p, sample.permission); !errors.Is(err, authorization.ErrPermissionDenied) {
				t.Errorf("%s all-of retained authority: %v", sample.label, err)
			}
		}
	})
}

func deliveredPermissionDecision(t *testing.T, path, permission string, decision AuthorizationDecision, err error, want bool, actor uuid.UUID) {
	t.Helper()
	if err != nil || decision.Allowed != want || decision.UserID != actor || decision.PermissionScope != "PLATFORM" || decision.AuthorizationVersion < 1 {
		t.Errorf("%s permission=%s allowed=%v want=%v actor=%s scope=%s version=%d err=%v", path, permission, decision.Allowed, want, decision.UserID, decision.PermissionScope, decision.AuthorizationVersion, err)
	}
}
