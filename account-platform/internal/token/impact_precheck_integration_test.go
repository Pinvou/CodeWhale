package token

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestTokenImpactPrecheckPostgresIntegration uses the actual Token migration
// chain and production repository methods. CI must provide
// PINVOU_TEST_TOKEN_DATABASE_URL and reject a skipped result.
func TestTokenImpactPrecheckPostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	now := time.Now().UTC().Add(-time.Minute)
	modelID, replacementID := uuid.New(), uuid.New()
	accountID, ownerID, walletID, credentialID, alternativeCredentialID, irrelevantCredentialID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	actorID, capabilityID, replacementCapabilityID, catalogID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	protocolID := uuid.MustParse("00000000-0000-7000-8200-000000000000")
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	var catalogVersion int64
	if err := repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'impact integration',$2,$3,$3) RETURNING version`, catalogID, actorID, now).Scan(&catalogVersion); err != nil {
		t.Fatal(err)
	}
	seed := `
INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES
($1,'provider','v1','model-one',$12,$8,$10),($2,'provider','v1','model-two',$13,$8,$10);
INSERT INTO model_capability_versions(id,model_id,version,protocol_definition_id,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,created_by,created_at) VALUES
($9,$1,1,$3,128000,8192,'["TEXT"]','v1',$8,$10),($11,$2,1,$3,128000,8192,'["TEXT"]','v1',$8,$10);
INSERT INTO model_catalog_entries(catalog_version,model_id,display_name,normalized_name,description,aliases,lifecycle_status,model_capability_version_id,protocol_definition_id,source_available) VALUES
($14,$1,'Model One','model one','','{}','PUBLISHED',$9,$3,true),($14,$2,'Model Two','model two','','{}','PUBLISHED',$11,$3,true);
INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES
($4,'USER',$5,$6,'Impact Account','ACTIVE',$8,$10,$10,'PLATFORM');
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status) VALUES
($7,$4,'provider','https://redacted.invalid','x'::bytea,'key',$12,1,'ACTIVE',ARRAY['model-one'],$10,$8,$10,'VALID'),
($15,$4,'provider','https://redacted.invalid','y'::bytea,'key',$13,2,'RETIRED',ARRAY['model-one'],$10,$8,$10,'VALID'),
($16,$4,'other-provider','https://redacted.invalid','z'::bytea,'key',$17,3,'PENDING',ARRAY['model-one'],$10,$8,$10,'VALID');
INSERT INTO routing_policies(id,token_account_id,model,priority,default_scope,health_status,updated_at,route_generation,protocol_code,model_id,protocol_version_id,reference_status,route_version,health_version,health_diagnostic_code,health_observed_at,health_expires_at,next_probe_at) VALUES
(gen_random_uuid(),$4,'model-one',100,'NONE','HEALTHY',$10,3,'CHAT',$1,$3,'RESOLVED',1,1,'PROBE_SUCCEEDED',$10,$10+interval '4 minutes',$10+interval '3 minutes');
INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,protocol_version_id,desired_generation,desired_signature,status,stage,policy_version,policy_snapshot,max_attempts,sla_deadline_at,lease_owner,lease_until,active_token_account_id,active_credential_id,requested_by,created_at,updated_at) VALUES
(gen_random_uuid(),'PERSONAL',$5,$6,$1,'model-one','CHAT',$3,3,$12,'READY','READY',1,'{}',3,$10+interval '1 hour','worker',$10+interval '1 minute',$4,$7,$8,$10,$10);
INSERT INTO usage_requests(id,user_id,session_id,token_account_id,credential_id,wallet_id,model,model_id,catalog_version,model_capability_version_id,protocol_definition_id,route_generation,model_snapshot,status,execution_status,execution_version,delivery_status,maximum_microcredits,request_snapshot,started_at,updated_at,idempotency_key_digest,request_fingerprint) VALUES
(gen_random_uuid(),$5,'session-one',$4,$7,$6,'model-one',$1,$14,$9,$3,3,'{}','UPSTREAM_ACTIVE','STREAMING',1,'STARTED',100,'{}',$10,$10,$12,$12);`
	if _, err := repository.pool.Exec(ctx, seed, modelID, replacementID, protocolID, accountID, ownerID, walletID, credentialID, actorID, capabilityID, now, replacementCapabilityID, digest, "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789", catalogVersion, alternativeCredentialID, irrelevantCredentialID, strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	model, err := repository.ModelLifecycleImpact(ctx, modelID, &replacementID)
	if err != nil || model.CurrentVersion.CatalogVersion == nil || *model.CurrentVersion.CatalogVersion != catalogVersion || model.Dependencies.Routes != 1 || model.Dependencies.ActiveLeases != 1 || model.Dependencies.InflightRequests != 1 || model.ReplacementReady == nil || !*model.ReplacementReady {
		t.Fatalf("model=%+v err=%v", model, err)
	}
	rollback, err := repository.CatalogRollbackImpact(ctx, catalogVersion)
	if err != nil || len(rollback.ModelIDs) != 2 || rollback.Dependencies.ReadyProvisionings != 1 {
		t.Fatalf("rollback=%+v err=%v", rollback, err)
	}
	account, err := repository.TokenAccountImpact(ctx, accountID)
	if err != nil || account.Dependencies.Routes != 1 || account.Dependencies.ActiveSessions != 1 || account.Dependencies.AlternativeCredentials != 0 || account.CurrentVersion.EvidenceSetSHA256 == "" {
		t.Fatalf("account=%+v err=%v", account, err)
	}
	route, err := repository.TokenRouteImpact(ctx, accountID, "model-one", "", "DELETE")
	if err != nil || route.CurrentVersion.RouteGeneration == nil || *route.CurrentVersion.RouteGeneration != 3 || route.Dependencies.ReadyProvisionings != 1 || route.Dependencies.AffectedCoverageTuples != 1 || route.Dependencies.RemainingReadyCoverage != 0 || route.Dependencies.CoverageGaps != 1 {
		t.Fatalf("route=%+v err=%v", route, err)
	}
	credential, err := repository.TokenCredentialImpact(ctx, accountID, &credentialID)
	if err != nil || credential.CurrentVersion.CredentialVersion == nil || *credential.CurrentVersion.CredentialVersion != 1 || credential.CurrentVersion.AccountVersion == nil || *credential.CurrentVersion.AccountVersion != 1 || credential.CredentialFingerprint != digest || credential.Dependencies.ActiveLeases != 1 || credential.Dependencies.AlternativeCredentials != 1 || !validImpactFingerprint(credential.RouteSetFingerprint) || !validImpactFingerprint(credential.ProvisioningSetFingerprint) || !validImpactFingerprint(credential.InflightSetFingerprint) || credential.ReplacementCredentialVersion == nil || *credential.ReplacementCredentialVersion != 2 {
		t.Fatalf("credential=%+v err=%v", credential, err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE route_provisionings SET lease_until=$1 WHERE active_credential_id=$2; UPDATE usage_requests SET execution_status='SUCCEEDED' WHERE credential_id=$2`, now.Add(-time.Minute), credentialID); err != nil {
		t.Fatal(err)
	}
	service := NewService(repository, nil, nil, nil, ProviderURLPolicy{}, nil, nil, 0).WithImpactPrechecks(true, repository, nil)
	projection, err := service.approvalCommandPrecheck(ctx, ApprovalSnapshotRequest{CommandType: "TOKEN_CREDENTIAL_REVOKE", TargetResourceType: "PROVIDER_CREDENTIAL", TargetResourceID: credentialID.String(), NormalizedCommand: json.RawMessage(`{"token_account_id":"` + accountID.String() + `","expected_account_version":1,"credential_id":"` + credentialID.String() + `","credential_version":1}`)})
	if err != nil || projection.BillingObservedAt != nil {
		t.Fatalf("credential approval projection=%+v err=%v", projection, err)
	}
	var frozen map[string]any
	if json.Unmarshal(projection.CriticalResourceVersions, &frozen) != nil || len(frozen) != 11 || string(projection.FinancialSnapshot) != "{}" {
		t.Fatalf("credential frozen snapshot=%s financial=%s", projection.CriticalResourceVersions, projection.FinancialSnapshot)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE provider_credentials SET provider='other-provider' WHERE id=$1`, alternativeCredentialID); err != nil {
		t.Fatal(err)
	}
	providerMismatch, err := repository.TokenCredentialImpact(ctx, accountID, &credentialID)
	if err != nil || providerMismatch.Dependencies.AlternativeCredentials != 0 {
		t.Fatalf("provider mismatch alternative=%+v err=%v", providerMismatch, err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE provider_credentials SET provider='provider' WHERE id=$1`, irrelevantCredentialID); err != nil {
		t.Fatal(err)
	}
	pendingCandidate, err := repository.TokenCredentialImpact(ctx, accountID, &credentialID)
	if err != nil || pendingCandidate.Dependencies.AlternativeCredentials != 1 {
		t.Fatalf("valid PENDING rotation candidate=%+v err=%v", pendingCandidate, err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE provider_credentials SET provider='other-provider' WHERE id=$1`, irrelevantCredentialID); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE provider_credentials SET provider='provider',allowed_models=ARRAY['other-model'] WHERE id=$1`, alternativeCredentialID); err != nil {
		t.Fatal(err)
	}
	modelMismatch, err := repository.TokenCredentialImpact(ctx, accountID, &credentialID)
	if err != nil || modelMismatch.Dependencies.AlternativeCredentials != 0 {
		t.Fatalf("model mismatch alternative=%+v err=%v", modelMismatch, err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE provider_credentials SET allowed_models=ARRAY['model-one'] WHERE id=$1`, alternativeCredentialID); err != nil {
		t.Fatal(err)
	}
	retired, err := repository.TokenCredentialImpact(ctx, accountID, &alternativeCredentialID)
	if err != nil || retired.Dependencies.ReadyProvisionings != 0 || retired.Dependencies.AlternativeCredentials != 0 {
		t.Fatalf("unused retired credential=%+v err=%v", retired, err)
	}
	seedPersonalAlternative := func(candidateAccountID, candidateCredentialID, candidateOwnerID, candidateWalletID, candidateModelID uuid.UUID, upstream, protocol, name string, generation int64) {
		t.Helper()
		_, seedErr := repository.pool.Exec(ctx, `
INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,$4,'ACTIVE',$5,$6,$6,'PLATFORM');
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status) VALUES($7,$1,'provider','https://redacted.invalid','x'::bytea,'key',$8,1,'ACTIVE',ARRAY[$9],$6,$5,$6,'VALID');
INSERT INTO routing_policies(id,token_account_id,model,priority,default_scope,health_status,updated_at,route_generation,protocol_code) VALUES(gen_random_uuid(),$1,$9,100,'NONE','HEALTHY',$6,$12,$10);
INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,desired_generation,desired_signature,status,stage,policy_version,policy_snapshot,max_attempts,sla_deadline_at,lease_owner,lease_until,active_token_account_id,active_credential_id,requested_by,created_at,updated_at) VALUES(gen_random_uuid(),'PERSONAL',$2,$3,$11,$9,$10,$12,$13,'READY','READY',1,'{}',3,$6+interval '1 hour','worker',$6+interval '1 minute',$1,$7,$5,$6,$6)`, candidateAccountID, candidateOwnerID, candidateWalletID, name, actorID, now, candidateCredentialID, "candidate-"+candidateCredentialID.String(), upstream, protocol, candidateModelID, generation, digest)
		if seedErr != nil {
			t.Fatal(seedErr)
		}
	}
	mismatchedAccountID, mismatchedCredentialID := uuid.New(), uuid.New()
	seedPersonalAlternative(mismatchedAccountID, mismatchedCredentialID, ownerID, uuid.New(), modelID, "model-one", "CHAT", "Wrong wallet", 4)
	seedPersonalAlternative(uuid.New(), uuid.New(), uuid.New(), walletID, modelID, "model-one", "CHAT", "Wrong personal owner", 5)
	seedPersonalAlternative(uuid.New(), uuid.New(), ownerID, walletID, modelID, "model-one", "EMBEDDING", "Wrong protocol", 6)
	seedPersonalAlternative(uuid.New(), uuid.New(), ownerID, walletID, replacementID, "model-two", "CHAT", "Wrong model", 7)
	mismatchedRoute, err := repository.TokenRouteImpact(ctx, accountID, "model-one", "", "DELETE")
	if err != nil || mismatchedRoute.Dependencies.RemainingReadyCoverage != 0 || mismatchedRoute.Dependencies.CoverageGaps != 1 {
		t.Fatalf("wrong-wallet route coverage=%+v err=%v", mismatchedRoute, err)
	}
	exactAccountID, exactCredentialID := uuid.New(), uuid.New()
	seedPersonalAlternative(exactAccountID, exactCredentialID, ownerID, walletID, modelID, "model-one", "CHAT", "Exact alternative", 8)
	exactRoute, err := repository.TokenRouteImpact(ctx, accountID, "model-one", "", "DELETE")
	if err != nil || exactRoute.Dependencies.AffectedCoverageTuples != 1 || exactRoute.Dependencies.RemainingReadyCoverage != 1 || exactRoute.Dependencies.CoverageGaps != 0 {
		t.Fatalf("exact route coverage=%+v err=%v", exactRoute, err)
	}
	crossAccountCredential, err := repository.TokenCredentialImpact(ctx, accountID, &credentialID)
	if err != nil || crossAccountCredential.Dependencies.AlternativeCredentials != 1 {
		t.Fatalf("cross-account credential counted as same-account alternative=%+v err=%v", crossAccountCredential, err)
	}
	orgID, unitID, orgWalletID := uuid.New(), uuid.New(), uuid.New()
	orgAccountID, unitAccountID, orgCredentialID, unitCredentialID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := repository.pool.Exec(ctx, `
