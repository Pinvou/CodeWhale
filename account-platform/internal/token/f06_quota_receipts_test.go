package token

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/google/uuid"
)

// Actual installed facade, HTTP authorization/delegation and HTTP Billing
// client. Controlled upstream receipts are not persisted financial effects.
func TestF06InstalledQuotaReceiptsPreserveUncertainty(t *testing.T) {
	for _, operation := range []string{"batch", "revoke"} {
		for _, tc := range []struct {
			name                                string
			status                              int
			malformed, network, denied, invalid bool
			want                                int
		}{
			{"success", 200, false, false, false, false, 200},
			{"bad-request", 400, false, false, false, false, 400},
			{"permission", 403, false, false, false, false, 403},
			{"missing", 404, false, false, false, false, 404},
			{"conflict", 409, false, false, false, false, 409},
			{"domain-invalid", 422, false, false, false, false, 422},
			{"rate-limit", 429, false, false, false, false, 429},
			{"rate-zero", 429, false, false, false, false, 429},
			{"rate-invalid-sign", 429, false, false, false, false, 429},
			{"rate-invalid-negative", 429, false, false, false, false, 429},
			{"credential-unknown", 401, false, false, false, false, 503},
			{"service-unknown", 503, false, false, false, false, 503},
			{"malformed-success", 200, true, false, false, false, 503},
			{"lost-response", 200, false, true, false, false, 503},
			{"local-permission-denied", 200, false, false, true, false, 403},
			{"invalid-local-body", 200, false, false, false, true, 400},
			{"null-array", 200, false, false, false, true, 400},
			{"missing-array", 200, false, false, false, true, 400},
			{"empty-array", 200, false, false, false, false, 200},
		} {
			if operation == "revoke" && (tc.name == "null-array" || tc.name == "missing-array" || tc.name == "empty-array") {
				continue
			}
			t.Run(operation+"/"+tc.name, func(t *testing.T) {
				user, org, grant := uuid.New(), uuid.New(), uuid.New()
				key := "f06-quota-unchanged-key"
				var calls, delegations atomic.Int32
				permission := "org.quota.allocate"
				suffix := "/quota-grants:batch"
				body := `{"items":[{"amount_microcredits":9007199254740993}]}`
				if operation == "revoke" {
					permission = "org.quota.revoke"
					suffix = "/quota-grants/" + grant.String() + "/revoke"
					body = `{"reason":"controlled revoke"}`
				}
				upstreamPath := "/internal/v1/organizations/" + org.String() + suffix
				authority := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					var in map[string]json.RawMessage
					if json.NewDecoder(r.Body).Decode(&in) != nil {
						t.Error("invalid authority body")
					}
					var actualOrg uuid.UUID
					_ = json.Unmarshal(in["target_organization_id"], &actualOrg)
					var actualPermission string
					_ = json.Unmarshal(in["permission"], &actualPermission)
					if actualOrg != org || actualPermission != permission {
						t.Error("wrong organization permission")
					}
					switch r.URL.Path {
					case "/internal/v1/authorization/check":
						w.Header().Set("X-Authorization-Version", "17")
						_ = json.NewEncoder(w).Encode(map[string]any{"allowed": !tc.denied, "user_id": user})
					case "/internal/v1/authorization/human-delegations":
						delegations.Add(1)
						var path, sha string
						_ = json.Unmarshal(in["http_path"], &path)
						_ = json.Unmarshal(in["request_sha256"], &sha)
						if path != upstreamPath || len(sha) != 64 {
							t.Error("delegation lost request binding")
						}
						w.WriteHeader(201)
						_, _ = fmt.Fprint(w, `{"delegation":"controlled-delegation"}`)
					default:
						t.Error("unexpected authority route")
						http.NotFound(w, r)
					}
				}))
				defer authority.Close()
				billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					raw, _ := io.ReadAll(r.Body)
					if r.URL.Path != upstreamPath || r.Header.Get("Idempotency-Key") != key || r.Header.Get("Authorization") != "Bearer controlled-service" || r.Header.Get("X-Pinvou-Human-Delegation") != "controlled-delegation" {
						t.Error("quota write lost original key/authority/target")
					}
					if operation == "batch" && !tc.invalid && tc.name != "empty-array" && !strings.Contains(string(raw), "9007199254740993") {
						t.Error("exact amount changed")
					}
					if tc.network {
						conn, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = conn.Close()
						return
					}
					status := tc.status
					if status == 200 && operation == "batch" {
						status = 207
					}
					if status == 429 {
						w.Header().Set("Retry-After", "7")
						if tc.name == "rate-zero" {
							w.Header().Set("Retry-After", "0")
						}
						if tc.name == "rate-invalid-sign" {
							w.Header().Set("Retry-After", "+7")
						}
						if tc.name == "rate-invalid-negative" {
							w.Header().Set("Retry-After", "-1")
						}
					}
					w.WriteHeader(status)
					if tc.malformed {
						_, _ = fmt.Fprint(w, `{"broken":`)
						return
					}
					if tc.status != 200 {
						_, _ = fmt.Fprint(w, `{"code":"CONTROLLED_REJECTION","message":"private upstream detail"}`)
						return
					}
					_, _ = fmt.Fprint(w, `{"accepted":true,"amount_microcredits":9007199254740993}`)
				}))
				defer billing.Close()
				auth, err := authorization.NewClient(authority.URL, "controlled-service")
				if err != nil {
					t.Fatal(err)
				}
				client, err := NewHTTPBillingClient(billing.URL, "controlled-service")
				if err != nil {
					t.Fatal(err)
				}
				client.WithHumanDelegations(auth)
				version := int64(3)
				s := NewService(fixedBillingContextResolver{AuthorizationContext{UserID: user, ContextType: "ORGANIZATION", OrganizationID: &org, OrganizationMembershipVersion: &version}}, nil, client).WithManagement(nil, nil, ProviderURLPolicy{}, "controlled-service", nil).WithAuthorizer(auth)
				if tc.invalid {
					if operation == "batch" {
						body = `{"items":{}}`
					} else {
						body = `{"reason":""}`
					}
				}
				if tc.name == "null-array" {
					body = `{"items":null}`
				}
				if tc.name == "missing-array" {
					body = `{}`
				}
				if tc.name == "empty-array" {
					body = `{"items":[]}`
				}
				req := httptest.NewRequest("POST", "/v1/organizations/"+org.String()+suffix, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer controlled-user")
				req.Header.Set("Idempotency-Key", key)
				out := httptest.NewRecorder()
				s.Handler(successfulBillingVerify).ServeHTTP(out, req)
				want := tc.want
				if want == 200 && operation == "batch" {
					want = 207
				}
				if out.Code != want {
					t.Fatalf("HTTP=%d body=%s want=%d", out.Code, out.Body, want)
				}
				if strings.Contains(out.Body.String(), "private upstream detail") {
					t.Fatal("upstream private detail leaked")
				}
				if tc.denied || tc.invalid {
					if calls.Load() != 0 || delegations.Load() != 0 {
						t.Fatal("local rejection reached upstream")
					}
				} else if calls.Load() != 1 || delegations.Load() != 1 {
					t.Fatal("write retried or authority missing")
				}
				if tc.want == 503 && !strings.Contains(out.Body.String(), "BILLING_UNAVAILABLE") {
					t.Fatal("unknown outcome presented as conflict")
				}
				if tc.want == 429 {
					wantRetry := "7"
					if tc.name == "rate-zero" {
						wantRetry = "0"
					}
					if tc.name == "rate-invalid-sign" || tc.name == "rate-invalid-negative" {
						wantRetry = ""
					}
					if out.Header().Get("Retry-After") != wantRetry {
						t.Fatal("rate-limit receipt changed legal/invalid delay-seconds")
					}
				}
				if tc.name == "success" && (out.Header().Get("Cache-Control") != "no-store" || !strings.Contains(out.Body.String(), "9007199254740993")) {
					t.Fatal("success receipt changed/cacheable")
				}
			})
		}
	}
}
