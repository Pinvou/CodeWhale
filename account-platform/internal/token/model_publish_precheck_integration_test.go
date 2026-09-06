package token

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Pinvou/pinvou3/account-platform/internal/platform/authorization"
	"github.com/Pinvou/pinvou3/account-platform/internal/platform/identity"
	"github.com/google/uuid"
)

// TestModelPublishPrecheckPostgresIntegration proves that the production
// command path re-reads the exact model draft, protocol and Billing evidence
// before creating a durable command result. CI supplies the Token database and
// rejects SKIP for this named release gate.
func TestModelPublishPrecheckPostgresIntegration(t *testing.T) {
	databaseURL := tokenPaginationIntegrationDatabaseURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	repository := newTokenQueryIntegrationRepository(t, ctx, databaseURL)
	now := time.Now().UTC().Add(-time.Minute)
	actorID, modelID := uuid.New(), uuid.New()
	var protocolID uuid.UUID
	if err := repository.pool.QueryRow(ctx, `SELECT id FROM protocol_definitions WHERE protocol_code='CHAT' AND version=1 AND status='PUBLISHED'`).Scan(&protocolID); err != nil {
		t.Fatal(err)
	}
	var catalogVersion int64
	if err := repository.pool.QueryRow(ctx, `INSERT INTO model_catalog_versions(id,reason,created_by,created_at,published_at) VALUES($1,'model publish precheck integration',$2,$3,$3) RETURNING version`, uuid.New(), actorID, now).Scan(&catalogVersion); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO model_identities(id,provider_id,adapter_version,upstream_model_ref,upstream_model_ref_digest,created_by,created_at) VALUES($1,'integration','v1','publish-model',$2,$3,$4)`, modelID, strings.Repeat("a", 64), actorID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.pool.Exec(ctx, `INSERT INTO model_catalog_drafts(model_id,base_catalog_version,draft_version,display_name,description,aliases,protocol_code,protocol_version,context_length,maximum_output_tokens,supported_modalities,tokenizer_version,updated_by,updated_at) VALUES($1,$2,1,'Publish model','','{}','CHAT',1,4096,1024,'["text"]','integration-v1',$3,$4)`, modelID, catalogVersion, actorID, now); err != nil {
		t.Fatal(err)
	}
	formulaID, priceID := uuid.New(), uuid.New()
	priceVersion := int64(4)
	pricing := stubImpactPricing{result: ImpactPriceCoverageResult{
		Items: []ImpactPriceCoverage{{ModelID: modelID, ProtocolCode: "CHAT", FormulaVersionID: &formulaID, CreditUnit: "MICROCREDIT", Covered: true, PriceVersionID: &priceID, PriceVersion: &priceVersion, EffectiveFrom: &now}},
		AsOf:  now.Add(time.Second), Source: "BILLING", FinancialFactRevision: 12, PriceSetFingerprint: strings.Repeat("b", 64),
	}}
	service := NewService(repository, nil, nil, nil, ProviderURLPolicy{}, nil, NewHTTPUpstreamClient(), 0).WithImpactPrechecks(true, nil, pricing)
	command := json.RawMessage(`{"model_id":"` + modelID.String() + `"}`)
	missing, err := service.buildTokenCommandPrecheck(ctx, TokenCommandPrecheckRequest{CommandType: "TOKEN_MODEL_PUBLISH", TargetResourceType: "MODEL", TargetResourceID: modelID.String(), Command: command})
	if err != nil || !slices.Contains(missing.Blockers, "MODEL_ROUTE_COVERAGE_MISSING") || !slices.Contains(missing.Blockers, "MODEL_PROVISIONING_COVERAGE_MISSING") {
		t.Fatalf("missing coverage precheck=%+v err=%v", missing, err)
	}

	accountID, ownerID, walletID, credentialID, routeID, provisioningID := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if _, err := repository.pool.Exec(ctx, `
INSERT INTO token_accounts(id,owner_type,owner_id,wallet_id,name,status,created_by,created_at,updated_at,management_mode) VALUES($1,'USER',$2,$3,'Publish account','ACTIVE',$4,$5,$5,'PLATFORM');
INSERT INTO token_share_policies(token_account_id,scope,updated_at) VALUES($1,'OWNER_UNIT_ONLY',$5);
INSERT INTO provider_credentials(id,token_account_id,provider,base_url,ciphertext,key_id,fingerprint,version,status,allowed_models,validated_at,created_by,created_at,validation_status) VALUES($6,$1,'integration','https://provider.invalid','x'::bytea,'integration-key','publish-fingerprint',1,'ACTIVE',ARRAY['publish-model'],$5,$4,$5,'VALID');
INSERT INTO routing_policies(id,token_account_id,model,protocol_code,priority,default_scope,health_status,updated_at,model_id,protocol_version_id,reference_status,route_version,route_generation,health_version,health_observed_at,health_expires_at,health_diagnostic_code,health_failure_count,next_probe_at) VALUES($7,$1,'publish-model','CHAT',100,'NONE','UNKNOWN',$5,$8,$9,'RESOLVED',1,1,1,$5,NULL,'NOT_YET_PROBED',0,$5);
INSERT INTO route_provisionings(id,scope_type,scope_id,wallet_id,model_id,upstream_model_ref,protocol_code,desired_generation,actual_generation,desired_signature,status,stage,policy_version,policy_snapshot,attempt_count,repair_round,max_attempts,sla_deadline_at,lease_owner,lease_until,next_retry_at,last_error_code,last_error_detail,active_token_account_id,active_credential_id,requested_by,created_at,updated_at,protocol_version_id) VALUES($10,'PERSONAL',$2,$3,$8,'publish-model','CHAT',1,1,$11,'PENDING','ROUTE_PENDING',1,'{}',0,0,3,$12,NULL,NULL,$12,NULL,NULL,$1,$6,$4,$5,$5,$9);`, accountID, ownerID, walletID, actorID, now, credentialID, routeID, modelID, protocolID, provisioningID, strings.Repeat("d", 64), now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	pending, err := service.buildTokenCommandPrecheck(ctx, TokenCommandPrecheckRequest{CommandType: "TOKEN_MODEL_PUBLISH", TargetResourceType: "MODEL", TargetResourceID: modelID.String(), Command: command})
	if err != nil || !slices.Contains(pending.Blockers, "MODEL_PROVISIONING_NOT_READY") {
		t.Fatalf("pending coverage precheck=%+v err=%v", pending, err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE route_provisionings SET status='READY',stage='READY',updated_at=transaction_timestamp() WHERE id=$1`, provisioningID); err != nil {
		t.Fatal(err)
	}
	precheck, err := service.buildTokenCommandPrecheck(ctx, TokenCommandPrecheckRequest{CommandType: "TOKEN_MODEL_PUBLISH", TargetResourceType: "MODEL", TargetResourceID: modelID.String(), Command: command})
	if err != nil || len(precheck.Blockers) != 0 || precheck.PriceCoverage == nil || precheck.PriceCoverage.FinancialFactRevision != 12 || len(precheck.ExpectedVersions) == 0 {
		t.Fatalf("precheck=%+v err=%v", precheck, err)
	}
	var frozen map[string]any
	if json.Unmarshal(precheck.ExpectedVersions, &frozen) != nil || len(frozen) != 13 || frozen["model_id"] != modelID.String() || frozen["price_set_fingerprint"] != strings.Repeat("b", 64) || frozen["stable_route_count"] != float64(1) || frozen["provisioning_count"] != float64(1) || frozen["ready_route_count"] != float64(1) {
		t.Fatalf("unexpected frozen versions: %s", precheck.ExpectedVersions)
	}
	explicitCommand := mustMarshalJSON(t, modelPublishCommand{ModelID: modelID, PublishMode: "IMMEDIATE"})
	explicitPrecheck, err := service.buildTokenCommandPrecheck(ctx, TokenCommandPrecheckRequest{CommandType: "TOKEN_MODEL_PUBLISH", TargetResourceType: "MODEL", TargetResourceID: modelID.String(), Command: explicitCommand})
	if err != nil || explicitPrecheck.PrecheckFingerprint != precheck.PrecheckFingerprint || !jsonSemanticEqual(explicitPrecheck.ExpectedVersions, precheck.ExpectedVersions) {
		t.Fatalf("legacy precheck=%s explicit=%s err=%v", precheck.PrecheckFingerprint, explicitPrecheck.PrecheckFingerprint, err)
	}

	authorizationServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Authorization-Version", "7")
		_ = json.NewEncoder(w).Encode(map[string]any{"allowed": true, "user_id": actorID, "granted_permissions": []string{"platform.models.manage"}})
	}))
	t.Cleanup(authorizationServer.Close)
	authorizer, err := authorization.NewClient(authorizationServer.URL, "integration-service-token")
	if err != nil {
		t.Fatal(err)
	}
	service.WithAuthorizer(authorizer)

	// Change one exact READY coverage fact after the operator froze the precheck. The handler
	// must reject before BeginTokenCommand can persist a PENDING result.
	if _, err := repository.pool.Exec(ctx, `UPDATE route_provisionings SET status='FAILED',stage='TEST_DRIFT',updated_at=transaction_timestamp() WHERE id=$1`, provisioningID); err != nil {
		t.Fatal(err)
	}
	versions := cloneExpectedVersions(t, precheck.ExpectedVersions)
	envelope := bindModelPublishRequestFingerprint(t, TokenCommandEnvelope{CommandType: "TOKEN_MODEL_PUBLISH", Reason: "publish only after authoritative model and price review", TargetResourceType: "MODEL", TargetResourceID: modelID.String(), PrecheckFingerprint: precheck.PrecheckFingerprint, Command: command}, versions)
	response := performModelPublishGovernedCommand(t, service, envelope, "model-publish-precheck-0001")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "COMMAND_PRECHECK_CHANGED") {
		t.Fatalf("drift response=%d %s", response.Code, response.Body.String())
	}
	assertNoModelPublishCommand(t, ctx, repository, actorID)

	// Restore READY and freeze a second snapshot, then mutate a route fact without
	// changing any coverage count. The fixed-order route fingerprint must still
	// invalidate the command before a result row can be created.
	if _, err := repository.pool.Exec(ctx, `UPDATE route_provisionings SET status='READY',stage='READY',updated_at=transaction_timestamp() WHERE id=$1`, provisioningID); err != nil {
		t.Fatal(err)
	}
	equalCountPrecheck, err := service.buildTokenCommandPrecheck(ctx, TokenCommandPrecheckRequest{CommandType: "TOKEN_MODEL_PUBLISH", TargetResourceType: "MODEL", TargetResourceID: modelID.String(), Command: command})
	if err != nil || len(equalCountPrecheck.Blockers) != 0 {
		t.Fatalf("equal-count baseline precheck=%+v err=%v", equalCountPrecheck, err)
	}
	if _, err := repository.pool.Exec(ctx, `UPDATE routing_policies SET priority=priority+1,updated_at=transaction_timestamp() WHERE id=$1`, routeID); err != nil {
		t.Fatal(err)
	}
	changedCountPrecheck, err := service.buildTokenCommandPrecheck(ctx, TokenCommandPrecheckRequest{CommandType: "TOKEN_MODEL_PUBLISH", TargetResourceType: "MODEL", TargetResourceID: modelID.String(), Command: command})
	if err != nil || len(changedCountPrecheck.Blockers) != 0 || changedCountPrecheck.PrecheckFingerprint == equalCountPrecheck.PrecheckFingerprint {
		t.Fatalf("equal-count route mutation was not fingerprinted: before=%s after=%s blockers=%v err=%v", equalCountPrecheck.PrecheckFingerprint, changedCountPrecheck.PrecheckFingerprint, changedCountPrecheck.Blockers, err)
	}
	equalCountVersions := cloneExpectedVersions(t, equalCountPrecheck.ExpectedVersions)
	equalCountEnvelope := envelope
	equalCountEnvelope.PrecheckFingerprint = equalCountPrecheck.PrecheckFingerprint
	equalCountEnvelope = bindModelPublishRequestFingerprint(t, equalCountEnvelope, equalCountVersions)
	response = performModelPublishGovernedCommand(t, service, equalCountEnvelope, "model-publish-precheck-0004")
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "COMMAND_PRECHECK_CHANGED") {
		t.Fatalf("equal-count drift response=%d %s", response.Code, response.Body.String())
	}
	assertNoModelPublishCommand(t, ctx, repository, actorID)

	wrongTarget := envelope
	wrongTarget.TargetResourceID = uuid.NewString()
	response = performModelPublishGovernedCommand(t, service, wrongTarget, "model-publish-precheck-0002")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INVALID_COMMAND") {
		t.Fatalf("wrong target response=%d %s", response.Code, response.Body.String())
	}
	assertNoModelPublishCommand(t, ctx, repository, actorID)

	extraCommand := envelope
	extraCommand.Command = json.RawMessage(`{"model_id":"` + modelID.String() + `","unexpected":1}`)
	response = performModelPublishGovernedCommand(t, service, extraCommand, "model-publish-precheck-0003")
	if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "INVALID_COMMAND") {
		t.Fatalf("extra command response=%d %s", response.Code, response.Body.String())
	}
	assertNoModelPublishCommand(t, ctx, repository, actorID)

	currentPrecheck, err := service.buildTokenCommandPrecheck(ctx, TokenCommandPrecheckRequest{CommandType: "TOKEN_MODEL_PUBLISH", TargetResourceType: "MODEL", TargetResourceID: modelID.String(), Command: explicitCommand})
	if err != nil || len(currentPrecheck.Blockers) != 0 {
		t.Fatalf("current publish precheck=%+v err=%v", currentPrecheck, err)
	}
	currentVersions := cloneExpectedVersions(t, currentPrecheck.ExpectedVersions)
	explicitEnvelope := bindModelPublishRequestFingerprint(t, TokenCommandEnvelope{CommandType: "TOKEN_MODEL_PUBLISH", Reason: "publish normalized compatibility command after review", TargetResourceType: "MODEL", TargetResourceID: modelID.String(), PrecheckFingerprint: currentPrecheck.PrecheckFingerprint, Command: explicitCommand}, currentVersions)
	legacyEnvelope := explicitEnvelope
	legacyEnvelope.Command = command
	first := performModelPublishGovernedCommand(t, service, legacyEnvelope, "model-publish-legacy-replay")
	second := performModelPublishGovernedCommand(t, service, explicitEnvelope, "model-publish-legacy-replay")
	var firstResult, secondResult TokenCommandResult
	if first.Code != http.StatusCreated || second.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &firstResult) != nil || json.Unmarshal(second.Body.Bytes(), &secondResult) != nil || firstResult.CommandReference != secondResult.CommandReference || firstResult.Status != "SUCCEEDED" {
		t.Fatalf("legacy replay first=%d/%s second=%d/%s", first.Code, first.Body.String(), second.Code, second.Body.String())
	}
}

func performModelPublishGovernedCommand(t *testing.T, service *Service, envelope TokenCommandEnvelope, key string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/admin/v1/commands", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	request.Header.Set("Authorization", "Bearer integration")
	response := httptest.NewRecorder()
	handler := identity.Middleware(func(context.Context, string) (identity.Principal, error) {
		return identity.Principal{Issuer: "https://issuer.integration.example", Subject: "model-operator", SessionID: "model-publish-session", AuthenticatedAt: time.Now().UTC()}, nil
	}, http.HandlerFunc(service.adminCreateTokenCommand))
	handler.ServeHTTP(response, request)
	return response
}

func assertNoModelPublishCommand(t *testing.T, ctx context.Context, repository *PostgresRepository, actorID uuid.UUID) {
	t.Helper()
	var count int
	if err := repository.pool.QueryRow(ctx, `SELECT count(*) FROM token_command_results WHERE actor_user_id=$1 AND command_type='TOKEN_MODEL_PUBLISH'`, actorID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("model publish command count=%d err=%v", count, err)
	}
}

func cloneExpectedVersions(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func mustMarshalJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