INSERT INTO token_accounts(id,owner_type,owner_id,owner_organization_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'ORG',$2,$2,$3,'Org source','ACTIVE',$4,$5,$5,'PLATFORM');
INSERT INTO token_accounts(id,owner_type,owner_id,owner_organization_id,owner_unit_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($6,'ORG_UNIT',$7,$2,$7,$3,'Unit alternative','ACTIVE',$4,$5,$5,'PLATFORM');
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status) VALUES
($8,$1,'provider','https://redacted.invalid','x'::bytea,'key',$10,1,'ACTIVE',ARRAY['model-one'],$5,$4,$5,'VALID'),
($9,$6,'provider','https://redacted.invalid','x'::bytea,'key',$11,1,'ACTIVE',ARRAY['model-one'],$5,$4,$5,'VALID');
INSERT INTO routing_policies(id,token_account_id,model,priority,default_scope,health_status,updated_at,route_generation,protocol_code) VALUES
(gen_random_uuid(),$1,'model-one',100,'ORG','HEALTHY',$5,3,'CHAT'),(gen_random_uuid(),$6,'model-one',100,'UNIT','HEALTHY',$5,4,'CHAT');
INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,desired_generation,desired_signature,status,stage,policy_version,policy_snapshot,max_attempts,sla_deadline_at,lease_owner,lease_until,active_token_account_id,active_credential_id,requested_by,created_at,updated_at) VALUES
(gen_random_uuid(),'ORGANIZATION',$2,$3,$12,'model-one','CHAT',3,$13,'READY','READY',1,'{}',3,$5+interval '1 hour','worker',$5+interval '1 minute',$1,$8,$4,$5,$5),
(gen_random_uuid(),'ORGANIZATION',$2,$3,$12,'model-one','CHAT',4,$13,'READY','READY',1,'{}',3,$5+interval '1 hour','worker',$5+interval '1 minute',$6,$9,$4,$5,$5)`, orgAccountID, orgID, orgWalletID, actorID, now, unitAccountID, unitID, orgCredentialID, unitCredentialID, "org-"+orgCredentialID.String(), "unit-"+unitCredentialID.String(), modelID, digest); err != nil {
		t.Fatal(err)
	}
	orgImpact, err := repository.TokenAccountImpact(ctx, orgAccountID)
	if err != nil || orgImpact.Dependencies.AlternativeAccounts != 1 {
		t.Fatalf("organization-to-unit alternative=%+v err=%v", orgImpact, err)
	}
	replacementUnitID, replacementUnitAccountID, replacementUnitCredentialID := uuid.New(), uuid.New(), uuid.New()
	if _, err := repository.pool.Exec(ctx, `
