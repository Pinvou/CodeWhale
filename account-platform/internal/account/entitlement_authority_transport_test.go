package account

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func entitlementTransportArtifacts(t *testing.T) approvalencoding.ProofArtifacts {
	t.Helper()
	var wire precheckWireResponse
	if err := json.Unmarshal([]byte(entitlementPrecheckOwnerFixture), &wire); err != nil {
		t.Fatal(err)
	}
	a, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{CommandType: wire.CommandType, PolicyVersion: wire.PolicyVersion, SnapshotSchemaVersion: wire.SnapshotSchemaVersion, TargetResourceType: wire.TargetResourceType, TargetResourceID: wire.TargetResourceID, NormalizedCommand: wire.NormalizedCommand, CriticalResourceVersions: wire.CriticalResourceVersions, FinancialSnapshot: wire.FinancialSnapshot})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestF09EntitlementProofTransport(t *testing.T) {
	artifacts := entitlementTransportArtifacts(t)
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
				now := time.Now().UTC()
				id := requestID
				if test.foreign {
					id = uuid.New()
				}
				body, err := approvalencoding.EncodeProofResponse(approvalencoding.ProofResponse{ApprovalRequestID: id, CommandType: artifacts.CommandType, CommandDigest: artifacts.CommandDigest, SnapshotDigest: artifacts.SnapshotDigest, PolicyVersion: 2, ProofSchemaVersion: approvalencoding.ProofSchemaVersion, DomainRevision: "controlled-owner-revision", Verdict: test.verdict, ReasonCode: test.reason, IssuedAt: now, ExpiresAt: now.Add(time.Minute)})
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

func TestF09EntitlementRecoveryTypedResult(t *testing.T) {
	id := uuid.NewString()
	raw := json.RawMessage(`{"correction_id":"` + id + `","package_id":"` + id + `","wallet_id":"` + id + `","source_component_id":"` + id + `","source_lot_id":"` + id + `","target_component_id":"` + id + `","target_lot_id":"` + id + `","source_type":"BASE","target_type":"BONUS","amount_microcredits":9007199254740993,"journal_entry_id":"` + id + `","status":"SUCCEEDED"}`)
	ledger := json.RawMessage(`{"journal_entry_id":"` + id + `"}`)
	t.Run("current-success-exact-int64", func(t *testing.T) {
		if !validBillingRecoveryPayload("ENTITLEMENT_COMPONENT_ADJUSTMENT", raw, ledger) {
			t.Fatal("installed effect rejected by recovery result client")
		}
	})
	t.Run("foreign-journal", func(t *testing.T) {
		if validBillingRecoveryPayload("ENTITLEMENT_COMPONENT_ADJUSTMENT", raw, json.RawMessage(`{"journal_entry_id":"`+uuid.NewString()+`"}`)) {
			t.Fatal("foreign ledger accepted")
		}
	})
	t.Run("int64-overflow", func(t *testing.T) {
		if validBillingRecoveryPayload("ENTITLEMENT_COMPONENT_ADJUSTMENT", json.RawMessage(strings.Replace(string(raw), "9007199254740993", "9223372036854775808", 1)), ledger) {
			t.Fatal("overflow accepted")
		}
	})
	t.Run("future-command", func(t *testing.T) {
		if validBillingRecoveryPayload("LEDGER_REPAIR", raw, ledger) {
			t.Fatal("future repair accepted")
		}
	})
}
