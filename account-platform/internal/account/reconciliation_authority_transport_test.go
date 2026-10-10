package account

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func reconciliationTransportArtifacts(t *testing.T) approvalencoding.ProofArtifacts {
	t.Helper()
	var wire precheckWireResponse
	if err := json.Unmarshal([]byte(f15OwnerProjection), &wire); err != nil {
		t.Fatal(err)
	}
	a, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{CommandType: wire.CommandType, PolicyVersion: wire.PolicyVersion, SnapshotSchemaVersion: wire.SnapshotSchemaVersion, TargetResourceType: wire.TargetResourceType, TargetResourceID: wire.TargetResourceID, NormalizedCommand: wire.NormalizedCommand, CriticalResourceVersions: wire.CriticalResourceVersions, FinancialSnapshot: wire.FinancialSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestF15ReconciliationRecoveryReceiptBinding(t *testing.T) {
	const result = `{"case_id":"00000000-0000-4000-8000-000000000001","run_id":"00000000-0000-4000-8000-000000000003","channel":"controlled","business_date":"2026-10-07","merchant_order_no":"controlled-order","amount_fen":9007199254740993,"currency":"CNY","provider_transaction_id":"controlled-provider","payment_evidence_id":"00000000-0000-4000-8000-000000000002","resolution_code":"PROVIDER_REFUND_CONFIRMED","recheck_case_count":0,"facts_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","ledger_effect":"NONE","status":"SUCCEEDED"}`
	const ledger = `{}`
	for _, test := range []struct {
		name, body, references string
		valid                  bool
	}{
		{"exact-int64-no-journal", result, ledger, true},
		{"foreign-journal", result, `{"journal_entry_id":"00000000-0000-4000-8000-000000000005"}`, false},
		{"fraction", strings.Replace(result, "9007199254740993", "1.5", 1), ledger, false},
		{"overflow", strings.Replace(result, "9007199254740993", "9223372036854775808", 1), ledger, false},
		{"zero-amount", strings.Replace(result, `"amount_fen":9007199254740993`, `"amount_fen":0`, 1), ledger, false},
		{"recheck-open", strings.Replace(result, `"recheck_case_count":0`, `"recheck_case_count":1`, 1), ledger, false},
		{"nonterminal", strings.Replace(result, "SUCCEEDED", "UNKNOWN", 1), ledger, false},
		{"unknown-field", strings.Replace(result, `"case_id":`, `"future_field":1,"case_id":`, 1), ledger, false},
		{"nil-run", strings.Replace(result, "00000000-0000-4000-8000-000000000003", uuid.Nil.String(), 1), ledger, false},
		{"ledger-effect", strings.Replace(result, "NONE", "POSTED", 1), ledger, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if validBillingRecoveryPayload("RECONCILIATION_FINANCIAL_REPAIR", json.RawMessage(test.body), json.RawMessage(test.references)) != test.valid {
				t.Fatal("reconciliation recovery receipt contract differs")
			}
		})
	}
}

func TestF15ReconciliationProofTransport(t *testing.T) {
	artifacts := reconciliationTransportArtifacts(t)
	for _, test := range []struct {
		name, verdict, reason string
		foreign               bool
		want                  error
	}{
		{"allow-current-owner", "ALLOW", "PROOF_VALID", false, nil},
		{"deny-changed-facts", "DENY", "SNAPSHOT_MISMATCH", false, ErrApprovalProofRejected},
		{"deny-foreign-command-reason", "DENY", "PROVIDER_STATE_CHANGED", false, ErrApprovalProofInvalid},
		{"foreign-request", "ALLOW", "PROOF_VALID", true, ErrApprovalProofInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			requestID := uuid.New()
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/approval-command-proofs" || r.Header.Get("Authorization") != "Bearer controlled-proof" {
					t.Error("incorrect proof authority request")
				}
				raw, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
					http.Error(w, "read", 500)
					return
				}
				request, err := approvalencoding.DecodeProofRequest(raw)
				if err != nil || request.ApprovalRequestID != requestID || request.Artifacts.CommandType != artifacts.CommandType || request.Artifacts.CommandDigest != artifacts.CommandDigest || request.Artifacts.SnapshotDigest != artifacts.SnapshotDigest {
					t.Error("request did not carry the selected frozen reconciliation facts")
					http.Error(w, "binding", 400)
					return
				}
				now := time.Now().UTC()
				id := requestID
				if test.foreign {
					id = uuid.New()
				}
				body, err := approvalencoding.EncodeProofResponse(approvalencoding.ProofResponse{ApprovalRequestID: id, CommandType: artifacts.CommandType, CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest, PolicyVersion: 1, ProofSchemaVersion: approvalencoding.ProofSchemaVersion, DomainRevision: "controlled-owner-revision", Verdict: test.verdict, ReasonCode: test.reason, IssuedAt: now, ExpiresAt: now.Add(time.Minute)})
				if err != nil {
					t.Error(err)
					http.Error(w, "fixture encode failed", 500)
					return
				}
				_, _ = w.Write(body)
			}))
			defer server.Close()
			source, err := NewHTTPApprovalProofSource(server.URL, "controlled-proof")
			if err != nil {
				t.Fatal(err)
			}
			proof, err := source.Proof(context.Background(), requestID, artifacts)
			if !errors.Is(err, test.want) {
				t.Fatalf("proof err=%v want=%v", err, test.want)
			}
			if calls != 1 {
				t.Fatalf("proof never reached its installed Billing authority: calls=%d", calls)
			}
			if test.want == nil && (proof.Response.ApprovalRequestID != requestID || proof.Response.CommandType != artifacts.CommandType) {
				t.Fatal("proof binding changed")
			}
		})
	}
}

