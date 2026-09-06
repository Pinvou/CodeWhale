package token

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// APT-125-R1's named release gate runs real handlers and all domain/result/
// audit/outbox writes against PostgreSQL. Only the not-yet-installed APT-138
// activation seam and external identity authorization are fixture inputs.
func TestRouteGovernedCommandsPostgresIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	repository := newTokenQueryIntegrationRepository(t, ctx, tokenPaginationIntegrationDatabaseURL(t))
	repository.now = func() time.Time { return time.Now().UTC() }
	actor, account, owner, wallet, model, capability, credential := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	protocol := uuid.MustParse("00000000-0000-7000-8200-000000000000")
	now := time.Now().UTC().Add(-time.Minute)
	var catalogVersion int64
	if err := repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'governed route test',$2,$3,$3) RETURNING version`, uuid.New(), actor, now).Scan(&catalogVersion); err != nil {
		t.Fatal(err)
	}
	_, err := repository.pool.Exec(ctx, `
INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'PROVIDER','adapter-v1','governed-model',$2,$3,$4);
INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($5,$1,1,$6,128000,8192,'["TEXT"]','v1',$3,$4);
INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES($7,$1,'Governed Model','governed model','','{}','PUBLISHED',$5,$6,true);
INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($8,'USER',$9,$10,'Governed account','ACTIVE',$3,$4,$4,'PLATFORM');
INSERT INTO token_share_policies(token_account_id,scope,updated_at) VALUES($8,'OWNER_UNIT_ONLY',$4);
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status) VALUES($11,$8,'PROVIDER','https://provider.invalid','x'::bytea,'key','governed-fingerprint',1,'ACTIVE',ARRAY['governed-model'],$4,$3,$4,'VALID');`, pgx.QueryExecModeSimpleProtocol, model, strings.Repeat("1", 64), actor, now, capability, protocol, catalogVersion, account, owner, wallet, credential)
	if err != nil {
		t.Fatal(err)
	}
	var epoch atomic.Int64
	epoch.Store(7)
	repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(_ context.Context, _ pgx.Tx, selected TokenCommandGovernanceActivation) error {
		if selected.State != "ACTIVE" || selected.Epoch != epoch.Load() {
			return ErrTokenRouteActivationChanged
		}
		return nil
	}))
	handler := newModelAccountGovernanceHandler(t, repository, nil, actor, AdminOwnerCandidate{ID: owner, Type: "USER", Status: "ACTIVE"})
	upsert := mustMarshalJSON(t, map[string]any{"token_account_id": account, "model_id": model, "protocol_version_id": protocol, "priority": 100, "default_scope": "NONE"})
	precheck := fetchModelAccountPrecheck(t, handler, "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", account.String(), upsert)
	if len(precheck.Blockers) != 0 {
		t.Fatalf("create incorrectly requires health/READY: %+v", precheck.Blockers)
	}
	envelope := modelAccountEnvelope(t, "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", account.String(), upsert, "create governed stable route", precheck)
	// Same-key concurrent HTTP writers return one immutable result, not one
	// success followed by a stale-version error after waiting for a snapshot.
	var wg sync.WaitGroup
	responses := make(chan TokenCommandResult, 4)
	failures := make(chan string, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			response := performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", envelope, "governed-route-create-key")
			var result TokenCommandResult
			if (response.Code != 200 && response.Code != 201) || json.Unmarshal(response.Body.Bytes(), &result) != nil {
				failures <- response.Body.String()
				return
			}
			responses <- result
		}()
	}
	wg.Wait()
	close(failures)
	close(responses)
	for failure := range failures {
		t.Fatal(failure)
	}
	var first TokenCommandResult
	for result := range responses {
		if first.CommandReference == uuid.Nil {
			first = result
		} else if result.CommandReference != first.CommandReference || string(result.RedactedResult) != string(first.RedactedResult) {
			t.Fatal("concurrent replay changed result")
		}
	}
	var route StableRouteResult
	if json.Unmarshal(first.RedactedResult, &route) != nil || route.ModelID != model || route.ProtocolVersionID != protocol || route.HealthStatus != "UNKNOWN" || route.RouteVersion != 1 || route.AccountVersion != 2 {
		t.Fatalf("invalid route result: %s", first.RedactedResult)
	}
	assertGovernedRouteEffects(t, ctx, repository, 1, 0, 0)
	// Exact facts, not wall-clock observation time, determine the precheck.
	driftPrecheck := fetchModelAccountPrecheck(t, handler, "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", account.String(), upsert)
	sameFacts := fetchModelAccountPrecheck(t, handler, "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", account.String(), upsert)
	if driftPrecheck.PrecheckFingerprint != sameFacts.PrecheckFingerprint {
		t.Fatal("observation time changed route fingerprint")
	}
	staleEnvelope := modelAccountEnvelope(t, "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", account.String(), upsert, "update route with stale evidence", driftPrecheck)
	if _, err = repository.pool.Exec(ctx, `UPDATE provider_credentials SET fingerprint='same-count-replaced-evidence' WHERE id=$1`, credential); err != nil {
		t.Fatal(err)
	}
	response := performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", staleEnvelope, "governed-route-drift-key")
	if response.Code != 409 {
		t.Fatalf("equal-count evidence drift=%d %s", response.Code, response.Body.String())
	}
	assertGovernedRouteEffects(t, ctx, repository, 1, 0, 0)
	// Epoch drift after HTTP selection cannot persist even a PENDING result.
	fresh := fetchModelAccountPrecheck(t, handler, "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", account.String(), upsert)
	update := modelAccountEnvelope(t, "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", account.String(), upsert, "update route after epoch change", fresh)
	epoch.Store(8)
	response = performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", update, "governed-route-epoch-key")
	if response.Code != 503 {
		t.Fatalf("epoch drift=%d %s", response.Code, response.Body.String())
	}
	assertGovernedRouteEffects(t, ctx, repository, 1, 0, 0)
	// Roll back after domain/result/audit/outbox writes, at the final guard.
	var checks atomic.Int32
	repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(_ context.Context, _ pgx.Tx, _ TokenCommandGovernanceActivation) error {
		if checks.Add(1) > 1 {
			return ErrTokenRouteActivationChanged
		}
		return nil
	}))
	response = performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", update, "governed-route-final-guard")
	if response.Code != 503 {
		t.Fatalf("final guard=%d %s", response.Code, response.Body.String())
	}
	assertGovernedRouteEffects(t, ctx, repository, 1, 0, 0)
	epoch.Store(7)
	repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(_ context.Context, _ pgx.Tx, _ TokenCommandGovernanceActivation) error { return nil }))
	// Delete creates a verifiable tombstone; repeat POST and GET cannot infer
	// absence from the deleted route's 404 or create another domain effect.
	deletion := mustMarshalJSON(t, map[string]any{"token_account_id": account, "route_id": route.ID})
	deletePrecheck := fetchModelAccountPrecheck(t, handler, "TOKEN_ROUTE_DELETE", "TOKEN_ROUTE", route.ID.String(), deletion)
	deleteEnvelope := modelAccountEnvelope(t, "TOKEN_ROUTE_DELETE", "TOKEN_ROUTE", route.ID.String(), deletion, "delete governed stable route", deletePrecheck)
	// Drop the real HTTP connection only after the actual handler committed.
	// Recovery must discover the reference via GET, without another DELETE.
	lostResponse := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buffer := httptest.NewRecorder()
		handler.ServeHTTP(buffer, r)
		if buffer.Code != http.StatusCreated {
			t.Errorf("delete before response loss=%d %s", buffer.Code, buffer.Body.String())
		}
		connection, _, hijackErr := w.(http.Hijacker).Hijack()
		if hijackErr == nil {
			_ = connection.Close()
		}
	}))
	defer lostResponse.Close()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, lostResponse.URL+"/admin/v1/commands", bytes.NewReader(mustMarshalJSON(t, deleteEnvelope)))
	request.Header.Set("Authorization", "Bearer integration")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "governed-route-delete-key")
	if returned, transportErr := lostResponse.Client().Do(request); transportErr == nil {
		_ = returned.Body.Close()
		t.Fatal("expected transport response loss")
	}
	lookup := httptest.NewRequest(http.MethodGet, "/admin/v1/commands/by-idempotency-key?command_type=TOKEN_ROUTE_DELETE", nil)
	lookup.Header.Set("Authorization", "Bearer integration")
	lookup.Header.Set("Idempotency-Key", "governed-route-delete-key")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, lookup)
	if response.Code != 200 {
		t.Fatalf("GET after response loss=%d %s", response.Code, response.Body.String())
	}
	var deleted TokenCommandResult
	_ = json.Unmarshal(response.Body.Bytes(), &deleted)
	var tombstone stableRouteDeleteResult
	if json.Unmarshal(deleted.RedactedResult, &tombstone) != nil || !tombstone.Deleted || tombstone.DeletedRouteVersion != 1 || tombstone.RouteVersion != 2 || tombstone.AccountVersion != 3 || tombstone.DeletedAt.IsZero() {
		t.Fatalf("invalid tombstone %s", deleted.RedactedResult)
	}
	for _, read := range []struct {
		method, path, key string
		body              any
	}{{http.MethodPost, "/admin/v1/commands", "governed-route-delete-key", deleteEnvelope}, {http.MethodGet, "/admin/v1/commands/" + deleted.CommandReference.String(), "", nil}} {
		got := performModelAccountRequest(handler, read.method, read.path, read.body, read.key)
		var replay TokenCommandResult
		if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &replay) != nil || replay.CommandReference != deleted.CommandReference || string(replay.RedactedResult) != string(deleted.RedactedResult) {
			t.Fatalf("delete recovery %d %s", got.Code, got.Body.String())
		}
	}
	assertGovernedRouteEffects(t, ctx, repository, 2, 1, 0)
	// Upsert's original result stays frozen even after that route is deleted.
	response = performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", envelope, "governed-route-create-key")
	if response.Code != 200 {
		t.Fatalf("old create replay %d %s", response.Code, response.Body.String())
	}
	for _, activation := range []TokenCommandGovernanceActivation{{State: "PRE_ACTIVATION"}, {State: "SAFE_ROLLBACK", Epoch: 9}} {
		var allowed atomic.Bool
		allowed.Store(true)
		var version atomic.Int64
		version.Store(11)
		inactive := newMutableModelAccountGovernanceHandlerWithActivation(t, repository, nil, actor, owner, &version, &allowed, activation)
		got := performModelAccountRequest(inactive, http.MethodPost, "/admin/v1/commands", envelope, "governed-route-inactive")
		if got.Code != 503 {
			t.Fatalf("inactive %s %d", activation.State, got.Code)
		}
	}
	var allowed atomic.Bool
	var version atomic.Int64
	version.Store(11)
	denied := newMutableModelAccountGovernanceHandler(t, repository, nil, actor, owner, &version, &allowed)
	if got := performModelAccountRequest(denied, http.MethodPost, "/admin/v1/commands", envelope, "governed-route-denied"); got.Code != 403 {
		t.Fatalf("permission=%d %s", got.Code, got.Body.String())
	}
	otherActorHandler := newModelAccountGovernanceHandler(t, repository, nil, uuid.New(), AdminOwnerCandidate{ID: owner, Type: "USER", Status: "ACTIVE"})
	for _, scope := range []struct {
		handler http.Handler
		status  int
	}{{denied, 403}, {otherActorHandler, 404}} {
		lookup := httptest.NewRequest(http.MethodGet, "/admin/v1/commands/by-idempotency-key?command_type=TOKEN_ROUTE_DELETE", nil)
		lookup.Header.Set("Authorization", "Bearer integration")
		lookup.Header.Set("Idempotency-Key", "governed-route-delete-key")
		got := httptest.NewRecorder()
		scope.handler.ServeHTTP(got, lookup)
		if got.Code != scope.status {
			t.Fatalf("by-key actor/permission isolation=%d want=%d %s", got.Code, scope.status, got.Body.String())
		}
	}
	for _, activation := range []TokenCommandGovernanceActivation{{State: "PRE_ACTIVATION"}, {State: "ACTIVE", Epoch: 7}, {State: "SAFE_ROLLBACK", Epoch: 9}} {
		var permitted atomic.Bool
		permitted.Store(true)
		selected := newMutableModelAccountGovernanceHandlerWithActivation(t, repository, nil, actor, owner, &version, &permitted, activation)
		want := http.StatusServiceUnavailable
		if activation.State == "ACTIVE" {
			want = http.StatusGone
		}
		for _, method := range []string{http.MethodPut, http.MethodDelete} {
			got := performModelAccountRequest(selected, method, "/admin/v2/token-accounts/"+account.String()+"/routes", map[string]any{}, "governed-route-retired-key")
			if got.Code != want {
				t.Fatalf("v2 %s/%s=%d want=%d %s", activation.State, method, got.Code, want, got.Body.String())
			}
		}
	}
	assertGovernedRouteEffects(t, ctx, repository, 2, 1, 0)
	// A common actor/type/key used concurrently for two different accounts is
	// a 409 mismatch, never a generic SQL 503 or two independent mutations.
	accountB := uuid.New()
	seedGovernedRouteAccount(t, ctx, repository, accountB, actor, now)
	otherCommand := mustMarshalJSON(t, map[string]any{"token_account_id": accountB, "model_id": model, "protocol_version_id": protocol, "priority": 100, "default_scope": "NONE"})
	otherPrecheck := fetchModelAccountPrecheck(t, handler, "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", accountB.String(), otherCommand)
	originalPrecheck := fetchModelAccountPrecheck(t, handler, "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", account.String(), upsert)
	envelopes := []TokenCommandEnvelope{modelAccountEnvelope(t, "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", account.String(), upsert, "same key across different accounts", originalPrecheck), modelAccountEnvelope(t, "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", accountB.String(), otherCommand, "same key across different accounts", otherPrecheck)}
	codes := make(chan int, 2)
	bodies := make(chan string, 2)
	for _, candidate := range envelopes {
		wg.Add(1)
		go func(candidate TokenCommandEnvelope) {
			defer wg.Done()
			got := performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", candidate, "governed-route-cross-account-key")
			codes <- got.Code
			bodies <- got.Body.String()
		}(candidate)
	}
	wg.Wait()
	close(codes)
	close(bodies)
	counts := map[int]int{}
	for code := range codes {
		counts[code]++
	}
	if counts[201] != 1 || counts[409] != 1 {
		for body := range bodies {
			t.Log(body)
		}
		t.Fatalf("cross-account same key codes=%v", counts)
	}
	assertGovernedRouteEffects(t, ctx, repository, 3, 1, 0)
	// The read and write select the same legacy unresolved row. Its health
	// facts must participate even though stable identifiers are still NULL.
	legacyAccount, legacyRoute := uuid.New(), uuid.New()
	seedGovernedRouteAccount(t, ctx, repository, legacyAccount, actor, now)
	if _, err = repository.pool.Exec(ctx, `INSERT INTO routing_policies(id,token_account_id,model,protocol_code,priority,default_scope,health_status,updated_at,updated_by,route_generation,reference_status,route_version,health_version) VALUES($1,$2,'governed-model','CHAT',100,'NONE','UNKNOWN',$3,$4,1,'LEGACY_UNRESOLVED',1,1)`, legacyRoute, legacyAccount, now, actor); err != nil {
		t.Fatal(err)
	}
	legacyCommand := mustMarshalJSON(t, map[string]any{"token_account_id": legacyAccount, "model_id": model, "protocol_version_id": protocol, "priority": 100, "default_scope": "NONE"})
	legacyPrecheck := fetchModelAccountPrecheck(t, handler, "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", legacyAccount.String(), legacyCommand)
	var legacyExpected map[string]any
	_ = json.Unmarshal(legacyPrecheck.ExpectedVersions, &legacyExpected)
	if legacyExpected["expected_route_version"] != float64(1) {
		t.Fatalf("legacy incorrectly previewed CREATE: %s", legacyPrecheck.ExpectedVersions)
	}
	legacyEnvelope := modelAccountEnvelope(t, "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", legacyAccount.String(), legacyCommand, "resolve legacy route after review", legacyPrecheck)
	if _, err = repository.pool.Exec(ctx, `UPDATE routing_policies SET health_version=health_version+1,health_status='UNHEALTHY' WHERE id=$1`, legacyRoute); err != nil {
		t.Fatal(err)
	}
	if got := performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", legacyEnvelope, "governed-route-legacy-stale"); got.Code != 409 {
		t.Fatalf("legacy health drift %d %s", got.Code, got.Body.String())
	}
	resolved := executeModelAccountHTTPCommand(t, handler, "TOKEN_ROUTE_UPSERT", "TOKEN_ROUTE", legacyAccount.String(), legacyCommand, "resolve the reviewed legacy route", "governed-route-legacy-fresh")
	var resolvedRoute StableRouteResult
	_ = json.Unmarshal(resolved.RedactedResult, &resolvedRoute)
	if resolvedRoute.ID != legacyRoute || resolvedRoute.RouteVersion != 2 || resolvedRoute.AccountVersion != 2 {
		t.Fatalf("legacy resolution selected wrong row: %s", resolved.RedactedResult)
	}
	assertGovernedRouteEffects(t, ctx, repository, 4, 1, 0)
	// A healthy READY route with a live lease and inflight request may not be
	// removed. All blockers come from real stored evidence, not admin input.
	provisioning, requestID := uuid.New(), uuid.New()
	_, err = repository.pool.Exec(ctx, `UPDATE routing_policies SET health_status='HEALTHY',health_observed_at=transaction_timestamp(),health_expires_at=transaction_timestamp()+interval '4 minutes',health_version=health_version+1 WHERE id=$1;
INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,protocol_version_id,desired_generation,actual_generation,desired_signature,status,stage,policy_version,policy_snapshot,max_attempts,sla_deadline_at,lease_owner,lease_until,active_token_account_id,active_credential_id,requested_by,created_at,updated_at)
SELECT $2,'PERSONAL',account.owner_id,account.wallet_id,$3,'governed-model','CHAT',$4,2,2,$5,'READY','READY',1,'{}',3,transaction_timestamp()+interval '1 hour','worker',transaction_timestamp()+interval '1 minute',account.id,credential.id,$6,transaction_timestamp(),transaction_timestamp() FROM token_accounts account JOIN provider_credentials credential ON credential.token_account_id=account.id WHERE account.id=$7;
INSERT INTO usage_requests(id,user_id,session_id,token_account_id,credential_id,wallet_id,model,model_id,catalog_version,model_capability_version_id,protocol_definition_id,route_generation,model_snapshot,status,execution_status,execution_version,delivery_status,maximum_microcredits,request_snapshot,started_at,updated_at,idempotency_key_digest,request_fingerprint)
SELECT $8,account.owner_id,'governed-route-inflight',account.id,credential.id,account.wallet_id,'governed-model',$3,$9,$10,$4,2,'{}','UPSTREAM_ACTIVE','STREAMING',1,'DELIVERING',100,'{}',transaction_timestamp(),transaction_timestamp(),$5,$5 FROM token_accounts account JOIN provider_credentials credential ON credential.token_account_id=account.id WHERE account.id=$7;`, pgx.QueryExecModeSimpleProtocol, legacyRoute, provisioning, model, protocol, strings.Repeat("7", 64), actor, legacyAccount, requestID, catalogVersion, capability)
	if err != nil {
		t.Fatal(err)
	}
	blockedCommand := mustMarshalJSON(t, map[string]any{"token_account_id": legacyAccount, "route_id": legacyRoute})
	blockedPrecheck := fetchModelAccountPrecheck(t, handler, "TOKEN_ROUTE_DELETE", "TOKEN_ROUTE", legacyRoute.String(), blockedCommand)
	for _, blocker := range []string{"READY_PROVISIONING_DEPENDS_ON_ROUTE", "READY_COVERAGE_WOULD_BE_LOST", "INFLIGHT_REQUESTS_PRESENT", "ACTIVE_LEASES_PRESENT"} {
		if !containsString(blockedPrecheck.Blockers, blocker) {
			t.Fatalf("missing %s in %+v", blocker, blockedPrecheck.Blockers)
		}
	}
	blockedEnvelope := modelAccountEnvelope(t, "TOKEN_ROUTE_DELETE", "TOKEN_ROUTE", legacyRoute.String(), blockedCommand, "must not delete active production route", blockedPrecheck)
	if got := performModelAccountRequest(handler, http.MethodPost, "/admin/v1/commands", blockedEnvelope, "governed-route-blocked-delete"); got.Code != 409 {
		t.Fatalf("dependency blocker %d %s", got.Code, got.Body.String())
	}
	assertGovernedRouteEffects(t, ctx, repository, 4, 1, 0)
}

func seedGovernedRouteAccount(t *testing.T, ctx context.Context, repository *PostgresRepository, account, actor uuid.UUID, now time.Time) {
	t.Helper()
	_, err := repository.pool.Exec(ctx, `INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'Additional governed account','ACTIVE',$4,$5,$5,'PLATFORM');
