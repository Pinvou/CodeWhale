package token

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/google/uuid"
)

// This seam counts handler dispatches but forwards validation to the real
// PostgreSQL repository with no storage available. Invalid input must stop
// before any database access, rather than being classified by the test fake.
type modelPolicyInputRepository struct {
	Repository
	calls int
}

func (*modelPolicyInputRepository) GetOrganizationModelPolicy(context.Context, uuid.UUID) (OrganizationModelPolicy, error) {
	panic("unexpected model policy read")
}
func (r *modelPolicyInputRepository) SaveOrganizationModelPolicy(ctx context.Context, org, actor uuid.UUID, authorizationVersion, expected int64, mode string, models []uuid.UUID, key, reason string) (OrganizationModelPolicy, error) {
	r.calls++
	return (&PostgresRepository{}).SaveOrganizationModelPolicy(ctx, org, actor, authorizationVersion, expected, mode, models, key, reason)
}

func TestOrganizationModelPolicyInvalidInputHTTP(t *testing.T) {
	for _, tc := range []struct {
		name, body, key string
	}{
		{"mode", `{"expected_version":0,"mode":"INVALID","model_ids":[],"reason":"controlled"}`, "controlled-policy-key"},
		{"reason-empty", `{"expected_version":0,"mode":"ALLOW_ALL","model_ids":[],"reason":"  "}`, "controlled-policy-key"},
		{"reason-long", `{"expected_version":0,"mode":"ALLOW_ALL","model_ids":[],"reason":"` + strings.Repeat("x", 501) + `"}`, "controlled-policy-key"},
		{"key", `{"expected_version":0,"mode":"ALLOW_ALL","model_ids":[],"reason":"controlled"}`, "short"},
		{"version", `{"expected_version":-1,"mode":"ALLOW_ALL","model_ids":[],"reason":"controlled"}`, "controlled-policy-key"},
		{"nil-model", `{"expected_version":0,"mode":"ALLOW_LIST","model_ids":["00000000-0000-0000-0000-000000000000"],"reason":"controlled"}`, "controlled-policy-key"},
		{"all-with-model", `{"expected_version":0,"mode":"ALLOW_ALL","model_ids":["` + uuid.NewString() + `"],"reason":"controlled"}`, "controlled-policy-key"},
		{"malformed-json", `{"mode":`, "controlled-policy-key"},
	} {
		for _, allowed := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/allowed-%t", tc.name, allowed), func(t *testing.T) {
				org, actor := uuid.New(), uuid.New()
				decision := &authorizationDecisionRecorder{allowed: allowed, userID: actor}
				authority := httptest.NewServer(decision)
				defer authority.Close()
				auth, err := authorization.NewClient(authority.URL, "controlled-service")
				if err != nil {
					t.Fatal(err)
				}
				repo := &modelPolicyInputRepository{}
				s := &Service{repository: repo, managementEnabled: true, authorizer: auth}
				req := httptest.NewRequest(http.MethodPut, "/v1/organizations/"+org.String()+"/model-policy", strings.NewReader(tc.body))
				req.Header.Set("Authorization", "Bearer controlled-user")
				req.Header.Set("Idempotency-Key", tc.key)
				out := httptest.NewRecorder()
				s.Handler(successfulBillingVerify).ServeHTTP(out, req)
				status, code, calls := http.StatusBadRequest, "INVALID_REQUEST", 1
				if !allowed {
					status, code, calls = http.StatusForbidden, "TOKEN_FORBIDDEN", 0
				}
				if tc.name == "malformed-json" {
					calls = 0
				}
				if out.Code != status || !strings.Contains(out.Body.String(), `"code":"`+code+`"`) || repo.calls != calls || out.Header().Get("X-Pinvou-Model-Rejection-v1") != "" {
					t.Fatal("input and authority receipts conflated", out.Code, out.Body.String(), repo.calls)
				}
				decision.mu.Lock()
				defer decision.mu.Unlock()
				if len(decision.permissions) != 1 || decision.permissions[0] != "org.models.manage" || decision.organizationID == nil || *decision.organizationID != org {
					t.Fatal("current organization authority was not preserved")
				}
			})
		}
	}
}

func TestOrganizationModelPolicyTrustedIdentityAndErrorBoundary(t *testing.T) {
	for _, tc := range []struct {
		name       string
		org, actor uuid.UUID
		version    int64
	}{
		{"organization", uuid.Nil, uuid.New(), 1},
		{"actor", uuid.New(), uuid.Nil, 1},
		{"authority-version", uuid.New(), uuid.New(), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := (&PostgresRepository{}).SaveOrganizationModelPolicy(context.Background(), tc.org, tc.actor, tc.version, -1, "INVALID", nil, "short", "")
			if !errors.Is(err, ErrForbidden) || errors.Is(err, errOrganizationPolicyInvalidInput) {
				t.Fatal("trusted identity failure changed", err)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		err    error
		status int
		code   string
	}{
		{"wrapped-input", fmt.Errorf("wrapped: %w", errOrganizationPolicyInvalidInput), 400, "INVALID_REQUEST"},
		{"spoofed-text", errors.New(errOrganizationPolicyInvalidInput.Error()), 500, "INTERNAL_ERROR"},
		{"storage", errors.New("private controlled storage failure"), 500, "INTERNAL_ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := httptest.NewRecorder()
			(&Service{}).writeError(out, httptest.NewRequest("PUT", "/controlled", nil), tc.err)
			if out.Code != tc.status || !strings.Contains(out.Body.String(), `"code":"`+tc.code+`"`) || strings.Contains(out.Body.String(), "private controlled") {
				t.Fatal("error boundary changed", out.Code, out.Body.String())
			}
		})
	}
}
