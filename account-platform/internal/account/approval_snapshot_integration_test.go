package account

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/serviceauth"
	tokenservice "github.com/Pinvou/pinvou3/account-platform/internal/token"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestApprovalSnapshotLifecyclePostgresIntegration exercises the production
// HTTP handlers and repository transaction chain. CI supplies the Account
// database and rejects SKIP for this named release gate.
func TestApprovalSnapshotLifecyclePostgresIntegration(t *testing.T) {
	t.Setenv("APP_ENV", "test")
	t.Setenv("WORKLOAD_IDENTITY_ISSUER", "")
	t.Setenv("WORKLOAD_IDENTITY_AUDIENCE", "")
	t.Setenv("WORKLOAD_IDENTITY_JWKS_URL", "")
	databaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_ACCOUNT_DATABASE_URL"))
	if databaseURL == "" {
		t.Skip("PINVOU_TEST_ACCOUNT_DATABASE_URL is not configured")
	}
	tokenDatabaseURL := strings.TrimSpace(os.Getenv("PINVOU_TEST_TOKEN_DATABASE_URL"))
	if tokenDatabaseURL == "" {
		t.Skip("PINVOU_TEST_TOKEN_DATABASE_URL is not configured")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository, _, _ := newTokenOwnerCandidateIntegrationRepository(t, ctx, databaseURL)
	repository.tokenApprovalCreateGate = func(commandType string) bool { return commandType == "TOKEN_MODEL_LIFECYCLE_CHANGE" }
	requester := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_MODEL_ADMIN", "snapshot-requester")
	secondRequester := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_MODEL_ADMIN", "snapshot-requester-two")
	approver := seedTokenOwnerCandidateActor(t, ctx, repository.pool, "PLATFORM_SUPER_ADMIN", "snapshot-approver")
	requesterPrincipal := approvalIntegrationPrincipal(requester)
	secondRequesterPrincipal := approvalIntegrationPrincipal(secondRequester)
	approverPrincipal := approvalIntegrationPrincipal(approver)
	tokenRepository, tokenConnection := newApprovalSnapshotTokenRepository(t, ctx, tokenDatabaseURL)
	modelID := seedApprovalSnapshotTokenModel(t, ctx, tokenConnection)
	pricing := approvalSnapshotPricingStub{modelID: modelID, observedAt: time.Now().UTC(), fingerprint: strings.Repeat("b", 64)}
	tokenService := tokenservice.NewService(tokenRepository, nil, nil, nil, tokenservice.ProviderURLPolicy{}, nil, nil, 0).WithImpactPrechecks(true, tokenRepository, pricing)
	serviceToken := "snapshot-integration-token"
	tokenHandler := tokenService.Handler(serviceToken, func(context.Context, string) (identity.Principal, error) { return identity.Principal{}, ErrForbidden })
	draft := ApprovalDraft{
		CommandType: "TOKEN_MODEL_LIFECYCLE_CHANGE", IdempotencyKey: "snapshot-lifecycle-create-0001",
		NormalizedCommand:  json.RawMessage(`{"model_id":"` + modelID.String() + `","target_lifecycle":"DEPRECATED","effective_at":"2026-09-04T08:00:00Z","existing_session_cutoff_at":null,"replacement_model_id":null}`),
		TargetResourceType: "MODEL", TargetResourceID: modelID.String(), Reason: "deprecate after independent impact review",
		RedactedEvidence: json.RawMessage(`[]`), ExpectedImpact: json.RawMessage(`{}`), CriticalResourceVersions: json.RawMessage(`{}`), FinancialSnapshot: json.RawMessage(`{}`),
	}

	var sourceMode atomic.Int32
	var sourceCalls atomic.Int32
	sourceServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceCalls.Add(1)
		if sourceMode.Load() == 1 {
			http.Error(w, `{"code":"UPSTREAM_UNAVAILABLE"}`, http.StatusServiceUnavailable)
			return
		}
		if sourceMode.Load() == 3 {
			http.Error(w, `{"code":"UPSTREAM_TIMEOUT"}`, http.StatusGatewayTimeout)
			return
		}
		tokenHandler.ServeHTTP(w, r)
	}))
	t.Cleanup(sourceServer.Close)
	source, err := NewHTTPApprovalSnapshotSource(sourceServer.URL, serviceToken)
	if err != nil {
		t.Fatal(err)
	}
	repository.WithApprovalSnapshotSource(source)
	service := NewService(repository, nil)

	// The test-only gate opens only the otherwise dormant create branch. Two
	// concurrent HTTP requests traverse strong authorization, the actor-scoped
	// idempotency lock, and the real Token authoritative precheck exactly once.
	createBody := approvalDraftBodyFromModel(draft)
	var wait sync.WaitGroup
	responses := make([]*httptest.ResponseRecorder, 2)
	wait.Add(len(responses))
	for index := range responses {
		go func(index int) {
			defer wait.Done()
			responses[index] = performApprovalIntegrationRequest(t, service.adminCreateApprovalDraft, requesterPrincipal, http.MethodPost, "/admin/v1/approval-requests", "", createBody)
		}(index)
	}
	wait.Wait()
	var seeded ApprovalRequest
	for index, response := range responses {
		var item ApprovalRequest
		if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &item) != nil || item.Status != "DRAFT" || len(item.CriticalResourceVersions) == 0 || len(item.FinancialSnapshot) == 0 {
			t.Fatalf("idempotent create status=%d body=%s", response.Code, response.Body.String())
		}
		if index == 0 {
			seeded = item
		} else if item.ID != seeded.ID || !sameJSONObject(item.CriticalResourceVersions, seeded.CriticalResourceVersions) || !sameJSONObject(item.FinancialSnapshot, seeded.FinancialSnapshot) {
			t.Fatalf("idempotent create returned different frozen responses: %s / %s", responses[0].Body.String(), responses[1].Body.String())
		}
	}
	if sourceCalls.Load() != 1 {
		t.Fatalf("concurrent create authoritative reads=%d, want 1", sourceCalls.Load())
	}
	var requestRows, snapshotRows, createEvents int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM approval_requests WHERE requester_user_id=$1 AND command_type='TOKEN_MODEL_LIFECYCLE_CHANGE' AND idempotency_key=$2`, requester.ID, draft.IdempotencyKey).Scan(&requestRows); err != nil {
		t.Fatal(err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM approval_request_snapshots WHERE approval_request_id=$1`, seeded.ID).Scan(&snapshotRows); err != nil {
		t.Fatal(err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM approval_request_state_events WHERE approval_request_id=$1 AND reason_code='CREATED'`, seeded.ID).Scan(&createEvents); err != nil {
		t.Fatal(err)
	}
	if requestRows != 1 || snapshotRows != 1 || createEvents != 1 {
		t.Fatalf("atomic create rows request=%d snapshot=%d event=%d", requestRows, snapshotRows, createEvents)
	}
	critical, financial := seeded.CriticalResourceVersions, seeded.FinancialSnapshot
	assertApprovalSnapshotKeys(t, critical, []string{
		"model_id", "catalog_version", "current_lifecycle", "target_lifecycle", "lifecycle_plan_version",
		"effective_at", "existing_session_cutoff_at", "replacement_model_id", "billing_price_version",
		"route_set_fingerprint", "provisioning_set_fingerprint", "inflight_request_set_fingerprint", "policy_version",
	})
	assertApprovalSnapshotKeys(t, financial, []string{
		"billing_price_revision", "price_set_fingerprint", "models_checked", "models_covered", "fully_covered", "billing_as_of",
	})
	draft.CriticalResourceVersions, draft.FinancialSnapshot = critical, financial
	financialTarget := uuid.NewString()
	concurrentBody := map[string]any{
		"command_type": "WALLET_ADJUSTMENT", "idempotency_key": "snapshot-concurrent-create-0005",
		"normalized_command":   map[string]any{"target_id": financialTarget, "adjustment_microcredits": 1},
		"target_resource_type": "WALLET", "target_resource_id": financialTarget, "reason": "repair an independently reviewed wallet fact",
		"redacted_evidence": []any{}, "expected_impact": map[string]any{},
		"critical_resource_versions": map[string]any{"target_id": financialTarget, "resource_version": 1, "financial_snapshot": map[string]any{}, "policy_version": 1},
		"financial_snapshot":         map[string]any{},
	}
	concurrentResponses := make([]*httptest.ResponseRecorder, 2)
	wait.Add(len(concurrentResponses))
	for index := range concurrentResponses {
		go func(index int) {
			defer wait.Done()
			concurrentResponses[index] = performApprovalIntegrationRequest(t, service.adminCreateApprovalDraft, approverPrincipal, http.MethodPost, "/admin/v1/approval-requests", "", concurrentBody)
		}(index)
	}
	wait.Wait()
	var concurrentID uuid.UUID
	for index, response := range concurrentResponses {
		var item ApprovalRequest
		if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &item) != nil || item.Status != "DRAFT" {
			t.Fatalf("concurrent create status=%d body=%s", response.Code, response.Body.String())
		}
		if index == 0 {
			concurrentID = item.ID
		} else if item.ID != concurrentID {
			t.Fatalf("concurrent create returned different requests %s/%s", concurrentID, item.ID)
		}
	}
	var concurrentRows int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM approval_requests WHERE requester_user_id=$1 AND command_type='WALLET_ADJUSTMENT' AND idempotency_key='snapshot-concurrent-create-0005'`, approver.ID).Scan(&concurrentRows); err != nil || concurrentRows != 1 {
		t.Fatalf("concurrent idempotent rows=%d err=%v", concurrentRows, err)
	}
	updateBody := approvalDraftBodyFromModel(draft)
	updateBody["expected_version"] = 1
	foreignUpdate := performApprovalIntegrationRequest(t, service.adminUpdateApprovalDraft, approverPrincipal, http.MethodPut, "/admin/v1/approval-requests/"+seeded.ID.String(), seeded.ID.String(), updateBody)
	if foreignUpdate.Code != http.StatusConflict || sourceCalls.Load() != 1 {
		t.Fatalf("foreign update status=%d body=%s downstream=%d", foreignUpdate.Code, foreignUpdate.Body.String(), sourceCalls.Load())
	}
	preActivationUpdate := performApprovalIntegrationRequest(t, service.adminUpdateApprovalDraft, requesterPrincipal, http.MethodPut, "/admin/v1/approval-requests/"+seeded.ID.String(), seeded.ID.String(), updateBody)
	if preActivationUpdate.Code != http.StatusServiceUnavailable || !strings.Contains(preActivationUpdate.Body.String(), "TOKEN_COMMAND_GOVERNANCE_INACTIVE") || sourceCalls.Load() != 1 {
		t.Fatalf("pre-activation update status=%d body=%s downstream=%d", preActivationUpdate.Code, preActivationUpdate.Body.String(), sourceCalls.Load())
	}
	assertApprovalStatusVersion(t, ctx, repository, seeded.ID, "DRAFT", 1)
	crossActor := performApprovalIntegrationRequest(t, service.adminCreateApprovalDraft, secondRequesterPrincipal, http.MethodPost, "/admin/v1/approval-requests", "", createBody)
	var crossActorItem ApprovalRequest
	if crossActor.Code != http.StatusCreated || json.Unmarshal(crossActor.Body.Bytes(), &crossActorItem) != nil || crossActorItem.ID == seeded.ID || crossActorItem.RequesterUserID != secondRequester.ID {
		t.Fatalf("cross-actor key was not isolated: %d %s", crossActor.Code, crossActor.Body.String())
	}
	if sourceCalls.Load() != 2 {
		t.Fatalf("cross-actor create authoritative reads=%d, want 2", sourceCalls.Load())
	}

	submit := performApprovalIntegrationRequest(t, service.adminSubmitApprovalRequest, requesterPrincipal, http.MethodPost, "/admin/v1/approval-requests/"+seeded.ID.String()+"/actions/submit", seeded.ID.String(), map[string]any{"expected_version": 1})
	var submitted ApprovalRequest
	if submit.Code != http.StatusOK || json.Unmarshal(submit.Body.Bytes(), &submitted) != nil || submitted.Status != "SUBMITTED" || submitted.Version != 2 || sourceCalls.Load() != 3 {
		t.Fatalf("submit status=%d body=%s calls=%d", submit.Code, submit.Body.String(), sourceCalls.Load())
	}

	transientDraft := draft
	transientDraft.IdempotencyKey = "snapshot-lifecycle-transient-0002"
	transientID := seedApprovalSnapshotDraft(t, ctx, repository, requester.ID, transientDraft)
	sourceMode.Store(1)
	transient := performApprovalIntegrationRequest(t, service.adminSubmitApprovalRequest, requesterPrincipal, http.MethodPost, "/admin/v1/approval-requests/"+transientID.String()+"/actions/submit", transientID.String(), map[string]any{"expected_version": 1})
	if transient.Code != http.StatusServiceUnavailable || !strings.Contains(transient.Body.String(), "APPROVAL_SNAPSHOT_UNAVAILABLE") {
		t.Fatalf("transient status=%d body=%s", transient.Code, transient.Body.String())
	}
	assertApprovalStatusVersion(t, ctx, repository, transientID, "DRAFT", 1)
	timeoutDraft := draft
	timeoutDraft.IdempotencyKey = "snapshot-lifecycle-timeout-0004"
	timeoutID := seedApprovalSnapshotDraft(t, ctx, repository, requester.ID, timeoutDraft)
	sourceMode.Store(3)
	timedOut := performApprovalIntegrationRequest(t, service.adminSubmitApprovalRequest, requesterPrincipal, http.MethodPost, "/admin/v1/approval-requests/"+timeoutID.String()+"/actions/submit", timeoutID.String(), map[string]any{"expected_version": 1})
	if timedOut.Code != http.StatusGatewayTimeout || !strings.Contains(timedOut.Body.String(), "APPROVAL_SNAPSHOT_TIMEOUT") {
		t.Fatalf("timeout status=%d body=%s", timedOut.Code, timedOut.Body.String())
	}
	assertApprovalStatusVersion(t, ctx, repository, timeoutID, "DRAFT", 1)

	driftDraft := draft
	driftDraft.IdempotencyKey = "snapshot-lifecycle-drift-0003"
	driftID := seedApprovalSnapshotDraft(t, ctx, repository, requester.ID, driftDraft)
	sourceMode.Store(0)
	if _, err := tokenConnection.Exec(ctx, `UPDATE model_catalog_entries SET lifecycle_status='DEPRECATED' WHERE model_id=$1`, modelID); err != nil {
		t.Fatal(err)
	}
	drift := performApprovalIntegrationRequest(t, service.adminSubmitApprovalRequest, requesterPrincipal, http.MethodPost, "/admin/v1/approval-requests/"+driftID.String()+"/actions/submit", driftID.String(), map[string]any{"expected_version": 1})
	if drift.Code != http.StatusConflict || !strings.Contains(drift.Body.String(), "APPROVAL_REQUEST_INVALIDATED") {
		t.Fatalf("drift status=%d body=%s", drift.Code, drift.Body.String())
	}
	assertApprovalStatusVersion(t, ctx, repository, driftID, "INVALIDATED", 2)

	sourceMode.Store(0)
	decide := performApprovalIntegrationRequest(t, service.adminDecideApprovalRequest, approverPrincipal, http.MethodPost, "/admin/v1/approval-requests/"+seeded.ID.String()+"/actions/decide", seeded.ID.String(), map[string]any{"expected_version": 2, "decision": "APPROVED", "reason": "independent evidence approved", "observed_versions": json.RawMessage(critical)})
	var approved ApprovalRequest
	if decide.Code != http.StatusOK || json.Unmarshal(decide.Body.Bytes(), &approved) != nil || approved.Status != "APPROVED" || approved.Version != 3 {
		t.Fatalf("decide status=%d body=%s", decide.Code, decide.Body.String())
	}
	var eventPayload json.RawMessage
	if err := repository.pool.QueryRow(ctx, `SELECT payload FROM outbox_events WHERE event_type='account.approval-command.authorized.v1' AND aggregate_id=$1`, seeded.ID).Scan(&eventPayload); err != nil || !strings.Contains(string(eventPayload), `"financial_snapshot"`) || !strings.Contains(string(eventPayload), strings.Repeat("b", 64)) {
		t.Fatalf("authorized event payload=%s err=%v", eventPayload, err)
	}

	executionCtx, err := serviceauth.ContextForJob(context.Background(), "token-command-executor", "APPROVAL_EXECUTION", "snapshot-chain")
	if err != nil {
		t.Fatal(err)
	}
	executionBody, _ := json.Marshal(map[string]any{"job_run_id": "snapshot-chain-job", "observed_versions": json.RawMessage(critical), "observed_financial_snapshot": json.RawMessage(financial)})
	executionRequest := httptest.NewRequest(http.MethodPost, "/internal/v1/approval-requests/"+seeded.ID.String()+"/executions", bytes.NewReader(executionBody)).WithContext(executionCtx)
	executionRequest.SetPathValue("approval_request_id", seeded.ID.String())
	executionRequest.Header.Set("Content-Type", "application/json")
	executionResponse := httptest.NewRecorder()
	service.beginApprovalExecution(executionResponse, executionRequest)
	var lease ApprovalExecution
	if executionResponse.Code != http.StatusOK || json.Unmarshal(executionResponse.Body.Bytes(), &lease) != nil || lease.Status != "EXECUTING" || !sameJSONObject(lease.CriticalResourceVersions, critical) || !sameJSONObject(lease.FinancialSnapshot, financial) {
		t.Fatalf("execution lease status=%d body=%s", executionResponse.Code, executionResponse.Body.String())
	}
}

