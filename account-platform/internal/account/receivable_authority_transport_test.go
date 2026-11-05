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

func receivableTransportArtifacts(t *testing.T) approvalencoding.ProofArtifacts {
	t.Helper()
	var wire precheckWireResponse
	if err := json.Unmarshal([]byte(f11OwnerProjection), &wire); err != nil {
		t.Fatal(err)
	}
	a, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{CommandType: wire.CommandType, PolicyVersion: wire.PolicyVersion, SnapshotSchemaVersion: wire.SnapshotSchemaVersion, TargetResourceType: wire.TargetResourceType, TargetResourceID: wire.TargetResourceID, NormalizedCommand: wire.NormalizedCommand, CriticalResourceVersions: wire.CriticalResourceVersions, FinancialSnapshot: wire.FinancialSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestF11ReceivableRecoveryRejectsVersionOverflow(t *testing.T) {
	const id = "00000000-0000-4000-8000-000000000001"
	raw := json.RawMessage(`{"case_id":"` + id + `","receivable_id":"` + id + `","wallet_id":"` + id + `","amount_microcredits":1,"receivable_version_before":9223372036854775807,"receivable_version_after":-9223372036854775808,"status_after":"WRITTEN_OFF","facts_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","journal_entry_id":"` + id + `","allocation_id":"` + id + `","status":"SUCCEEDED"}`)
	if validBillingRecoveryPayload("RECEIVABLE_WRITE_OFF", raw, json.RawMessage(`{"journal_entry_id":"`+id+`"}`)) {
		t.Fatal("wrapped successor version accepted as a confirmed effect")
	}
}

func TestF11ReceivableProofTransport(t *testing.T) {
	artifacts := receivableTransportArtifacts(t)
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
					t.Error("request did not carry the selected frozen receivable facts")
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