func TestF15ReconciliationDateIsolationAndFrozenHash(t *testing.T) {
	original := ApprovalPrecheckRequest{CommandType: "RECONCILIATION_FINANCIAL_REPAIR", TargetResourceID: "00000000-0000-4000-8000-000000000001", PaymentEvidenceID: "00000000-0000-4000-8000-000000000002", BusinessDate: "2026-10-07", ReasonCode: "PROVIDER_REFUND_CONFIRMED"}
	for _, command := range []string{"ENTITLEMENT_COMPONENT_ADJUSTMENT", "LEDGER_REPAIR", "RECEIVABLE_WRITE_OFF", "PAYMENT_FINANCIAL_REPAIR", "REFUND_EXCEPTION_REPAIR", "CHARGEBACK_FINANCIAL_REPAIR"} {
		t.Run("old-command-date-denied-"+command, func(t *testing.T) {
			v := original
			v.CommandType = command
			if validApprovalPrecheckRequest(v) {
				t.Fatal("foreign business date accepted")
			}
		})
	}
	a := reconciliationTransportArtifacts(t)
	for _, test := range []struct {
		name, from, to string
		valid          bool
	}{
		{"date-hash", "2026-10-07", "2026-10-08", true},
		{"evidence-hash", "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000006", true},
		{"invalid-date", "2026-10-07", "2026-02-30", false},
		{"wrong-reason", "PROVIDER_REFUND_CONFIRMED", "OTHER", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var wire precheckWireResponse
			if json.Unmarshal([]byte(strings.Replace(f15OwnerProjection, test.from, test.to, 1)), &wire) != nil {
				t.Fatal("fixture")
			}
			v, e := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{CommandType: wire.CommandType, PolicyVersion: wire.PolicyVersion, SnapshotSchemaVersion: wire.SnapshotSchemaVersion, TargetResourceType: wire.TargetResourceType, TargetResourceID: wire.TargetResourceID, NormalizedCommand: wire.NormalizedCommand, CriticalResourceVersions: wire.CriticalResourceVersions, FinancialSnapshot: wire.FinancialSnapshot})
			if test.valid {
				if e != nil || v.CommandDigest == a.CommandDigest || v.SnapshotDigest != a.SnapshotDigest {
					t.Fatal("intent hash binding", e)
				}
			} else if e == nil {
				t.Fatal("invalid freeze accepted")
			}
		})
	}
}

func TestF15ReconciliationNilSummaryRejected(t *testing.T) {
	if approvalV2Command(ApprovalRequest{CommandType: "RECONCILIATION_FINANCIAL_REPAIR", PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "RECONCILIATION_CASE", TargetResourceID: uuid.Nil.String()}) != nil {
		t.Fatal("nil target reported as available")
	}
}
