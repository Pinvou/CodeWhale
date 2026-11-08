package account

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalconsumer"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalencoding"
	"github.com/google/uuid"
)

// Separate live loopback endpoints ensure Token observations cannot be routed
// to Billing; no actual Token governance activation or domain execution is claimed.
func TestF04TokenRecoveryResultAuthority(t *testing.T) {
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("Token recovery contacted Billing")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer billing.Close()
	for _, descriptor := range approvalconsumer.TokenCommands() {
		t.Run(descriptor.CommandType, func(t *testing.T) {
			candidate := ApprovalRecoveryCandidate{RequestID: uuid.New(), ExecutionID: uuid.New(), OriginalEventID: uuid.New(),
				Durable: descriptor.Durable, Workload: descriptor.Workload, CommandType: descriptor.CommandType,
				PolicyVersion: descriptor.PolicyVersion, CommandFingerprint: strings.Repeat("a", 64), OriginalKey: "owned-original-key", ApprovalEpoch: 1,
				Investigation: approvalconsumer.ClassifyProofFailure(descriptor.CommandType, descriptor.PolicyVersion, "DOMAIN_REVISION_CHANGED")}
			digest := sha256.Sum256([]byte(candidate.OriginalKey))
			reference := uuid.New()
			invalid := false
			token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/internal/v1/approval-recovery/results/"+candidate.RequestID.String() ||
					r.Header.Get("Authorization") != "Bearer controlled-recovery" || r.URL.Query().Get("command_type") != candidate.CommandType ||
					r.URL.Query().Get("command_fingerprint") != candidate.CommandFingerprint || r.URL.Query().Get("original_key_digest") != hex.EncodeToString(digest[:]) || len(r.URL.Query()) != 3 {
					t.Error("Token recovery request did not preserve the original binding")
				}
				view := tokenRecoveryResultView{SchemaVersion: 1, ApprovalRequestID: candidate.RequestID, CommandType: candidate.CommandType,
					CommandFingerprint: candidate.CommandFingerprint, OriginalKeyDigest: hex.EncodeToString(digest[:]), State: "SUCCEEDED", CommandReference: &reference, ObservedAt: time.Now().UTC()}
				if invalid {
					view.OriginalKeyDigest = strings.Repeat("b", 64)
				}
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(view)
			}))
			defer token.Close()
			client, err := NewApprovalRecoveryResultClientWithToken(billing.URL, token.URL, "controlled-recovery")
			if err != nil {
				t.Fatal(err)
			}
			result, err := client.ObserveRecoveryResult(context.Background(), candidate)
			if err != nil || result.State != "SUCCEEDED" || result.CommandReference == nil || *result.CommandReference != reference {
				t.Fatalf("valid Token recovery rejected: %v", err)
			}
			invalid = true
			if _, err = client.ObserveRecoveryResult(context.Background(), candidate); err == nil {
				t.Fatal("different original key accepted")
			}
		})
	}
}

func TestF04TokenProofAuthority(t *testing.T) {
	requestID, accountID, credentialID := uuid.New(), uuid.New(), uuid.New()
	fingerprint := strings.Repeat("a", 64)
	artifacts, err := approvalencoding.ConvertLegacyProofInput(approvalencoding.LegacyProofInput{
		CommandType: "TOKEN_CREDENTIAL_REVOKE", PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: credentialID.String(),
		NormalizedCommand: mustJSON(map[string]any{"token_account_id": accountID, "expected_account_version": 7, "credential_id": credentialID, "credential_version": 3}),
		CriticalResourceVersions: mustJSON(map[string]any{"token_account_id": accountID, "token_account_version": 7, "credential_id": credentialID, "credential_version": 3, "credential_status": "ACTIVE", "credential_fingerprint": fingerprint,
			"active_route_set_fingerprint": fingerprint, "provisioning_lease_set_fingerprint": fingerprint, "inflight_request_set_fingerprint": fingerprint, "replacement_credential_version": nil, "policy_version": 1}),
		FinancialSnapshot: json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	billing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("Token proof contacted Billing")
		w.WriteHeader(503)
	}))
	defer billing.Close()
	invalid := false
	token := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		request, decodeErr := approvalencoding.DecodeProofRequest(raw)
		if err != nil || decodeErr != nil || request.ApprovalRequestID != requestID || r.URL.Path != "/internal/v1/approval-command-proofs" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer controlled-proof" {
			t.Error("invalid proof request binding")
			w.WriteHeader(400)
			return
		}
		now := time.Now().UTC().Truncate(time.Second)
		proof := approvalencoding.ProofResponse{ApprovalRequestID: requestID, CommandType: request.Artifacts.CommandType, CommandDigest: request.Artifacts.CommandDigest,
			SnapshotDigest: request.Artifacts.SnapshotDigest, PolicyVersion: 1, ProofSchemaVersion: "1", DomainRevision: "controlled-live-revision", Verdict: "ALLOW", ReasonCode: "PROOF_VALID", IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
		if invalid {
			proof.ApprovalRequestID = uuid.New()
		}
		encoded, err := approvalencoding.EncodeProofResponse(proof)
		if err != nil {
			t.Error(err)
		}
		_, _ = w.Write(encoded)
	}))
	defer token.Close()
	client, err := NewHTTPApprovalProofSourceWithToken(billing.URL, token.URL, "controlled-proof")
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Proof(context.Background(), requestID, artifacts)
	if err != nil || result.Digest == "" {
		t.Fatalf("valid Token proof rejected: %v", err)
	}
	invalid = true
	if _, err = client.Proof(context.Background(), requestID, artifacts); err == nil {
		t.Fatal("proof for another request accepted")
	}
	legacy, _ := NewHTTPApprovalProofSource(billing.URL, "controlled-proof")
	if _, err = legacy.Proof(context.Background(), requestID, artifacts); err == nil {
		t.Fatal("missing Token endpoint did not fail closed")
	}
}