func approvalIntegrationPrincipal(actor integrationActor) identity.Principal {
	return identity.Principal{Issuer: actor.Issuer, Subject: actor.Subject, SessionID: "snapshot-integration-session", AuthenticatedAt: time.Now().UTC()}
}

func approvalDraftBodyFromModel(draft ApprovalDraft) map[string]any {
	return map[string]any{
		"command_type": draft.CommandType, "idempotency_key": draft.IdempotencyKey, "normalized_command": draft.NormalizedCommand,
		"target_resource_type": draft.TargetResourceType, "target_resource_id": draft.TargetResourceID, "reason": draft.Reason,
		"redacted_evidence": draft.RedactedEvidence, "expected_impact": draft.ExpectedImpact,
		"critical_resource_versions": draft.CriticalResourceVersions, "financial_snapshot": draft.FinancialSnapshot,
	}
}

func performApprovalIntegrationRequest(t *testing.T, handler http.HandlerFunc, principal identity.Principal, method, path, requestID string, body any) *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(method, path, bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer integration")
	request.Header.Set("Content-Type", "application/json")
	if requestID != "" {
		request.SetPathValue("approval_request_id", requestID)
	}
	response := httptest.NewRecorder()
	identity.Middleware(func(context.Context, string) (identity.Principal, error) { return principal, nil }, handler).ServeHTTP(response, request)
	return response
}

