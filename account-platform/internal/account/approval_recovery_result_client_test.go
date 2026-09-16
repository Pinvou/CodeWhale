package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/google/uuid"
)

func TestRecoveryResultClientBindsOriginalReferencesAndRejectsCacheableResponses(t *testing.T) {
	for _, descriptor := range []approvalconsumer.Descriptor{approvalconsumer.All()[0], approvalconsumer.All()[3]} {
		t.Run(descriptor.CommandType, func(t *testing.T) {
			candidate := ApprovalRecoveryCandidate{RequestID: uuid.New(), ExecutionID: uuid.New(), OriginalEventID: uuid.New(),
				OriginalKey: "private-original-key", Durable: descriptor.Durable, Workload: descriptor.Workload,
				CommandType: descriptor.CommandType, PolicyVersion: descriptor.PolicyVersion, ApprovalEpoch: 2,
				CommandFingerprint: strings.Repeat("a", 64), Investigation: approvalconsumer.ClassifyProofFailure(descriptor.CommandType, descriptor.PolicyVersion, "")}
			cache := "no-store"
			wrongBinding := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer dedicated" || r.URL.Path != "/internal/v1/approval-recovery/results/"+candidate.RequestID.String() ||
					r.URL.Query().Get("command_type") != candidate.CommandType || strings.Contains(r.URL.String(), candidate.OriginalKey) {
					t.Errorf("unexpected result query: %s", r.URL.String())
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Cache-Control", cache)
				id := candidate.RequestID
				if wrongBinding {
					id = uuid.New()
				}
				var body any
				if descriptor.Workload == approvalconsumer.TokenWorkload {
					digest := sha256.Sum256([]byte(candidate.OriginalKey))
					if r.URL.Query().Get("original_key_digest") != hex.EncodeToString(digest[:]) {
						t.Error("original digest missing")
					}
					reference := uuid.New()
					body = tokenRecoveryResultView{SchemaVersion: 1, ApprovalRequestID: id, CommandType: candidate.CommandType,
						CommandFingerprint: candidate.CommandFingerprint, OriginalKeyDigest: hex.EncodeToString(digest[:]), State: "SUCCEEDED", CommandReference: &reference, ObservedAt: time.Now().UTC()}
				} else {
					if r.URL.Query().Get("original_event_id") != candidate.OriginalEventID.String() || r.URL.Query().Get("approval_epoch") != "2" {
						t.Error("original Billing binding missing")
					}
					body = billingRecoveryResultView{SchemaVersion: 1, ApprovalRequestID: id, OriginalEventID: candidate.OriginalEventID,
						ApprovalEpoch: 2, CommandType: candidate.CommandType, State: "SUCCEEDED",
						DomainResult:     json.RawMessage(`{"adjustment_id":"` + uuid.NewString() + `","wallet_id":"` + uuid.NewString() + `","status":"POSTED","version":2}`),
						LedgerReferences: json.RawMessage(`{"journal_entry_id":"` + uuid.NewString() + `"}`), ObservedAt: time.Now().UTC()}
				}
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()
			client, err := NewApprovalRecoveryResultClient(server.URL, server.URL, "dedicated")
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.ObserveRecoveryResult(context.Background(), candidate)
			if err != nil || result.State != "SUCCEEDED" {
				t.Fatalf("valid original result rejected: %+v %v", result, err)
			}
			cache = "public"
			if _, err := client.ObserveRecoveryResult(context.Background(), candidate); err == nil {
				t.Fatal("cacheable result accepted")
			}
			cache, wrongBinding = "no-store", true
			if _, err := client.ObserveRecoveryResult(context.Background(), candidate); err == nil {
				t.Fatal("mismatched original result accepted")
			}
		})
	}
}
