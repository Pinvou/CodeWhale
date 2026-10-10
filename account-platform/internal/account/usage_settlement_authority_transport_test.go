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

func usageTransportArtifacts(t *testing.T) approvalencoding.ProofArtifacts {
	t.Helper()
	var wire precheckWireResponse
	if json.Unmarshal([]byte(f16OwnerProjection), &wire) != nil {
		t.Fatal("fixture")
	}
	a, e := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{CommandType: wire.CommandType, PolicyVersion: wire.PolicyVersion, SnapshotSchemaVersion: wire.SnapshotSchemaVersion, TargetResourceType: wire.TargetResourceType, TargetResourceID: wire.TargetResourceID, NormalizedCommand: wire.NormalizedCommand, CriticalResourceVersions: wire.CriticalResourceVersions, FinancialSnapshot: wire.FinancialSnapshot})
	if e != nil {
		t.Fatal(e)
	}
	return a
}
func TestF16UsageProofTransport(t *testing.T) {
	artifacts := usageTransportArtifacts(t)
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
					t.Error("request did not carry the selected frozen Usage facts")
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

func TestF16UsageRecoveryReceiptBinding(t *testing.T) {
	const result = `{"case_id":"00000000-0000-4000-8000-000000000007","usage_request_id":"00000000-0000-4000-8000-000000000002","hold_id":"00000000-0000-4000-8000-000000000001","wallet_id":"00000000-0000-4000-8000-000000000004","loss_review_id":"00000000-0000-4000-8000-000000000003","evidence_kind":"LOSS_REVIEW","expected_microcredits":9223372036854775807,"settled_microcredits":9007199254740993,"overage_microcredits":9214364837600034814,"allocations_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","facts_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","journal_entry_id":"00000000-0000-4000-8000-000000000005","receivable_id":null,"status":"SUCCEEDED"}`
	const ledger = `{"journal_entry_id":"00000000-0000-4000-8000-000000000005"}`
	for _, test := range []struct {
		name, body, references string
		valid                  bool
	}{
		{"exact-int64", result, ledger, true},
		{"foreign-journal", result, strings.Replace(ledger, "00000000-0000-4000-8000-000000000005", "00000000-0000-4000-8000-000000000006", 1), false},
		{"fraction", strings.Replace(result, "9007199254740993", "1.5", 1), ledger, false},
		{"overflow", strings.Replace(result, "9007199254740993", "9223372036854775808", 1), ledger, false},
		{"settlement-inconsistent", strings.Replace(result, "9007199254740993", "9007199254740992", 1), ledger, false},
		{"nil-review", strings.Replace(result, "00000000-0000-4000-8000-000000000003", uuid.Nil.String(), 1), ledger, false},
		{"missing-review", strings.Replace(result, `"loss_review_id":"00000000-0000-4000-8000-000000000003",`, "", 1), ledger, false},
		{"wrong-evidence", strings.Replace(result, "LOSS_REVIEW", "COMMITTED_SHORTFALL", 1), ledger, false},
		{"negative-settled", strings.Replace(result, "9007199254740993", "-1", 1), ledger, false},
		{"nonterminal", strings.Replace(result, "SUCCEEDED", "UNKNOWN", 1), ledger, false},
		{"unknown-field", strings.Replace(result, `"case_id":`, `"future_field":1,"case_id":`, 1), ledger, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if validBillingRecoveryPayload("USAGE_SETTLEMENT_REPAIR", json.RawMessage(test.body), json.RawMessage(test.references)) != test.valid {
				t.Fatal("receipt/journal binding")
			}
		})
	}
}
func TestF16UsageOriginalIntentAndClosedFreeze(t *testing.T) {
	a := usageTransportArtifacts(t)
	for _, test := range []struct {
		name, from, to string
		valid          bool
		commandChanged bool
	}{
		{"usage-id", "00000000-0000-4000-8000-000000000002", "00000000-0000-4000-8000-000000000006", true, true},
		{"review-id", "00000000-0000-4000-8000-000000000003", "00000000-0000-4000-8000-000000000006", true, true},
		{"updated-facts", "2026-10-07T00:00:00.123456789Z", "2026-10-08T00:00:00.123456789Z", true, false},
		{"wrong-kind", "LOSS_REVIEW", "COMMITTED_SHORTFALL", false, false},
		{"wrong-reason", "MISSING_USAGE_SETTLEMENT", "OTHER", false, false},
		{"fraction", "9007199254740993", "1.5", false, false},
		{"sum-mismatch", "9214364837600034814", "9214364837600034813", false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var wire precheckWireResponse
			if json.Unmarshal([]byte(strings.Replace(f16OwnerProjection, test.from, test.to, 1)), &wire) != nil {
				t.Fatal("fixture")
			}
			v, e := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{CommandType: wire.CommandType, PolicyVersion: wire.PolicyVersion, SnapshotSchemaVersion: wire.SnapshotSchemaVersion, TargetResourceType: wire.TargetResourceType, TargetResourceID: wire.TargetResourceID, NormalizedCommand: wire.NormalizedCommand, CriticalResourceVersions: wire.CriticalResourceVersions, FinancialSnapshot: wire.FinancialSnapshot})
			if test.valid {
				if e != nil {
					t.Fatal(e)
				}
				if test.commandChanged && (v.CommandDigest == a.CommandDigest || v.SnapshotDigest != a.SnapshotDigest) {
					t.Fatal("original UUID intent hash")
				}
				if !test.commandChanged && (v.CommandDigest != a.CommandDigest || v.SnapshotDigest == a.SnapshotDigest) {
					t.Fatal("mutable frozen-fact hash")
				}
			} else if e == nil {
				t.Fatal("invalid closed freeze accepted")
			}
		})
	}
	for _, old := range []string{"ENTITLEMENT_COMPONENT_ADJUSTMENT", "LEDGER_REPAIR", "RECEIVABLE_WRITE_OFF", "PAYMENT_FINANCIAL_REPAIR", "REFUND_EXCEPTION_REPAIR", "CHARGEBACK_FINANCIAL_REPAIR", "RECONCILIATION_FINANCIAL_REPAIR"} {
		t.Run("old-command-foreign-fields-"+old, func(t *testing.T) {
			v := ApprovalPrecheckRequest{CommandType: old, TargetResourceID: "00000000-0000-4000-8000-000000000001", UsageRequestID: "00000000-0000-4000-8000-000000000002", LossReviewID: "00000000-0000-4000-8000-000000000003", ReasonCode: "MISSING_USAGE_SETTLEMENT"}
			if validApprovalPrecheckRequest(v) {
				t.Fatal("Usage fields accepted outside command")
			}
		})
	}
	if approvalV2Command(ApprovalRequest{CommandType: "USAGE_SETTLEMENT_REPAIR", PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "USAGE_HOLD", TargetResourceID: uuid.Nil.String()}) != nil {
		t.Fatal("nil target available")
	}
}
