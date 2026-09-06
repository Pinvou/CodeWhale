package token

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/httpserver"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestRuntimeGovernedCommandsPostgresIntegration is a release gate for the
// APT-130-R1 adapter. CI provides a real Token database and rejects SKIP.
func TestRuntimeGovernedCommandsPostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	activeGuard := tokenRouteActivationGuardFunc(func(context.Context, pgx.Tx, TokenCommandGovernanceActivation) error { return nil })
	repository.WithTokenRouteActivationTransactionGuard(activeGuard)
	actorID, otherActorID := uuid.New(), uuid.New()
	authorizationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var input struct {
			Subject string `json:"subject"`
		}
		_ = json.NewDecoder(request.Body).Decode(&input)
		resolvedActor := actorID
		if input.Subject == "other" {
			resolvedActor = otherActorID
		}
		w.Header().Set("X-Authorization-Version", "11")
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "user_id": resolvedActor, "granted_permissions": []string{"platform.tokens.manage", "platform.usage.reconcile.retry"}})
	}))
	t.Cleanup(authorizationServer.Close)
	authorizer, err := authorization.NewClient(authorizationServer.URL, "runtime-governance-integration")
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(repository, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).
		WithAuthorizer(authorizer).
		WithImpactPrechecks(true, nil, nil).
		WithTokenCommandGovernanceActivation(testTokenGovernanceActivationReader{activation: TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 1}})
	handler := httpserver.CorrelationHandler(service.Handler("", func(_ context.Context, token string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.integration.example", Subject: token, SessionID: "runtime-session-" + token, AuthenticatedAt: time.Now().UTC()}, nil
	}))

	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	modelID, accountID, credentialID, ownerID, walletID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	provisioningID, concurrentProvisioningID, canonicalProvisioningID, usageID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	digest := strings.Repeat("a", 64)
	if _, err := repository.pool.Exec(ctx, `
INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'runtime','v1','runtime-model',$2,$3,$4);
INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($5,'USER',$6,$7,'Runtime account','ACTIVE',$3,$4,$4,'PLATFORM');
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status) VALUES($8,$5,'runtime','https://provider.invalid','x'::bytea,'runtime-key','runtime-fingerprint',1,'ACTIVE',ARRAY['runtime-model'],$4,$3,$4,'VALID');
INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,desired_generation,desired_signature,status,stage,policy_version,policy_snapshot,max_attempts,sla_deadline_at,active_token_account_id,active_credential_id,requested_by,created_at,updated_at) VALUES
($9,'PERSONAL',$6,$7,$1,'runtime-model','CHAT',3,$2,'FAILED','TEST_FAILED',1,'{}',3,$4::timestamptz+interval '1 hour',$5,$8,$3,$4,$4),
($10,'PERSONAL',$6,$7,$1,'runtime-model','CHAT',4,$2,'REPAIR_REQUIRED','TEST_REPAIR',1,'{}',3,$4::timestamptz+interval '1 hour',$5,$8,$3,$4,$4),
($11,'PERSONAL',$6,$7,$1,'runtime-model','CHAT',5,$2,'FAILED','TEST_CANONICAL',1,'{}',3,$4::timestamptz+interval '1 hour',$5,$8,$3,$4,$4);
INSERT INTO usage_requests(id,user_id,session_id,token_account_id,credential_id,wallet_id,model,status,maximum_microcredits,request_snapshot,started_at,updated_at,idempotency_key_digest,request_fingerprint,last_persisted_sequence) VALUES($12,$6,'runtime-session',$5,$8,$7,'runtime-model','REVIEW',100,'{}',$4,$4,$2,$2,9);`, pgx.QueryExecModeSimpleProtocol, modelID, digest, actorID, now, accountID, ownerID, walletID, credentialID, provisioningID, concurrentProvisioningID, canonicalProvisioningID, usageID); err != nil {
		t.Fatal(err)
	}

	provisioningCommand := mustMarshalJSON(t, map[string]any{"provisioning_id": provisioningID, "expected_provisioning_version": now.Format(time.RFC3339Nano), "target_generation": int64(3), "replay_model_request": false})
	provisioningPrecheck := runtimePrecheck(t, service, "TOKEN_ROUTE_PROVISIONING_REPAIR", "PROVISIONING", provisioningID, provisioningCommand)
	provisioningEnvelope := runtimeEnvelope(t, "TOKEN_ROUTE_PROVISIONING_REPAIR", "PROVISIONING", provisioningID, provisioningCommand, provisioningPrecheck)
	assertActivationRejects := func(envelope TokenCommandEnvelope) {
		t.Helper()
		const facts = `SELECT (SELECT count(*) FROM token_command_results)+(SELECT count(*) FROM token_audit_logs)+(SELECT count(*) FROM outbox_events)+(SELECT count(*) FROM route_provisioning_commands)+(SELECT count(*) FROM route_provisioning_audit)+(SELECT count(*) FROM usage_reconciliation_commands)`
		var before int64
		if err := repository.pool.QueryRow(ctx, facts).Scan(&before); err != nil {
			t.Fatal(err)
		}
		for _, rejectCall := range []int{1, 2} {
			calls := 0
			repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(context.Context, pgx.Tx, TokenCommandGovernanceActivation) error {
				calls++
				if calls == rejectCall {
					return ErrTokenRouteActivationChanged
				}
				return nil
			}))
			response := runtimeCommandRequest(t, handler, envelope, "runtime-epoch-rejected-command", "actor")
			var after int64
			if err := repository.pool.QueryRow(ctx, facts).Scan(&after); err != nil || response.Code != http.StatusServiceUnavailable || calls != rejectCall || after != before {
				t.Fatalf("activation %s at call %d: status=%d calls=%d facts=%d→%d err=%v body=%s", envelope.CommandType, rejectCall, response.Code, calls, before, after, err, response.Body.String())
			}
		}
		repository.WithTokenRouteActivationTransactionGuard(activeGuard)
	}
	assertActivationRejects(provisioningEnvelope)
	invalidEnvelopes := []TokenCommandEnvelope{provisioningEnvelope, provisioningEnvelope, provisioningEnvelope, provisioningEnvelope, provisioningEnvelope}
	invalidEnvelopes[0].Command = mustMarshalJSON(t, map[string]any{"provisioning_id": provisioningID, "expected_provisioning_version": now.Format(time.RFC3339Nano), "target_generation": int64(3), "replay_model_request": true})
	invalidEnvelopes[1].Command = mustMarshalJSON(t, map[string]any{"provisioning_id": provisioningID, "expected_provisioning_version": now.Format(time.RFC3339Nano), "target_generation": int64(3), "replay_model_request": false, "unexpected": "must-not-pass"})
	invalidEnvelopes[2].TargetResourceID = uuid.NewString()
	invalidEnvelopes[3].TargetResourceType = "USAGE_REQUEST"
	invalidEnvelopes[4].Command = mustMarshalJSON(t, map[string]any{"provisioning_id": provisioningID, "expected_provisioning_version": now.Format(time.RFC3339Nano), "target_generation": int64(3)})
	for index, invalid := range invalidEnvelopes {
		response := runtimeCommandRequest(t, handler, invalid, "runtime-invalid-command-000"+string(rune('1'+index)), "actor")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INVALID_COMMAND") {
			t.Fatalf("invalid command %d status=%d body=%s", index, response.Code, response.Body.String())
		}
	}
	var invalidPersisted int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_command_results WHERE command_type='TOKEN_ROUTE_PROVISIONING_REPAIR'`).Scan(&invalidPersisted); err != nil || invalidPersisted != 0 {
		t.Fatalf("invalid commands persisted=%d err=%v", invalidPersisted, err)
	}
	first := runtimeCommandRequest(t, handler, provisioningEnvelope, "runtime-provisioning-command-0001", "actor")
	if first.Code != http.StatusOK {
		t.Fatalf("provisioning status=%d body=%s", first.Code, first.Body.String())
	}
	firstResult := decodeRuntimeCommandResult(t, first)
	if firstResult.Status != "SUCCEEDED" || firstResult.CommandType != "TOKEN_ROUTE_PROVISIONING_REPAIR" || !strings.Contains(string(firstResult.RedactedResult), `"replay_model_request":false`) {
		t.Fatalf("unexpected provisioning result: %+v", firstResult)
	}
	recovered := runtimeCommandResultRequest(t, handler, firstResult.CommandReference, "actor")
	if recovered.Code != http.StatusOK {
		t.Fatalf("result recovery=%d %s", recovered.Code, recovered.Body.String())
	}
	recoveredResult := decodeRuntimeCommandResult(t, recovered)
	if recoveredResult.CommandReference != firstResult.CommandReference || recoveredResult.Status != "SUCCEEDED" || recoveredResult.AuditRequestID == "" {
		t.Fatalf("unexpected recovered result: %+v", recoveredResult)
	}
	otherRecovery := runtimeCommandResultRequest(t, handler, firstResult.CommandReference, "other")
	if otherRecovery.Code != http.StatusNotFound {
		t.Fatalf("cross-actor result recovery=%d %s", otherRecovery.Code, otherRecovery.Body.String())
	}
	assertRuntimeCommandFacts(t, ctx, repository, provisioningID, actorID, 1, 1, 1)
	replay := runtimeCommandRequest(t, handler, provisioningEnvelope, "runtime-provisioning-command-0001", "actor")
	if replay.Code != http.StatusOK || decodeRuntimeCommandResult(t, replay).CommandReference != firstResult.CommandReference {
		t.Fatalf("provisioning replay=%d %s", replay.Code, replay.Body.String())
	}
	assertRuntimeCommandFacts(t, ctx, repository, provisioningID, actorID, 1, 1, 1)
	otherActor := runtimeCommandRequest(t, handler, provisioningEnvelope, "runtime-provisioning-command-0001", "other")
	if otherActor.Code != http.StatusConflict || !strings.Contains(otherActor.Body.String(), "COMMAND_PRECHECK_CHANGED") {
		t.Fatalf("cross-actor recovery=%d %s", otherActor.Code, otherActor.Body.String())
	}
	var otherActorCommands int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_command_results WHERE actor_user_id=$1`, otherActorID).Scan(&otherActorCommands); err != nil || otherActorCommands != 0 {
		t.Fatalf("cross-actor command persistence=%d err=%v", otherActorCommands, err)
	}

	concurrentCommand := mustMarshalJSON(t, map[string]any{"provisioning_id": concurrentProvisioningID, "expected_provisioning_version": now.Format(time.RFC3339Nano), "target_generation": int64(4), "replay_model_request": false})
	concurrentPrecheck := runtimePrecheck(t, service, "TOKEN_ROUTE_PROVISIONING_REPAIR", "PROVISIONING", concurrentProvisioningID, concurrentCommand)
	concurrentEnvelope := runtimeEnvelope(t, "TOKEN_ROUTE_PROVISIONING_REPAIR", "PROVISIONING", concurrentProvisioningID, concurrentCommand, concurrentPrecheck)
	responses := make([]*httptest.ResponseRecorder, 2)
	var wait sync.WaitGroup
	for index := range responses {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			responses[index] = runtimeCommandRequest(t, handler, concurrentEnvelope, "runtime-provisioning-command-0002", "actor")
		}(index)
	}
	wait.Wait()
	for _, response := range responses {
		if response.Code != http.StatusOK {
			t.Fatalf("concurrent response=%d %s", response.Code, response.Body.String())
		}
	}
	if decodeRuntimeCommandResult(t, responses[0]).CommandReference != decodeRuntimeCommandResult(t, responses[1]).CommandReference {
		t.Fatal("concurrent same-actor idempotency did not recover one command")
	}
	assertRuntimeCommandFacts(t, ctx, repository, concurrentProvisioningID, actorID, 1, 1, 1)

	canonicalCommand := mustMarshalJSON(t, map[string]any{"provisioning_id": canonicalProvisioningID, "expected_provisioning_version": now.Format(time.RFC3339Nano), "target_generation": int64(5), "replay_model_request": false})
	canonicalPrecheck := runtimePrecheck(t, service, "TOKEN_ROUTE_PROVISIONING_REPAIR", "PROVISIONING", canonicalProvisioningID, canonicalCommand)
	canonicalEnvelope := runtimeEnvelope(t, "TOKEN_ROUTE_PROVISIONING_REPAIR", "PROVISIONING", canonicalProvisioningID, canonicalCommand, canonicalPrecheck)
	canonicalEnvelope.CommandType = "  token_route_provisioning_repair  "
	canonicalReason := "repair the frozen provisioning generation after review"
	canonicalEnvelope.Reason = strings.Repeat(" ", 2100) + canonicalReason + strings.Repeat(" ", 2100)
	canonicalResponse := runtimeCommandRequest(t, handler, canonicalEnvelope, "runtime-canonical-command-0001", "actor")
	if canonicalResponse.Code != http.StatusOK {
		t.Fatalf("canonical response=%d %s", canonicalResponse.Code, canonicalResponse.Body.String())
	}
	canonicalResult := decodeRuntimeCommandResult(t, canonicalResponse)
	if canonicalResult.CommandType != "TOKEN_ROUTE_PROVISIONING_REPAIR" || canonicalResult.Status != "SUCCEEDED" {
		t.Fatalf("canonical result=%+v", canonicalResult)
	}
	var genericReason, domainReason string
	if err := repository.pool.QueryRow(ctx, `SELECT result.reason,audit.reason FROM token_command_results result JOIN route_provisioning_audit audit ON audit.provisioning_id=$1 AND audit.action='GOVERNED_REPAIR_REQUESTED' WHERE result.command_reference=$2`, canonicalProvisioningID, canonicalResult.CommandReference).Scan(&genericReason, &domainReason); err != nil || genericReason != canonicalReason || domainReason != canonicalReason {
		t.Fatalf("canonical reasons generic=%q domain=%q err=%v", genericReason, domainReason, err)
	}

	usageCommand := mustMarshalJSON(t, map[string]any{"usage_request_id": usageID, "expected_usage_version": now.Format(time.RFC3339Nano), "evidence_high_watermark": int64(9), "replay_model_request": false})
	usagePrecheck := runtimePrecheck(t, service, "TOKEN_USAGE_RECONCILIATION_RETRY", "USAGE_REQUEST", usageID, usageCommand)
	usageEnvelope := runtimeEnvelope(t, "TOKEN_USAGE_RECONCILIATION_RETRY", "USAGE_REQUEST", usageID, usageCommand, usagePrecheck)
	assertActivationRejects(usageEnvelope)
	usageResponse := runtimeCommandRequest(t, handler, usageEnvelope, "runtime-reconciliation-command-01", "actor")
	if usageResponse.Code != http.StatusOK {
		t.Fatalf("usage status=%d body=%s", usageResponse.Code, usageResponse.Body.String())
	}
	usageResult := decodeRuntimeCommandResult(t, usageResponse)
	if usageResult.Status != "SUCCEEDED" || !strings.Contains(string(usageResult.RedactedResult), `"status":"QUEUED"`) || !strings.Contains(string(usageResult.RedactedResult), `"replay_model_request":false`) {
		t.Fatalf("unexpected usage result: %+v", usageResult)
	}
	var commands, audits, attempts int
	if err := repository.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM usage_reconciliation_commands WHERE usage_request_id=$1),(SELECT count(*) FROM token_audit_logs WHERE resource_type='USAGE_REQUEST' AND resource_id=$1 AND action='USAGE_RECONCILIATION_GOVERNED'),(SELECT count(*) FROM usage_attempts WHERE usage_request_id=$1)`, usageID).Scan(&commands, &audits, &attempts); err != nil || commands != 1 || audits != 1 || attempts != 0 {
		t.Fatalf("usage commands=%d audits=%d attempts=%d err=%v", commands, audits, attempts, err)
	}

	// Freeze, then change an equal-shape evidence fact. The handler must reject
	// before token_command_results persistence and never schedule a model call.
	staleID := uuid.New()
	if _, err := repository.pool.Exec(ctx, `INSERT INTO usage_requests(id,user_id,session_id,token_account_id,credential_id,wallet_id,model,status,maximum_microcredits,request_snapshot,started_at,updated_at,idempotency_key_digest,request_fingerprint,last_persisted_sequence) VALUES($1,$2,'stale-session',$3,$4,$5,'runtime-model','REVIEW',100,'{}',$6,$6,$7,$7,2)`, staleID, ownerID, accountID, credentialID, walletID, now, strings.Repeat("b", 64)); err != nil {
		t.Fatal(err)
	}
	staleCommand := mustMarshalJSON(t, map[string]any{"usage_request_id": staleID, "expected_usage_version": now.Format(time.RFC3339Nano), "evidence_high_watermark": int64(2), "replay_model_request": false})
	stalePrecheck := runtimePrecheck(t, service, "TOKEN_USAGE_RECONCILIATION_RETRY", "USAGE_REQUEST", staleID, staleCommand)
	staleEnvelope := runtimeEnvelope(t, "TOKEN_USAGE_RECONCILIATION_RETRY", "USAGE_REQUEST", staleID, staleCommand, stalePrecheck)
	driftTx, err := repository.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := driftTx.Exec(ctx, `UPDATE usage_requests SET last_persisted_sequence=3,updated_at=transaction_timestamp() WHERE id=$1`, staleID); err != nil {
		_ = driftTx.Rollback(ctx)
		t.Fatal(err)
	}
	staleResponses := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		staleResponses <- runtimeCommandRequest(t, handler, staleEnvelope, "runtime-reconciliation-command-02", "actor")
	}()
	waitForRuntimeUsageLock(t, ctx, repository)
	if err := driftTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	staleResponse := <-staleResponses
	if staleResponse.Code != http.StatusConflict || !strings.Contains(staleResponse.Body.String(), "COMMAND_PRECHECK_CHANGED") {
		t.Fatalf("stale response=%d %s", staleResponse.Code, staleResponse.Body.String())
	}
	var staleCommands, staleDomainCommands, staleAudits, staleAttempts int
	if err := repository.pool.QueryRow(ctx, `SELECT
  (SELECT count(*) FROM token_command_results WHERE target_resource_id=$1),
  (SELECT count(*) FROM usage_reconciliation_commands WHERE usage_request_id=$2),
  (SELECT count(*) FROM token_audit_logs WHERE resource_type='USAGE_REQUEST' AND resource_id=$2 AND action='USAGE_RECONCILIATION_GOVERNED'),
  (SELECT count(*) FROM usage_attempts WHERE usage_request_id=$2)`, staleID.String(), staleID).Scan(&staleCommands, &staleDomainCommands, &staleAudits, &staleAttempts); err != nil || staleCommands != 0 || staleDomainCommands != 0 || staleAudits != 0 || staleAttempts != 0 {
		t.Fatalf("stale generic=%d domain=%d audits=%d attempts=%d err=%v", staleCommands, staleDomainCommands, staleAudits, staleAttempts, err)
	}
	var pending int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_command_results WHERE command_type IN ('TOKEN_ROUTE_PROVISIONING_REPAIR','TOKEN_USAGE_RECONCILIATION_RETRY') AND status='PENDING'`).Scan(&pending); err != nil || pending != 0 {
		t.Fatalf("runtime generic PENDING rows=%d err=%v", pending, err)
	}
}

