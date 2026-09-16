package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

func approvalProofClientFixture(t *testing.T) (approvalencoding.ProofArtifacts, []byte) {
	t.Helper()
	body, err := os.ReadFile("../platform/approvalencoding/testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		ProofVectors []struct {
			CommandType           string `json:"command_type"`
			PolicyVersion         int64  `json:"policy_version"`
			SnapshotSchemaVersion int    `json:"snapshot_schema_version"`
			Legacy                struct {
				TargetResourceType string `json:"target_resource_type"`
				TargetResourceID   string `json:"target_resource_id"`
				NormalizedJSON     string `json:"normalized_json"`
				CriticalJSON       string `json:"critical_json"`
				FinancialJSON      string `json:"financial_json"`
			} `json:"legacy"`
		} `json:"proof_vectors"`
		ProofResponseVectors []struct {
			ResponseJSON string `json:"response_json"`
		} `json:"proof_response_vectors"`
	}
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatal(err)
	}
	wallet := fixture.ProofVectors[0]
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: wallet.CommandType, PolicyVersion: wallet.PolicyVersion, SnapshotSchemaVersion: wallet.SnapshotSchemaVersion,
		TargetResourceType: wallet.Legacy.TargetResourceType, TargetResourceID: wallet.Legacy.TargetResourceID,
		NormalizedCommand: json.RawMessage(wallet.Legacy.NormalizedJSON), CriticalResourceVersions: json.RawMessage(wallet.Legacy.CriticalJSON), FinancialSnapshot: json.RawMessage(wallet.Legacy.FinancialJSON),
	})
	if err != nil {
		t.Fatal(err)
	}
	return artifacts, []byte(fixture.ProofResponseVectors[0].ResponseJSON)
}

func TestApprovalProofClientBoundsAndIdentity(t *testing.T) {
	artifacts, golden := approvalProofClientFixture(t)
	requestID := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	// The frozen wire golden has a 2026 timestamp; issue a fresh response
	// preserving the closed request/digest binding for the live HTTP test.
	issued := time.Now().UTC().Truncate(time.Millisecond)
	golden = []byte(strings.ReplaceAll(strings.ReplaceAll(string(golden), "2026-09-15T01:04:05.678Z", issued.Format(time.RFC3339Nano)),
		"2026-09-15T01:05:05.678Z", issued.Add(time.Minute).Format(time.RFC3339Nano)))
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/internal/v1/approval-command-proofs" || r.Header.Get("Authorization") != "Bearer test-workload" {
			t.Errorf("untrusted proof request: %s %s %s", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(golden)
	}))
	defer server.Close()
	source, err := NewHTTPApprovalProofSource(server.URL, server.URL, "test-workload")
	if err != nil {
		t.Fatal(err)
	}
	result, err := source.Proof(context.Background(), requestID, artifacts)
	if err != nil || result.Digest == "" || result.Response.Verdict != "ALLOW" || calls.Load() != 1 {
		t.Fatalf("proof result=%+v calls=%d err=%v", result, calls.Load(), err)
	}
	bad := artifacts
	bad.CommandDigest = strings.Repeat("a", 64)
	if _, err := source.Proof(context.Background(), requestID, bad); !errors.Is(err, ErrApprovalProofInvalid) || calls.Load() != 1 {
		t.Fatalf("invalid artifacts invoked domain: calls=%d err=%v", calls.Load(), err)
	}
	server.Close()
	if _, err := source.Proof(context.Background(), requestID, artifacts); !errors.Is(err, ErrApprovalProofUnavailable) {
		t.Fatalf("network failure=%v", err)
	}
}

func TestApprovalProofClientFailureClasses(t *testing.T) {
	artifacts, golden := approvalProofClientFixture(t)
	requestID := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	issued := time.Now().UTC().Truncate(time.Millisecond)
	golden = []byte(strings.ReplaceAll(strings.ReplaceAll(string(golden), "2026-09-15T01:04:05.678Z", issued.Format(time.RFC3339Nano)),
		"2026-09-15T01:05:05.678Z", issued.Add(time.Minute).Format(time.RFC3339Nano)))
	for _, test := range []struct {
		name     string
		status   int
		response []byte
		delay    time.Duration
		want     error
	}{
		{"oversized", 200, []byte(strings.Repeat("x", 65<<10)), 0, ErrApprovalProofTooLarge},
		{"malformed", 200, []byte(`{"verdict":"ALLOW"}`), 0, ErrApprovalProofInvalid},
		{"denied", 200, []byte(strings.Replace(strings.Replace(string(golden), `"verdict":"ALLOW"`, `"verdict":"DENY"`, 1), `"reason_code":"PROOF_VALID"`, `"reason_code":"RESOURCE_STATE_CHANGED"`, 1)), 0, ErrApprovalProofRejected},
		{"wrong-command-reason", 200, []byte(strings.Replace(strings.Replace(string(golden), `"verdict":"ALLOW"`, `"verdict":"DENY"`, 1), `"reason_code":"PROOF_VALID"`, `"reason_code":"PROVIDER_STATE_CHANGED"`, 1)), 0, ErrApprovalProofInvalid},
		{"forbidden", 403, nil, 0, ErrApprovalProofUnavailable},
		{"gateway-timeout", 504, nil, 0, ErrApprovalProofTimeout},
		{"deadline", 200, golden, 40 * time.Millisecond, ErrApprovalProofTimeout},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.delay != 0 {
					time.Sleep(test.delay)
				}
				w.WriteHeader(test.status)
				_, _ = w.Write(test.response)
			}))
			defer server.Close()
			source, err := NewHTTPApprovalProofSource(server.URL, server.URL, "test-workload")
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			if test.delay != 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, 5*time.Millisecond)
				defer cancel()
			}
			result, err := source.Proof(ctx, requestID, artifacts)
			if !errors.Is(err, test.want) {
				t.Fatalf("got %v want %v", err, test.want)
			}
			if errors.Is(err, ErrApprovalProofRejected) && (result.Response.ReasonCode != "RESOURCE_STATE_CHANGED" || result.Digest == "") {
				t.Fatalf("verified denial lost audit binding: %+v", result)
			}
		})
	}
}

func TestApprovalProofCommandReasonWhitelist(t *testing.T) {
	for _, command := range []string{"WALLET_ADJUSTMENT", "PAYMENT_PROVIDER_RESUME", "PAYMENT_PRODUCT_RESUME", "TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK", "TOKEN_CREDENTIAL_REVOKE"} {
		if !approvalProofReasonAllowed(command, "ALLOW", "PROOF_VALID") || approvalProofReasonAllowed(command, "DENY", "PROOF_VALID") || approvalProofReasonAllowed(command, "DENY", "UNKNOWN") {
			t.Fatalf("invalid reason boundary: %s", command)
		}
	}
	if approvalProofReasonAllowed("WALLET_ADJUSTMENT", "DENY", "PROVIDER_STATE_CHANGED") || approvalProofReasonAllowed("TOKEN_CREDENTIAL_REVOKE", "DENY", "EVIDENCE_MISSING") || approvalProofReasonAllowed("UNKNOWN", "DENY", "RESOURCE_STATE_CHANGED") {
		t.Fatal("reason whitelist widened across commands")
	}
}
