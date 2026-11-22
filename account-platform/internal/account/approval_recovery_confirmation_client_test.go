package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/google/uuid"
)

func TestRecoveryConfirmationClientSendsNoResultAndRequiresClosedAck(t *testing.T) {
	descriptor := approvalconsumer.BillingCommands()[0]
	candidate := ApprovalRecoveryCandidate{RequestID: uuid.New(), ExecutionID: uuid.New(), OriginalEventID: uuid.New(), OriginalKey: "original-key",
		Durable: descriptor.Durable, Workload: descriptor.Workload, CommandType: descriptor.CommandType,
		PolicyVersion: descriptor.PolicyVersion, CommandFingerprint: "a1" + "00000000000000000000000000000000000000000000000000000000000000",
		ApprovalEpoch: 1, RequestVersion: 1, Investigation: approvalconsumer.ClassifyProofFailure(descriptor.CommandType, descriptor.PolicyVersion, "")}
	lease := RecoveryQueryLease{RequestID: candidate.RequestID, Token: uuid.New(), Until: time.Now().UTC().Add(15 * time.Second), QueryCount: 1, Version: 1}
	for _, tc := range []struct {
		name, cache, body string
		status            int
		wantSuccess       bool
	}{
		{"closed ack", "no-store", "", http.StatusNoContent, true},
		{"cacheable", "public", "", http.StatusNoContent, false},
		{"redirect", "no-store", "", http.StatusFound, false},
		{"ordinary success", "no-store", "{}", http.StatusOK, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/approval-recovery/confirm" ||
					r.Header.Get("Authorization") != "Bearer dedicated" || r.Header.Get("Content-Type") != "application/json" {
					t.Errorf("unexpected recovery confirmation request: %s %s", r.Method, r.URL)
				}
				var payload map[string]json.RawMessage
				if json.NewDecoder(r.Body).Decode(&payload) != nil || len(payload) != 2 || len(payload["candidate"]) == 0 || len(payload["lease"]) == 0 {
					t.Error("worker transmitted a result or an incomplete binding")
				}
				w.Header().Set("Cache-Control", tc.cache)
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			client, err := NewApprovalRecoveryConfirmationClient(server.URL, "dedicated")
			if err != nil {
				t.Fatal(err)
			}
			err = client.ConfirmOriginalSuccess(context.Background(), candidate, lease)
			if (err == nil) != tc.wantSuccess || (err != nil && !errors.Is(err, ErrApprovalRecoveryConfirmationUnavailable)) {
				t.Fatalf("confirmation error=%v", err)
			}
		})
	}
}