func waitForRuntimeUsageLock(t *testing.T, ctx context.Context, repository *PostgresRepository) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting bool
		if err := repository.pool.QueryRow(ctx, `SELECT EXISTS(
  SELECT 1 FROM pg_stat_activity
  WHERE pid<>pg_backend_pid()
    AND wait_event_type='Lock'
    AND query LIKE '%SELECT id FROM usage_requests WHERE id=$1 FOR UPDATE%'
)`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("runtime command did not reach the usage resource lock")
}

func runtimePrecheck(t *testing.T, service *Service, commandType, resourceType string, id uuid.UUID, command json.RawMessage) TokenCommandPrecheckResult {
	t.Helper()
	result, err := service.buildTokenCommandPrecheck(context.Background(), TokenCommandPrecheckRequest{CommandType: commandType, TargetResourceType: resourceType, TargetResourceID: id.String(), Command: command})
	if err != nil || len(result.Blockers) != 0 {
		t.Fatalf("precheck type=%s result=%+v err=%v", commandType, result, err)
	}
	return result
}

func runtimeEnvelope(t *testing.T, commandType, resourceType string, id uuid.UUID, command json.RawMessage, precheck TokenCommandPrecheckResult) TokenCommandEnvelope {
	t.Helper()
	versions := cloneExpectedVersions(t, precheck.ExpectedVersions)
	versions["precheck_fingerprint"] = precheck.PrecheckFingerprint
	versions["request_fingerprint"] = strings.Repeat("c", 64)
	return TokenCommandEnvelope{CommandType: commandType, Reason: "perform only the reviewed runtime recovery action", TargetResourceType: resourceType, TargetResourceID: id.String(), ExpectedVersions: mustMarshalJSON(t, versions), PrecheckFingerprint: precheck.PrecheckFingerprint, Command: command}
}

