package portal

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestF15ReconciliationFinancialRepairProductionApprovalBFF(t *testing.T) {
	id := uuid.NewString()
	create := `{"command_type":"RECONCILIATION_FINANCIAL_REPAIR","idempotency_key":"controlled-reconciliation-key","target_resource_type":"RECONCILIATION_CASE","target_resource_id":"00000000-0000-4000-8000-000000000001","normalized_command":{"reconciliation_case_id":"00000000-0000-4000-8000-000000000001","payment_evidence_id":"00000000-0000-4000-8000-000000000002","business_date":"2026-10-07","resolution_code":"PROVIDER_REFUND_CONFIRMED"},"reason":"Controlled reconciliation correction reason","redacted_evidence":[],"expected_impact":{},"critical_resource_versions":{"run_id":"00000000-0000-4000-8000-000000000003","case_id":"00000000-0000-4000-8000-000000000001","provider_snapshot_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","facts_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","policy_version":1},"financial_snapshot":{"amount_fen":9007199254740993,"currency":"CNY","provider_transaction_id":"controlled-provider-transaction","ledger_effect":"NONE"}}`
	precheck := `{"command_type":"RECONCILIATION_FINANCIAL_REPAIR","target_resource_id":"00000000-0000-4000-8000-000000000001","payment_evidence_id":"00000000-0000-4000-8000-000000000002","reason_code":"PROVIDER_REFUND_CONFIRMED","business_date":"2026-10-07"}`
	for _, test := range []struct {
		name, method, path, body, upstream string
		session, csrf                      bool
		control                            string
		want                               int
	}{
		{"create-lossless", "POST", "/api/admin/approvals/requests", create, "/admin/v2/approval-requests", true, true, "1", 201},
		{"update-original-intent", "PUT", "/api/admin/approvals/requests/" + id, strings.TrimSuffix(create, "}") + `,"expected_version":9007199254740993}`, "/admin/v2/approval-requests/" + id, true, true, "1", 204},
		{"readonly-owner-precheck", "POST", "/api/admin/account/admin/v2/approval-command-prechecks", precheck, "/admin/v2/approval-command-prechecks", true, true, "", 200},
		{"create-session-required", "POST", "/api/admin/approvals/requests", create, "", false, true, "1", 401},
		{"create-csrf-required", "POST", "/api/admin/approvals/requests", create, "", true, false, "1", 403},
		{"stale-control", "POST", "/api/admin/approvals/requests", create, "", true, true, "2", 400},
		{"future-command-denied", "POST", "/api/admin/approvals/requests", strings.Replace(create, "RECONCILIATION_FINANCIAL_REPAIR", "UNINSTALLED_FINANCIAL_REPAIR", 1), "", true, true, "1", 400},
		{"duplicate-outer-command-denied", "POST", "/api/admin/approvals/requests", strings.TrimSuffix(create, "}") + `,"command_type":"RECONCILIATION_FINANCIAL_REPAIR"}`, "", true, true, "1", 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &financeWireStore{memoryStore: newMemoryStore(), wire: map[string]int{}}
			service := testService(t, store)
			session := validSession(uuid.NewString())
			session.ApprovalV2Eligible = true
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				if r.URL.Path == "/admin/v2/approval-capability" {
					_, _ = io.WriteString(w, `{"schema_version":1,"mode":"PROOF_REQUIRED","approval_wire_version":2,"proof_required":true,"epoch":1,"authorization_relay_state":"RUNNING","authorization_relay_epoch":1,"version":1,"request_id":"controlled-reconciliation-repair-capability"}`)
					return
				}
				calls.Add(1)
				raw, err := io.ReadAll(r.Body)
				if err != nil || string(raw) != test.body || r.URL.Path != test.upstream || r.Method != test.method || r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Cookie") != "" || r.Header.Get("X-CSRF-Token") != "" || r.Header.Get("X-Internal-Service-Token") != "" {
					t.Error("Portal altered frozen facts, route or server-only authority")
				}
				if strings.Contains(test.path, "/approvals/") && r.Header.Get("X-Pinvou-Approval-Action-Control-Version") != "1" {
					t.Error("Portal did not bind current approval generation")
				}
				status := test.want
				if status == 204 {
					status = 200
				}
				w.WriteHeader(status)
				if test.want == 201 || test.want == 204 {
					_, _ = io.WriteString(w, `{"approval_request_id":"`+id+`"}`)
				} else {
					_, _ = io.WriteString(w, `{"controlled":true}`)
				}
			}))
			defer upstream.Close()
			service.config.AccountURL = parseTestURL(t, upstream.URL)
			request := financeBrowserRequest(service, session, test.method, test.path, test.body)
			request.Header.Set("Authorization", "Bearer forged-browser")
			request.Header.Set("X-Internal-Service-Token", "forged-browser")
			if !test.session {
				request.Header.Del("Cookie")
			}
			if !test.csrf {
				request.Header.Del("X-CSRF-Token")
			}
			if test.control != "" {
				request.Header.Set("X-Pinvou-Approval-Action-Control-Version", test.control)
			}
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, request)
			if response.Code != test.want {
				t.Fatalf("Portal status=%d want=%d body=%s", response.Code, test.want, response.Body.String())
			}
			wantCalls := int32(0)
			if test.want == 200 || test.want == 201 || test.want == 204 {
				wantCalls = 1
			}
			if calls.Load() != wantCalls {
				t.Fatalf("protected upstream calls=%d want=%d", calls.Load(), wantCalls)
			}
			if response.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("approval or precheck authority was cacheable")
			}
		})
	}
}

