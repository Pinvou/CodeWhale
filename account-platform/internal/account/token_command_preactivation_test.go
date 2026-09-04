package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/google/uuid"
)

func TestTokenApprovalCreationValidatesCallerInputBeforePreActivationGate(t *testing.T) {
	repository := &PostgresRepository{}
	principal := identity.Principal{Issuer: "https://issuer.example", Subject: "operator", SessionID: "session", AuthenticatedAt: time.Now()}
	for _, commandType := range []string{"TOKEN_MODEL_LIFECYCLE_CHANGE", "TOKEN_MODEL_CATALOG_ROLLBACK", "TOKEN_CREDENTIAL_REVOKE"} {
		_, err := repository.CreateApprovalDraft(context.Background(), principal, ApprovalDraft{CommandType: commandType})
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("%s incomplete request was not rejected before storage: %v", commandType, err)
		}
	}
}

func TestTokenApprovalCriticalResourceVersionsAreExactTypedAndSecretSafe(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	valid := map[string]map[string]any{
		"TOKEN_MODEL_LIFECYCLE_CHANGE": {
			"model_id": uuid.NewString(), "catalog_version": 7, "current_lifecycle": "PUBLISHED", "target_lifecycle": "DEPRECATED", "lifecycle_plan_version": 0,
			"effective_at": time.Now().UTC().Format(time.RFC3339Nano), "existing_session_cutoff_at": nil, "replacement_model_id": nil, "billing_price_version": 4,
			"route_set_fingerprint": fingerprint, "provisioning_set_fingerprint": fingerprint, "inflight_request_set_fingerprint": fingerprint, "policy_version": 2,
		},
		"TOKEN_MODEL_CATALOG_ROLLBACK": {
			"current_catalog_version": 9, "source_catalog_version": 7, "catalog_diff_fingerprint": fingerprint, "billing_price_version": 4,
			"route_set_fingerprint": fingerprint, "provisioning_set_fingerprint": fingerprint, "inflight_request_set_fingerprint": fingerprint, "policy_version": 2,
		},
		"TOKEN_CREDENTIAL_REVOKE": {
			"token_account_id": uuid.NewString(), "token_account_version": 8, "credential_id": uuid.NewString(), "credential_version": 3,
			"credential_status": "ACTIVE", "credential_fingerprint": fingerprint, "active_route_set_fingerprint": fingerprint,
			"provisioning_lease_set_fingerprint": fingerprint, "inflight_request_set_fingerprint": fingerprint, "replacement_credential_version": nil, "policy_version": 1,
		},
	}
	for commandType, fields := range valid {
		policy, err := approvalpolicy.Lookup(commandType)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(fields)
		if _, err := canonicalApprovalCriticalVersions(policy, raw); err != nil {
			t.Fatalf("%s valid snapshot rejected: %v", commandType, err)
		}
		for name, mutate := range map[string]func(map[string]any){
			"missing":    func(candidate map[string]any) { delete(candidate, policy.CriticalResourceFields[0]) },
			"extra":      func(candidate map[string]any) { candidate["unexpected"] = 1 },
			"wrong type": func(candidate map[string]any) { candidate["policy_version"] = "1" },
			"secret": func(candidate map[string]any) {
				delete(candidate, policy.CriticalResourceFields[0])
				candidate["credential_secret"] = "forbidden"
			},
			"provider address": func(candidate map[string]any) {
				delete(candidate, policy.CriticalResourceFields[0])
				candidate["provider_url"] = "https://private.invalid"
			},
		} {
			candidate := make(map[string]any, len(fields)+1)
			for key, value := range fields {
				candidate[key] = value
			}
			mutate(candidate)
			raw, _ := json.Marshal(candidate)
			if _, err := canonicalApprovalCriticalVersions(policy, raw); err == nil {
				t.Fatalf("%s accepted %s snapshot", commandType, name)
			}
		}
	}
	if _, err := canonicalSafeJSONObject(json.RawMessage(`{"credential_id":"` + uuid.NewString() + `","credential_version":2,"credential_status":"RETIRED","credential_fingerprint":"` + fingerprint + `"}`)); err != nil {
		t.Fatalf("safe credential result metadata was rejected: %v", err)
	}
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"credential_secret":"value"}`), json.RawMessage(`{"nested":{"provider_url":"https://private.invalid"}}`),
		json.RawMessage(`{"accessToken":"value"}`), json.RawMessage(`{"refreshToken":"value"}`), json.RawMessage(`{"apiKey":"value"}`), json.RawMessage(`{"privateKey":"value"}`),
		json.RawMessage(`{"credential_id":{"secret":"masquerade"}}`), json.RawMessage(`{"credential_version":"2"}`), json.RawMessage(`{"nested":[[{"credential_status":"SECRET"}]]}`),
	} {
		if _, err := canonicalSafeJSONObject(raw); err == nil {
			t.Fatalf("sensitive recursive result was accepted: %s", raw)
		}
		if _, err := canonicalSafeJSON(raw); err == nil {
			t.Fatalf("sensitive normalized command was accepted: %s", raw)
		}
	}
}

func TestTokenApprovalFinancialSnapshotIsClosedAlignedAndSecretSafe(t *testing.T) {
	policy, err := approvalpolicy.Lookup("TOKEN_MODEL_LIFECYCLE_CHANGE")
	if err != nil {
		t.Fatal(err)
	}
	priceFingerprint := strings.Repeat("a", 64)
	financial := json.RawMessage(`{"billing_price_revision":7,"price_set_fingerprint":"` + priceFingerprint + `","models_checked":2,"models_covered":2,"fully_covered":true,"billing_as_of":"2026-09-04T00:00:00Z"}`)
	canonicalFinancial, err := canonicalApprovalFinancialSnapshot(policy, financial)
	if err != nil {
		t.Fatal(err)
	}
	critical := json.RawMessage(`{"model_id":"` + uuid.NewString() + `","catalog_version":7,"current_lifecycle":"PUBLISHED","target_lifecycle":"DEPRECATED","lifecycle_plan_version":0,"effective_at":"2026-09-04T00:00:00Z","existing_session_cutoff_at":null,"replacement_model_id":null,"billing_price_version":7,"route_set_fingerprint":"` + priceFingerprint + `","provisioning_set_fingerprint":"` + priceFingerprint + `","inflight_request_set_fingerprint":"` + priceFingerprint + `","policy_version":2}`)
	canonicalCritical, err := canonicalApprovalCriticalVersions(policy, critical)
	if err != nil || !tokenApprovalSnapshotsAligned(canonicalCritical, canonicalFinancial) {
		t.Fatalf("aligned snapshots rejected: %v", err)
	}
	for name, raw := range map[string]json.RawMessage{
		"extra":            json.RawMessage(`{"billing_price_revision":7,"price_set_fingerprint":"` + priceFingerprint + `","models_checked":2,"models_covered":2,"fully_covered":true,"billing_as_of":"2026-09-04T00:00:00Z","extra":1}`),
		"partial":          json.RawMessage(`{"billing_price_revision":7}`),
		"uncovered":        json.RawMessage(`{"billing_price_revision":7,"price_set_fingerprint":"` + priceFingerprint + `","models_checked":2,"models_covered":1,"fully_covered":true,"billing_as_of":"2026-09-04T00:00:00Z"}`),
		"secret":           json.RawMessage(`{"billing_price_revision":7,"price_set_fingerprint":"` + priceFingerprint + `","models_checked":2,"models_covered":2,"fully_covered":true,"billing_as_of":"2026-09-04T00:00:00Z","api_key":"forbidden"}`),
		"invalid revision": json.RawMessage(`{"billing_price_revision":0,"price_set_fingerprint":"` + priceFingerprint + `","models_checked":2,"models_covered":2,"fully_covered":true,"billing_as_of":"2026-09-04T00:00:00Z"}`),
		"empty coverage":   json.RawMessage(`{"billing_price_revision":7,"price_set_fingerprint":"` + priceFingerprint + `","models_checked":0,"models_covered":0,"fully_covered":false,"billing_as_of":"2026-09-04T00:00:00Z"}`),
	} {
		if _, err := canonicalApprovalFinancialSnapshot(policy, raw); err == nil {
			t.Fatalf("accepted %s financial snapshot", name)
		}
	}
	drifted := append(json.RawMessage(nil), critical...)
	drifted = json.RawMessage(strings.Replace(string(drifted), `"billing_price_version":7`, `"billing_price_version":8`, 1))
	driftedCanonical, err := canonicalApprovalCriticalVersions(policy, drifted)
	if err != nil || tokenApprovalSnapshotsAligned(driftedCanonical, canonicalFinancial) {
		t.Fatalf("revision drift alignment err=%v", err)
	}
	credentialPolicy, err := approvalpolicy.Lookup("TOKEN_CREDENTIAL_REVOKE")
	if err != nil {
		t.Fatal(err)
	}
	credentialFinancial, err := canonicalApprovalFinancialSnapshot(credentialPolicy, json.RawMessage(`{}`))
	credentialCritical := json.RawMessage(`{"token_account_id":"` + uuid.NewString() + `","token_account_version":8,"credential_id":"` + uuid.NewString() + `","credential_version":3,"credential_status":"ACTIVE","credential_fingerprint":"` + priceFingerprint + `","active_route_set_fingerprint":"` + priceFingerprint + `","provisioning_lease_set_fingerprint":"` + priceFingerprint + `","inflight_request_set_fingerprint":"` + priceFingerprint + `","replacement_credential_version":null,"policy_version":1}`)
	canonicalCredential, criticalErr := canonicalApprovalCriticalVersions(credentialPolicy, credentialCritical)
	if err != nil || criticalErr != nil || !tokenApprovalSnapshotsAligned(canonicalCredential, credentialFinancial) {
		t.Fatalf("credential 11+0 snapshot rejected: financial=%v critical=%v", err, criticalErr)
	}
	if _, err := canonicalApprovalFinancialSnapshot(credentialPolicy, json.RawMessage(`{"billing_price_revision":7}`)); err == nil {
		t.Fatal("credential revoke accepted a Billing snapshot")
	}
	for name, raw := range map[string]json.RawMessage{
		"missing": nil,
		"null":    json.RawMessage(`null`),
		"array":   json.RawMessage(`[]`),
		"extra":   json.RawMessage(`{"extra":true}`),
	} {
		draft := ApprovalDraft{CommandType: "TOKEN_CREDENTIAL_REVOKE", IdempotencyKey: "credential-revoke-strict", NormalizedCommand: json.RawMessage(`{}`), TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: uuid.NewString(), Reason: "revoke after complete impact review", FinancialSnapshot: raw}
		if _, _, err := validateApprovalDraftRequest(draft); err == nil {
			t.Fatalf("credential revoke accepted %s financial snapshot", name)
		}
	}
}

func TestApprovalExecutorIdentityComesOnlyFromTrustedServiceContext(t *testing.T) {
	for _, service := range []string{"billing-command-executor", "token-command-executor"} {
		ctx, err := serviceauth.ContextForJob(context.Background(), service, "APPROVAL_EXECUTION", uuid.NewString())
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/approval-requests/id/executions", nil).WithContext(ctx)
		identity, ok := approvalExecutorFromContext(httptest.NewRecorder(), request)
		if !ok || identity != service {
			t.Fatalf("trusted service %s resolved as %q", service, identity)
		}
	}
	for commandType, wrong := range map[string]string{"WALLET_ADJUSTMENT": "token-command-executor", "TOKEN_CREDENTIAL_REVOKE": "billing-command-executor"} {
		policy, err := approvalpolicy.Lookup(commandType)
		if err != nil {
			t.Fatal(err)
		}
		if policy.ExecutorIdentity == wrong {
			t.Fatalf("%s accepted cross-domain executor %s", commandType, wrong)
		}
	}
	response := httptest.NewRecorder()
	if _, ok := approvalExecutorFromContext(response, httptest.NewRequest(http.MethodPost, "/internal/v1/approval-requests/id/executions", nil)); ok || response.Code != http.StatusForbidden {
		t.Fatalf("untrusted request status=%d", response.Code)
	}
	ctx, err := serviceauth.ContextForJob(context.Background(), "token-command-executor", "APPROVAL_EXECUTION", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	requestID := uuid.New()
	request := httptest.NewRequest(http.MethodPost, "/internal/v1/approval-requests/"+requestID.String()+"/executions", strings.NewReader(`{"executor_identity":"billing-command-executor","job_run_id":"job-1","observed_versions":{}}`)).WithContext(ctx)
	request.SetPathValue("approval_request_id", requestID.String())
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	NewService(&PostgresRepository{}, nil).beginApprovalExecution(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("body-supplied executor identity was not rejected: status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestApprovalExecutionErrorClassIsStableAndSecretFree(t *testing.T) {
	for _, code := range []string{"DOMAIN_EXECUTION_FAILED", "COMMAND_SNAPSHOT_MISMATCH", "DEPENDENCY_TIMEOUT_2"} {
		if !validStableExecutionErrorCode(code) {
			t.Fatalf("stable error class rejected: %s", code)
		}
	}
	for _, code := range []string{"provider returned 500", "domain_execution_failed", "UPSTREAM_TOKEN_ABC123", "PROVIDER_KEY_ABC123", strings.Repeat("A", 121), " LEADING_SPACE"} {
		if validStableExecutionErrorCode(code) {
			t.Fatalf("unsafe error class accepted: %q", code)
		}
		if err := (&PostgresRepository{}).CompleteApprovalExecution(context.Background(), uuid.New(), "billing-command-executor", "job-1", "FAILED", json.RawMessage(`{}`), json.RawMessage(`{}`), code); !errors.Is(err, ErrConflict) {
			t.Fatalf("unsafe error class reached persistence: %q err=%v", code, err)
		}
	}
}

func TestTokenApprovalProductionRouteValidatesRequestBeforePreActivationGate(t *testing.T) {
	service := NewService(&PostgresRepository{}, nil)
	handler := identity.Middleware(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.example", Subject: "operator", SessionID: "session", AuthenticatedAt: time.Now()}, nil
	}, http.HandlerFunc(service.adminCreateApprovalDraft))
	request := httptest.NewRequest(http.MethodPost, "/admin/v1/approval-requests", strings.NewReader(`{"command_type":"TOKEN_MODEL_LIFECYCLE_CHANGE"}`))
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "CONFLICT") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