func seedApprovalSnapshotDraft(t *testing.T, ctx context.Context, repository *PostgresRepository, requesterID uuid.UUID, draft ApprovalDraft) uuid.UUID {
	t.Helper()
	policy, normalized, commandFingerprint, err := normalizeApprovalDraft(draft)
	if err != nil {
		t.Fatal(err)
	}
	var bindingID uuid.UUID
	var authorizationVersion int64
	if err := repository.pool.QueryRow(ctx, `SELECT id FROM role_assignments WHERE user_id=$1 AND role_code='PLATFORM_MODEL_ADMIN' AND revoked_at IS NULL ORDER BY created_at DESC LIMIT 1`, requesterID).Scan(&bindingID); err != nil {
		t.Fatal(err)
	}
	if err := repository.pool.QueryRow(ctx, `SELECT COALESCE((SELECT version FROM authorization_versions WHERE user_id=$1 AND scope='PLATFORM'),1)`, requesterID).Scan(&authorizationVersion); err != nil {
		t.Fatal(err)
	}
	id := uuid.Must(uuid.NewV7())
	now := time.Now().UTC()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO approval_requests(id,command_type,policy_version,requester_user_id,requester_binding_id,requester_authorization_version,idempotency_key,normalized_command,command_fingerprint,target_resource_type,target_resource_id,reason,redacted_evidence,expected_impact,status,version,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,'DRAFT',1,$15,$16,$16)`, id, policy.CommandType, policy.PolicyVersion, requesterID, bindingID, authorizationVersion, draft.IdempotencyKey, normalized, commandFingerprint[:], draft.TargetResourceType, draft.TargetResourceID, draft.Reason, draft.RedactedEvidence, draft.ExpectedImpact, now.Add(policy.ExpiresAfter), now); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO approval_request_snapshots(approval_request_id,snapshot_schema_version,critical_resource_versions,financial_snapshot,policy_snapshot,requester_authorization_snapshot,created_at) VALUES($1,$2,$3,$4,'{}','{}',$5)`, id, policy.SnapshotSchemaVersion, draft.CriticalResourceVersions, draft.FinancialSnapshot, now); err != nil {
		t.Fatal(err)
	}
	return id
}

