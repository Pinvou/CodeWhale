package token

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/approvalpolicy"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

type testTokenGovernanceActivationReader struct {
	activation TokenCommandGovernanceActivation
	err        error
}

type testApprovedTokenExecutor struct{}

type countingApprovedTokenExecutor struct{ prechecks int }

func (executor *countingApprovedTokenExecutor) PrecheckApprovedTokenCommand(context.Context, string, TokenCommandEnvelope) (json.RawMessage, error) {
	executor.prechecks++
	return json.RawMessage(`{}`), nil
}

func (*countingApprovedTokenExecutor) ExecuteApprovedTokenCommand(context.Context, string, string, TokenCommandEnvelope) (json.RawMessage, json.RawMessage, string, error) {
	return nil, nil, "", errors.New("must not execute")
}

func (testApprovedTokenExecutor) PrecheckApprovedTokenCommand(context.Context, string, TokenCommandEnvelope) (json.RawMessage, error) {
	return json.RawMessage(`{"version":1}`), nil
}

func (testApprovedTokenExecutor) ExecuteApprovedTokenCommand(context.Context, string, string, TokenCommandEnvelope) (json.RawMessage, json.RawMessage, string, error) {
	return json.RawMessage(`{"version":2}`), json.RawMessage(`{"status":"ok"}`), "", nil
}

func (reader testTokenGovernanceActivationReader) CurrentTokenCommandGovernanceActivation(context.Context) (TokenCommandGovernanceActivation, error) {
	return reader.activation, reader.err
}

func TestNineteenTokenCommandSpecsMatchRegistry(t *testing.T) {
	if len(tokenCommandSpecs) != 19 {
		t.Fatalf("expected 19 governed commands, got %d", len(tokenCommandSpecs))
	}
	for commandType, spec := range tokenCommandSpecs {
		policy, err := approvalpolicy.Lookup(commandType)
		if err != nil || (policy.Mode == approvalpolicy.ApprovalRequired) != spec.Approval || policy.RequesterPermission != spec.Permissions[0] {
			t.Fatalf("command spec/registry mismatch for %s: %#v %#v %v", commandType, spec, policy, err)
		}
	}
}

func TestNormalizeTokenCommandRejectsUnsafeOrIncompleteInput(t *testing.T) {
	base := TokenCommandEnvelope{
		CommandType: "TOKEN_ACCOUNT_STATUS_CHANGE", Reason: "disable account after impact review", TargetResourceType: "TOKEN_ACCOUNT",
		TargetResourceID: "01a00000-0000-7000-8000-000000000001", PrecheckFingerprint: strings.Repeat("a", 64),
		ExpectedVersions: json.RawMessage(`{"token_account_id":"01a00000-0000-7000-8000-000000000001","expected_account_version":4,"target_status":"DISABLED","precheck_fingerprint":"` + strings.Repeat("a", 64) + `","request_fingerprint":"` + strings.Repeat("b", 64) + `"}`), Command: json.RawMessage(`{"target_status":"DISABLED"}`),
	}
	if _, _, _, _, err := normalizeTokenCommand(base, "stable-command-key-0001", ""); err != nil {
		t.Fatalf("valid command was rejected: %v", err)
	}
	for name, mutate := range map[string]func(*TokenCommandEnvelope){
		"force": func(item *TokenCommandEnvelope) { item.Command = json.RawMessage(`{"force":true}`) },
		"secret": func(item *TokenCommandEnvelope) {
			item.Command = json.RawMessage(`{"api_key":"redacted-is-still-forbidden"}`)
		},
		"camel case secret": func(item *TokenCommandEnvelope) {
			item.Command = json.RawMessage(`{"nested":[{"refreshToken":"forbidden"}]}`)
		},
		"credential metadata masquerade": func(item *TokenCommandEnvelope) {
			item.Command = json.RawMessage(`{"credential_id":{"apiKey":"forbidden"}}`)
		},
		"missing CAS": func(item *TokenCommandEnvelope) { item.ExpectedVersions = json.RawMessage(`{"token_account_id":"x"}`) },
	} {
		candidate := base
		mutate(&candidate)
		if _, _, _, _, err := normalizeTokenCommand(candidate, "stable-command-key-0001", ""); err == nil {
			t.Fatalf("%s input was accepted", name)
		}
	}
}

