package portal

import (
	"context"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestF14RefundAndChargebackThroughProductionPortal(t *testing.T) {
	id := uuid.NewString()

	evidenceID, anomalyID := uuid.NewString(), uuid.NewString()
	refundBody := `{"command_type":"REFUND_EXCEPTION_REPAIR","idempotency_key":"controlled-refund-repair-key","target_resource_type":"REFUND","target_resource_id":"` + id + `","normalized_command":{"refund_id":"` + id + `","payment_evidence_id":"` + evidenceID + `","anomaly_case_id":"` + anomalyID + `","reason_code":"PROVIDER_REFUNDED_LOCAL_REVERSAL_MISSING"},"reason":"Controlled payment repair approval reason","redacted_evidence":[],"expected_impact":{},"critical_resource_versions":{"refund_updated_at":"2026-10-01T00:00:00Z","anomaly_version":9007199254740993,"facts_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","policy_version":1},"financial_snapshot":{"amount_fen":9007199254740993,"credit_microcredits":9223372036854775807,"provider_refund_id":"controlled-provider-refund","wallet_id":"` + id + `","wallet_version":9007199254740993,"package_id":"` + id + `","revoked_microcredits":9223372036854775806,"shortfall_microcredits":1}}`
	for _, test := range []struct {
		name, method, path, body string
		session, csrf            bool
		want                     int
		header                   string
	}{

		{"installed-refund-parent-typed-create", "POST", "/api/admin/approvals/requests", refundBody, true, true, 201, "1"},
		{"installed-chargeback-typed-create", "POST", "/api/admin/approvals/requests", `{"command_type":"CHARGEBACK_FINANCIAL_REPAIR","idempotency_key":"controlled-chargeback-key","target_resource_type":"CHARGEBACK_CASE","target_resource_id":"00000000-0000-4000-8000-000000000001","normalized_command":{"chargeback_case_id":"00000000-0000-4000-8000-000000000001","payment_evidence_id":"00000000-0000-4000-8000-000000000002","expected_case_version":9007199254740993,"reason_code":"BASE_SHORTFALL_COMPENSATION"},"reason":"Controlled chargeback correction reason","redacted_evidence":[],"expected_impact":{},"critical_resource_versions":{"chargeback_case_version":9007199254740993,"facts_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","policy_version":1},"financial_snapshot":{"base_shortfall_microcredits":9223372036854775807,"wallet_id":"00000000-0000-4000-8000-000000000003","wallet_version":9007199254740993,"package_id":"00000000-0000-4000-8000-000000000004","component_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","provider_chargeback_id":"controlled-provider-chargeback"}}`, true, true, 201, "1"},
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

func TestF14ClassifierProductionDirectory(t *testing.T) {
	for _, test := range []struct {
		name, command string
		want          int
	}{
		{"installed-chargeback", "CHARGEBACK_FINANCIAL_REPAIR", 200},
		{"future-not-installed", "UNINSTALLED_FINANCIAL_REPAIR", 503},
		{"predecessor-entitlement", "ENTITLEMENT_COMPONENT_ADJUSTMENT", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &financeWireStore{memoryStore: newMemoryStore(), wire: map[string]int{}}
			service := testService(t, store)
			session := validSession(uuid.NewString())
			session.ApprovalV2Eligible = true
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			body := `{"schema_version":1,"platform_authorization_version":7,"items":[{"command_type":"` + test.command + `","policy_version":1,"snapshot_schema_version":1,"risk_class":"R2","visible":true,"requestable":true}]}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				if r.URL.Path == "/admin/v2/approval-capability" {
					_, _ = io.WriteString(w, `{"schema_version":1,"mode":"PROOF_REQUIRED","approval_wire_version":2,"proof_required":true,"epoch":1,"authorization_relay_state":"RUNNING","authorization_relay_epoch":1,"version":1,"request_id":"controlled-capability"}`)
					return
				}
				if r.Method != "GET" || r.URL.Path != "/admin/v2/approval-directory" || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Cookie") != "" || r.Header.Get("X-Internal-Service-Token") != "" {
					t.Error("production directory changed route or browser authority")
				}
				w.Header().Set("X-Platform-Authorization-Version", "7")
				_, _ = io.WriteString(w, body)
			}))
			defer upstream.Close()
			service.config.AccountURL = parseTestURL(t, upstream.URL)
			request := financeBrowserRequest(service, session, "GET", "/api/admin/approvals/directory", "")
			request.Header.Set("X-Internal-Service-Token", "forged-browser")
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("actual directory status=%d want=%d body=%s", response.Code, test.want, response.Body.String())
			}
			if test.want == 200 && response.Body.String() != body {
				t.Fatal("directory changed owner facts")
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("directory cacheable")
			}
		})
	}
}
