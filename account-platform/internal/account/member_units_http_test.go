package account

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// The production Service router and handler are used with controlled domain
// interfaces. The public PG test separately proves persistent scope/receipt CAS.
type memberUnitsHTTPRepository struct {
	personalBoundaryRepository
	calls                int
	organization, target uuid.UUID
	actor                identity.Principal
	version              int64
	units                []uuid.UUID
	key                  string
	platform             bool
	err                  error
	result               MembershipMutationResult
	allowPlatform        bool
}

func (f *memberUnitsHTTPRepository) ReplaceMemberUnits(_ context.Context, p identity.Principal, org, target uuid.UUID, version int64, units []uuid.UUID, key string, platform bool) (MembershipMutationResult, error) {
	f.calls++
	f.actor, f.organization, f.target, f.version, f.units, f.key, f.platform = p, org, target, version, units, key, platform
	return f.result, f.err
}
func (f *memberUnitsHTTPRepository) Authorize(_ context.Context, _ identity.Principal, permission string, _ *uuid.UUID, _ *uuid.UUID) (AuthorizationDecision, error) {
	return AuthorizationDecision{Allowed: f.allowPlatform && permission == PermissionPlatformOrganizationsMembersManage, UserStatus: "ACTIVE", UserID: f.userID, Permission: permission}, nil
}

type memberUnitsLifecycleHTTPFixture struct {
	calls              int
	err                error
	organization, user uuid.UUID
	version            int64
	status             string
}

func (f *memberUnitsLifecycleHTTPFixture) ApplyMemberLifecycle(_ context.Context, org, user uuid.UUID, status string, version int64) (OrganizationMemberLifecycleState, error) {
	f.calls++
	f.organization, f.user, f.version, f.status = org, user, version, status
	return OrganizationMemberLifecycleState{OrganizationID: org, UserID: user, MembershipVersion: version, Status: status}, f.err
}

