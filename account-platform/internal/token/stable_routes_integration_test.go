package token

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/migrations"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestStableRoutesPostgresIntegration is the release gate for APT-125 stable
// route CRUD, idempotent replay, legacy migration and rollback compatibility.
func TestStableRoutesPostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	t.Run("stable CRUD and frozen replay", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
		repository.now = func() time.Time { return time.Now().UTC() }
		now := time.Now().UTC().Add(-time.Minute)
		actorA, actorB, ownerID, walletID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		accountID, credentialID, modelID, capabilityV1, catalogID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
		protocolV1 := uuid.MustParse("00000000-0000-7000-8200-000000000000")
		var catalogVersion int64
		if err := repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'stable route integration',$2,$3,$3) RETURNING version`, catalogID, actorA, now).Scan(&catalogVersion); err != nil {
			t.Fatal(err)
		}
		if _, err := repository.pool.Exec(ctx, `
INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'PROVIDER','adapter-v1','stable-model',$2,$3,$4);
INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($5,$1,1,$6,128000,8192,'["TEXT"]','v1',$3,$4);
INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES($7,$1,'Stable Model','stable model','','{}','PUBLISHED',$5,$6,true);
INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($8,'USER',$9,$10,'Stable account','ACTIVE',$3,$4,$4,'PLATFORM');
INSERT INTO token_share_policies(token_account_id,scope,updated_at) VALUES($8,'OWNER_UNIT_ONLY',$4);
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status) VALUES($11,$8,'PROVIDER','https://provider.invalid','x'::bytea,'key','stable-fingerprint',1,'ACTIVE',ARRAY['stable-model'],$4,$3,$4,'VALID');`, modelID, strings.Repeat("1", 64), actorA, now, capabilityV1, protocolV1, catalogVersion, accountID, ownerID, walletID, credentialID); err != nil {
			t.Fatal(err)
		}

		input := StableRouteInput{ModelID: modelID, ProtocolVersionID: protocolV1, Priority: 100, DefaultScope: "NONE", ExpectedAccountVersion: 1, ExpectedRouteVersion: 0, IdempotencyKey: "stable-route-key-0001", Reason: "create stable route for integration"}
		var wg sync.WaitGroup
		type actorResult struct {
			actor  uuid.UUID
			result StableRouteResult
		}
		results := make(chan actorResult, 2)
		errorsFound := make(chan error, 2)
		for _, actor := range []uuid.UUID{actorA, actorB} {
			wg.Add(1)
			go func(actor uuid.UUID) {
				defer wg.Done()
				result, err := repository.UpsertStableRoute(ctx, accountID, input, actor)
				if err != nil {
					errorsFound <- err
					return
				}
				results <- actorResult{actor: actor, result: result}
			}(actor)
		}
		wg.Wait()
		close(results)
		close(errorsFound)
		if len(results) != 1 || len(errorsFound) != 1 {
			t.Fatalf("dual actor result count=%d error count=%d", len(results), len(errorsFound))
		}
		var first StableRouteResult
		var winningActor uuid.UUID
		for result := range results {
			first, winningActor = result.result, result.actor
		}
		for err := range errorsFound {
			if !errors.Is(err, ErrIdempotencyConflict) {
				t.Fatalf("dual actor error=%v", err)
			}
		}
		if first.AccountVersion != 2 || first.RouteVersion != 1 || first.HealthStatus != "UNKNOWN" || first.ModelID != modelID || first.ProtocolVersionID != protocolV1 {
			t.Fatalf("unexpected stable result: %#v", first)
		}
		replayed, err := repository.UpsertStableRoute(ctx, accountID, input, winningActor)
		if err != nil || !reflect.DeepEqual(replayed, first) {
			t.Fatalf("frozen replay=%#v err=%v", replayed, err)
		}
		provisioner := &PostgresDefaultRouteProvisioner{
			repository: repository,
			config: DefaultRouteConfig{
				Provider:       "PROVIDER",
				AdapterVersion: "adapter-v1",
				BaseURL:        "https://provider.invalid",
				Secret:         []byte("integration-secret"),
				Models:         []string{"stable-model"},
				Priority:       100,
			},
			workerID: "stable-route-integration",
		}
		modelV1 := CatalogModel{ModelID: modelID, ProtocolCode: "CHAT", ProtocolVersion: 1, ProtocolDefinitionID: protocolV1, ModelCapabilityVersionID: capabilityV1, LifecycleStatus: "PUBLISHED", SourceAvailable: true, ProviderID: "PROVIDER", AdapterVersion: "adapter-v1", UpstreamModelRef: "stable-model"}
		actorC, pendingWallet, failedWallet, repairWallet := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		pendingV1, err := provisioner.Ensure(ctx, AuthorizationContext{UserID: actorA, ContextType: "PERSONAL"}, pendingWallet, modelV1)
		if err != nil || pendingV1.Status != "PENDING" || pendingV1.ProtocolVersionID == nil || *pendingV1.ProtocolVersionID != protocolV1 {
			t.Fatalf("protocol v1 pending provisioning=%#v err=%v", pendingV1, err)
		}
		failedV1, err := provisioner.Ensure(ctx, AuthorizationContext{UserID: actorB, ContextType: "PERSONAL"}, failedWallet, modelV1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = repository.pool.Exec(ctx, `UPDATE route_provisionings SET status='FAILED',stage='TEST_FAILED',next_retry_at=$2 WHERE id=$1`, failedV1.ID, now); err != nil {
			t.Fatal(err)
		}
		repairV1, err := provisioner.Ensure(ctx, AuthorizationContext{UserID: actorC, ContextType: "PERSONAL"}, repairWallet, modelV1)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = repository.pool.Exec(ctx, `UPDATE route_provisionings SET status='REPAIR_REQUIRED',stage='TEST_REPAIR_REQUIRED',protocol_version_id=NULL,next_retry_at=NULL WHERE id=$1`, repairV1.ID); err != nil {
			t.Fatal(err)
		}

		protocolV2, capabilityV2, catalogIDV2 := uuid.New(), uuid.New(), uuid.New()
		if _, err = repository.pool.Exec(ctx, `
