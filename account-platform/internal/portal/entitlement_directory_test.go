package portal

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

// Exercise the installed Portal route and its Account HTTP projection. The
// local upstream supplies controlled authority, not an external issuer or DB.
func TestF09EntitlementApprovalDirectoryInstalledRoute(t *testing.T) {
	for _, test := range []struct {
		name, fault string
		wire        int
		want        int
		code        string
	}{
		{"typed-session", "", 2, http.StatusOK, ""},
		{"legacy-session-read-only", "", 1, http.StatusOK, ""},
		{"account-permission-denied", "forbidden", 2, http.StatusForbidden, "PERMISSION_DENIED"},
		{"unknown-future-command", "unknown", 2, http.StatusServiceUnavailable, "APPROVAL_PROTOCOL_UNAVAILABLE"},
		{"directory-failure", "unavailable", 2, http.StatusServiceUnavailable, "APPROVAL_PROTOCOL_UNAVAILABLE"},
		{"invalid-directory-json", "json", 2, http.StatusServiceUnavailable, "APPROVAL_PROTOCOL_UNAVAILABLE"},
		{"directory-cacheable", "cache", 2, http.StatusServiceUnavailable, "APPROVAL_PROTOCOL_UNAVAILABLE"},
		{"directory-version-mismatch", "version", 2, http.StatusServiceUnavailable, "APPROVAL_PROTOCOL_UNAVAILABLE"},
		{"authority-changed-after-read", "control", 2, http.StatusConflict, "APPROVAL_ACTION_CONTROL_CHANGED"},
		{"session-required", "session", 2, http.StatusUnauthorized, "LOGIN_REQUIRED"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &financeWireStore{memoryStore: newMemoryStore(), wire: map[string]int{}}
			service := testService(t, store)
			session := validSession(uuid.NewString())
			session.ApprovalV2Eligible = true
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			commands := []string{"WALLET_ADJUSTMENT", "PAYMENT_PROVIDER_RESUME", "PAYMENT_PRODUCT_RESUME", "TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK", "TOKEN_CREDENTIAL_REVOKE", "ENTITLEMENT_COMPONENT_ADJUSTMENT"}
			if test.fault == "unknown" {
				commands = append(commands, "UNRECOGNIZED_NEXT_APPROVAL_COMMAND")
			}
			items := make([]string, 0, len(commands))
			for _, command := range commands {
				policy, schema := 1, 1
				switch command {
				case "PAYMENT_PROVIDER_RESUME", "TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK", "ENTITLEMENT_COMPONENT_ADJUSTMENT":
					policy, schema = 2, 2
				}
				risk := "FINANCIAL_FACT_CHANGE"
				if strings.HasPrefix(command, "TOKEN_") {
					risk = "GOVERNANCE_OR_NORMAL_STATE_MACHINE"
				}
				items = append(items, fmt.Sprintf(`{"command_type":%q,"policy_version":%d,"snapshot_schema_version":%d,"risk_class":%q,"visible":true,"requestable":true}`, command, policy, schema, risk))
			}
			projection := `{"schema_version":1,"platform_authorization_version":7,"items":[` + strings.Join(items, ",") + `]}`
			var capabilityCalls, directoryCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Cookie") != "" || r.Header.Get("X-CSRF-Token") != "" {
					t.Error("directory request did not retain server-side session authority")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				switch r.URL.Path {
				case "/admin/v2/approval-capability":
					version := 1
					if capabilityCalls.Add(1) == 2 && test.fault == "control" {
						version = 2
					}
					mode, proof := "PROOF_REQUIRED", true
					if test.wire == 1 {
						mode, proof = "LEGACY", false
					}
					_, _ = fmt.Fprintf(w, `{"schema_version":1,"mode":%q,"approval_wire_version":%d,"proof_required":%t,"epoch":1,"authorization_relay_state":"RUNNING","authorization_relay_epoch":1,"version":%d,"request_id":"controlled-directory"}`, mode, test.wire, proof, version)
				case "/admin/v2/approval-directory":
					directoryCalls.Add(1)
					w.Header().Set("X-Platform-Authorization-Version", "7")
					switch test.fault {
					case "forbidden":
						w.WriteHeader(http.StatusForbidden)
						return
					case "unavailable":
						w.WriteHeader(http.StatusInternalServerError)
						return
					case "json":
						_, _ = io.WriteString(w, `{"items":`)
						return
					case "cache":
						w.Header().Set("Cache-Control", "public")
					case "version":
						w.Header().Set("X-Platform-Authorization-Version", "8")
					}
					_, _ = io.WriteString(w, projection)
				default:
					t.Error("directory reached an unexpected Account endpoint")
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer upstream.Close()
			service.config.AccountURL = parseTestURL(t, upstream.URL)
			request := financeBrowserRequest(service, session, http.MethodGet, "/api/admin/approvals/directory", "")
			request.Header.Set("Authorization", "Bearer untrusted-browser")
			if test.fault == "session" {
				request.Header.Del("Cookie")
			}
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("installed directory status=%d want=%d body=%s", response.Code, test.want, response.Body.String())
			}
			if response.Header().Get("Cache-Control") != "no-store" || strings.Contains(response.Body.String(), session.AccessToken) {
				t.Fatal("directory leaked session authority or became cacheable")
			}
			if test.want == http.StatusOK {
				if response.Body.String() != projection || response.Header().Get("X-Platform-Authorization-Version") != "7" || capabilityCalls.Load() != 2 || directoryCalls.Load() != 1 {
					t.Fatal("valid directory was not forwarded unchanged after authority recheck")
				}
			} else {
				if !strings.Contains(response.Body.String(), `"code":"`+test.code+`"`) || strings.Contains(response.Body.String(), `"items"`) {
					t.Fatal("rejected directory exposed projection or lost bounded error")
				}
				if test.fault == "session" {
					if capabilityCalls.Load() != 0 || directoryCalls.Load() != 0 {
						t.Fatal("unauthenticated directory reached Account")
					}
				} else if directoryCalls.Load() != 1 {
					t.Fatal("directory did not exercise the actual Account HTTP path")
				}
			}
		})
	}
}
