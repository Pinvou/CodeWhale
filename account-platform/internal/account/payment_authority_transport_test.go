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
	"testing"
	"time"
)

func paymentTransportArtifacts(t *testing.T) approvalencoding.ProofArtifacts {
	t.Helper()
	var wire precheckWireResponse
	if err := json.Unmarshal([]byte(f12OwnerProjection), &wire); err != nil {
		t.Fatal(err)
	}
	a, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{CommandType: wire.CommandType, PolicyVersion: wire.PolicyVersion, SnapshotSchemaVersion: wire.SnapshotSchemaVersion, TargetResourceType: wire.TargetResourceType, TargetResourceID: wire.TargetResourceID, NormalizedCommand: wire.NormalizedCommand, CriticalResourceVersions: wire.CriticalResourceVersions, FinancialSnapshot: wire.FinancialSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestF12PaymentProofTransport(t *testing.T) {
	artifacts := paymentTransportArtifacts(t)
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
					t.Error("request did not carry the selected frozen payment facts")
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

func TestF12PaymentReferenceCannotEnterOldCommands(t *testing.T) {
	const a = "00000000-0000-4000-8000-000000000001"
	const b = "00000000-0000-4000-8000-000000000002"
	const c = "00000000-0000-4000-8000-000000000003"
	for _, request := range []ApprovalPrecheckRequest{
		{CommandType: "ENTITLEMENT_COMPONENT_ADJUSTMENT", TargetResourceID: a, PaymentOrderID: b, ReasonCode: "CONTROLLED_CORRECTION"},
		{CommandType: "LEDGER_REPAIR", TargetResourceID: a, TargetJournalEntryID: b, WrongLedgerAccountID: b, CorrectLedgerAccountID: c, AmountMicrocredits: 1, ReasonCode: "ATTRIBUTION_TO_WRONG_PLATFORM_ACCOUNT"},
		{CommandType: "RECEIVABLE_WRITE_OFF", TargetResourceID: a, WriteOffMicrocredits: 1, ExpectedVersion: 1, ReasonCode: "APPROVED_RECEIVABLE_WRITE_OFF"},
	} {
		t.Run(request.CommandType, func(t *testing.T) {
			if !validApprovalPrecheckRequest(request) {
				t.Fatal("original request unexpectedly rejected")
			}
			request.PaymentEvidenceID = b
			if validApprovalPrecheckRequest(request) {
				t.Fatal("foreign payment evidence widened old input")
			}
		})
	}
}
