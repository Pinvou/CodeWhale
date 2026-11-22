package account

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"context"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

func TestF06MemberCursorScopeAndBounds(t *testing.T) {
	org, unit, user := uuid.New(), uuid.New(), uuid.New()
	encoded := encodeMemberListCursor("members", org, &unit, "同名", user)
	for _, tc := range []struct {
		name, resource, cursor string
		org                    uuid.UUID
		unit                   *uuid.UUID
		limit, wantLimit       int
		reject                 bool
	}{
		{"default", "members", "", org, &unit, 0, 50, false},
		{"bounded", "members", "", org, &unit, 500, 200, false},
		{"same-scope", "members", encoded, org, &unit, 2, 2, false},
		{"wrong-org", "members", encoded, uuid.New(), &unit, 2, 0, true},
		{"wrong-unit", "members", encoded, org, nil, 2, 0, true},
		{"wrong-resource", "quota-members", encoded, org, &unit, 2, 0, true},
		{"malformed", "members", "not-a-cursor", org, &unit, 2, 0, true},
		{"oversized", "members", strings.Repeat("x", 8193), org, &unit, 2, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit, position, err := memberListPosition(MemberListQuery{Limit: tc.limit, Cursor: tc.cursor}, tc.resource, tc.org, tc.unit)
			if tc.reject {
				if err == nil {
					t.Fatal("invalid cursor accepted")
				}
				return
			}
			if err != nil || limit != tc.wantLimit || (tc.cursor != "" && (position == nil || position.Name != "同名" || position.UserID != user)) {
				t.Fatalf("limit/position %d %+v %v", limit, position, err)
			}
		})
	}
	raw, err := json.Marshal(MemberPage{Items: []Member{}})
	if err != nil || string(raw) != `{"items":[],"next_cursor":null}` {
		t.Fatal("last page must explicitly disclose cursor exhaustion", string(raw), err)
	}
}

// The installed router and lifecycle adapter run. The controlled repository
// counts mutations; this is not a PostgreSQL or remote-domain authority proof.
func TestF06MembershipRemovalExplicitIntent(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	for _, route := range []struct {
		name, prefix string
		admin        bool
	}{{"ordinary", "/v1", false}, {"administrator", "/admin/v1", true}} {
		t.Run(route.name, func(t *testing.T) {
			for _, tc := range []struct {
				name, header, query string
				want, calls         int
			}{
				{"missing", "", "", 400, 0},
				{"short", "x", "", 400, 0},
				{"header", "original-remove-operation-key", "", 200, 1},
				{"query", "", "original-remove-operation-key", 200, 1},
				{"header-precedence", "original-remove-operation-key", "different-query-key", 200, 1},
			} {
				t.Run(tc.name, func(t *testing.T) {
					org, user := uuid.New(), uuid.New()
					p := identity.Principal{Issuer: "controlled", Subject: uuid.NewString(), SessionID: "same-session"}
					repo := &removeRouteRepository{t: t, principal: p, admin: route.admin, expectedPermission: PermissionPlatformOrganizationsMembersManage, result: MembershipMutationResult{OrganizationID: org, Member: Member{MembershipID: uuid.New(), UserID: user, Status: "REVOKED", Version: 2}}}
					lifecycle := &memberUnitsLifecycleHTTPFixture{}
					handler := NewService(repo, nil).WithOrganizationMemberLifecycle(lifecycle).Handler(func(context.Context, string) (identity.Principal, error) { return p, nil })
					request := httptest.NewRequest(http.MethodDelete, route.prefix+"/organizations/"+org.String()+"/members/"+user.String()+"?idempotency_key="+tc.query, nil)
					request.Header.Set("Authorization", "Bearer controlled-user")
					request.Header.Set("Idempotency-Key", tc.header)
					response := httptest.NewRecorder()
					handler.ServeHTTP(response, request)
					if response.Code != tc.want || repo.calls != tc.calls || lifecycle.calls != tc.calls {
						t.Fatalf("status=%d mutations=%d projections=%d body=%s", response.Code, repo.calls, lifecycle.calls, response.Body.String())
					}
				})
			}
		})
	}
}
