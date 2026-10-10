package portal

import (
	"context"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestF09EntitlementThroughProductionPortal(t *testing.T) {
	id := uuid.NewString()
	body := `{"command_type":"ENTITLEMENT_COMPONENT_ADJUSTMENT","idempotency_key":"controlled-correction-key","target_resource_type":"ENTITLEMENT_PACKAGE","target_resource_id":"` + id + `","normalized_command":{"package_id":"` + id + `","payment_order_id":"` + id + `","reason_code":"ORIGINAL_SPLIT_MISMATCH"},"reason":"Controlled correction approval reason","redacted_evidence":[],"expected_impact":{},"critical_resource_versions":{"package_id":"` + id + `","wallet_id":"` + id + `","wallet_version":9007199254740993,"payment_order_id":"` + id + `","payment_transaction_id":"` + id + `","source_component_id":"` + id + `","source_lot_id":"` + id + `","target_component_id":null,"target_lot_id":null,"source_type":"BASE","target_type":"BONUS","amount_microcredits":1,"source_evidence_digest":"` + strings.Repeat("a", 64) + `","facts_digest":"` + strings.Repeat("b", 64) + `","policy_version":2},"financial_snapshot":{"package_total_microcredits":9223372036854775807,"source_original_microcredits":9223372036854775807,"target_original_microcredits":0,"effective_total_microcredits":9223372036854775807}}`
	if validApprovalBFFMutation("create", []byte(body), 1, 1, "1") {
		t.Fatal("entitlement policy2 accepted legacy wire1")
	}
	if validApprovalBFFMutation("create", []byte(strings.Replace(body, `"policy_version":2`, `"policy_version":1`, 1)), 2, 1, "1") {
		t.Fatal("entitlement policy2 accepted mismatched frozen policy")
	}
	for _, test := range []struct {
		name, method, path, body string
		session, csrf            bool
		want                     int
		header                   string
	}{
		{"installed-owner-precheck", "POST", "/api/admin/account/admin/v2/approval-command-prechecks", `{"command_type":"ENTITLEMENT_COMPONENT_ADJUSTMENT","target_resource_id":"` + id + `","payment_order_id":"` + id + `","reason_code":"ORIGINAL_SPLIT_MISMATCH"}`, true, true, 200, ""},
		{"precheck-required-session", "POST", "/api/admin/account/admin/v2/approval-command-prechecks", `{}`, false, false, 401, ""},
		{"precheck-required-csrf", "POST", "/api/admin/account/admin/v2/approval-command-prechecks", `{}`, true, false, 403, ""},
		{"precheck-no-read-alias", "GET", "/api/admin/account/admin/v2/approval-command-prechecks", ``, true, true, 404, ""},
		{"installed-typed-create", "POST", "/api/admin/approvals/requests", body, true, true, 201, "1"},
		{"future-command-rejected", "POST", "/api/admin/approvals/requests", strings.Replace(body, "ENTITLEMENT_COMPONENT_ADJUSTMENT", "USAGE_SETTLEMENT_REPAIR", 1), true, true, 400, "1"},
		{"create-stale-control-rejected", "POST", "/api/admin/approvals/requests", body, true, true, 400, "2"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &financeWireStore{memoryStore: newMemoryStore(), wire: map[string]int{}}
			service := testService(t, store)
			session := validSession(uuid.NewString())
			session.ApprovalV2Eligible = true
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			var mutations atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				if r.URL.Path == "/admin/v2/approval-capability" {
					_, _ = io.WriteString(w, `{"schema_version":1,"mode":"PROOF_REQUIRED","approval_wire_version":2,"proof_required":true,"epoch":1,"authorization_relay_state":"RUNNING","authorization_relay_epoch":1,"version":1,"request_id":"controlled-capability"}`)
					return
				}
				mutations.Add(1)
				raw, err := io.ReadAll(r.Body)
				if err != nil || string(raw) != test.body || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Cookie") != "" || r.Header.Get("X-CSRF-Token") != "" || r.Method != test.method {
					t.Error("Portal changed facts or forwarded browser authority")
				}
				if test.path == "/api/admin/approvals/requests" {
					if r.URL.Path != "/admin/v2/approval-requests" || r.Header.Get("X-Pinvou-Approval-Action-Control-Version") != "1" {
						t.Error("typed route/control changed")
					}
					w.WriteHeader(201)
					_, _ = io.WriteString(w, `{"approval_request_id":"`+id+`"}`)
					return
				}
				if r.URL.Path != "/admin/v2/approval-command-prechecks" {
					t.Error("wrong precheck owner route")
				}
				_, _ = io.WriteString(w, `{"controlled_owner_projection":true}`)
			}))
			defer upstream.Close()
			service.config.AccountURL = parseTestURL(t, upstream.URL)
			request := financeBrowserRequest(service, session, test.method, test.path, test.body)
			if !test.session {
				request.Header.Del("Cookie")
			}
			if !test.csrf {
				request.Header.Del("X-CSRF-Token")
			}
			request.Header.Set("Authorization", "Bearer forged-browser")
			request.Header.Set("X-Internal-Service-Token", "forged-browser")
			if test.header != "" {
				request.Header.Set("X-Pinvou-Approval-Action-Control-Version", test.header)
			}
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("actual Portal status=%d want=%d body=%s", response.Code, test.want, response.Body.String())
			}
			expected := int32(0)
			if test.want == 200 || test.want == 201 {
				expected = 1
			}
			if mutations.Load() != expected {
				t.Fatalf("upstream mutations=%d want=%d", mutations.Load(), expected)
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("authority response cacheable")
			}
		})
	}
}
