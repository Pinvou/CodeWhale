package account

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvaleffect"
	"github.com/google/uuid"
)

// Controlled authority receipts test HTTP composition; they are not a claim
// that a real financial command or provider transaction executed in this test.
func TestApprovalRecoveryRuntimeHTTPContinuesAfterConfirmationFailure(t *testing.T) {
	var descriptor approvalconsumer.Descriptor
	for _, installed := range approvalconsumer.BillingCommands() {
		if installed.CommandType == "WALLET_ADJUSTMENT" {
			descriptor = installed
			break
		}
	}
	if descriptor.CommandType != "WALLET_ADJUSTMENT" {
		t.Fatal("wallet recovery descriptor is not installed")
	}
	items := make([]ApprovalRecoveryCandidate, 2)
	for i := range items {
		items[i] = ApprovalRecoveryCandidate{RequestID: uuid.New(), ExecutionID: uuid.New(), OriginalEventID: uuid.New(), OriginalKey: "controlled-original-key", Durable: descriptor.Durable, Workload: descriptor.Workload, CommandType: descriptor.CommandType, PolicyVersion: descriptor.PolicyVersion, CommandFingerprint: strings.Repeat("a", 64), ApprovalEpoch: 1, RequestVersion: 1, RequestStatus: "EXECUTING", ExecutionStatus: "EXECUTING", ExecutionDeadline: time.Now().Add(time.Minute), Investigation: approvalconsumer.ClassifyProofFailure(descriptor.CommandType, descriptor.PolicyVersion, "")}
	}
	confirmCalls := make(map[uuid.UUID]int)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer controlled-recovery" {
			t.Error("frozen recovery credential missing")
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/internal/v1/approval-recovery/plan" {
			c := validAdminApprovalControl(AdminApprovalModeLegacy, AdminApprovalRelayRunning)
			_ = json.NewEncoder(w).Encode(approvalRecoveryPlanView{SchemaVersion: 1, ObservedAt: time.Now().UTC(), Control: approvalRecoveryControlView{ControlID: c.ControlID, SchemaVersion: c.SchemaVersion, Mode: c.Mode, ApprovalWireVersion: c.ApprovalWireVersion, ProofRequired: c.ProofRequired, AuthorizationRelayState: c.AuthorizationRelayState, ApprovalEpoch: c.ApprovalEpoch, AuthorizationRelayEpoch: c.AuthorizationRelayEpoch, Version: c.Version, UpdatedAt: c.UpdatedAt}, Candidates: items})
			return
		}
		if r.URL.Path == "/internal/v1/approval-recovery/confirm" {
			var body approvalRecoveryConfirmation
			if json.NewDecoder(r.Body).Decode(&body) != nil || body.Lease.RequestID != body.Candidate.RequestID || body.Candidate.OriginalKey != "controlled-original-key" {
				t.Error("confirmation changed original binding")
			}
			confirmCalls[body.Candidate.RequestID]++
			if body.Candidate.RequestID == items[0].RequestID {
				w.WriteHeader(http.StatusServiceUnavailable)
			} else {
				w.WriteHeader(http.StatusNoContent)
			}
			return
		}
		for _, item := range items {
			if r.URL.Path == "/internal/v1/approval-effects/"+item.RequestID.String() {
				_ = json.NewEncoder(w).Encode(approvaleffect.Observation{SchemaVersion: 1, Authority: approvaleffect.AuthorityBilling, ApprovalRequestID: item.RequestID, CommandType: item.CommandType, CommandFingerprint: item.CommandFingerprint, State: approvaleffect.StateObserved, ObservedAt: time.Now().UTC()})
				return
			}
			if r.URL.Path == "/internal/v1/approval-recovery/results/"+item.RequestID.String() {
				_ = json.NewEncoder(w).Encode(billingRecoveryResultView{SchemaVersion: 1, ApprovalRequestID: item.RequestID, OriginalEventID: item.OriginalEventID, ApprovalEpoch: item.ApprovalEpoch, CommandType: item.CommandType, State: "SUCCEEDED", DomainResult: json.RawMessage(`{"adjustment_id":"0198f110-f538-7f30-a8b1-f3984fd459a0","wallet_id":"0198f110-f538-7f30-a8b1-f3984fd459a1","status":"POSTED","version":1}`), LedgerReferences: json.RawMessage(`{"journal_entry_id":"0198f110-f538-7f30-a8b1-f3984fd459a2"}`), ObservedAt: time.Now().UTC()})
				return
			}
		}
		t.Error("unexpected recovery route, possibly a financial execution")
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	leaser := &recoveryLeaserStub{}
	client := &http.Client{Timeout: time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	runtime, err := NewApprovalRecoveryRuntime(server.URL, server.URL, "controlled-recovery", leaser, client)
	if err != nil {
		t.Fatal(err)
	}
	pass, err := runtime.RunOnce(context.Background(), 2)
	if err == nil || pass.Candidates != 2 || pass.Unavailable != 1 || pass.Confirmed != 1 || pass.Invalidated != 0 || leaser.claimCalls != 2 || leaser.releaseCalls != 2 || confirmCalls[items[0].RequestID] != 1 || confirmCalls[items[1].RequestID] != 1 {
		t.Fatalf("confirmation failure hid next candidate: %+v", pass)
	}
}