func TestF15ReconciliationFinancialRepairProductionDirectoryBFF(t *testing.T) {
	for _, command := range []string{"RECONCILIATION_FINANCIAL_REPAIR", "UNINSTALLED_FINANCIAL_REPAIR"} {
		t.Run(command, func(t *testing.T) {
			store := &financeWireStore{memoryStore: newMemoryStore(), wire: map[string]int{}}
			service := testService(t, store)
			session := validSession(uuid.NewString())
			session.ApprovalV2Eligible = true
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			body := `{"schema_version":1,"platform_authorization_version":8,"items":[{"command_type":"` + command + `","policy_version":1,"snapshot_schema_version":1,"risk_class":"FINANCIAL_FACT_CHANGE","visible":true,"requestable":true}]}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				if r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Cookie") != "" {
					t.Error("directory browser authority escaped")
				}
				if r.URL.Path == "/admin/v2/approval-capability" {
					_, _ = io.WriteString(w, `{"schema_version":1,"mode":"PROOF_REQUIRED","approval_wire_version":2,"proof_required":true,"epoch":1,"authorization_relay_state":"RUNNING","authorization_relay_epoch":1,"version":1,"request_id":"controlled-reconciliation-repair-directory"}`)
					return
				}
				if r.Method != "GET" || r.URL.Path != "/admin/v2/approval-directory" {
					t.Error("directory production route changed")
				}
				w.Header().Set("X-Platform-Authorization-Version", "8")
				_, _ = io.WriteString(w, body)
			}))
			defer upstream.Close()
			service.config.AccountURL = parseTestURL(t, upstream.URL)
			response := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(response, financeBrowserRequest(service, session, "GET", "/api/admin/approvals/directory", ""))
			if command == "RECONCILIATION_FINANCIAL_REPAIR" {
				if response.Code != 200 || response.Body.String() != body || response.Header().Get("X-Platform-Authorization-Version") != "8" {
					t.Fatalf("current directory not preserved: %d %s", response.Code, response.Body.String())
				}
			} else if response.Code != 503 {
				t.Fatalf("future directory was not refused: %d", response.Code)
			}
		})
	}
}
