package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func TestPostgresRoleGrantReplayBindsCompleteFacts(t *testing.T) {
	for _, mode := range []string{"organization", "admin_organization", "admin_platform"} {
		t.Run(mode, func(t *testing.T) {
			repo := accountDatabase(t)
			ctx := context.Background()
			admin := fixturePlatformAdmin(t, repo)
			owner, _ := accountUser(t, repo)
			member, target := accountUser(t, repo)
			org := accountOrganization(t, repo, owner)
			accountInvitation(t, repo, owner, member, org.ID, []string{"MEMBER"})
			request := RoleBindingGrant{UserID: target.UserID, OrganizationID: &org.ID, RoleCode: "AUDITOR", IdempotencyKey: "owned-role-replay-key"}
			if mode == "admin_platform" {
				request.OrganizationID, request.RoleCode = nil, "PLATFORM_AUDITOR"
			}
			grant := func(request RoleBindingGrant) (RoleBinding, error) {
				if mode == "organization" {
					return repo.GrantOrganizationRoleBinding(ctx, owner, request)
				}
				return repo.AdminGrantRoleBinding(ctx, admin, request)
			}
			first, err := grant(request)
			if err != nil {
				t.Fatal(err)
			}
			if replay, err := grant(request); err != nil || replay.ID != first.ID || !replay.StartsAt.Equal(first.StartsAt) {
				t.Fatal("omitted start replay did not preserve the first server time")
			}
			exact := request
			exact.StartsAt = &first.StartsAt
			if replay, err := grant(exact); err != nil || replay.ID != first.ID {
				t.Fatal("explicit exact first-start replay did not preserve the binding")
			}
			t.Run("foreign_replay", func(t *testing.T) {
				foreign, _ := accountUser(t, repo)
				var err error
				if mode == "organization" {
					_, err = repo.GrantOrganizationRoleBinding(ctx, foreign, request)
				} else {
					// Admin repository methods are behind the production permission
					// middleware; test that boundary instead of bypassing it.
					body, _ := json.Marshal(map[string]any{"user_id": request.UserID, "organization_id": request.OrganizationID, "role_code": request.RoleCode, "idempotency_key": request.IdempotencyKey})
					handler := NewService(repo, nil).Handler(func(context.Context, string) (identity.Principal, error) { return foreign, nil })
					httpRequest := httptest.NewRequest(http.MethodPost, "/admin/v1/role-bindings", strings.NewReader(string(body)))
					httpRequest.Header.Set("Authorization", "Bearer controlled")
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, httpRequest)
					if response.Code != http.StatusForbidden {
						t.Fatal("foreign Admin replay bypassed production permission middleware")
					}
					return
				}
				if !errors.Is(err, ErrForbidden) {
					t.Fatal("foreign logical replay bypassed the grant's current authority")
				}
			})
			for _, fact := range []string{"role", "start", "expiry", "unit"} {
				t.Run(fact, func(t *testing.T) {
					changed := request
					switch fact {
					case "role":
						changed.RoleCode = "ORG_ADMIN"
						if mode == "admin_platform" {
							changed.RoleCode = "PLATFORM_BILLING_OPERATOR"
						}
					case "start":
						start := first.StartsAt.Add(-time.Second)
						changed.StartsAt = &start
					case "expiry":
						expiry := time.Now().Add(time.Hour)
						changed.ExpiresAt = &expiry
					case "unit":
						unit := uuid.New()
						changed.UnitID = &unit
					}
					if _, err := grant(changed); !errors.Is(err, ErrConflict) {
						t.Fatal("same logical grant accepted different authorization facts")
					}
				})
			}
			var events int
			if err := repo.pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='account.role-binding.changed.v1'`, first.ID).Scan(&events); err != nil || events != 1 {
				t.Fatal("grant replay or conflict duplicated role lifecycle events")
			}
		})
	}
}
