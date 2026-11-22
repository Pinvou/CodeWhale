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

func TestF13ClassifierProductionCreate(t *testing.T) {
	id, evidenceID, anomalyID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	template := `{"command_type":"REFUND_EXCEPTION_REPAIR","idempotency_key":"controlled-refund-repair-key","target_resource_type":"REFUND","target_resource_id":"` + id + `","normalized_command":{"refund_id":"` + id + `","payment_evidence_id":"` + evidenceID + `","anomaly_case_id":"` + anomalyID + `","reason_code":"PROVIDER_REFUNDED_LOCAL_REVERSAL_MISSING"},"reason":"Controlled payment repair approval reason","redacted_evidence":[],"expected_impact":{},"critical_resource_versions":{"refund_updated_at":"2026-10-01T00:00:00Z","anomaly_version":9007199254740993,"facts_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","policy_version":1},"financial_snapshot":{"amount_fen":9007199254740993,"credit_microcredits":9223372036854775807,"provider_refund_id":"controlled-provider-refund","wallet_id":"` + id + `","wallet_version":9007199254740993,"package_id":"` + id + `","revoked_microcredits":9223372036854775806,"shortfall_microcredits":1}}`
	for _, test := range []struct {
		name, command string
		csrf          bool
		want          int
	}{
		{"installed-refund", "REFUND_EXCEPTION_REPAIR", true, 201},
		{"future-not-installed", "UNINSTALLED_FINANCIAL_REPAIR", true, 400},
		{"csrf-still-required", "REFUND_EXCEPTION_REPAIR", false, 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &financeWireStore{memoryStore: newMemoryStore(), wire: map[string]int{}}
			service := testService(t, store)
			session := validSession(uuid.NewString())
			session.ApprovalV2Eligible = true
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			body := strings.Replace(template, "REFUND_EXCEPTION_REPAIR", test.command, 1)
			var mutations atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				if r.URL.Path == "/admin/v2/approval-capability" {
					_, _ = io.WriteString(w, `{"schema_version":1,"mode":"PROOF_REQUIRED","approval_wire_version":2,"proof_required":true,"epoch":1,"authorization_relay_state":"RUNNING","authorization_relay_epoch":1,"version":1,"request_id":"controlled-capability"}`)
					return
				}
				mutations.Add(1)
				got, err := io.ReadAll(r.Body)
				if err != nil || string(got) != body || r.Method != "POST" || r.URL.Path != "/admin/v2/approval-requests" || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Cookie") != "" || r.Header.Get("X-CSRF-Token") != "" || r.Header.Get("X-Internal-Service-Token") != "" || r.Header.Get("X-Pinvou-Approval-Action-Control-Version") != "1" {
					t.Error("production Portal changed facts/route or browser authority")
				}
				w.WriteHeader(201)
				_, _ = io.WriteString(w, `{"approval_request_id":"`+id+`"}`)
			}))
			defer upstream.Close()
			service.config.AccountURL = parseTestURL(t, upstream.URL)
			request := financeBrowserRequest(service, session, "POST", "/api/admin/approvals/requests", body)
			request.Header.Set("X-Pinvou-Approval-Action-Control-Version", "1")
			request.Header.Set("Authorization", "Bearer forged-browser")
			request.Header.Set("X-Internal-Service-Token", "forged-browser")
			if !test.csrf {
				request.Header.Del("X-CSRF-Token")
			}
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("actual Portal status=%d want=%d body=%s", response.Code, test.want, response.Body.String())
			}
			wantMutations := int32(0)
			if test.want == 201 {
				wantMutations = 1
			}
			if mutations.Load() != wantMutations {
				t.Fatalf("upstream mutations=%d want=%d", mutations.Load(), wantMutations)
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("authority response cacheable")
			}
		})
	}
}

func TestF13ClassifierProductionDirectory(t *testing.T) {
	for _, test := range []struct {
		name, command string
		want          int
	}{
		{"installed-refund", "REFUND_EXCEPTION_REPAIR", 200},
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
			body := `{"schema_version":1,"platform_authorization_version":7,"items":[{"command_type":"` + test.command + `","policy_version":1,"snapshot_schema_version":1,"risk_class":"FINANCIAL_FACT_CHANGE","visible":true,"requestable":true}]}`
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