func runtimeCommandRequest(t *testing.T, handler http.Handler, envelope TokenCommandEnvelope, key, actor string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/admin/v1/commands", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+actor)
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("X-Pinvou-Actor", actor)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func runtimeCommandResultRequest(t *testing.T, handler http.Handler, reference uuid.UUID, actor string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/admin/v1/commands/"+reference.String(), nil)
	request.Header.Set("Authorization", "Bearer "+actor)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func decodeRuntimeCommandResult(t *testing.T, response *httptest.ResponseRecorder) TokenCommandResult {
	t.Helper()
	var result TokenCommandResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func assertRuntimeCommandFacts(t *testing.T, ctx context.Context, repository *PostgresRepository, provisioningID, actorID uuid.UUID, wantRound, wantDomainCommands, wantAudits int) {
	t.Helper()
	var round, domainCommands, audits int
	if err := repository.pool.QueryRow(ctx, `SELECT p.repair_round,(SELECT count(*) FROM route_provisioning_commands WHERE provisioning_id=p.id AND actor_id=$2 AND command_type='PROVISIONING_REPAIR_GOVERNED'),(SELECT count(*) FROM route_provisioning_audit WHERE provisioning_id=p.id AND actor_id=$2 AND action='GOVERNED_REPAIR_REQUESTED') FROM route_provisionings p WHERE p.id=$1`, provisioningID, actorID).Scan(&round, &domainCommands, &audits); err != nil || round != wantRound || domainCommands != wantDomainCommands || audits != wantAudits {
		t.Fatalf("round=%d commands=%d audits=%d err=%v", round, domainCommands, audits, err)
	}
}