INSERT INTO protocol_definitions(id,protocol_code,version,status,request_schema,created_at,published_at) VALUES($1,'CHAT',2,'PUBLISHED','{}',$4,$4);
INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($2,$3,2,$1,128000,8192,'["TEXT"]','v2',$5,$4);
INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($6,'protocol v2',$5,$4,$4);
INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES((SELECT max(version) FROM model_catalog_versions),$3,'Stable Model','stable model','','{}','PUBLISHED',$2,$1,true);`, protocolV2, capabilityV2, modelID, now, actorA, catalogIDV2); err != nil {
			t.Fatal(err)
		}
		modelV2 := modelV1
		modelV2.ProtocolVersion, modelV2.ProtocolDefinitionID, modelV2.ModelCapabilityVersionID = 2, protocolV2, capabilityV2
		// Simulate a pre-APT-125/corrupted signature collision so reuse safety is
		// proven by the stored protocol ID as well as the new signature input.
		if _, err = repository.pool.Exec(ctx, `UPDATE route_provisionings SET desired_signature=CASE id WHEN $1 THEN $4 WHEN $2 THEN $5 ELSE $6 END WHERE id IN ($1,$2,$3)`, pendingV1.ID, failedV1.ID, repairV1.ID, provisioner.desiredSignature("PERSONAL", actorA, pendingWallet, modelV2), provisioner.desiredSignature("PERSONAL", actorB, failedWallet, modelV2), provisioner.desiredSignature("PERSONAL", actorC, repairWallet, modelV2)); err != nil {
			t.Fatal(err)
		}
		pendingV2, err := provisioner.Ensure(ctx, AuthorizationContext{UserID: actorA, ContextType: "PERSONAL"}, pendingWallet, modelV2)
		if err != nil || pendingV2.ID == pendingV1.ID || pendingV2.DesiredGeneration <= pendingV1.DesiredGeneration || pendingV2.ProtocolVersionID == nil || *pendingV2.ProtocolVersionID != protocolV2 {
			t.Fatalf("old PENDING provisioning was reused across protocol versions: v1=%#v v2=%#v err=%v", pendingV1, pendingV2, err)
		}
		failedV2, err := provisioner.Ensure(ctx, AuthorizationContext{UserID: actorB, ContextType: "PERSONAL"}, failedWallet, modelV2)
		if err != nil || failedV2.ID == failedV1.ID || failedV2.DesiredGeneration <= failedV1.DesiredGeneration || failedV2.ProtocolVersionID == nil || *failedV2.ProtocolVersionID != protocolV2 {
			t.Fatalf("old FAILED provisioning was reused across protocol versions: v1=%#v v2=%#v err=%v", failedV1, failedV2, err)
		}
		repairV2, err := provisioner.Ensure(ctx, AuthorizationContext{UserID: actorC, ContextType: "PERSONAL"}, repairWallet, modelV2)
		if err != nil || repairV2.ID == repairV1.ID || repairV2.DesiredGeneration <= repairV1.DesiredGeneration || repairV2.ProtocolVersionID == nil || *repairV2.ProtocolVersionID != protocolV2 {
			t.Fatalf("old REPAIR_REQUIRED provisioning was reused across protocol versions: v1=%#v v2=%#v err=%v", repairV1, repairV2, err)
		}
		for _, old := range []RouteProvisioning{pendingV1, failedV1} {
			frozen, frozenErr := provisioner.provisioningModel(ctx, old.ID)
			if frozenErr != nil || frozen.ProtocolDefinitionID != protocolV1 || frozen.ProtocolDefinitionID == protocolV2 {
				t.Fatalf("old provisioning resolved against a newer protocol: provisioning=%s model=%#v err=%v", old.ID, frozen, frozenErr)
			}
		}
		if frozen, frozenErr := provisioner.provisioningModel(ctx, repairV1.ID); frozenErr == nil || frozen.ProtocolDefinitionID == protocolV2 {
			t.Fatalf("protocol-less old provisioning resolved against the new protocol: model=%#v err=%v", frozen, frozenErr)
		}
		second, err := repository.UpsertStableRoute(ctx, accountID, StableRouteInput{ModelID: modelID, ProtocolVersionID: protocolV2, Priority: 90, DefaultScope: "NONE", ExpectedAccountVersion: 2, ExpectedRouteVersion: 0, IdempotencyKey: "stable-route-key-0002", Reason: "create second protocol version route"}, actorA)
		if err != nil || second.ID == first.ID || second.AccountVersion != 3 {
			t.Fatalf("protocol coexistence=%#v err=%v", second, err)
		}
		if err = repository.DeleteStableRoute(ctx, accountID, StableRouteDeleteInput{RouteID: first.ID, ExpectedAccountVersion: 3, ExpectedRouteVersion: 1, IdempotencyKey: "stable-route-key-0003", Reason: "delete first protocol route safely"}, actorA); err != nil {
			t.Fatal(err)
		}
		replayed, err = repository.UpsertStableRoute(ctx, accountID, input, winningActor)
		if err != nil || !reflect.DeepEqual(replayed, first) {
			t.Fatalf("replay after delete changed: %#v err=%v", replayed, err)
		}
		preChecks := 0
		repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(_ context.Context, _ pgx.Tx, selected TokenCommandGovernanceActivation) error {
			preChecks++
			if selected.State != "PRE_ACTIVATION" || selected.Epoch != 0 || preChecks > 1 {
				return ErrTokenRouteActivationChanged
			}
			return nil
		}))
		preContext := context.WithValue(ctx, tokenRouteActivationContextKey{}, tokenRouteActivationSelection{TokenCommandGovernanceActivation: TokenCommandGovernanceActivation{State: "PRE_ACTIVATION", Epoch: 0}, transactional: true})
		if _, err = repository.UpsertRoute(preContext, accountID, "stable-model", "CHAT", 55, "NONE", "HEALTHY", actorA); !errors.Is(err, ErrTokenRouteActivationChanged) || preChecks != 2 {
			t.Fatalf("PRE_ACTIVATION to ACTIVE race was not rejected at commit: checks=%d err=%v", preChecks, err)
		}
		catalogIDV3 := uuid.New()
		if _, err = repository.pool.Exec(ctx, `
INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'restore protocol v1',$2,$3,$3);
INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES((SELECT max(version) FROM model_catalog_versions),$4,'Stable Model','stable model','','{}','PUBLISHED',$5,$6,true);`, catalogIDV3, actorA, now, modelID, capabilityV1, protocolV1); err != nil {
			t.Fatal(err)
		}
		recreated, err := repository.UpsertStableRoute(ctx, accountID, StableRouteInput{ModelID: modelID, ProtocolVersionID: protocolV1, Priority: 80, DefaultScope: "NONE", ExpectedAccountVersion: 4, ExpectedRouteVersion: 0, IdempotencyKey: "stable-route-key-0004", Reason: "recreate deleted stable route"}, actorA)
		if err != nil || recreated.ID == first.ID || recreated.AccountVersion != 5 || recreated.RouteVersion != 1 {
			t.Fatalf("deleted stable route was not recreated independently: %#v err=%v", recreated, err)
		}
		exactBefore, err := repository.StableTokenRouteImpact(ctx, accountID, StableRouteImpactInput{Action: "DELETE", RouteID: recreated.ID})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = repository.pool.Exec(ctx, `UPDATE routing_policies SET priority=priority+1 WHERE id=$1`, second.ID); err != nil {
			t.Fatal(err)
		}
		exactAfterOtherProtocol, err := repository.StableTokenRouteImpact(ctx, accountID, StableRouteImpactInput{Action: "DELETE", RouteID: recreated.ID})
		if err != nil || exactAfterOtherProtocol.CurrentVersion.EvidenceSetSHA256 != exactBefore.CurrentVersion.EvidenceSetSHA256 {
			t.Fatalf("same protocol code on another protocol version contaminated exact impact: before=%s after=%s err=%v", exactBefore.CurrentVersion.EvidenceSetSHA256, exactAfterOtherProtocol.CurrentVersion.EvidenceSetSHA256, err)
		}
		replacementCredential := uuid.New()
		if _, err = repository.pool.Exec(ctx, `DELETE FROM provider_credentials WHERE id=$1; INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status) VALUES($2,$3,'PROVIDER','https://provider.invalid','x'::bytea,'key','replacement-fingerprint',2,'ACTIVE',ARRAY['stable-model'],$4,$5,$4,'VALID')`, credentialID, replacementCredential, accountID, now, actorA); err != nil {
			t.Fatal(err)
		}
		exactAfterEqualReplacement, err := repository.StableTokenRouteImpact(ctx, accountID, StableRouteImpactInput{Action: "DELETE", RouteID: recreated.ID})
		if err != nil || exactAfterEqualReplacement.Dependencies != exactBefore.Dependencies || exactAfterEqualReplacement.CurrentVersion.EvidenceSetSHA256 == exactBefore.CurrentVersion.EvidenceSetSHA256 {
			t.Fatalf("equal-count credential replacement did not invalidate exact evidence: before=%#v after=%#v err=%v", exactBefore, exactAfterEqualReplacement, err)
		}
		activeChecks := 0
		repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(_ context.Context, _ pgx.Tx, selected TokenCommandGovernanceActivation) error {
			activeChecks++
			if selected.State != "ACTIVE" || selected.Epoch != 1 || activeChecks > 1 {
				return ErrTokenRouteActivationChanged
			}
			return nil
		}))
		activeContext := context.WithValue(ctx, tokenRouteActivationContextKey{}, tokenRouteActivationSelection{TokenCommandGovernanceActivation: TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 1}, transactional: true})
		_, err = repository.UpsertStableRoute(activeContext, accountID, StableRouteInput{ModelID: modelID, ProtocolVersionID: protocolV1, Priority: 75, DefaultScope: "NONE", ExpectedAccountVersion: 5, ExpectedRouteVersion: 1, IdempotencyKey: "stable-route-race-0005", Reason: "reject activation rollback during commit"}, actorA)
		if !errors.Is(err, ErrTokenRouteActivationChanged) || activeChecks != 2 {
			t.Fatalf("ACTIVE to SAFE_ROLLBACK race was not rejected at commit: checks=%d err=%v", activeChecks, err)
		}
		repository.WithTokenRouteActivationTransactionGuard(tokenRouteActivationGuardFunc(func(_ context.Context, _ pgx.Tx, _ TokenCommandGovernanceActivation) error { return nil }))
		var mutations, live int
		if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM routing_policy_mutations`).Scan(&mutations); err != nil {
			t.Fatal(err)
		}
		if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM routing_policies WHERE token_account_id=$1 AND reference_status='RESOLVED'`, accountID).Scan(&live); err != nil {
			t.Fatal(err)
		}
		if mutations != 4 || live != 2 {
			t.Fatalf("mutations=%d live=%d", mutations, live)
		}
		evidence, err := repository.StableTokenRouteImpact(ctx, accountID, StableRouteImpactInput{Action: "DELETE", RouteID: second.ID})
		if err != nil || evidence.ResourceID != second.ID.String() || evidence.CurrentVersion.AccountVersion == nil || evidence.CurrentVersion.RouteVersion == nil || evidence.CurrentVersion.ProtocolDefinitionID == nil {
			t.Fatalf("stable impact=%#v err=%v", evidence, err)
		}
		handler := newStableRouteIntegrationHandler(t, repository, actorA)
		body, _ := json.Marshal(map[string]any{"model_id": modelID, "protocol_version_id": protocolV1, "priority": 70, "default_scope": "NONE", "expected_account_version": 5, "expected_route_version": 1, "reason": "verify the stable HTTP response contract"})
		request := httptest.NewRequest(http.MethodPut, "/admin/v2/token-accounts/"+accountID.String()+"/routes", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer integration")
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Idempotency-Key", "stable-route-key-0005")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("stable HTTP status=%d body=%s", response.Code, response.Body.String())
		}
		var HTTPResult StableRouteResult
		if err = json.Unmarshal(response.Body.Bytes(), &HTTPResult); err != nil || HTTPResult.ModelID != modelID || HTTPResult.ProtocolVersionID != protocolV1 || HTTPResult.AccountVersion != 6 {
			t.Fatalf("stable HTTP response=%#v body=%s err=%v", HTTPResult, response.Body.String(), err)
		}
		legacyRequest := httptest.NewRequest(http.MethodPut, "/admin/v1/token-accounts/"+accountID.String()+"/routes", bytes.NewBufferString(`{"model":"stable-model","protocol_code":"CHAT","priority":70,"default_scope":"NONE","health_status":"HEALTHY"}`))
		legacyRequest.Header.Set("Authorization", "Bearer integration")
		legacyRequest.Header.Set("Content-Type", "application/json")
		legacyResponse := httptest.NewRecorder()
		handler.ServeHTTP(legacyResponse, legacyRequest)
		if legacyResponse.Code != http.StatusGone {
			t.Fatalf("active governance did not retire v1 HTTP contract: status=%d body=%s", legacyResponse.Code, legacyResponse.Body.String())
		}
		loaded, err := migrations.Load("../../migrations/token")
		if err != nil {
			t.Fatal(err)
		}
		connection, err := repository.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Release()
		if err = migrations.DownOne(ctx, connection.Conn(), loaded); err == nil || !strings.Contains(err.Error(), "rollback refused") {
			t.Fatalf("downgrade did not fail closed with immutable route history: %v", err)
		}
		migrations.Unlock(ctx, connection.Conn())
		if err = repository.pool.QueryRow(ctx, `SELECT count(*) FROM routing_policy_mutations`).Scan(&mutations); err != nil || mutations != 5 {
			t.Fatalf("failed downgrade did not preserve mutation history: mutations=%d err=%v", mutations, err)
		}
	})

	t.Run("legacy resolution and down", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		connection, err := pgx.Connect(ctx, databaseURL)
		if err != nil {
			t.Fatal(err)
		}
		defer connection.Close(context.Background())
		schema := "stable_route_migration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		identifier := pgx.Identifier{schema}.Sanitize()
		if _, err = connection.Exec(ctx, `CREATE SCHEMA `+identifier); err != nil {
			t.Fatal(err)
		}
		defer connection.Exec(context.Background(), `DROP SCHEMA `+identifier+` CASCADE`)
		if _, err = connection.Exec(ctx, `SET search_path TO `+identifier); err != nil {
			t.Fatal(err)
		}
		loaded, err := migrations.Load("../../migrations/token")
		if err != nil {
			t.Fatal(err)
		}
		before, through := make([]migrations.Migration, 0), make([]migrations.Migration, 0)
		for _, migration := range loaded {
			if migration.Version <= 24 {
				before = append(before, migration)
			}
			if migration.Version <= 25 {
				through = append(through, migration)
			}
		}
		if err = migrations.Up(ctx, connection, before); err != nil {
			t.Fatal(err)
		}
		migrations.Unlock(ctx, connection)
		now := time.Now().UTC().Add(-time.Minute)
		actor, protocolID := uuid.New(), uuid.MustParse("00000000-0000-7000-8200-000000000000")
		ambiguousAccount, resolvableAccount, owner, wallet := uuid.New(), uuid.New(), uuid.New(), uuid.New()
		modelA, modelB, capabilityA, capabilityB, catalogID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
		var catalogVersion int64
		if err = connection.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'legacy migration',$2,$3,$3) RETURNING version`, catalogID, actor, now).Scan(&catalogVersion); err != nil {
			t.Fatal(err)
		}
		if _, err = connection.Exec(ctx, `
INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'P1','adapter-v1','same-name',$2,$5,$6),($3,'P2','adapter-v1','same-name',$4,$5,$6);
INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES($7,$1,1,$8,100,10,'["TEXT"]','v1',$5,$6),($9,$3,1,$8,100,10,'["TEXT"]','v1',$5,$6);
INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES($10,$1,'A','a','PUBLISHED',$7,$8,true),($10,$3,'B','b','PUBLISHED',$9,$8,true);
INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($11,'USER',$12,$13,'Ambiguous','ACTIVE',$5,$6,$6,'PLATFORM'),($14,'USER',$12,$13,'Resolvable','ACTIVE',$5,$6,$6,'PLATFORM');
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,created_by,created_at,validation_status) VALUES(gen_random_uuid(),$11,'P1','https://p1.invalid','x','key','a1',1,'ACTIVE',ARRAY['same-name'],$5,$6,'VALID'),(gen_random_uuid(),$11,'P2','https://p2.invalid','x','key','a2',2,'RETIRED',ARRAY['same-name'],$5,$6,'VALID'),(gen_random_uuid(),$14,'P1','https://p1.invalid','x','key','r1',1,'ACTIVE',ARRAY['same-name'],$5,$6,'VALID'),(gen_random_uuid(),$14,'P1','https://p1.invalid','x','key','r2',2,'RETIRED',ARRAY['same-name'],$5,$6,'VALID');
INSERT INTO routing_policies(id,token_account_id,model,priority,default_scope,health_status,updated_at,route_generation,protocol_code) VALUES(gen_random_uuid(),$11,'same-name',10,'NONE','HEALTHY',$6,1,'CHAT'),(gen_random_uuid(),$14,'same-name',10,'NONE','HEALTHY',$6,1,'CHAT');`, modelA, strings.Repeat("a", 64), modelB, strings.Repeat("b", 64), actor, now, capabilityA, protocolID, capabilityB, catalogVersion, ambiguousAccount, owner, wallet, resolvableAccount); err != nil {
			t.Fatal(err)
		}
		if err = migrations.Up(ctx, connection, through); err != nil {
			t.Fatal(err)
		}
		migrations.Unlock(ctx, connection)
		var ambiguous, resolved string
		if err = connection.QueryRow(ctx, `SELECT reference_status FROM routing_policies WHERE token_account_id=$1`, ambiguousAccount).Scan(&ambiguous); err != nil {
			t.Fatal(err)
		}
		if err = connection.QueryRow(ctx, `SELECT reference_status FROM routing_policies WHERE token_account_id=$1`, resolvableAccount).Scan(&resolved); err != nil {
			t.Fatal(err)
		}
		if ambiguous != "LEGACY_UNRESOLVED" || resolved != "RESOLVED" {
			t.Fatalf("ambiguous=%s resolved=%s", ambiguous, resolved)
		}
		protocolV2, secondRoute := uuid.New(), uuid.New()
		if _, err = connection.Exec(ctx, `
INSERT INTO protocol_definitions(id,protocol_code,version,status,request_schema,created_at,published_at)
VALUES($1,'CHAT',2,'PUBLISHED','{}',$2,$2);
INSERT INTO routing_policies(
    id,token_account_id,model,priority,default_scope,health_status,updated_at,route_generation,protocol_code,
    model_id,protocol_version_id,reference_status,route_version,health_version,health_observed_at,
    health_diagnostic_code,health_failure_count,next_probe_at
) VALUES($3,$4,'same-name',9,'NONE','STALE',$2,2,'CHAT',$5,$1,'RESOLVED',1,1,$2,'TEST_SECOND_PROTOCOL',0,$2);`, protocolV2, now, secondRoute, resolvableAccount, modelA); err != nil {
			t.Fatal(err)
		}
		if err = migrations.DownOne(ctx, connection, through); err == nil || !strings.Contains(err.Error(), "rollback refused") {
			t.Fatalf("downgrade accepted multi-protocol facts that v24 cannot represent: %v", err)
		}
		migrations.Unlock(ctx, connection)
		var preserved int
		if err = connection.QueryRow(ctx, `SELECT count(*) FROM routing_policies WHERE token_account_id=$1 AND model='same-name'`, resolvableAccount).Scan(&preserved); err != nil || preserved != 2 {
			t.Fatalf("failed downgrade did not preserve multi-protocol facts: routes=%d err=%v", preserved, err)
		}
		if _, err = connection.Exec(ctx, `DELETE FROM routing_policies WHERE id=$1`, secondRoute); err != nil {
			t.Fatal(err)
		}
		if err = migrations.DownOne(ctx, connection, through); err != nil {
			t.Fatal(err)
		}
		var oldUnique bool
		if err = connection.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_constraint WHERE conrelid='routing_policies'::regclass AND conname='routing_policies_token_account_id_model_key')`).Scan(&oldUnique); err != nil || !oldUnique {
			t.Fatalf("legacy unique restored=%v err=%v", oldUnique, err)
		}
	})
}

func newStableRouteIntegrationHandler(t *testing.T, repository *PostgresRepository, actor uuid.UUID) http.Handler {
	t.Helper()
	authorizationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Authorization-Version", "1")
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "user_id": actor, "granted_permissions": []string{"platform.tokens.manage"}})
	}))
	t.Cleanup(authorizationServer.Close)
	authorizer, err := authorization.NewClient(authorizationServer.URL, "integration-service-token")
	if err != nil {
		t.Fatal(err)
	}
	activation := tokenCommandActivationReaderFunc(func(context.Context) (TokenCommandGovernanceActivation, error) {
		return TokenCommandGovernanceActivation{State: "ACTIVE", Epoch: 1}, nil
	})
	service := NewService(repository, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).
		WithAuthorizer(authorizer).
		WithTokenCommandGovernanceActivation(activation)
	verify := func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.integration.example", Subject: actor.String(), SessionID: "stable-route-integration", AuthenticatedAt: time.Now().UTC()}, nil
	}
	return service.Handler("", verify)
}

type tokenRouteActivationGuardFunc func(context.Context, pgx.Tx, TokenCommandGovernanceActivation) error

func (guard tokenRouteActivationGuardFunc) LockAndCheckTokenRouteActivation(ctx context.Context, tx pgx.Tx, selected TokenCommandGovernanceActivation) error {
	return guard(ctx, tx, selected)
}
