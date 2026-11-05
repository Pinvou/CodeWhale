package account

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

type entitlementPrecheckRouteRepository struct {
	personalBoundaryRepository
	allowed bool
}

func (r *entitlementPrecheckRouteRepository) Authorize(_ context.Context, _ identity.Principal, permission string, _ *uuid.UUID, _ *uuid.UUID) (AuthorizationDecision, error) {
	return AuthorizationDecision{UserID: r.userID, UserStatus: "ACTIVE", Allowed: r.allowed && permission == "platform.billing.repairs.request", Permission: permission, AuthorizationVersion: 1}, nil
}

type entitlementPrecheckRouteSource struct{ calls int }

func (s *entitlementPrecheckRouteSource) Precheck(_ context.Context, request ApprovalPrecheckRequest) (ApprovalPrecheckProjection, error) {
	s.calls++
	return ApprovalPrecheckProjection{CommandType: request.CommandType, PolicyVersion: 2, SnapshotSchemaVersion: 2, TargetResourceType: "ENTITLEMENT_PACKAGE", TargetResourceID: request.TargetResourceID, NormalizedCommand: json.RawMessage(`{}`), CriticalResourceVersions: json.RawMessage(`{}`), FinancialSnapshot: json.RawMessage(`{}`)}, nil
}

// This exercises the actual registered Account middleware, not a direct
// handler call. Owner transport validation has separate closed-wire tests.
func TestEntitlementPrecheckRegisteredPermissionAndRecentAuth(t *testing.T) {
	now := time.Now().UTC()
	for _, test := range []struct {
		name, method, path     string
		allowed, authenticated bool
		authTime               time.Time
		want, calls            int
	}{
		{"allowed", "POST", "/admin/v2/approval-command-prechecks", true, true, now, 200, 1},
		{"permission", "POST", "/admin/v2/approval-command-prechecks", false, true, now, 403, 0},
		{"stale", "POST", "/admin/v2/approval-command-prechecks", true, true, now.Add(-16 * time.Minute), 401, 0},
		{"no_session", "POST", "/admin/v2/approval-command-prechecks", true, false, now, 401, 0},
		{"get", "GET", "/admin/v2/approval-command-prechecks", true, true, now, 405, 0},
		{"extra", "POST", "/admin/v2/approval-command-prechecks/extra", true, true, now, 404, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			repo := &entitlementPrecheckRouteRepository{personalBoundaryRepository: personalBoundaryRepository{userID: uuid.New()}, allowed: test.allowed}
			source := &entitlementPrecheckRouteSource{}
			service := NewService(repo, nil).WithApprovalPrechecks(source)
			service.now = func() time.Time { return now }
			handler := service.Handler(func(context.Context, string) (identity.Principal, error) {
				return identity.Principal{Issuer: "owned-test", Subject: "operator", SessionID: "session", AuthenticatedAt: test.authTime}, nil
			})
			body := `{"command_type":"ENTITLEMENT_COMPONENT_ADJUSTMENT","target_resource_id":"` + uuid.NewString() + `","payment_order_id":"` + uuid.NewString() + `","reason_code":"ORIGINAL_SPLIT_MISMATCH"}`
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(body))
			if test.authenticated {
				request.Header.Set("Authorization", "Bearer controlled")
			}
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.want || source.calls != test.calls {
				t.Fatalf("status=%d owner_calls=%d", response.Code, source.calls)
			}
		})
	}
}
