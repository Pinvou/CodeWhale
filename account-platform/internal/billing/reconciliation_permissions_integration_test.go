package billing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/accountschema"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Project actual RBAC SQL (versions below 50 plus the frozen metadata bridge
// and permission migration 92) into an owned schema, then exercise Gin Require
// against those real grants. Separate role-isolation migrations require their
// own database/actor fixture and are not claimed as verified by this test.
func TestReconciliationPermissionsFromActualRoleGrants(t *testing.T) {
	raw := os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL")
	if raw == "" {
		t.Skip("requires isolated PostgreSQL")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, raw)
	if err != nil {
		t.Fatal("fixture connection failed")
	}
	defer c.Close(context.Background())
	schema := "reconcile_permissions_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quoted := pgx.Identifier{schema}.Sanitize()
	if _, err = c.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		t.Fatal(err)
	}
	defer c.Exec(context.Background(), "DROP SCHEMA "+quoted+" CASCADE")
	if _, err = c.Exec(ctx, "SET search_path TO "+quoted); err != nil {
		t.Fatal(err)
	}
	installed, err := migrations.Load("../../migrations/account")
	if err != nil {
		t.Fatal(err)
	}
	var projected []migrations.Migration
	for _, migration := range installed {
		if migration.Version < 50 || migration.Version == 92 || migration.Version == 900001 {
			projected = append(projected, migration)
		}
	}
	if err = accountschema.Up(ctx, c, projected); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"PLATFORM_MODEL_ADMIN", "PLATFORM_BILLING_OPERATOR", "PLATFORM_BILLING_APPROVER", "PLATFORM_AUDITOR"} {
		t.Run(role, func(t *testing.T) {
			var grantCount int
			if err := c.QueryRow(ctx, `SELECT count(*) FROM role_permissions WHERE role_code=$1 AND permission_code LIKE 'platform.billing.reconciliation.%'`, role).Scan(&grantCount); err != nil {
				t.Fatal(err)
			}
			want := 1
			if role == "PLATFORM_MODEL_ADMIN" {
				want = 0
			}
			if role == "PLATFORM_BILLING_OPERATOR" {
				want = 3
			}
			if grantCount != want {
				t.Fatalf("financial grants=%d want=%d", grantCount, want)
			}
			if role == "PLATFORM_MODEL_ADMIN" {
				var usageCount int
				if err := c.QueryRow(ctx, `SELECT count(*) FROM role_permissions WHERE role_code=$1 AND permission_code IN ('platform.usage.read','platform.usage.reconcile.retry')`, role).Scan(&usageCount); err != nil || usageCount != 2 {
					t.Fatal("existing usage grants changed")
				}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Permission string `json:"permission"`
					Subject    string `json:"subject"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || body.Subject != role {
					t.Error("invalid verified principal")
				}
				var allowed bool
				if err := c.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM role_permissions WHERE role_code=$1 AND permission_code=$2)`, role, body.Permission).Scan(&allowed); err != nil {
					t.Error(err)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"allowed": allowed, "user_id": uuid.NewString()})
			}))
			defer server.Close()
			authorizer, err := authorization.NewClient(server.URL, "fixture-service")
			if err != nil {
				t.Fatal(err)
			}
			s := NewService(nil).WithAuthorizer(authorizer)
			h := s.Handler("fixture-service", func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "fixture", Subject: role}, nil
			})
			for _, route := range []struct{ method, path, permission string }{
				{"GET", "/admin/v1/reconciliation-runs", "read"},
				{"POST", "/admin/v1/reconciliation-imports", "run"},
				{"POST", "/admin/v1/reconciliation-cases/" + uuid.NewString() + "/commands", "manage"},
			} {
				// Only denied paths need business dependencies: Require must reject
				// before any handler or financial body is read.
				if role == "PLATFORM_BILLING_OPERATOR" || (route.permission == "read" && role != "PLATFORM_MODEL_ADMIN") {
					continue
				}
				r := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
				r.Header.Set("Authorization", "Bearer fixture")
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != 403 {
					t.Fatalf("%s got %d", route.path, w.Code)
				}
			}
		})
	}
}