func assertApprovalStatusVersion(t *testing.T, ctx context.Context, repository *PostgresRepository, id uuid.UUID, status string, version int64) {
	t.Helper()
	var actualStatus string
	var actualVersion int64
	if err := repository.pool.QueryRow(ctx, `SELECT status,version FROM approval_requests WHERE id=$1`, id).Scan(&actualStatus, &actualVersion); err != nil || actualStatus != status || actualVersion != version {
		t.Fatalf("approval=%s status=%s/%s version=%d/%d err=%v", id, actualStatus, status, actualVersion, version, err)
	}
}

func assertApprovalSnapshotKeys(t *testing.T, raw json.RawMessage, expected []string) {
	t.Helper()
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil {
		t.Fatalf("decode approval snapshot: %v", err)
	}
	if len(value) != len(expected) {
		t.Fatalf("approval snapshot keys=%v, want=%v", value, expected)
	}
	for _, key := range expected {
		if _, ok := value[key]; !ok {
			t.Fatalf("approval snapshot missing %q: %s", key, raw)
		}
	}
}

type approvalSnapshotPricingStub struct {
	modelID     uuid.UUID
	observedAt  time.Time
	fingerprint string
}

func (stub approvalSnapshotPricingStub) ImpactPriceCoverage(_ context.Context, queries []tokenservice.ImpactPriceQuery) (tokenservice.ImpactPriceCoverageResult, error) {
	formulaID, priceID, priceVersion := uuid.MustParse("00000000-0000-7000-8300-000000000001"), uuid.MustParse("00000000-0000-7000-8300-000000000002"), int64(11)
	effectiveFrom := stub.observedAt.Add(-time.Hour)
	items := make([]tokenservice.ImpactPriceCoverage, 0, len(queries))
	for _, query := range queries {
		if query.ModelID != stub.modelID || query.ProtocolCode != "CHAT" || query.CreditUnit != "MICROCREDIT" {
			return tokenservice.ImpactPriceCoverageResult{}, ErrConflict
		}
		items = append(items, tokenservice.ImpactPriceCoverage{ModelID: query.ModelID, ProtocolCode: query.ProtocolCode, FormulaVersionID: &formulaID, CreditUnit: query.CreditUnit, Covered: true, PriceVersionID: &priceID, PriceVersion: &priceVersion, EffectiveFrom: &effectiveFrom})
	}
	return tokenservice.ImpactPriceCoverageResult{Items: items, AsOf: stub.observedAt, Source: "BILLING", FinancialFactRevision: 11, PriceSetFingerprint: stub.fingerprint}, nil
}

