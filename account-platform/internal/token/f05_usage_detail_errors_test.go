package token

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type usageDetailErrorRepository struct {
	Repository
	usage UsageRequest
	err   error
	calls int
}

func (r *usageDetailErrorRepository) GetUsage(context.Context, uuid.UUID) (UsageRequest, error) {
	r.calls++
	return r.usage, r.err
}

// The actual registered HTTP route uses the existing context and HTTP
// authorization fixture. Only storage is controlled; this is not a PG test.
func TestF05UsageDetailStorageErrorAndAuthorityBoundary(t *testing.T) {
	user, usageID, org := uuid.New(), uuid.New(), uuid.New()
	for _, tc := range []struct {
		name, kind, code string
		status, calls    int
	}{
		{"storage-failure-is-not-missing", "storage", "INTERNAL_ERROR", 500, 1},
		{"wrapped-storage-failure-is-not-missing", "wrapped-storage", "INTERNAL_ERROR", 500, 1},
		{"storage-failure-with-partial-foreign-row", "partial-storage", "INTERNAL_ERROR", 500, 1},
		{"missing-remains-hidden", "missing", "TOKEN_NOT_FOUND", 404, 1},
		{"wrapped-missing-remains-hidden", "wrapped-missing", "TOKEN_NOT_FOUND", 404, 1},
		{"other-users-row-remains-hidden", "foreign-user", "TOKEN_NOT_FOUND", 404, 1},
		{"organization-row-hidden-from-personal-context", "foreign-context", "TOKEN_NOT_FOUND", 404, 1},
		{"same-user-other-session-keeps-safe-detail", "valid", "", 200, 1},
		{"permission-denial-does-not-read-storage", "denied", "TOKEN_FORBIDDEN", 403, 0},
		{"unavailable-organization-context-does-not-read-storage", "organization-context", "TOKEN_FORBIDDEN", 403, 0},
		{"malformed-id-does-not-read-storage", "invalid-id", "INVALID_REQUEST", 400, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolved := AuthorizationContext{UserID: user, ContextType: "PERSONAL", SessionID: "current-session"}
			if tc.kind == "organization-context" {
				resolved.ContextType, resolved.OrganizationID = "ORGANIZATION", &org
			}
			service, authority, closeServer := newModelUsageAuthorizationService(t, resolved)
			defer closeServer()
			authority.allowed = tc.kind != "denied"
			repo := &usageDetailErrorRepository{usage: UsageRequest{
				ID: usageID, UserID: user, SessionID: "original-other-session", Model: "controlled-detail-model",
				Status: "UPSTREAM_ACTIVE", ExecutionStatus: "DISPATCHING", DeliveryStatus: "DELIVERING",
				MaximumMicrocredits: 9007199254740993, RequestSnapshot: json.RawMessage(`{"private":"synthetic-hidden-request"}`),
			}}
			switch tc.kind {
			case "storage", "partial-storage":
				repo.err = errors.New("synthetic-storage-diagnostic-must-not-leak")
			case "wrapped-storage":
				repo.err = fmt.Errorf("synthetic-wrapper: %w", errors.New("synthetic-storage-diagnostic-must-not-leak"))
			case "missing":
				repo.err = ErrNotFound
			case "wrapped-missing":
				repo.err = fmt.Errorf("synthetic-private-missing-context: %w", ErrNotFound)
			case "foreign-context":
				repo.usage.OrganizationID = &org
			}
			if tc.kind == "foreign-user" || tc.kind == "partial-storage" {
				repo.usage.UserID = uuid.New()
			}
			service.repository = repo
			id := usageID.String()
			if tc.kind == "invalid-id" {
				id = "not-a-uuid"
			}
			request := httptest.NewRequest(http.MethodGet, "/v1/usage-requests/"+id, nil)
			request.Header.Set("Authorization", "Bearer controlled-user")
			response := httptest.NewRecorder()
			service.Handler(successfulBillingVerify).ServeHTTP(response, request)
			if response.Code != tc.status || repo.calls != tc.calls {
				t.Fatalf("status=%d storage calls=%d; want %d/%d", response.Code, repo.calls, tc.status, tc.calls)
			}
			body := response.Body.String()
			var payload struct {
				Code  string    `json:"code"`
				ID    uuid.UUID `json:"usage_request_id"`
				Model string    `json:"model"`
			}
			if json.Unmarshal(response.Body.Bytes(), &payload) != nil || payload.Code != tc.code {
				t.Fatal("existing error classification/projection changed", body)
			}
			for _, private := range []string{"synthetic-storage-diagnostic", "synthetic-wrapper", "synthetic-private-missing", "synthetic-hidden-request", "request_snapshot"} {
				if strings.Contains(body, private) {
					t.Fatal("response leaked storage diagnostic or internal snapshot")
				}
			}
			if tc.status == http.StatusOK {
				if payload.ID != usageID || payload.Model != repo.usage.Model || !strings.Contains(body, `"maximum_microcredits":9007199254740993`) || response.Header().Get("Cache-Control") != "no-store" {
					t.Fatal("authorized safe user-wide detail lost identity/int64/no-store", body, response.Header())
				}
			} else if strings.Contains(body, repo.usage.Model) || strings.Contains(body, "UPSTREAM_ACTIVE") || strings.Contains(body, "DISPATCHING") {
				t.Fatal("failed or hidden detail disclosed resource state")
			}
			authority.mu.Lock()
			defer authority.mu.Unlock()
			if tc.kind == "organization-context" {
				if len(authority.permissions) != 0 {
					t.Fatal("unavailable context dispatched authorization")
				}
			} else if len(authority.permissions) != 1 || authority.permissions[0] != "personal.usage.read" || authority.organizationID != nil {
				t.Fatal("current personal authority scope changed", authority.permissions, authority.organizationID)
			}
		})
	}
}