func TestTokenCommandSensitiveScanIsRecursiveWithCredentialMetadataWhitelist(t *testing.T) {
	for _, safe := range []json.RawMessage{
		json.RawMessage(`{"credential_id":"` + uuid.NewString() + `","credential_version":2,"credential_status":"ACTIVE","credential_fingerprint":"` + strings.Repeat("a", 64) + `"}`),
		json.RawMessage(`{"nested":[{"token_account_id":"` + uuid.NewString() + `"}]}`),
		json.RawMessage(`{"validation_status":"VALID","credential":{"credential_id":"` + uuid.NewString() + `","credential_type":"NEW_API","fingerprint":"` + strings.Repeat("b", 64) + `","version":2,"status":"ACTIVE","validation_status":"VALID","allowed_models":["flash"],"validated_at":"2026-09-04T12:00:00Z","responsible_actor_id":"` + uuid.NewString() + `","created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-04T12:00:00Z"}}`),
	} {
		if containsSensitiveCommandField(safe) {
			t.Fatalf("safe credential metadata was rejected: %s", safe)
		}
	}
	for _, unsafe := range []json.RawMessage{
		json.RawMessage(`{"nested":[{"secret":"value"}]}`),
		json.RawMessage(`{"result":{"credential_ciphertext":"value"}}`),
		json.RawMessage(`{"provider":{"endpoint":"https://private.invalid"}}`),
		json.RawMessage(`{"access_token":"value"}`),
		json.RawMessage(`{"accessToken":"value"}`),
		json.RawMessage(`{"refreshToken":"value"}`),
		json.RawMessage(`{"apiKey":"value"}`),
		json.RawMessage(`{"privateKey":"value"}`),
		json.RawMessage(`{"credential_id":{"secret":"masquerade"}}`),
		json.RawMessage(`{"credential_version":"2"}`),
		json.RawMessage(`{"nested":[[{"credential_status":"SECRET"}]]}`),
		json.RawMessage(`{"credential":{"credential_id":"` + uuid.NewString() + `","credential_type":"NEW_API","fingerprint":"` + strings.Repeat("b", 64) + `","version":2,"status":"ACTIVE","validation_status":"VALID","allowed_models":["flash"],"responsible_actor_id":"` + uuid.NewString() + `","created_at":"2026-09-01T00:00:00Z","updated_at":"2026-09-04T12:00:00Z","provider_url":"https://private.invalid"}}`),
	} {
		if !containsSensitiveCommandField(unsafe) {
			t.Fatalf("sensitive field was accepted: %s", unsafe)
		}
	}
	badBefore := tokenCommandBegin{Envelope: TokenCommandEnvelope{}, BeforeState: json.RawMessage(`{"nested":{"secret":"value"}}`)}
	if _, _, err := (&PostgresRepository{}).BeginTokenCommand(context.Background(), badBefore); !errors.Is(err, ErrTokenCommandInvalid) {
		t.Fatalf("sensitive before state reached persistence: %v", err)
	}
	if _, err := (&PostgresRepository{}).CompleteTokenCommand(context.Background(), uuid.New(), "SUCCEEDED", json.RawMessage(`{}`), json.RawMessage(`{"nested":{"secret":"value"}}`), ""); !errors.Is(err, ErrTokenCommandInvalid) {
		t.Fatalf("sensitive result reached persistence: %v", err)
	}
}