INSERT INTO token_share_policies(token_account_id,scope,updated_at) VALUES($1,'OWNER_UNIT_ONLY',$5);
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status) VALUES($6,$1,'PROVIDER','https://provider.invalid','x'::bytea,'key','additional-fingerprint',1,'ACTIVE',ARRAY['governed-model'],$5,$4,$5,'VALID');`, pgx.QueryExecModeSimpleProtocol, account, uuid.New(), uuid.New(), actor, now, uuid.New())
	if err != nil {
		t.Fatal(err)
	}
}

func assertGovernedRouteEffects(t *testing.T, ctx context.Context, repository *PostgresRepository, commands, tombstones, legacy int) {
	t.Helper()
	for _, check := range []struct {
		sql  string
		want int
	}{{`SELECT count(*) FROM token_command_results`, commands}, {`SELECT count(*) FROM token_audit_logs WHERE action IN ('TOKEN_ROUTE_UPSERT','TOKEN_ROUTE_DELETE')`, commands}, {`SELECT count(*) FROM outbox_events WHERE event_type='token.management-command.executed.v1'`, commands}, {`SELECT count(*) FROM routing_policy_tombstones`, tombstones}, {`SELECT count(*) FROM routing_policy_mutations`, legacy}} {
		var count int
		if err := repository.pool.QueryRow(ctx, check.sql).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != check.want {
			t.Fatalf("%s got %d want %d", check.sql, count, check.want)
		}
	}
}