func newApprovalSnapshotTokenRepository(t *testing.T, ctx context.Context, databaseURL string) (*tokenservice.PostgresRepository, *pgx.Conn) {
	t.Helper()
	connection, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close(context.Background()) })
	schema := "account_snapshot_token_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	identifier := pgx.Identifier{schema}.Sanitize()
	if _, err := connection.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cleanupCancel()
		_, _ = connection.Exec(cleanupCtx, `DROP SCHEMA `+identifier+` CASCADE`)
	})
	if _, err := connection.Exec(ctx, `SET search_path TO `+identifier); err != nil {
		t.Fatal(err)
	}
	loaded, err := migrations.Load("../../migrations/token")
	if err != nil {
		t.Fatal(err)
	}
	if err := migrations.Up(ctx, connection, loaded); err != nil {
		t.Fatal(err)
	}
	migrations.Unlock(ctx, connection)
	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	secrets, err := tokenservice.NewLocalAESProvider("snapshot-integration-key", []byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatal(err)
	}
	repository, err := tokenservice.NewPostgresRepository(ctx, parsed.String(), secrets)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(repository.Close)
	return repository, connection
}

func seedApprovalSnapshotTokenModel(t *testing.T, ctx context.Context, connection *pgx.Conn) uuid.UUID {
	t.Helper()
	var protocolID uuid.UUID
	if err := connection.QueryRow(ctx, `SELECT id FROM protocol_definitions WHERE protocol_code='CHAT' AND version=1 AND status='PUBLISHED'`).Scan(&protocolID); err != nil {
		t.Fatal(err)
	}
	actorID, modelID, capabilityID, catalogID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC().Add(-time.Hour)
	var catalogVersion int64
	if err := connection.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'account snapshot integration',$2,$3,$3) RETURNING version`, catalogID, actorID, now).Scan(&catalogVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(ctx, `
INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'INTEGRATION','v1','account-snapshot-model',$2,$3,$4);
INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($5,$1,1,$6,4096,1024,'["TEXT"]','integration-v1',$3,$4);
INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES($7,$1,'Account snapshot model','account snapshot model','','{}','PUBLISHED',$5,$6,true);`, modelID, strings.Repeat("a", 64), actorID, now, capabilityID, protocolID, catalogVersion); err != nil {
		t.Fatal(err)
	}
	return modelID
}
