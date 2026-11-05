package portal

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

func TestApprovalDecisionReceiptPortalHTTP(t *testing.T) {
	id := uuid.NewString()
	event := uuid.NewString()
	for _, tc := range []struct {
		name, method, action, body, header string
		want                               int
		session                            bool
		badReceipt                         string
	}{
		{"new-decision", "POST", "decide", `{"expected_version":4,"decision":"APPROVED","reason":"Controlled reason","command_protocol":1,"action_control_version":7}`, "7", 204, true, ""},
		{"read-original-under-current-control", "GET", "decision-result?command_protocol=1&expected_version=4&origin_action_control_version=1&decision=APPROVED&reason_digest=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "", "7", 200, true, ""},
		{"stale-write", "POST", "decide", `{"expected_version":4,"decision":"APPROVED","reason":"Controlled reason","command_protocol":1,"action_control_version":7}`, "1", 400, true, ""},
		{"read-requires-session", "GET", "decision-result?command_protocol=1&expected_version=4&origin_action_control_version=1&decision=APPROVED&reason_digest=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "", "7", 401, false, ""},
		{"read-requires-current-control", "GET", "decision-result?command_protocol=1&expected_version=4&origin_action_control_version=1&decision=APPROVED&reason_digest=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "", "1", 400, true, ""},
		{"read-rejects-extra-query", "GET", "decision-result?command_protocol=1&expected_version=4&origin_action_control_version=1&decision=APPROVED&reason_digest=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa&extra=1", "", "7", 400, true, ""},
		{"wrong-result-is-not-original-proof", "GET", "decision-result?command_protocol=1&expected_version=4&origin_action_control_version=1&decision=APPROVED&reason_digest=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "", "7", 503, true, "origin"},
		{"private-result-is-not-forwarded", "GET", "decision-result?command_protocol=1&expected_version=4&origin_action_control_version=1&decision=APPROVED&reason_digest=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "", "7", 503, true, "private"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &financeWireStore{memoryStore: newMemoryStore(), wire: map[string]int{}}
			service := testService(t, store)
			session := validSession(uuid.NewString())
			session.ApprovalV2Eligible = true
			if err := store.SaveSession(context.Background(), session); err != nil {
				t.Fatal(err)
			}
			var writes, reads atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", "no-store")
				if r.URL.Path == "/admin/v2/approval-capability" {
					io.WriteString(w, `{"schema_version":1,"mode":"PROOF_REQUIRED","approval_wire_version":2,"proof_required":true,"epoch":1,"authorization_relay_state":"RUNNING","authorization_relay_epoch":1,"version":7,"request_id":"controlled"}`)
					return
				}
				if r.Header.Get("Authorization") != "Bearer "+session.AccessToken || r.Header.Get("Cookie") != "" || r.Header.Get("X-CSRF-Token") != "" || r.Header.Get("X-Pinvou-Approval-Action-Control-Version") != "7" {
					t.Error("current authority/control transport changed")
				}
				if r.Method == "POST" {
					writes.Add(1)
					raw, _ := io.ReadAll(r.Body)
					if r.URL.Path != "/admin/v2/approval-requests/"+id+"/actions/decide" || string(raw) != tc.body {
						t.Error("withdraw original CAS/protocol changed")
					}
					json.NewEncoder(w).Encode(map[string]any{"approval_request_id": id})
					return
				}
				reads.Add(1)
				if r.URL.Path != "/admin/v2/approval-requests/"+id+"/actions/decision-result" || r.URL.Query().Get("expected_version") != "4" || r.URL.Query().Get("origin_action_control_version") != "1" {
					t.Error("confirmation upgraded the original invocation")
				}
				body := map[string]any{"approval_request_id": id, "command_protocol": 1, "operation": "DECIDE", "decision": "APPROVED", "expected_version": 4, "result_version": 5, "origin_action_control_version": 1, "action_control_version": 7, "reason_digest": strings.Repeat("a", 64), "receipt_event_id": event, "decision_id": event, "recorded_at": "2026-10-25T00:00:00Z"}
				if tc.badReceipt == "origin" {
					body["origin_action_control_version"] = 7
				}
				if tc.badReceipt == "private" {
					body["normalized_command"] = map[string]any{}
				}
				json.NewEncoder(w).Encode(body)
			}))
			defer upstream.Close()
			service.config.AccountURL = parseTestURL(t, upstream.URL)
			r := financeBrowserRequest(service, session, tc.method, "/api/admin/approvals/requests/"+id+"/actions/"+tc.action, tc.body)
			r.Header.Set("X-Pinvou-Approval-Action-Control-Version", tc.header)
			r.Header.Set("Authorization", "Bearer forged-browser")
			if tc.method == "GET" {
				r.Header.Del("X-CSRF-Token") // A proof read has no mutation admission.
			}
			if !tc.session {
				r.Header.Del("Cookie")
			}
			w := httptest.NewRecorder()
			service.Handler(nil).ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", w.Code, tc.want, w.Body.String())
			}
			if tc.method == "GET" && writes.Load() != 0 {
				t.Fatal("confirmation issued a write")
			}
			if tc.want == 204 && writes.Load() != 1 {
				t.Fatal("withdraw not dispatched exactly once")
			}
			if tc.want == 400 || tc.want == 401 {
				if writes.Load() != 0 || reads.Load() != 0 {
					t.Fatal("rejected request dispatched")
				}
			}
			if strings.Contains(tc.action, "decision-result") && tc.want == 200 && reads.Load() != 1 {
				t.Fatal("confirmation missing")
			}
		})
	}
}