func TestF04TokenSnapshotCompleteBodyAndConfiguration(t *testing.T) {
	for _, raw := range []string{"https://authority.invalid?", "https://user@authority.invalid", "https://:443", "file:///private", "https://authority.invalid#fragment"} {
		if _, err := NewHTTPApprovalSnapshotSource(raw, "controlled"); err == nil {
			t.Fatalf("ambiguous snapshot authority accepted: %s", raw)
		}
	}
	accountID, credentialID := uuid.New(), uuid.New()
	fingerprint := strings.Repeat("a", 64)
	input := ApprovalSnapshotRequest{CommandType: "TOKEN_CREDENTIAL_REVOKE", TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: credentialID.String()}
	valid := string(mustJSON(map[string]any{"command_type": input.CommandType, "target_resource_type": input.TargetResourceType, "target_resource_id": input.TargetResourceID,
		"critical_resource_versions": map[string]any{"token_account_id": accountID, "token_account_version": 7, "credential_id": credentialID, "credential_version": 3, "credential_status": "ACTIVE",
			"credential_fingerprint": fingerprint, "active_route_set_fingerprint": fingerprint, "provisioning_lease_set_fingerprint": fingerprint, "inflight_request_set_fingerprint": fingerprint, "replacement_credential_version": nil, "policy_version": 1},
		"financial_snapshot": map[string]any{}, "precheck_fingerprint": fingerprint, "token_observed_at": time.Now().UTC()}))
	for name, body := range map[string]string{"valid": valid, "second-object": valid + ` {}`, "trailing-garbage": valid + ` trailing`, "oversize": valid + strings.Repeat(" ", 512<<10)} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, body) }))
			defer server.Close()
			client, _ := NewHTTPApprovalSnapshotSource(server.URL, "controlled")
			_, err := client.PrecheckApprovalSnapshot(context.Background(), input)
			if name == "valid" && err != nil {
				t.Fatal("complete valid snapshot rejected", err)
			}
			if name != "valid" && err == nil {
				t.Fatal("incomplete/invalid snapshot accepted")
			}
		})
	}
}

func TestF04TokenOriginalIntentFingerprint(t *testing.T) {
	for _, command := range []string{"TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK", "TOKEN_CREDENTIAL_REVOKE", "WALLET_ADJUSTMENT"} {
		t.Run(command, func(t *testing.T) {
			draft := ApprovalDraft{CommandType: command, NormalizedCommand: json.RawMessage(`{"action":"owned"}`), TargetResourceType: "MODEL", TargetResourceID: uuid.New().String(), Reason: "owned original", CriticalResourceVersions: json.RawMessage(`{}`), FinancialSnapshot: json.RawMessage(`{}`)}
			before, err := approvalDraftOriginalFingerprint(command, draft)
			if err != nil {
				t.Fatal(err)
			}
			draft.CriticalResourceVersions = json.RawMessage(`{"actual_version":7}`)
			draft.FinancialSnapshot = json.RawMessage(`{"actual_amount":11}`)
			after, err := approvalDraftOriginalFingerprint(command, draft)
			if err != nil {
				t.Fatal(err)
			}
			if approvalSnapshotCommand(command) != (before == after) {
				t.Fatal("snapshot ownership binding changed")
			}
			draft.Reason = "changed intent"
			changed, err := approvalDraftOriginalFingerprint(command, draft)
			if err != nil || changed == after {
				t.Fatal("changed caller intent not bound", err)
			}
		})
	}
}
func TestF04TokenInactiveHTTPError(t *testing.T) {
	writer := httptest.NewRecorder()
	(&Service{}).writeError(writer, httptest.NewRequest(http.MethodPost, "/admin/v1/approvals", nil), ErrTokenGovernanceInactive)
	if writer.Code != http.StatusServiceUnavailable || writer.Header().Get("Cache-Control") != "no-store" || !strings.Contains(writer.Body.String(), "TOKEN_COMMAND_GOVERNANCE_INACTIVE") {
		t.Fatal("inactive governance did not preserve stable unavailable contract", writer.Code, writer.Body.String())
	}
}