UPDATE token_accounts SET status='DISABLED',updated_at=$1 WHERE id=$2;
INSERT INTO token_accounts(id,owner_type,owner_id,owner_organization_id,owner_unit_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($3,'ORG_UNIT',$4,$5,$4,$6,'Unit replacement','ACTIVE',$7,$1,$1,'PLATFORM');
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status) VALUES($8,$3,'provider','https://redacted.invalid','x'::bytea,'key',$9,1,'ACTIVE',ARRAY['model-one'],$1,$7,$1,'VALID');
INSERT INTO routing_policies(id,token_account_id,model,priority,default_scope,health_status,updated_at,route_generation,protocol_code) VALUES(gen_random_uuid(),$3,'model-one',100,'UNIT','HEALTHY',$1,5,'CHAT');
INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,desired_generation,desired_signature,status,stage,policy_version,policy_snapshot,max_attempts,sla_deadline_at,lease_owner,lease_until,active_token_account_id,active_credential_id,requested_by,created_at,updated_at) VALUES(gen_random_uuid(),'ORGANIZATION',$5,$6,$10,'model-one','CHAT',5,$11,'READY','READY',1,'{}',3,$1+interval '1 hour','worker',$1+interval '1 minute',$3,$8,$7,$1,$1)`, now.Add(time.Second), unitAccountID, replacementUnitAccountID, replacementUnitID, orgID, orgWalletID, actorID, replacementUnitCredentialID, "unit-replacement-"+replacementUnitCredentialID.String(), modelID, digest); err != nil {
		t.Fatal(err)
	}
	replacedOrgImpact, err := repository.TokenAccountImpact(ctx, orgAccountID)
	if err != nil || replacedOrgImpact.Dependencies.AlternativeAccounts != 1 || replacedOrgImpact.CurrentVersion.EvidenceETag == orgImpact.CurrentVersion.EvidenceETag {
		t.Fatalf("same-count organization-to-unit replacement before=%+v after=%+v err=%v", orgImpact, replacedOrgImpact, err)
	}
	before := account.CurrentVersion.EvidenceETag
	if _, err := repository.pool.Exec(ctx, `UPDATE token_accounts SET updated_at=updated_at+interval '1 second' WHERE id=$1`, accountID); err != nil {
		t.Fatal(err)
	}
	after, err := repository.TokenAccountImpact(ctx, accountID)
	if err != nil || after.CurrentVersion.EvidenceETag == before {
		t.Fatalf("CAS evidence did not change: before=%s after=%s err=%v", before, after.CurrentVersion.EvidenceETag, err)
	}
	assertETagChange := func(label string, mutate func() error) {
		t.Helper()
		beforeImpact, loadErr := repository.TokenAccountImpact(ctx, accountID)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if err := mutate(); err != nil {
			t.Fatalf("%s mutate: %v", label, err)
		}
		afterImpact, loadErr := repository.TokenAccountImpact(ctx, accountID)
		if loadErr != nil || beforeImpact.CurrentVersion.EvidenceETag == afterImpact.CurrentVersion.EvidenceETag {
			t.Fatalf("%s did not alter evidence: before=%s after=%s err=%v", label, beforeImpact.CurrentVersion.EvidenceETag, afterImpact.CurrentVersion.EvidenceETag, loadErr)
		}
	}
	assertETagChange("equal-count route identity", func() error {
		_, err := repository.pool.Exec(ctx, `UPDATE routing_policies SET id=$2 WHERE token_account_id=$1 AND model='model-one'`, accountID, uuid.New())
		return err
	})
	assertETagChange("route health", func() error {
		_, err := repository.pool.Exec(ctx, `UPDATE routing_policies SET health_status='DEGRADED' WHERE token_account_id=$1 AND model='model-one'`, accountID)
		return err
	})
	assertETagChange("provisioning generation", func() error {
		_, err := repository.pool.Exec(ctx, `UPDATE route_provisionings SET actual_generation=2 WHERE active_token_account_id=$1`, accountID)
		return err
	})
	strict, err := repository.TokenRouteImpact(ctx, accountID, "model-one", "", "DELETE")
	if err != nil || strict.Dependencies.ReadyProvisionings != 0 {
		t.Fatalf("stale generation counted READY: %+v err=%v", strict, err)
	}
	assertETagChange("inflight session", func() error {
		_, err := repository.pool.Exec(ctx, `UPDATE usage_requests SET session_id='session-two' WHERE token_account_id=$1`, accountID)
		return err
	})
	assertETagChange("credential validation", func() error {
		_, err := repository.pool.Exec(ctx, `UPDATE provider_credentials SET validation_status='INVALID' WHERE id=$1`, credentialID)
		return err
	})
	assertETagChange("credential provider", func() error {
		_, err := repository.pool.Exec(ctx, `UPDATE provider_credentials SET provider='changed-provider' WHERE id=$1`, alternativeCredentialID)
		return err
	})
	assertETagChange("credential allowed model set", func() error {
		_, err := repository.pool.Exec(ctx, `UPDATE provider_credentials SET allowed_models=ARRAY['model-two'] WHERE id=$1`, alternativeCredentialID)
		return err
	})
	for _, statement := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE route_provisionings SET actual_generation=3 WHERE active_token_account_id=$1`, []any{accountID}},
		{`UPDATE provider_credentials SET validation_status='VALID' WHERE id=$1`, []any{credentialID}},
		{`UPDATE token_accounts SET status='DISABLED' WHERE id=$1`, []any{accountID}},
	} {
		if _, err := repository.pool.Exec(ctx, statement.sql, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	disabled, err := repository.ModelLifecycleImpact(ctx, modelID, nil)
	if err != nil || disabled.Dependencies.ReadyProvisionings != 0 {
		t.Fatalf("disabled account counted READY: %+v err=%v", disabled, err)
	}
	if _, err := repository.TokenRouteImpact(ctx, accountID, "missing-model", "", "DELETE"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing DELETE err=%v", err)
	}
	if _, err := repository.TokenRouteImpact(ctx, accountID, "missing-model", "CHAT", "UPSERT"); !errors.Is(err, ErrModelUnavailable) {
		t.Fatalf("missing UPSERT err=%v", err)
	}
	create, err := repository.TokenRouteImpact(ctx, accountID, "model-two", "CHAT", "UPSERT")
	if err != nil || create.CurrentStatus != "CREATE" || create.CurrentVersion.RouteGeneration == nil || *create.CurrentVersion.RouteGeneration != 0 {
		t.Fatalf("missing UPSERT=%+v err=%v", create, err)
	}
}