func TestMemberUnitPUTHandlerBoundary(t *testing.T) {
	org, target, actor := uuid.New(), uuid.New(), uuid.New()
	for _, tc := range []struct {
		name, body, key                        string
		authenticated, platform, allowPlatform bool
		repositoryError, lifecycleError        error
		want                                   int
		calls                                  int
	}{
		{"malformed", `{"unit_ids":`, "unit-put-http-key", true, false, false, nil, nil, 400, 0},
		{"trailing", `{"unit_ids":[],"expected_version":7}{}`, "unit-put-http-key", true, false, false, nil, nil, 400, 0},
		{"unknown-field", `{"unit_ids":[],"expected_version":7,"unexpected":true}`, "unit-put-http-key", true, false, false, nil, nil, 400, 0},
		{"omitted-units", `{"expected_version":7}`, "unit-put-http-key", true, false, false, nil, nil, 400, 0},
		{"null-units", `{"unit_ids":null,"expected_version":7}`, "unit-put-http-key", true, false, false, nil, nil, 400, 0},
		{"invalid-version", `{"unit_ids":[],"expected_version":0}`, "unit-put-http-key", true, false, false, nil, nil, 400, 0},
		{"missing-key", `{"unit_ids":[],"expected_version":7}`, "", true, false, false, nil, nil, 400, 0},
		{"missing-identity", `{"unit_ids":[],"expected_version":7}`, "unit-put-http-key", false, false, false, nil, nil, 401, 0},
		{"empty-unscoped", `{"unit_ids":[],"expected_version":7}`, "unit-put-http-key", true, false, false, nil, nil, 200, 1},
		{"authority-denied", `{"unit_ids":[],"expected_version":7}`, "unit-put-http-key", true, false, false, ErrForbidden, nil, 403, 1},
		{"stale-version", `{"unit_ids":[],"expected_version":7}`, "unit-put-http-key", true, false, false, ErrConflict, nil, 409, 1},
		{"unknown-projection", `{"unit_ids":[],"expected_version":7}`, "unit-put-http-key", true, false, false, nil, errors.New("controlled unavailable"), 503, 1},
		{"platform-denied-before-domain", `{"unit_ids":[],"expected_version":7}`, "unit-put-http-key", true, true, false, nil, nil, 403, 0},
		{"platform-current-permission", `{"unit_ids":[],"expected_version":7}`, "unit-put-http-key", true, true, true, nil, nil, 200, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := &memberUnitsHTTPRepository{personalBoundaryRepository: personalBoundaryRepository{userID: actor}, allowPlatform: tc.allowPlatform, err: tc.repositoryError, result: MembershipMutationResult{OrganizationID: org, Member: Member{UserID: target, Status: "ACTIVE", Version: 8}}}
			projection := &memberUnitsLifecycleHTTPFixture{err: tc.lifecycleError}
			principal := identity.Principal{Issuer: "https://owned.invalid", Subject: "unit-http-actor", SessionID: "unit-http-session", AuthenticatedAt: time.Now()}
			h := NewService(repo, nil).WithOrganizationMemberLifecycle(projection).Handler(func(context.Context, string) (identity.Principal, error) { return principal, nil })
			prefix := "/v1/organizations/"
			if tc.platform {
				prefix = "/admin/v1/organizations/"
			}
			r := httptest.NewRequest(http.MethodPut, prefix+org.String()+"/members/"+target.String()+"/units", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Idempotency-Key", tc.key)
			if tc.authenticated {
				r.Header.Set("Authorization", "Bearer controlled")
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != tc.want || repo.calls != tc.calls {
				t.Fatalf("status=%d calls=%d want=%d/%d", w.Code, repo.calls, tc.want, tc.calls)
			}
			if tc.platform && tc.want == http.StatusOK && (w.Header().Get("X-Command-Type") != "ACCOUNT_MEMBER_UNITS_REPLACE" || w.Header().Get("X-Command-Policy-Version") != "1") {
				t.Fatal("platform mutation bypassed its registered audit-only command policy")
			}
			if repo.calls > 0 && (repo.actor.Subject != principal.Subject || repo.organization != org || repo.target != target || repo.version != 7 || repo.units == nil || len(repo.units) != 0 || repo.key != tc.key || repo.platform != tc.platform) {
				t.Fatal("handler changed actor, CAS, explicit empty scope, key or platform boundary")
			}
			wantProjection := 0
			if tc.calls > 0 && tc.repositoryError == nil {
				wantProjection = 1
			}
			if projection.calls != wantProjection {
				t.Fatal("projection called before authoritative local mutation")
			}
			if wantProjection > 0 && (projection.organization != org || projection.user != target || projection.version != 8) {
				t.Fatal("projection used caller version instead of committed version")
			}
			if tc.want == 200 && (w.Header().Get("Cache-Control") != "no-store" || strings.Contains(w.Body.String(), "affected_unit_ids")) {
				t.Fatal("public response leaked internal receipt or lacked no-store")
			}
			if tc.lifecycleError != nil && !strings.Contains(w.Body.String(), "ORGANIZATION_MEMBER_LIFECYCLE_UNAVAILABLE") {
				t.Fatal("projection failure advertised success")
			}
		})
	}
}

func TestMemberUnitsReplayCurrentLifecycle(t *testing.T) {
	for _, status := range []string{"DISABLED", "REVOKED"} {
		t.Run(status, func(t *testing.T) {
			org, target, actor := uuid.New(), uuid.New(), uuid.New()
			repo := &memberUnitsHTTPRepository{personalBoundaryRepository: personalBoundaryRepository{userID: actor}, result: MembershipMutationResult{OrganizationID: org, Member: Member{UserID: target, Status: status, Version: 10}, Replayed: true}}
			projection := &memberUnitsLifecycleHTTPFixture{}
			principal := identity.Principal{Issuer: "https://owned.invalid", Subject: "unit-http-actor", SessionID: "unit-http-session", AuthenticatedAt: time.Now()}
			h := NewService(repo, nil).WithOrganizationMemberLifecycle(projection).Handler(func(context.Context, string) (identity.Principal, error) { return principal, nil })
			request := httptest.NewRequest(http.MethodPut, "/v1/organizations/"+org.String()+"/members/"+target.String()+"/units", strings.NewReader(`{"unit_ids":[],"expected_version":7}`))
			request.Header.Set("Authorization", "Bearer controlled")
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Idempotency-Key", "unit-replay-lifecycle-key")
			response := httptest.NewRecorder()
			h.ServeHTTP(response, request)
			if response.Code != 200 || projection.calls != 1 || projection.status != status || projection.version != 10 {
				t.Fatalf("current replay projected wrong lifecycle: HTTP=%d status=%s version=%d calls=%d", response.Code, projection.status, projection.version, projection.calls)
			}
		})
	}
}