func TestF04TokenTypedApprovalDetail(t *testing.T) {
	model, plan, replacement, accountID, credential := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	at := time.Now().UTC().Add(time.Hour).Truncate(time.Second).Format(time.RFC3339Nano)
	f := strings.Repeat("a", 64)
	encode := func(v any) json.RawMessage {
		b, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		return b
	}
	financial := encode(map[string]any{"billing_price_revision": 1, "price_set_fingerprint": f, "models_checked": 1, "models_covered": 1, "fully_covered": true, "billing_as_of": at})
	cases := []ApprovalRequest{
		{CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", PolicyVersion: 2, SnapshotSchemaVersion: 2, TargetResourceType: "MODEL", TargetResourceID: model.String(),
			NormalizedCommand:        encode(map[string]any{"model_id": model, "lifecycle_plan_id": plan, "lifecycle_plan_version": int64(9007199254740993), "target_lifecycle": "REMOVED", "effective_at": at, "existing_session_cutoff_at": at, "replacement_model_id": replacement}),
			CriticalResourceVersions: encode(map[string]any{"model_id": model, "catalog_version": 1, "current_lifecycle": "PUBLISHED", "target_lifecycle": "REMOVED", "lifecycle_plan_version": int64(9007199254740993), "effective_at": at, "existing_session_cutoff_at": at, "replacement_model_id": replacement, "billing_price_version": 1, "route_set_fingerprint": f, "provisioning_set_fingerprint": f, "inflight_request_set_fingerprint": f, "policy_version": 2}), FinancialSnapshot: financial},
		{CommandType: "TOKEN_MODEL_CATALOG_ROLLBACK", PolicyVersion: 2, SnapshotSchemaVersion: 2, TargetResourceType: "MODEL_CATALOG", TargetResourceID: "9007199254740993",
			NormalizedCommand: json.RawMessage(`{"source_catalog_version":9007199254740993}`), CriticalResourceVersions: encode(map[string]any{"current_catalog_version": int64(9007199254740994), "source_catalog_version": int64(9007199254740993), "catalog_diff_fingerprint": f, "billing_price_version": 1, "route_set_fingerprint": f, "provisioning_set_fingerprint": f, "inflight_request_set_fingerprint": f, "policy_version": 2}), FinancialSnapshot: financial},
		{CommandType: "TOKEN_CREDENTIAL_REVOKE", PolicyVersion: 1, SnapshotSchemaVersion: 1, TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: credential.String(),
			NormalizedCommand: encode(map[string]any{"token_account_id": accountID, "expected_account_version": int64(9007199254740993), "credential_id": credential, "credential_version": 3}), CriticalResourceVersions: encode(map[string]any{"token_account_id": accountID, "token_account_version": int64(9007199254740993), "credential_id": credential, "credential_version": 3, "credential_status": "ACTIVE", "credential_fingerprint": f, "active_route_set_fingerprint": f, "provisioning_lease_set_fingerprint": f, "inflight_request_set_fingerprint": f, "replacement_credential_version": nil, "policy_version": 1}), FinancialSnapshot: json.RawMessage(`{}`)},
	}
	for _, item := range cases {
		t.Run(item.CommandType, func(t *testing.T) {
			detail, e := approvalV2Detail(item, 1)
			if e != nil {
				t.Fatal(e)
			}
			raw := string(encode(detail))
			if !strings.Contains(raw, `"9007199254740993"`) || strings.Contains(raw, "normalized_command") || strings.Contains(raw, "critical_resource_versions") || strings.Contains(raw, "financial_snapshot") {
				t.Fatal("typed intent missing or raw proof leaked", raw)
			}
			if item.CommandType == "TOKEN_MODEL_LIFECYCLE_CHANGE" && (!strings.Contains(raw, plan.String()) || !strings.Contains(raw, replacement.String()) || !strings.Contains(raw, `"existing_session_cutoff_at"`)) {
				t.Fatal("lifecycle plan intent truncated", raw)
			}
			wrong := item
			wrong.PolicyVersion = 999
			if _, e = approvalV2Detail(wrong, 1); e == nil {
				t.Fatal("unknown schema presented")
			}
		})
	}
}