func TestTokenCommandDecodeFailuresReturnStableBadRequest(t *testing.T) {
	for _, raw := range []string{`{"command_type":`, `{"command_type":"TOKEN_MODEL_DRAFT_SAVE","unknown":true}`} {
		request := httptest.NewRequest(http.MethodPost, "/token/admin/v1/commands", strings.NewReader(raw))
		response := httptest.NewRecorder()
		(&Service{authorizer: &authorization.Client{}}).adminCreateTokenCommand(response, request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INVALID_COMMAND") {
			t.Fatalf("new command decode status=%d body=%s", response.Code, response.Body.String())
		}
	}
	ctx, err := serviceauth.ContextForJob(context.Background(), "token-command-executor", "APPROVAL_EXECUTION", uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{`{"approval_request_id":`, `{"executor_identity":"billing-command-executor"}`} {
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/commands/model-lifecycle/execute", strings.NewReader(raw)).WithContext(ctx)
		response := httptest.NewRecorder()
		(&Service{approvedCommandExecutor: testApprovedTokenExecutor{}}).executeApprovedModelLifecycle(response, request)
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INVALID_APPROVED_COMMAND") {
			t.Fatalf("approved command decode status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestApprovedCredentialRevokeRejectsNonEmptyFinancialSnapshotBeforePersistence(t *testing.T) {
	accountID, credentialID, approvalID, actorID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	fingerprint := strings.Repeat("a", 64)
	expected := json.RawMessage(`{"token_account_id":"` + accountID.String() + `","token_account_version":2,"credential_id":"` + credentialID.String() + `","credential_version":1,"credential_status":"ACTIVE","credential_fingerprint":"` + strings.Repeat("b", 64) + `","active_route_set_fingerprint":"` + strings.Repeat("c", 64) + `","provisioning_lease_set_fingerprint":"` + strings.Repeat("d", 64) + `","inflight_request_set_fingerprint":"` + strings.Repeat("e", 64) + `","replacement_credential_version":2,"policy_version":1}`)
	envelope := TokenCommandEnvelope{
		CommandType: "TOKEN_CREDENTIAL_REVOKE", Reason: "revoke the approved credential after impact review", TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: credentialID.String(),
		ExpectedVersions: expected, PrecheckFingerprint: fingerprint,
		Command: json.RawMessage(`{"token_account_id":"` + accountID.String() + `","expected_account_version":2,"credential_id":"` + credentialID.String() + `","credential_version":1}`),
	}
	for name, financial := range map[string]json.RawMessage{
		"missing": nil,
		"null":    json.RawMessage(`null`),
		"array":   json.RawMessage(`[]`),
		"extra":   json.RawMessage(`{"billing_price_revision":1}`),
	} {
		t.Run(name, func(t *testing.T) {
			executor := &countingApprovedTokenExecutor{}
			body := approvedTokenCommandBody{ApprovalRequestID: approvalID, RequesterUserID: actorID, AuthorizationVersion: 3, CommandFingerprint: fingerprint, ExecutionIdempotencyKey: "approval:" + approvalID.String() + ":TOKEN_CREDENTIAL_REVOKE:" + fingerprint, CriticalResourceVersions: expected, FinancialSnapshot: financial, Envelope: envelope}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			ctx, err := serviceauth.ContextForJob(context.Background(), "token-command-executor", "APPROVAL_EXECUTION", uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/internal/v1/commands/credential-revoke/execute", strings.NewReader(string(raw))).WithContext(ctx)
			response := httptest.NewRecorder()
			(&Service{approvedCommandExecutor: executor}).executeApprovedCredentialRevoke(response, request)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INVALID_APPROVED_COMMAND") || executor.prechecks != 0 {
				t.Fatalf("status=%d prechecks=%d body=%s", response.Code, executor.prechecks, response.Body.String())
			}
		})
	}
}

func TestTokenCommandStableErrorCodeIsBoundedAndSecretFree(t *testing.T) {
	for _, code := range []string{"COMMAND_RESULT_UNKNOWN", "DOMAIN_EXECUTION_FAILED", "PRECHECK_CHANGED_2"} {
		if !validStableTokenErrorCode(code) {
			t.Fatalf("stable error code rejected: %s", code)
		}
	}
	for _, code := range []string{"provider returned 500", "command_result_unknown", "UPSTREAM_TOKEN_ABC123", "PROVIDER_KEY_ABC123", strings.Repeat("A", 121), " TRAILING"} {
		if validStableTokenErrorCode(code) {
			t.Fatalf("unsafe stable error code accepted: %q", code)
		}
		if _, err := (&PostgresRepository{}).CompleteTokenCommand(context.Background(), uuid.New(), "UNKNOWN", json.RawMessage(`{}`), json.RawMessage(`{}`), code); !errors.Is(err, ErrTokenCommandInvalid) {
			t.Fatalf("unsafe code reached persistence: %q err=%v", code, err)
		}
	}
}

func TestTokenCommandActorUsesAccountAuthorityForNonUUIDSubject(t *testing.T) {
	stableActor := uuid.New()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Subject        string   `json:"subject"`
			AllPermissions []string `json:"all_permissions"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.Subject != "external-non-uuid-subject" || len(body.AllPermissions) != 1 || body.AllPermissions[0] != "platform.models.manage" {
			t.Fatalf("unexpected Account authorization request: %#v", body)
		}
		w.Header().Set("X-Authorization-Version", "17")
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "user_id": stableActor})
	}))
	t.Cleanup(server.Close)
	client, err := authorization.NewClient(server.URL, "service-token")
	if err != nil {
		t.Fatal(err)
	}
	service := &Service{authorizer: client}
	actor, version, err := service.authorizedTokenCommandActor(context.Background(), identity.Principal{Issuer: "https://issuer.example", Subject: "external-non-uuid-subject"}, "TOKEN_MODEL_DRAFT_SAVE")
	if err != nil || actor != stableActor || version != 17 {
		t.Fatalf("actor=%s version=%d err=%v", actor, version, err)
	}
}

func TestApprovedTokenExecutorRejectsCrossDomainIdentityAndRequiresStableExecutionKey(t *testing.T) {
	approvalID := uuid.New()
	fingerprint := strings.Repeat("a", 64)
	envelope := TokenCommandEnvelope{CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", Reason: "apply approved model lifecycle transition", TargetResourceType: "MODEL", TargetResourceID: uuid.NewString(), ExpectedVersions: json.RawMessage(`{}`), PrecheckFingerprint: fingerprint, Command: json.RawMessage(`{}`)}
	body := approvedTokenCommandBody{ApprovalRequestID: approvalID, RequesterUserID: uuid.New(), AuthorizationVersion: 1, CommandFingerprint: fingerprint, ExecutionIdempotencyKey: "approval:" + approvalID.String() + ":TOKEN_MODEL_LIFECYCLE_CHANGE:" + fingerprint, Envelope: envelope}
	raw, _ := json.Marshal(body)
	for _, scenario := range []struct {
		name, service string
		mutate        func(*approvedTokenCommandBody)
		want          int
	}{
		{name: "billing executor cannot impersonate token", service: "billing-command-executor", want: http.StatusForbidden},
		{name: "token executor wrong execution key", service: "token-command-executor", mutate: func(item *approvedTokenCommandBody) { item.ExecutionIdempotencyKey = "approval:wrong" }, want: http.StatusBadRequest},
		{name: "token executor accepted before repository lookup", service: "token-command-executor", want: http.StatusServiceUnavailable},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			candidate := body
			if scenario.mutate != nil {
				scenario.mutate(&candidate)
				raw, _ = json.Marshal(candidate)
			} else {
				raw, _ = json.Marshal(candidate)
			}
			ctx, err := serviceauth.ContextForJob(context.Background(), scenario.service, "APPROVAL_EXECUTION", uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/internal/v1/commands/model-lifecycle/execute", strings.NewReader(string(raw))).WithContext(ctx)
			response := httptest.NewRecorder()
			(&Service{approvedCommandExecutor: testApprovedTokenExecutor{}}).executeApprovedModelLifecycle(response, request)
			if response.Code != scenario.want {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}
}

func TestNewCommandEnvelopeAndInternalExecutorsStayClosedWithoutActivation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	service := &Service{}
	for _, target := range []struct{ method, path string }{
		{http.MethodPost, "/admin/v1/commands"},
		{http.MethodGet, "/admin/v1/commands/01a00000-0000-7000-8000-000000000001"},
		{http.MethodPost, "/internal/v1/commands/model-lifecycle/execute"},
	} {
		recorder := httptest.NewRecorder()
		context, _ := gin.CreateTestContext(recorder)
		request := httptest.NewRequest(target.method, target.path, nil)
		context.Request = request
		service.requireNewTokenCommandGovernanceActive()(context)
		if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "TOKEN_COMMAND_GOVERNANCE_INACTIVE") {
			t.Fatalf("pre-activation endpoint did not fail closed: %s %s => %d %s", target.method, target.path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestApprovedExecutorProductionRouteRequiresServiceAuthThenFailsPreActivation(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	handler := (&Service{}).Handler("executor-test-token", nil)
	for _, authenticated := range []bool{false, true} {
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/commands/model-lifecycle/execute", strings.NewReader(`{}`))
		if authenticated {
			if err := serviceauth.SetAuthorization(request, "executor-test-token"); err != nil {
				t.Fatal(err)
			}
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if !authenticated && response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated executor status=%d body=%s", response.Code, response.Body.String())
		}
		if authenticated && (response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "TOKEN_COMMAND_GOVERNANCE_INACTIVE")) {
			t.Fatalf("pre-activation executor status=%d body=%s", response.Code, response.Body.String())
		}
	}
}

func TestLegacyDirectSelectionPreservesPreActivationAndFailsClosedAtActive(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scenario := range []struct {
		name       string
		reader     TokenCommandGovernanceActivationReader
		wantStatus int
		wantNext   bool
	}{
		{name: "record absent", reader: nil, wantStatus: http.StatusNoContent, wantNext: true},
		{name: "pre activation", reader: testTokenGovernanceActivationReader{activation: TokenCommandGovernanceActivation{State: "PRE_ACTIVATION"}}, wantStatus: http.StatusNoContent, wantNext: true},
		{name: "active", reader: testTokenGovernanceActivationReader{activation: TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 1}}, wantStatus: http.StatusForbidden},
		{name: "safe rollback", reader: testTokenGovernanceActivationReader{activation: TokenCommandGovernanceActivation{State: "SAFE_ROLLBACK", Epoch: 2}}, wantStatus: http.StatusServiceUnavailable},
		{name: "reader error", reader: testTokenGovernanceActivationReader{err: errors.New("read failed")}, wantStatus: http.StatusServiceUnavailable},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			service := &Service{commandGovernanceActivation: scenario.reader}
			router := gin.New()
			next := false
			router.POST("/legacy", service.selectLegacyTokenDirect(), func(c *gin.Context) { next = true; c.Status(http.StatusNoContent) })
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/legacy", nil))
			if response.Code != scenario.wantStatus || next != scenario.wantNext {
				t.Fatalf("status=%d next=%v", response.Code, next)
			}
		})
	}
}

func TestStableRouteVersionSelectionIsAtomicWithLegacyRetirement(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, scenario := range []struct {
		name         string
		reader       TokenCommandGovernanceActivationReader
		legacyStatus int
		legacyNext   bool
		stableStatus int
		stableNext   bool
	}{
		{name: "record absent", legacyStatus: http.StatusNoContent, legacyNext: true, stableStatus: http.StatusServiceUnavailable},
		{name: "pre activation", reader: testTokenGovernanceActivationReader{activation: TokenCommandGovernanceActivation{State: "PRE_ACTIVATION"}}, legacyStatus: http.StatusNoContent, legacyNext: true, stableStatus: http.StatusServiceUnavailable},
		{name: "active", reader: testTokenGovernanceActivationReader{activation: TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 1}}, legacyStatus: http.StatusGone, stableStatus: http.StatusNoContent, stableNext: true},
		{name: "safe rollback", reader: testTokenGovernanceActivationReader{activation: TokenCommandGovernanceActivation{State: "SAFE_ROLLBACK", Epoch: 2}}, legacyStatus: http.StatusServiceUnavailable, stableStatus: http.StatusServiceUnavailable},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			service := &Service{commandGovernanceActivation: scenario.reader}
			legacyNext, stableNext := false, false
			legacy := gin.New()
			legacy.PUT("/legacy", service.selectLegacyTokenRouteDirect(), func(c *gin.Context) { legacyNext = true; c.Status(http.StatusNoContent) })
			legacyResponse := httptest.NewRecorder()
			legacy.ServeHTTP(legacyResponse, httptest.NewRequest(http.MethodPut, "/legacy", nil))
			stable := gin.New()
			stable.PUT("/stable", service.requireNewTokenCommandGovernanceActive(), func(c *gin.Context) { stableNext = true; c.Status(http.StatusNoContent) })
			stableResponse := httptest.NewRecorder()
			stable.ServeHTTP(stableResponse, httptest.NewRequest(http.MethodPut, "/stable", nil))
			if legacyResponse.Code != scenario.legacyStatus || legacyNext != scenario.legacyNext || stableResponse.Code != scenario.stableStatus || stableNext != scenario.stableNext {
				t.Fatalf("legacy=%d/%v stable=%d/%v", legacyResponse.Code, legacyNext, stableResponse.Code, stableNext)
			}
		})
	}
}
