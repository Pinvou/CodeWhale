package token

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// These real identity/Account-authorization boundaries require no database.
// Missing-repository 503 is distinct from a genuine scoped receipt 404, whose
// actor/type/key isolation remains covered by the opt-in public PostgreSQL leaf.
func TestInstalledCommandLookupCurrentAuthorityWithoutRepository(t *testing.T) {
	actor := uuid.New()
	calls := 0
	authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var query struct {
			Subject     string   `json:"subject"`
			Permissions []string `json:"all_permissions"`
		}
		if json.NewDecoder(r.Body).Decode(&query) != nil || len(query.Permissions) != 1 || query.Permissions[0] != "platform.tokens.manage" {
			t.Errorf("installed provisioning lookup used wrong authorization contract")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("X-Authorization-Version", "1")
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": query.Subject == "actor", "user_id": actor})
	}))
	defer authority.Close()
	authorizer, err := authorization.NewClient(authority.URL, "controlled-service")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{authorizer: authorizer}
	handler := identity.Middleware(func(_ context.Context, bearer string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.example", Subject: bearer}, nil
	}, http.HandlerFunc(service.adminTokenCommandByIdempotencyKey))
	for _, tc := range []struct {
		name, subject, key, command string
		status, queries             int
	}{
		{"authorized-missing-owner-not-notfound", "actor", uuid.NewString(), "TOKEN_ROUTE_PROVISIONING_REPAIR", 503, 1},
		{"different-actor-authority-denied", "foreign", uuid.NewString(), "TOKEN_ROUTE_PROVISIONING_REPAIR", 403, 1},
		{"invalid-key-after-authority", "actor", "short", "TOKEN_ROUTE_PROVISIONING_REPAIR", 400, 1},
		{"unknown-command-does-not-query", "actor", uuid.NewString(), "TOKEN_FUTURE_UNKNOWN", 400, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls
			request := httptest.NewRequest(http.MethodGet, "/admin/v1/commands/by-idempotency-key?command_type="+tc.command, nil)
			request.Header.Set("Authorization", "Bearer "+tc.subject)
			request.Header.Set("Idempotency-Key", tc.key)
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, request)
			if out.Code != tc.status || calls-before != tc.queries {
				t.Fatalf("status=%d queries=%d expected=%d/%d body=%s", out.Code, calls-before, tc.status, tc.queries, out.Body.String())
			}
			if tc.status == 503 && !strings.Contains(out.Body.String(), "COMMAND_RESULT_UNAVAILABLE") {
				t.Fatal("missing capability must not claim an absent receipt")
			}
			if tc.status == 403 && !strings.Contains(out.Body.String(), "PERMISSION_DENIED") {
				t.Fatal("actor rejection must preserve permission boundary")
			}
			if out.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("lookup response cacheable")
			}
		})
	}
}
