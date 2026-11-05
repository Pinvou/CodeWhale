package account

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

type ledgerProofHTTPStub struct {
	Repository
	calls int
}

func (s *ledgerProofHTTPStub) ReadLedgerRepairAuthorizationProofs(_ context.Context, ids []ledgerRepairProofIdentity) ([]ledgerRepairProofItem, error) {
	s.calls++
	out := make([]ledgerRepairProofItem, len(ids))
	for i, id := range ids {
		out[i] = ledgerRepairProofItem{ledgerRepairProofIdentity: id, Status: "UNAVAILABLE"}
	}
	return out, nil
}
func TestLedgerRepairAuthorizationProofBatchBoundary(t *testing.T) {
	requestID, eventID := uuid.NewString(), uuid.NewString()
	body := fmt.Sprintf(`{"schema_version":1,"items":[{"approval_request_id":%q,"authorization_event_id":%q,"approval_epoch":1}]}`, requestID, eventID)
	stub := &ledgerProofHTTPStub{}
	service := NewService(stub, nil)
	for _, tc := range []struct {
		name, service, body, url string
		status, calls            int
	}{
		{"billing", "billing-service", body, "/internal/v1/ledger-repair/authorization-proofs:batch", 200, 1},
		{"other", "token-gateway", body, "/internal/v1/ledger-repair/authorization-proofs:batch", 403, 1},
		{"static-token", "development-static-token", body, "/internal/v1/ledger-repair/authorization-proofs:batch", 403, 1},
		{"duplicate-field", "billing-service", strings.Replace(body, `"approval_epoch":1`, `"approval_epoch":1,"approval_epoch":1`, 1), "/internal/v1/ledger-repair/authorization-proofs:batch", 400, 1},
		{"bad-case", "billing-service", strings.Replace(body, requestID, strings.ToUpper(requestID), 1), "/internal/v1/ledger-repair/authorization-proofs:batch", 400, 1},
		{"extra-query", "billing-service", body, "/internal/v1/ledger-repair/authorization-proofs:batch?x=1", 400, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, err := serviceauth.ContextForJob(context.Background(), tc.service, "TEST_PROOF", uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, tc.url, strings.NewReader(tc.body)).WithContext(ctx)
			out := httptest.NewRecorder()
			service.ledgerRepairAuthorizationProofBatch(out, req)
			if out.Code != tc.status || stub.calls != tc.calls || out.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status=%d calls=%d", out.Code, stub.calls)
			}
			if tc.status == 200 {
				var got ledgerRepairProofResponse
				if json.Unmarshal(out.Body.Bytes(), &got) != nil || got.SchemaVersion != 1 || len(got.Items) != 1 || got.Items[0].Status != "UNAVAILABLE" || got.Items[0].ApprovalRequestID != requestID || got.Items[0].AuthorizationEventID != eventID {
					t.Fatal("invalid bounded response")
				}
			}
		})
	}
}
func TestLedgerRepairHistoricalExecutionKeyDigest(t *testing.T) {
	if got := ledgerRepairHistoricalExecutionKeyDigest("  approval:legacy-0001 \t\n"); got != "f546660307c2429b22bd84384f559bef74a6c24b32ce11d29dbf30f2a350d306" {
		t.Fatal("historical financial key wire changed")
	}
	if ledgerRepairHistoricalExecutionKeyDigest("  ") != "" {
		t.Fatal("empty key accepted")
	}
}
func TestLedgerRepairProofRouteRegisteredWithServiceAuth(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_CA_FILE", "")
	service := NewService(&ledgerProofHTTPStub{}, nil).WithServiceToken("controlled")
	handler := service.Handler(nil)
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/ledger-repair/authorization-proofs:batch", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer controlled")
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	if out.Code != http.StatusForbidden {
		t.Fatalf("static token unexpectedly authorized or route not registered: %d", out.Code)
	}
}

func TestLedgerRepairProofRouteWithBillingWorkloadOIDC(t *testing.T) {
	_, token := configureEntitlementProofIdentity(t)
	var rules []map[string]any
	if err := json.Unmarshal([]byte(os.Getenv("WORKLOAD_IDENTITY_RULES_JSON")), &rules); err != nil {
		t.Fatal(err)
	}
	if len(rules) != 2 {
		t.Fatal("unexpected issuer fixture")
	}
	rules[1]["service"] = "billing-service"
	rules[1]["permissions"] = []string{"internal.account.ledger-repair-proof.read"}
	encoded, err := json.Marshal(rules)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WORKLOAD_IDENTITY_RULES_JSON", string(encoded))
	stub := &ledgerProofHTTPStub{}
	handler := NewService(stub, nil).Handler(nil)
	body := fmt.Sprintf(`{"schema_version":1,"items":[{"approval_request_id":%q,"authorization_event_id":%q,"approval_epoch":1}]}`, uuid.NewString(), uuid.NewString())
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/ledger-repair/authorization-proofs:batch", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	if out.Code != http.StatusOK || stub.calls != 1 {
		t.Fatalf("billing OIDC identity rejected: status=%d calls=%d", out.Code, stub.calls)
	}
}
